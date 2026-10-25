package controller

import (
	"deployer/internal/model"
	"deployer/internal/store"
	"net/http"
)

func cancelOperationTx(t *store.Tx, id string) error {
	o, err := store.Get[model.Operation](t, "operations", id)
	if err != nil {
		return err
	}
	if o.Terminal() {
		return nil
	}
	o.CancelRequested = true
	o.UpdatedAt = model.Now()
	if o.Stage == "queued" {
		o.Result = "cancelled"
		o.Stage = "finished"
		o.FinishedAt = model.Now()
		o.Error = "管理员已取消，未派发执行"
		d, err := store.Get[model.Deployment](t, "deployments", model.NodeKey(o.NodeID, o.AppID))
		if err != nil {
			return err
		}
		if d.Revision == o.Revision {
			if err = store.Put(t, "deployments", model.NodeKey(o.NodeID, o.AppID), o.Before); err != nil {
				return err
			}
		}
	}
	return store.Put(t, "operations", id, o)
}
func (s *Server) cancelOperation(w http.ResponseWriter, r *http.Request) error {
	s.batchMu.Lock()
	defer s.batchMu.Unlock()
	err := s.DB.Update(func(t *store.Tx) error {
		if err := cancelOperationTx(t, r.PathValue("id")); err != nil {
			return err
		}
		return audit(t, actor(r).ID, "cancel_operation", r.PathValue("id"))
	})
	if err != nil {
		return err
	}
	jsonOut(w, 200, map[string]bool{"ok": true})
	return nil
}
