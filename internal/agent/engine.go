package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"deployer/internal/model"
	"deployer/internal/packages"
	"deployer/internal/store"
	"deployer/internal/supervisor"
)

type Downloader func(context.Context, model.Version, string) error
type Engine struct {
	Root       string
	NodeID     string
	DB         *store.Store
	Supervisor supervisor.Interface
	Download   Downloader
	mu         sync.Mutex
	active     map[string]bool
	cacheMu    sync.RWMutex
}

func NewEngine(root, node string, db *store.Store, sup supervisor.Interface, download Downloader) *Engine {
	return &Engine{Root: root, NodeID: node, DB: db, Supervisor: sup, Download: download, active: map[string]bool{}}
}
func (e *Engine) spec(o model.Operation, version string, c model.Config) supervisor.Spec {
	return supervisor.Spec{Root: e.Root, AppID: o.AppID, Version: version, OperationID: o.ID, Env: c.Env}
}
func (e *Engine) Event(o model.Operation, message string) error {
	for _, v := range o.Config.Env {
		if len(v) >= 3 {
			message = strings.ReplaceAll(message, v, "[redacted]")
		}
	}
	for _, v := range o.Before.Config.Env {
		if len(v) >= 3 {
			message = strings.ReplaceAll(message, v, "[redacted]")
		}
	}
	if len(message) > 12000 {
		message = message[:12000] + "\n[输出截断]"
	}
	return e.DB.Update(func(t *store.Tx) error {
		n, err := store.LastSeq(t, o.ID)
		if err != nil {
			return err
		}
		ev := model.Event{OperationID: o.ID, Seq: n + 1, Time: model.Now(), Stage: o.Stage, Message: message}
		if err = store.AddEvent(t, o.ID, ev.Seq, ev); err != nil {
			return err
		}
		if err = store.TrimEvents(t, o.ID, 2000); err != nil {
			return err
		}
		return store.Put(t, "event_pending", o.ID, o.ID)
	})
}
func (e *Engine) phase(o *model.Operation, stage string) error {
	o.Stage = stage
	o.UpdatedAt = model.Now()
	if err := e.DB.Update(func(t *store.Tx) error { return store.Put(t, "operations", o.ID, *o) }); err != nil {
		return err
	}
	return e.Event(*o, stage)
}
func (e *Engine) Accept(o model.Operation) (bool, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if o.NodeID != e.NodeID || !model.ValidName(o.AppID) || !model.ValidName(o.ID) || len(o.ID) < 12 {
		return false, fmt.Errorf("无效任务")
	}
	if o.Version != "" && !model.ValidName(o.Version) {
		return false, fmt.Errorf("无效版本")
	}
	if err := model.ValidEnv(o.Config.Env); err != nil {
		return false, err
	}
	fresh := false
	err := e.DB.Update(func(t *store.Tx) error {
		old, err := store.Get[model.Operation](t, "operations", o.ID)
		if err == nil {
			if old.NodeID != o.NodeID || old.AppID != o.AppID {
				return fmt.Errorf("任务身份不匹配")
			}
			return nil
		}
		if !errors.Is(err, store.ErrNotFound) {
			return err
		}
		d, err := store.Get[model.Deployment](t, "deployments", o.AppID)
		if errors.Is(err, store.ErrNotFound) {
			d = model.Deployment{NodeID: e.NodeID, AppID: o.AppID, DesiredState: "stopped", RunState: "stopped"}
		} else if err != nil {
			return err
		}
		if (d.Revision != o.ExpectedRevision && !(o.Action == "reconcile" && o.ExpectedRevision >= d.Revision)) || o.Revision != o.ExpectedRevision+1 {
			return fmt.Errorf("任务前置修订不匹配")
		}
		ops, err := store.Query[model.Operation](t, "operations", store.Filter{Owner: e.NodeID, Scope: o.AppID, States: []string{"active", "needs_attention"}})
		if err != nil {
			return err
		}
		for _, x := range ops {
			if x.AppID == o.AppID && (!x.Terminal() || (x.Result == "needs_attention" && o.Action != "reconcile")) {
				return fmt.Errorf("此 App 有未完成或需处理的任务")
			}
		}
		o.Before = d
		o.Stage = "accepted"
		o.Result = ""
		o.Acked = false
		d.Revision = o.Revision
		if o.Action == "remove_version" && o.Version == d.Current {
			d.DesiredState = "stopped"
		}
		switch o.Action {
		case "deploy", "switch", "start", "restart":
			d.DesiredVersion = o.Version
			d.DesiredState = "stopped"
			if o.Start {
				d.DesiredState = "running"
			}
		case "stop", "detach", "purge_app", "reconcile":
			d.DesiredState = "stopped"
		}
		if err = store.Put(t, "deployments", o.AppID, d); err != nil {
			return err
		}
		if err = store.Put(t, "operations", o.ID, o); err != nil {
			return err
		}
		fresh = true
		return nil
	})
	return fresh, err
}

