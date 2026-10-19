package controller

import (
	"deployer/internal/model"
	"deployer/internal/store"
	"errors"
	"net/http"
)

func versionUnreferenced(t *store.Tx, app, version string) error {
	matches := func(v string) bool { return version == "" || v == version }
	deps, e := store.List[model.Deployment](t, "deployments")
	if e != nil {
		return e
	}
	for _, d := range deps {
		if d.AppID == app && ((d.Current != "" && matches(d.Current)) || (d.Previous != "" && matches(d.Previous)) || (d.DesiredVersion != "" && matches(d.DesiredVersion))) {
			return fail(409, "应用仍被部署或回退记录引用，请先解除部署")
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
				return fail(409, "主机 "+n.Name+" 仍安装此应用，请先删除主机上的版本")
			}
		}
	}
	ops, e := store.List[model.Operation](t, "operations")
	if e != nil {
		return e
	}
	for _, o := range ops {
		if o.AppID == app && matches(o.Version) && (!o.Terminal() || o.Result == "needs_attention") {
			return fail(409, "应用存在执行中或待核对的操作")
		}
	}
	batches, e := store.List[Batch](t, "batches")
	if e != nil {
		return e
	}
	for _, b := range batches {
		if b.AppID == app && matches(b.Request.Version) && b.State == "running" {
			return fail(409, "批量任务仍引用此应用")
		}
	}
	return nil
}

// Keep version tombstones for immutable package names, but remove the empty
// application itself so it cannot remain as a ghost card after its last delete.
func removeEmptyApp(t *store.Tx, app string) error {
	versions, e := store.List[model.Version](t, "versions")
	if e != nil {
		return e
	}
	found := false
	for _, v := range versions {
		if v.AppID == app {
			found = true
			if v.Status != "deleted" {
				return nil
			}
		}
	}
	if found {
		return store.Delete(t, "apps", app)
	}
	return nil
}
func (s *Server) deleteApp(w http.ResponseWriter, r *http.Request) error {
	app := r.PathValue("app")
	if !model.ValidName(app) || r.URL.Query().Get("confirm") != "true" {
		return fail(400, "需要有效应用标识并确认删除")
	}
	s.packagesMu.Lock()
	defer s.packagesMu.Unlock()
	var versions []model.Version
	err := s.DB.Update(func(t *store.Tx) error {
		if _, e := store.Get[model.App](t, "apps", app); e != nil {
			return e
		}
		if e := versionUnreferenced(t, app, ""); e != nil {
			return e
		}
		all, e := store.List[model.Version](t, "versions")
		if e != nil {
			return e
		}
		for _, v := range all {
			if v.AppID == app && v.Status != "deleted" {
				v.Status = "deleting"
				versions = append(versions, v)
				if e = store.Put(t, "versions", model.Key(app, v.Version), v); e != nil {
					return e
				}
			}
		}
		if len(versions) == 0 {
			if e = store.Delete(t, "apps", app); e != nil {
				return e
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	err = s.DB.Update(func(t *store.Tx) error {
		for _, v := range versions {
			if e := s.removeArchive(t, v); e != nil {
				return e
			}
			v.Status = "deleted"
			if e := store.Put(t, "versions", model.Key(app, v.Version), v); e != nil {
				return e
			}
		}
		if e := store.Delete(t, "apps", app); e != nil {
			return e
		}
		return audit(t, actor(r).ID, "delete_app", app)
	})
	if err != nil {
		return err
	}
	jsonOut(w, 200, map[string]string{"id": app, "status": "deleted"})
	return nil
}
