package controller

import (
	"deployer/internal/model"
	"deployer/internal/store"
	"net/http"
)

func removeEmptyApp(t *store.Tx, app string) error {
	versions, err := store.List[model.Version](t, "versions")
	if err != nil {
		return err
	}
	for _, v := range versions {
		if v.AppID == app {
			return nil
		}
	}
	return store.Delete(t, "apps", app)
}
func (s *Server) deleteApp(w http.ResponseWriter, r *http.Request) error {
	return s.requestDeletion(w, r, "")
}
