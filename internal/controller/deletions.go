package controller

import (
	"deployer/internal/model"
	"deployer/internal/store"
	"errors"
	"net/http"
)

type Deletion struct {
	ID         string   `json:"id"`
	AppID      string   `json:"app_id"`
	Version    string   `json:"version"`
	Operations []string `json:"operations"`
	State      string   `json:"state"`
	Error      string   `json:"error,omitempty"`
}

func deletionUnlocked(t *store.Tx, app string) error {
	jobs, err := store.List[Deletion](t, "deletions")
	if err != nil {
		return err
	}
	for _, j := range jobs {
		if j.AppID == app && j.State != "completed" {
			return fail(409, "应用正在删除，请等待客户端清理完成；失败时先核对操作记录并重试删除")
		}
	}
	return nil
}
func (s *Server) requestDeletion(w http.ResponseWriter, r *http.Request, version string) error {
	app := r.PathValue("app")
	if !model.ValidName(app) || (version != "" && !model.ValidName(version)) || r.URL.Query().Get("confirm") != "true" {
		return fail(400, "请明确确认停止并永久删除客户端目录及数据")
	}
	s.packagesMu.Lock()
	defer s.packagesMu.Unlock()
	var job Deletion
	err := s.DB.Update(func(t *store.Tx) error {
		id := model.Key(app, version)
		old, e := store.Get[Deletion](t, "deletions", id)
		if e == nil && old.State == "completed" && version == "" {
			if _, exists := store.Get[model.App](t, "apps", app); exists == nil {
				old.State = "failed"
			}
		}
		if e == nil && old.State != "failed" {
			job = old
			return nil
		}
		if e != nil && !errors.Is(e, store.ErrNotFound) {
			return e
		}
		if e == nil {
			if e = store.Delete(t, "deletions", id); e != nil {
				return e
			}
		}
		if e = deletionUnlocked(t, app); e != nil {
			return e
		}
		if _, e = store.Get[model.App](t, "apps", app); e != nil {
			return e
		}
		vs, e := store.List[model.Version](t, "versions")
		if e != nil {
			return e
		}
		found := version == ""
		for _, v := range vs {
			if v.AppID == app && (version == "" || v.Version == version) {
				found = true
			}
		}
		if !found {
			return fail(404, "版本不存在")
		}
		ops, e := store.List[model.Operation](t, "operations")
		if e != nil {
			return e
		}
		for _, o := range ops {
			if o.AppID == app && (!o.Terminal() || o.Result == "needs_attention") {
				return fail(409, "应用有执行中或待核对操作，请完成后重试删除")
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
		targets := map[string]model.Deployment{}
		deps, e := store.List[model.Deployment](t, "deployments")
		if e != nil {
			return e
		}
		matches := func(v string) bool { return v != "" && (version == "" || version == v) }
		for _, d := range deps {
			if d.AppID == app && (version == "" || matches(d.Current) || matches(d.Previous) || matches(d.DesiredVersion) || matches(d.RunningVersion)) {
				targets[d.NodeID] = d
			}
		}
		nodes, e := store.List[model.Node](t, "nodes")
		if e != nil {
			return e
		}
		for _, n := range nodes {
			items, e := store.Get[[]model.Installation](t, "inventories", n.ID)
			if e != nil && !errors.Is(e, store.ErrNotFound) {
				return e
			}
			for _, i := range items {
				if i.AppID == app && matches(i.Version) {
					if _, ok := targets[n.ID]; !ok {
						d, e := store.Get[model.Deployment](t, "deployments", model.NodeKey(n.ID, app))
						if e != nil {
							return e
						}
						targets[n.ID] = d
					}
				}
			}
		}
		job = Deletion{ID: id, AppID: app, Version: version, State: "pending", Operations: []string{}}
		for node, d := range targets {
			action := "remove_version"
			if version == "" {
				action = "purge_app"
			}
			o := model.Operation{ID: model.ID(), NodeID: node, AppID: app, Version: version, Action: action, ExpectedRevision: d.Revision, Revision: d.Revision + 1, Before: d, Config: d.Config, Stage: "queued", CreatedAt: model.Now(), UpdatedAt: model.Now(), Actor: actor(r).ID}
			d.Revision = o.Revision
			if version == "" || version == d.Current {
				d.DesiredState = "stopped"
			}
			if e = store.Put(t, "deployments", model.NodeKey(node, app), d); e != nil {
				return e
			}
			if e = store.Put(t, "operations", o.ID, o); e != nil {
				return e
			}
			job.Operations = append(job.Operations, o.ID)
		}
		for _, v := range vs {
			if v.AppID == app && (version == "" || v.Version == version) && v.Status != "deleted" {
				v.Status = "removing_clients"
				if e = store.Put(t, "versions", model.Key(app, v.Version), v); e != nil {
					return e
				}
			}
		}
		if e = store.Put(t, "deletions", id, job); e != nil {
			return e
		}
		if e = audit(t, actor(r).ID, "delete_requested", id); e != nil {
			return e
		}
		return s.advanceDeletions(t)
	})
	if err != nil {
		return err
	}
	if err = s.recoverImports(); err != nil {
		return err
	}
	if err = s.DB.View(func(t *store.Tx) error { var e error; job, e = store.Get[Deletion](t, "deletions", job.ID); return e }); err != nil {
		return err
	}
	status := 202
	if job.State == "completed" {
		status = 200
	}
	jsonOut(w, status, job)
	return nil
}
func (s *Server) advanceDeletions(t *store.Tx) error {
	jobs, err := store.List[Deletion](t, "deletions")
	if err != nil {
		return err
	}
	for _, j := range jobs {
		if j.State == "completed" {
			continue
		}
		done := true
		j.Error = ""
		j.State = "pending"
		for _, id := range j.Operations {
			o, e := store.Get[model.Operation](t, "operations", id)
			if e != nil {
				return e
			}
			if o.Result != "succeeded" {
				done = false
				if o.Terminal() {
					j.State = "failed"
					j.Error = o.NodeID + ": " + o.Error
				}
				continue
			}
			items, e := store.Get[[]model.Installation](t, "inventories", o.NodeID)
			if e != nil && !errors.Is(e, store.ErrNotFound) {
				return e
			}
			for _, i := range items {
				if i.AppID == j.AppID && (j.Version == "" || i.Version == j.Version) {
					done = false
				}
			}
		}
		if done {
			vs, e := store.List[model.Version](t, "versions")
			if e != nil {
				return e
			}
			for _, v := range vs {
				if v.AppID == j.AppID && (j.Version == "" || v.Version == j.Version) && v.Status != "deleted" {
					v.Status = "deleting"
					if e = store.Put(t, "versions", model.Key(v.AppID, v.Version), v); e != nil {
						return e
					}
				}
			}
			j.State = "cleaned"
		}
		if err = store.Put(t, "deletions", j.ID, j); err != nil {
			return err
		}
	}
	return nil
}
