package agent

import (
	"context"
	"deployer/internal/model"
	"deployer/internal/packages"
	"deployer/internal/store"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Purge is resumable: stop every target before deleting any directory, and
// remove inventory only after filesystem removal succeeds. No clean hook is
// needed: explicit removal includes version configuration, logs and data.
func (e *Engine) purge(ctx context.Context, o *model.Operation, d model.Deployment) error {
	next, err := e.purgeFiles(ctx, o, d, func(stage string) error { return e.phase(o, stage) }, func(s string) { _ = e.Event(*o, s) })
	if err != nil {
		requested, cancelErr := e.cancelRequested(o.ID)
		if cancelErr != nil {
			return cancelErr
		}
		if requested && ctx.Err() != nil {
			return e.finish(o, next, "needs_attention", "管理员已取消清理，保留当前现场，请管理员核对")
		}
		return e.finish(o, next, "failed", err.Error())
	}
	return e.finish(o, next, "succeeded", "")
}
func (e *Engine) purgeFiles(ctx context.Context, o *model.Operation, d model.Deployment, phase func(string) error, log func(string)) (model.Deployment, error) {
	all := o.Action == "purge_app" || o.Action == "detach"
	if all || o.Version == d.Current {
		d.DesiredState = "stopped"
	}
	root := filepath.Join(e.Root, "apps", o.AppID)
	versions := filepath.Join(root, "versions")
	for _, p := range []string{e.Root, filepath.Join(e.Root, "apps"), root, versions} {
		if err := packages.PlainDir(p); err != nil && !os.IsNotExist(err) {
			return d, err
		}
	}
	targets := map[string]bool{}
	if all {
		entries, err := os.ReadDir(versions)
		if err != nil && !os.IsNotExist(err) {
			return d, err
		}
		for _, entry := range entries {
			if !model.ValidName(entry.Name()) {
				return d, fmt.Errorf("非法版本目录")
			}
			targets[entry.Name()] = true
		}
	} else {
		targets[o.Version] = true
	}
	if err := e.DB.View(func(t *store.Tx) error {
		items, err := store.List[model.Installation](t, "installations")
		if err != nil {
			return err
		}
		for _, i := range items {
			if i.AppID == o.AppID && (all || i.Version == o.Version) {
				targets[i.Version] = true
			}
		}
		return nil
	}); err != nil {
		return d, err
	}
	if err := phase("stopping"); err != nil {
		return d, err
	}
	for version := range targets {
		sp := e.spec(*o, version, d.Config)
		if err := packages.PlainDir(sp.Dir()); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return d, err
		}
		for _, child := range []string{"run", "logs"} {
			if err := packages.PlainDir(filepath.Join(sp.Dir(), child)); err != nil && !os.IsNotExist(err) {
				return d, err
			}
		}
		if err := e.Supervisor.Stop(ctx, sp, log); err != nil {
			return d, fmt.Errorf("停止 %s 失败，目录保留: %v", version, err)
		}
		st, err := e.Supervisor.Status(ctx, sp)
		if err != nil || st.Running {
			return d, fmt.Errorf("无法确认 %s 已停止，目录保留: %v", version, err)
		}
	}
	if err := phase("removing"); err != nil {
		return d, err
	}
	if all {
		if err := os.RemoveAll(root); err != nil {
			return d, err
		}
	} else {
		if err := os.RemoveAll(filepath.Join(versions, o.Version)); err != nil {
			return d, err
		}
	}
	if all || targets[d.Current] {
		d.Current = ""
		d.Config = model.Config{}
		d.RunningVersion = ""
		d.RunState = "stopped"
		d.DesiredState = "stopped"
		d.Health = "unverified"
	}
	if all || targets[d.DesiredVersion] {
		d.DesiredVersion = ""
	}
	if err := e.currentLink(d); err != nil {
		return d, err
	}
	if !all {
		entries, err := os.ReadDir(versions)
		if err != nil && !os.IsNotExist(err) {
			return d, err
		}
		if len(entries) == 0 {
			if err := os.RemoveAll(root); err != nil {
				return d, err
			}
		}
	}
	// Sync surviving parents before acknowledging deletion.
	if err := store.SyncDir(versions); err != nil && !os.IsNotExist(err) {
		return d, err
	}
	if err := store.SyncDir(filepath.Join(e.Root, "apps")); err != nil && !os.IsNotExist(err) {
		return d, err
	}
	err := e.DB.Update(func(t *store.Tx) error {
		for version := range targets {
			if err := store.Delete(t, "installations", model.Key(o.AppID, version)); err != nil {
				return err
			}
		}
		if all || strings.HasPrefix(d.CleanupError, "删除 "+model.Key(o.AppID, o.Version)+" 失败:") {
			d.CleanupError = ""
		}
		if d.Current == "" {
			d.Error = ""
		}
		return store.Put(t, "deployments", o.AppID, d)
	})
	return d, err
}

// SyncDeleted executes a heartbeat instruction without creating an operation.
// Local state is kept on failure, so the next heartbeat requests cleanup again.
func (e *Engine) SyncDeleted(ctx context.Context, item model.Deleted) error {
	if !model.ValidName(item.AppID) || (item.Version != "" && !model.ValidName(item.Version)) {
		return fmt.Errorf("无效删除目标")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.active[item.AppID] {
		return nil
	}
	var d model.Deployment
	err := e.DB.View(func(t *store.Tx) error {
		ops, err := store.Query[model.Operation](t, "operations", store.Filter{Scope: item.AppID, States: []string{"active"}})
		if err != nil {
			return err
		}
		if len(ops) > 0 {
			return fmt.Errorf("应用操作尚未完成")
		}
		d, err = store.Get[model.Deployment](t, "deployments", item.AppID)
		if errors.Is(err, store.ErrNotFound) {
			d = model.Deployment{NodeID: e.NodeID, AppID: item.AppID}
			return nil
		}
		return err
	})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	o := model.Operation{AppID: item.AppID, Version: item.Version, Action: "remove_version"}
	if item.Version == "" {
		o.Action = "purge_app"
	}
	next, cleanupErr := e.purgeFiles(ctx, &o, d, func(string) error { return nil }, func(string) {})
	if cleanupErr != nil {
		next.CleanupError = fmt.Sprintf("删除 %s 失败: %v", model.Key(item.AppID, item.Version), cleanupErr)
		for _, v := range d.Config.Env {
			if len(v) >= 3 {
				next.CleanupError = strings.ReplaceAll(next.CleanupError, v, "[redacted]")
			}
		}
		if err = e.DB.Update(func(t *store.Tx) error { return store.Put(t, "deployments", item.AppID, next) }); err != nil {
			return err
		}
	}
	return cleanupErr
}