func (e *Engine) Run(ctx context.Context, id string) error {
	var o model.Operation
	if err := e.DB.View(func(t *store.Tx) error {
		var err error
		o, err = store.Get[model.Operation](t, "operations", id)
		return err
	}); err != nil {
		return err
	}
	if o.Terminal() {
		return nil
	}
	e.mu.Lock()
	if e.active[o.AppID] {
		e.mu.Unlock()
		return fmt.Errorf("App 正在执行操作")
	}
	e.active[o.AppID] = true
	e.mu.Unlock()
	defer func() { e.mu.Lock(); delete(e.active, o.AppID); e.mu.Unlock() }()
	ctx, cancel := context.WithTimeout(ctx, 20*time.Minute)
	defer cancel()
	log := func(s string) { _ = e.Event(o, s) }
	d := o.Before
	d.Revision = o.Revision
	d.Error = ""
	var err error
	disturbed := false
	switch o.Action {
	case "reconcile":
		if err = e.phase(&o, "stopping"); err != nil {
			break
		}
		var installations []model.Installation
		var previousOps []model.Operation
		if err = e.DB.View(func(t *store.Tx) error {
			var er error
			installations, er = store.List[model.Installation](t, "installations")
			if er == nil {
				previousOps, er = store.List[model.Operation](t, "operations")
			}
			return er
		}); err != nil {
			break
		}
		disturbed = true
		if q, ok := e.Supervisor.(interface {
			Quiesce(context.Context, supervisor.Spec) error
		}); ok {
			for _, old := range previousOps {
				if old.AppID != o.AppID || old.Result != "needs_attention" {
					continue
				}
				for _, version := range []string{old.Version, old.Before.Current} {
					if version == "" {
						continue
					}
					if err = q.Quiesce(ctx, e.spec(old, version, old.Config)); err != nil {
						break
					}
				}
				if err != nil {
					break
				}
			}
		}
		if err != nil {
			break
		}
		for _, ins := range installations {
			if ins.AppID != o.AppID {
				continue
			}
			if err = e.Supervisor.Stop(ctx, e.spec(o, ins.Version, d.Config), log); err != nil {
				break
			}
		}
		if err != nil {
			break
		}
		d.DesiredState = "stopped"
		d.DesiredVersion = d.Current
		d.RunState = "stopped"
		d.RunningVersion = ""
		d.Health = "unverified"
		return e.DB.Update(func(t *store.Tx) error {
			ops, er := store.List[model.Operation](t, "operations")
			if er != nil {
				return er
			}
			for _, old := range ops {
				if old.AppID == o.AppID && old.Result == "needs_attention" {
					old.Result = "failed"
					old.Error += "；已通过操作 " + o.ID + " 确认所有版本停止"
					old.Acked = false
					if er = store.Put(t, "operations", old.ID, old); er != nil {
						return er
					}
					if er = store.Put(t, "outbox", old.ID, old); er != nil {
						return er
					}
				}
			}
			for _, ins := range installations {
				if ins.AppID == o.AppID {
					ins.Initialized = false
					if er = store.Put(t, "installations", model.Key(ins.AppID, ins.Version), ins); er != nil {
						return er
					}
				}
			}
			return e.finishTx(t, &o, d, "succeeded", "")
		})
	case "install", "deploy", "switch", "start", "restart":
		if err = e.phase(&o, "preparing"); err == nil {
			err = e.prepare(ctx, &o, log)
		}
		if err != nil {
			break
		}
		if o.Action == "install" {
			break
		}
		if d.Current != "" && o.Action != "start" {
			if err = e.phase(&o, "stopping"); err != nil {
				break
			}
			disturbed = true
			err = e.Supervisor.Stop(ctx, e.spec(o, d.Current, d.Config), log)
			if err != nil {
				break
			}
		}
		if o.Start {
			if err = e.phase(&o, "starting"); err != nil {
				break
			}
			disturbed = true
			sp := e.spec(o, o.Version, o.Config)
			if err = e.Supervisor.Start(ctx, sp); err != nil {
				break
			}
			st, statusErr := e.Supervisor.Status(ctx, sp)
			if statusErr != nil {
				err = statusErr
				break
			}
			if err = e.recordStartedAt(o.AppID, o.Version, st.StartedAt); err != nil {
				break
			}
			if err = e.phase(&o, "checking"); err != nil {
				break
			}
			if err = supervisor.Healthy(ctx, e.Supervisor, sp, o.Config.Health); err != nil {
				break
			}
			d.RunState = "running"
			d.RunningVersion = o.Version
			d.Health = "healthy"
			if o.Config.Health.Type == "process" {
				d.Health = "process_running"
			}
		} else {
			d.RunState = "stopped"
			d.RunningVersion = ""
			d.Health = "unverified"
		}
		d.Current = o.Version
		d.Config = o.Config
		d.DesiredVersion = o.Version
		d.DesiredState = "stopped"
		if o.Start {
			d.DesiredState = "running"
		}
	case "purge_app", "detach", "remove_version":
		return e.purge(ctx, &o, d)
	case "stop":
		if err = e.phase(&o, "stopping"); err != nil {
			break
		}
		if d.Current != "" {
			disturbed = true
			err = e.Supervisor.Stop(ctx, e.spec(o, d.Current, d.Config), log)
			if err != nil {
				break
			}
		}
		d.DesiredState = "stopped"
		d.RunState = "stopped"
		d.RunningVersion = ""
		d.Health = "unverified"

	case "clean_data":
		if o.Version == d.Current && d.DesiredState != "stopped" {
			err = fmt.Errorf("必须先停止当前版本")
			break
		}
		var ins model.Installation
		err = e.DB.View(func(t *store.Tx) error {
			var er error
			ins, er = store.Get[model.Installation](t, "installations", model.Key(o.AppID, o.Version))
			return er
		})
		if err != nil {
			break
		}
		if ins.DataEpoch != o.DataEpoch {
			err = fmt.Errorf("数据代次已变化，拒绝旧清理任务")
			break
		}
		sp := e.spec(o, o.Version, d.Config)
		var st supervisor.Status
		st, err = e.Supervisor.Status(ctx, sp)
		if err != nil {
			break
		}
		if st.Running {
			err = fmt.Errorf("目标版本仍有进程运行")
			break
		}
		if err = e.phase(&o, "cleaning"); err != nil {
			break
		}
		if err = packages.PlainDir(filepath.Join(sp.Dir(), "data")); err != nil {
			break
		}
		ins.Initialized = false
		if err = e.DB.Update(func(t *store.Tx) error { return store.Put(t, "installations", model.Key(o.AppID, o.Version), ins) }); err != nil {
			break
		}
		if err = e.Supervisor.Hook(ctx, sp, "clean.sh", log); err != nil {
			break
		}
		entries, er := os.ReadDir(filepath.Join(sp.Dir(), "data"))
		if er != nil {
			err = er
			break
		}
		if len(entries) > 0 {
			err = fmt.Errorf("clean.sh 未清空 data 目录")
			break
		}
		ins.Initialized = false
		ins.DataEpoch++
		ins.DataBytes = 0
		err = e.DB.Update(func(t *store.Tx) error {
			if er := store.Put(t, "installations", model.Key(o.AppID, o.Version), ins); er != nil {
				return er
			}
			return e.finishTx(t, &o, d, "succeeded", "")
		})
		return err
	case "logs":
		if d.Current == "" {
			err = fmt.Errorf("没有当前版本")
			break
		}
		var out string
		out, err = e.Supervisor.Logs(ctx, e.spec(o, d.Current, d.Config))
		log(out)
	default:
		err = fmt.Errorf("未知操作 %s", o.Action)
	}
	if ctx.Err() != nil {
		_ = e.Event(o, "执行上下文结束；保留阶段记录供重启恢复")
		return ctx.Err()
	}
	if err != nil {
		_ = e.Event(o, err.Error())
		if disturbed && (o.Action == "deploy" || o.Action == "switch" || o.Action == "start" || o.Action == "restart") {
			return e.reportFailure(ctx, &o, err.Error())
		}
		if disturbed {
			d.DesiredState = "stopped"
			d.RunState = "unknown"
			d.Health = "unknown"
			return e.finish(&o, d, "needs_attention", err.Error())
		}
		return e.finish(&o, d, "failed", err.Error())
	}
	if err = e.phase(&o, "committing"); err != nil {
		return err
	}
	return e.finish(&o, d, "succeeded", "")
}

