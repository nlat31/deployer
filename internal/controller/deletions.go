package controller

import (
	"deployer/internal/model"
	"deployer/internal/store"
	"errors"
	"net/http"
	"sort"
)

// Repository contents are authoritative. Cleanup is derived anew from each
// heartbeat; no deletion jobs, acknowledgement barriers or tombstones exist.
func (s *Server) requestDeletion(w http.ResponseWriter, r *http.Request, version string) error {
	app := r.PathValue("app")
	if !model.ValidName(app) || (version != "" && !model.ValidName(version)) || r.URL.Query().Get("confirm") != "true" {
		return fail(400, "请明确确认停止并永久删除客户端目录及数据")
	}
	s.packagesMu.Lock()
	defer s.packagesMu.Unlock()
	err := s.DB.Update(func(t *store.Tx) error {
		ops, e := store.List[model.Operation](t, "operations")
		if e != nil {
			return e
		}
		for _, o := range ops {
			if o.AppID == app && !o.Terminal() {
				return fail(409, "应用有执行中的操作，请完成后重试删除")
			}
		}
		batches, e := store.List[Batch](t, "batches")
		if e != nil {
			return e
		}
		for _, b := range batches {
			if b.AppID == app && b.State == "running" {
				return fail(409, "应用仍有批量部署任务")
			}
		}
		vs, e := store.List[model.Version](t, "versions")
		if e != nil {
			return e
		}
		remaining := false
		for _, v := range vs {
			if v.AppID != app {
				continue
			}
			if version != "" && v.Version != version && v.Status != "deleted" {
				remaining = true
				continue
			}
			if e = store.Delete(t, "versions", model.Key(app, v.Version)); e != nil {
				return e
			}
		}
		if version == "" || !remaining {
			if e = store.Delete(t, "apps", app); e != nil {
				return e
			}
		}
		return audit(t, actor(r).ID, "delete_repository", model.Key(app, version))
	})
	if err != nil {
		return err
	}
	// Unreferenced files are also swept at startup, so an interrupted removal
	// converges without introducing deletion state into the database.
	if err = s.recoverImports(); err != nil {
		return err
	}
	jsonOut(w, 200, map[string]bool{"ok": true})
	return nil
}
func deletedFor(t *store.Tx, p model.Poll) ([]model.Deleted, error) {
	candidates := map[string]map[string]bool{}
	add := func(app, version string) {
		if candidates[app] == nil {
			candidates[app] = map[string]bool{}
		}
		if version != "" {
			candidates[app][version] = true
		}
	}
	for _, app := range p.Apps {
		if !model.ValidName(app) {
			return nil, fail(400, "无效应用目录")
		}
		add(app, "")
	}
	for _, i := range p.Installations {
		add(i.AppID, i.Version)
	}
	for _, d := range p.Deployments {
		if d.Current == "" && d.DesiredVersion == "" && d.RunningVersion == "" && d.CleanupError == "" {
			continue
		}
		add(d.AppID, d.Current)
		add(d.AppID, d.DesiredVersion)
		add(d.AppID, d.RunningVersion)
	}
	out := []model.Deleted{}
	for app, versions := range candidates {
		_, err := store.Get[model.App](t, "apps", app)
		if errors.Is(err, store.ErrNotFound) {
			out = append(out, model.Deleted{AppID: app})
			continue
		}
		if err != nil {
			return nil, err
		}
		for version := range versions {
			v, err := store.Get[model.Version](t, "versions", model.Key(app, version))
			if errors.Is(err, store.ErrNotFound) || err == nil && (v.Status == "deleted" || v.Status == "deleting" || v.Status == "removing_clients") {
				out = append(out, model.Deleted{AppID: app, Version: version})
			} else if err != nil {
				return nil, err
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return model.Key(out[i].AppID, out[i].Version) < model.Key(out[j].AppID, out[j].Version)
	})
	return out, nil
}
