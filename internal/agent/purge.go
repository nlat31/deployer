package agent

import (
	"context"
	"deployer/internal/model"
	"deployer/internal/packages"
	"deployer/internal/store"
	"fmt"
	"os"
	"path/filepath"
)

// Purge is resumable: stop every target before deleting any directory, and
// remove inventory only after filesystem removal succeeds. No clean hook is
// needed: explicit removal includes version configuration, logs and data.
func (e *Engine) purge(ctx context.Context, o *model.Operation, d model.Deployment) error {
	all := o.Action == "purge_app" || o.Action == "detach"
	if all || o.Version == d.Current {
		d.DesiredState = "stopped"
	}
	root := filepath.Join(e.Root, "apps", o.AppID)
	versions := filepath.Join(root, "versions")
	for _, p := range []string{e.Root, filepath.Join(e.Root, "apps"), root, versions} {
		if err := packages.PlainDir(p); err != nil && !os.IsNotExist(err) {
			return e.finish(o, d, "failed", err.Error())
		}
	}
	targets := map[string]bool{}
	if all {
		entries, err := os.ReadDir(versions)
		if err != nil && !os.IsNotExist(err) {
			return e.finish(o, d, "failed", err.Error())
		}
		for _, entry := range entries {
			if !model.ValidName(entry.Name()) {
				return e.finish(o, d, "failed", "非法版本目录")
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
		return err
	}
	if err := e.phase(o, "stopping"); err != nil {
		return err
	}
	for version := range targets {
		sp := e.spec(*o, version, d.Config)
		if err := packages.PlainDir(sp.Dir()); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return e.finish(o, d, "failed", err.Error())
		}
		for _, child := range []string{"run", "logs"} {
			if err := packages.PlainDir(filepath.Join(sp.Dir(), child)); err != nil && !os.IsNotExist(err) {
				return e.finish(o, d, "failed", err.Error())
			}
		}
		if err := e.Supervisor.Stop(ctx, sp, func(s string) { _ = e.Event(*o, s) }); err != nil {
			return e.finish(o, d, "failed", fmt.Sprintf("停止 %s 失败，目录保留: %v", version, err))
		}
		st, err := e.Supervisor.Status(ctx, sp)
		if err != nil || st.Running {
			return e.finish(o, d, "failed", fmt.Sprintf("无法确认 %s 已停止，目录保留: %v", version, err))
		}
	}
	if err := e.phase(o, "removing"); err != nil {
		return err
	}
	if all {
		if err := os.RemoveAll(root); err != nil {
			return e.finish(o, d, "failed", err.Error())
		}
	} else {
		if err := os.RemoveAll(filepath.Join(versions, o.Version)); err != nil {
			return e.finish(o, d, "failed", err.Error())
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
	if all || targets[d.Previous] {
		d.Previous = ""
		d.PreviousConfig = model.Config{}
	}
	if all || targets[d.DesiredVersion] {
		d.DesiredVersion = ""
	}
	if err := e.currentLink(d); err != nil {
		return err
	}
	if !all {
		entries, err := os.ReadDir(versions)
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		if len(entries) == 0 {
			if err := os.RemoveAll(root); err != nil {
				return err
			}
		}
	}
	// Sync surviving parents before acknowledging deletion.
	if err := store.SyncDir(versions); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := store.SyncDir(filepath.Join(e.Root, "apps")); err != nil && !os.IsNotExist(err) {
		return err
	}
	return e.DB.Update(func(t *store.Tx) error {
		for version := range targets {
			if err := store.Delete(t, "installations", model.Key(o.AppID, version)); err != nil {
				return err
			}
		}
		return e.finishTx(t, o, d, "succeeded", "")
	})
}
