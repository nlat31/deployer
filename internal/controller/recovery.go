package controller

import (
	"errors"

	"deployer/internal/model"
	"deployer/internal/store"
)

// Explicit current-format backup restoration is a runtime recovery event.
// The restored snapshot must never replay actions that may have completed since
// it was taken. Keep a marker for late unacknowledged results and log batches.
func (s *Server) recoverRestoredNode(t *store.Tx, n model.Node, p model.Poll) (bool, error) {
	first, err := store.Get[bool](t, "restored_nodes", n.ID)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if first {
		ops, err := store.Query[model.Operation](t, "operations", store.Filter{Owner: n.ID, States: []string{"active"}})
		if err != nil {
			return false, err
		}
		for _, o := range ops {
			o.Result = "needs_attention"
			o.Stage = "finished"
			o.Error = "控制端已从备份恢复，任务结果需要核对"
			o.FinishedAt = model.Now()
			if err = store.Put(t, "operations", o.ID, o); err != nil {
				return false, err
			}
		}
	}
	for _, d := range p.Deployments {
		if !model.ValidName(d.AppID) || d.Revision < 0 {
			return false, fail(400, "无效恢复状态")
		}
		old, err := store.Get[model.Deployment](t, "deployments", model.NodeKey(n.ID, d.AppID))
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return false, err
		}
		if !first && err == nil && d.Revision <= old.Revision {
			continue
		}
		if _, err = store.Get[model.App](t, "apps", d.AppID); errors.Is(err, store.ErrNotFound) {
			if err = store.Put(t, "apps", d.AppID, model.App{ID: d.AppID, Name: d.AppID, Health: d.Config.Health}); err != nil {
				return false, err
			}
		} else if err != nil {
			return false, err
		}
		d.NodeID = n.ID
		d.ObservedAt = model.Now()
		d.Error = "控制端已从备份恢复，请停止并解除阻塞后重新部署"
		if err = store.Put(t, "deployments", model.NodeKey(n.ID, d.AppID), d); err != nil {
			return false, err
		}
		o := model.Operation{ID: hash("restore:" + n.ID + ":" + d.AppID), NodeID: n.ID, AppID: d.AppID, Action: "reconcile", Revision: d.Revision, Stage: "finished", Result: "needs_attention", Error: d.Error, CreatedAt: model.Now(), FinishedAt: model.Now(), Actor: "backup-recovery"}
		if err = store.Put(t, "operations", o.ID, o); err != nil {
			return false, err
		}
	}
	if first {
		if err = store.Put(t, "restored_nodes", n.ID, false); err != nil {
			return false, err
		}
		if err = audit(t, "agent:"+n.ID, "backup_reconnect", n.ID); err != nil {
			return false, err
		}
	}
	return true, nil
}