func (e *Engine) finishTx(t *store.Tx, o *model.Operation, d model.Deployment, result, message string) error {
	for _, cfg := range []model.Config{o.Config, o.Before.Config} {
		for _, value := range cfg.Env {
			if len(value) >= 3 {
				message = strings.ReplaceAll(message, value, "[redacted]")
			}
		}
	}
	o.Result = result
	o.Stage = "finished"
	o.Error = message
	o.UpdatedAt = model.Now()
	o.FinishedAt = o.UpdatedAt
	o.Acked = false
	d.NodeID = e.NodeID
	d.AppID = o.AppID
	d.Revision = o.Revision
	d.ObservedAt = model.Now()
	d.Error = message
	if err := store.Put(t, "deployments", o.AppID, d); err != nil {
		return err
	}
	if err := store.Put(t, "operations", o.ID, *o); err != nil {
		return err
	}
	return store.Put(t, "outbox", o.ID, *o)
}
func (e *Engine) finish(o *model.Operation, d model.Deployment, result, message string) error {
	err := e.DB.Update(func(t *store.Tx) error { return e.finishTx(t, o, d, result, message) })
	if err != nil {
		return err
	}
	_ = e.Event(*o, result+": "+message)
	return e.currentLink(d)
}
func (e *Engine) currentLink(d model.Deployment) error {
	root := filepath.Join(e.Root, "apps", d.AppID)
	for _, p := range []string{e.Root, filepath.Join(e.Root, "apps"), root} {
		if err := packages.PlainDir(p); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	link := filepath.Join(root, "current")
	if d.Current == "" {
		err := os.Remove(link)
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if er := os.MkdirAll(root, 0755); er != nil {
		return er
	}
	temp := filepath.Join(root, ".current-"+model.ID())
	if err := os.Symlink(filepath.Join("versions", d.Current), temp); err != nil {
		return err
	}
	defer os.Remove(temp)
	if err := os.Rename(temp, link); err != nil {
		return err
	}
	return store.SyncDir(root)
}

// Record the selected target and observed process without stopping, restarting,
// or rolling back application versions. Only an administrator changes versions.
func (e *Engine) reportFailure(ctx context.Context, o *model.Operation, cause string) error {
	d := o.Before
	d.Revision = o.Revision
	if o.Stage == "stopping" {
		d.DesiredState = "stopped"
		d.RunState = "unknown"
		d.Health = "unknown"
		return e.finish(o, d, "needs_attention", cause)
	}
	if d.Current != "" && d.Current != o.Version {
		old, err := e.Supervisor.Status(ctx, e.spec(*o, d.Current, d.Config))
		if err != nil || old.Running {
			d.DesiredState = "stopped"
			d.RunState = "unknown"
			d.Health = "unknown"
			return e.finish(o, d, "needs_attention", cause+"；旧版本状态需管理员核对")
		}
	}
	d.Current = o.Version
	d.Config = o.Config
	d.DesiredVersion = o.Version
	d.DesiredState = "stopped"
	if o.Start {
		d.DesiredState = "running"
	}
	st, err := e.Supervisor.Status(ctx, e.spec(*o, o.Version, o.Config))
	if err != nil {
		d.RunState = "unknown"
		d.Health = "unknown"
		return e.finish(o, d, "needs_attention", cause+"；进程状态检查失败: "+err.Error())
	}
	d.RunState = "stopped"
	d.RunningVersion = ""
	d.Health = "unhealthy"
	if st.Running {
		d.RunState = "running"
		d.RunningVersion = o.Version
	}
	return e.finish(o, d, "failed", cause)
}

func (e *Engine) prepare(ctx context.Context, o *model.Operation, log func(string)) error {
	e.cacheMu.RLock()
	defer e.cacheMu.RUnlock()
	if o.Artifact == nil || o.Artifact.AppID != o.AppID || o.Artifact.Version != o.Version {
		return fmt.Errorf("缺少匹配的包信息")
	}
	v := *o.Artifact
	if v.OS != runtime.GOOS || v.Arch != runtime.GOARCH {
		return fmt.Errorf("版本包平台与当前主机不匹配")
	}
	if len(v.Digest) != 64 || strings.Trim(v.Digest, "0123456789abcdef") != "" || v.Size < 1 || v.Size > packages.MaxArchive {
		return fmt.Errorf("无效的版本包摘要或大小")
	}
	sp := e.spec(*o, o.Version, o.Config)
	var ins model.Installation
	err := e.DB.View(func(t *store.Tx) error {
		var er error
		ins, er = store.Get[model.Installation](t, "installations", model.Key(o.AppID, o.Version))
		return er
	})
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	if ins.Digest != "" && ins.Digest != v.Digest {
		return fmt.Errorf("已安装版本摘要不匹配")
	}
	if ins.Digest == "" {
		if _, er := os.Lstat(sp.Dir()); er == nil {
			return fmt.Errorf("发现未登记的版本目录，拒绝覆盖")
		}
		ins = model.Installation{AppID: o.AppID, Version: o.Version, Digest: v.Digest, DataEpoch: 1}
		if err = e.DB.Update(func(t *store.Tx) error { return store.Put(t, "installations", model.Key(o.AppID, o.Version), ins) }); err != nil {
			return err
		}
	}
	if _, err = os.Stat(sp.Dir()); os.IsNotExist(err) {
		archive := filepath.Join(e.Root, "cache", v.Digest+".tar.gz")
		d, er := packages.Digest(archive)
		if er != nil || d != v.Digest {
			tmp := archive + ".part-" + o.ID
			if er = e.Download(ctx, v, tmp); er != nil {
				os.Remove(tmp)
				return er
			}
			d, er = packages.Digest(tmp)
			if er != nil || d != v.Digest {
				os.Remove(tmp)
				return fmt.Errorf("下载包摘要不匹配")
			}
			if er = os.Rename(tmp, archive); er != nil {
				return er
			}
			if er = store.SyncDir(filepath.Dir(archive)); er != nil {
				return er
			}
		}
		stage := filepath.Join(e.Root, "staging", o.ID)
		if er = os.RemoveAll(stage); er != nil {
			return er
		}
		if _, er = packages.Extract(archive, stage, v); er != nil {
			return er
		}
		if er = os.MkdirAll(filepath.Dir(sp.Dir()), 0755); er != nil {
			return er
		}
		for _, parent := range []string{filepath.Join(e.Root, "apps"), filepath.Join(e.Root, "apps", o.AppID), filepath.Dir(sp.Dir())} {
			if er = os.Chmod(parent, 0755); er != nil {
				return er
			}
		}
		if er = os.Rename(stage, sp.Dir()); er != nil {
			return er
		}
		if er = store.SyncDir(filepath.Dir(sp.Dir())); er != nil {
			return er
		}
	} else if err != nil {
		return err
	}
	if err = packages.PlainDir(sp.Dir()); err != nil {
		return err
	}
	// Registration precedes downloading for crash recovery. Only timestamp a
	// deployment once its validated package actually exists on disk.
	if ins.InstalledAt == 0 {
		ins.InstalledAt = model.Now()
		if err = e.DB.Update(func(t *store.Tx) error { return store.Put(t, "installations", model.Key(o.AppID, o.Version), ins) }); err != nil {
			return err
		}
	}
	for _, dir := range []string{"config", "data", "logs", "run"} {
		if err = os.MkdirAll(filepath.Join(sp.Dir(), dir), 0700); err != nil {
			return err
		}
		if err = packages.PlainDir(filepath.Join(sp.Dir(), dir)); err != nil {
			return err
		}
	}
	if !ins.Initialized {
		if err = copyDefaults(filepath.Join(sp.Dir(), "config.defaults"), filepath.Join(sp.Dir(), "config")); err != nil {
			return err
		}
		if err = e.Supervisor.Prepare(sp); err != nil {
			return err
		}
		if err = e.phase(o, "setting_up"); err != nil {
			return err
		}
		if err = e.Supervisor.Hook(ctx, sp, "setup.sh", log); err != nil {
			return err
		}
		ins.Initialized = true
		return e.DB.Update(func(t *store.Tx) error { return store.Put(t, "installations", model.Key(o.AppID, o.Version), ins) })
	}
	return nil
}
func copyDefaults(src, dst string) error {
	if _, err := os.Stat(src); os.IsNotExist(err) {
		return nil
	}
	return filepath.WalkDir(src, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("默认配置不能包含链接")
		}
		if d.IsDir() {
			return os.MkdirAll(target, 0700)
		}
		if _, err = os.Lstat(target); err == nil {
			return nil
		}
		in, err := os.Open(p)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		_, err = io.Copy(out, in)
		if err == nil {
			err = out.Sync()
		}
		ce := out.Close()
		if err == nil {
			err = ce
		}
		return err
	})
}

