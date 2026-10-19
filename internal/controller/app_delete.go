package controller

import (
	"deployer/internal/model"
	"deployer/internal/store"
	"net/http"
)

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
	return s.requestDeletion(w, r, "")
}