func (e *Engine) Recover(ctx context.Context) error {
	var ops []model.Operation
	err := e.DB.View(func(t *store.Tx) error {
		var er error
		ops, er = store.Query[model.Operation](t, "operations", store.Filter{States: []string{"active"}})
		return er
	})
	if err != nil {
		return err
	}
	for _, o := range ops {
		if o.Terminal() {
			continue
		}
		if err := e.RecoverOperation(ctx, o); err != nil {
			return err
		}
	}
	return nil
}
func (e *Engine) RecoverOperation(ctx context.Context, o model.Operation) error {
	if q, ok := e.Supervisor.(interface {
		Quiesce(context.Context, supervisor.Spec) error
	}); ok && o.Version != "" {
		if er := q.Quiesce(ctx, e.spec(o, o.Version, o.Config)); er != nil {
			return er
		}
		if o.Before.Current != "" && o.Before.Current != o.Version {
			if er := q.Quiesce(ctx, e.spec(o, o.Before.Current, o.Before.Config)); er != nil {
				return er
			}
		}
	}
	switch o.Stage {
	case "starting", "checking", "recovering":
		if er := e.reportFailure(ctx, &o, "Agent 重启时发现未提交的版本切换，请管理员核对"); er != nil {
			return er
		}
	case "stopping":
		if o.Action == "switch" || o.Action == "deploy" || o.Action == "restart" || o.Action == "start" {
			if er := e.reportFailure(ctx, &o, "Agent 重启时发现中断的停止阶段，请管理员核对"); er != nil {
				return er
			}
		} else if er := e.Run(ctx, o.ID); er != nil {
			return er
		}
	default:
		if er := e.Run(ctx, o.ID); er != nil {
			return er
		}
	}
	return nil
}

func (e *Engine) Observe(ctx context.Context) error {
	var deps []model.Deployment
	if err := e.DB.View(func(t *store.Tx) error {
		var er error
		deps, er = store.List[model.Deployment](t, "deployments")
		return er
	}); err != nil {
		return err
	}
	for _, d := range deps {
		if err := e.observeOne(ctx, d); err != nil {
			return err
		}
	}
	return nil
}

func (e *Engine) observeOne(ctx context.Context, d model.Deployment) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	active := e.active[d.AppID]
	if active {
		return nil
	}
	if err := e.DB.View(func(t *store.Tx) error {
		var err error
		d, err = store.Get[model.Deployment](t, "deployments", d.AppID)
		if err != nil {
			return err
		}
		ops, err := store.Query[model.Operation](t, "operations", store.Filter{Owner: e.NodeID, Scope: d.AppID, States: []string{"active", "needs_attention"}, Limit: 1})
		if err == nil && len(ops) > 0 {
			active = true
		}
		return err
	}); err != nil {
		return err
	}
	if active {
		return nil
	}
	if d.Current == "" {
		return nil
	}
	sp := supervisor.Spec{Root: e.Root, AppID: d.AppID, Version: d.Current, OperationID: model.ID(), Env: d.Config.Env}
	st, err := e.Supervisor.Status(ctx, sp)
	if err == nil {
		if recordErr := e.recordStartedAt(d.AppID, d.Current, st.StartedAt); recordErr != nil {
			return recordErr
		}
	}
	d.ObservedAt = model.Now()
	if err != nil {
		d.RunState = "unknown"
		d.Health = "unknown"
		d.Error = "进程状态检查失败: " + err.Error()
	} else if st.Running {
		d.RunState = "running"
		d.RunningVersion = d.Current
		if err := supervisor.Probe(ctx, e.Supervisor, sp, d.Config.Health); err != nil {
			d.Health = "unhealthy"
			d.Error = err.Error()
		} else if d.Config.Health.Type == "process" {
			d.Health = "process_running"
			d.Error = ""
		} else {
			d.Health = "healthy"
			d.Error = ""
		}
	} else {
		d.RunState = "stopped"
		d.RunningVersion = ""
		d.Health = "unverified"
		if d.DesiredState == "running" {
			d.Health = "unhealthy"
			if d.Error == "" {
				d.Error = "主进程未运行"
			}
		}
	}
	// Health bodies are application-owned; apply the same configured-secret
	// redaction as task errors before publishing them to the controller.
	for _, cfg := range []model.Config{d.Config} {
		for _, value := range cfg.Env {
			if len(value) >= 3 {
				d.Error = strings.ReplaceAll(d.Error, value, "[redacted]")
			}
		}
	}
	if err = e.DB.Update(func(t *store.Tx) error {
		current, er := store.Get[model.Deployment](t, "deployments", d.AppID)
		if er != nil {
			return er
		}
		if current.Revision != d.Revision {
			return nil
		}
		return store.Put(t, "deployments", d.AppID, d)
	}); err != nil {
		return err
	}
	_ = e.currentLink(d)
	return nil
}

// Startup only observes applications. A previous running intent is not a new
// administrator command and must never start an application after a host reboot.
func (e *Engine) Boot(ctx context.Context) error {
	return e.Observe(ctx)
}

// The process record survives Agent restarts and interrupted operations. A
// repeated start.sh invocation keeps that record, so it cannot reset this time.
func (e *Engine) recordStartedAt(app, version string, startedAt int64) error {
	if startedAt <= 0 {
		return nil
	}
	return e.DB.Update(func(t *store.Tx) error {
		key := model.Key(app, version)
		ins, err := store.Get[model.Installation](t, "installations", key)
		if err != nil {
			return err
		}
		if ins.StartedAt == startedAt {
			return nil
		}
		ins.StartedAt = startedAt
		return store.Put(t, "installations", key, ins)
	})
}
