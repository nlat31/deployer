package controller

import (
	"net/http"
	"strconv"

	"deployer/internal/model"
	"deployer/internal/store"
)

// Removing an offline host is a local bookkeeping operation. It cannot stop
// remote processes. A later authenticated heartbeat registers it again.
func (s *Server) deleteNode(w http.ResponseWriter, r *http.Request) error {
	if r.URL.Query().Get("confirm") != "true" {
		return fail(400, "必须确认删除离线主机")
	}
	id := r.PathValue("node")
	if !model.ValidName(id) {
		return fail(400, "无效主机标识")
	}
	s.batchMu.Lock()
	defer s.batchMu.Unlock()
	err := s.DB.Update(func(t *store.Tx) error {
		if err := removeOfflineNode(t, id); err != nil {
			return err
		}
		return audit(t, actor(r).ID, "delete_offline_node", id)
	})
	if err != nil {
		return err
	}
	jsonOut(w, 200, map[string]bool{"ok": true})
	return nil
}

func removeOfflineNode(t *store.Tx, id string) error {
	n, err := store.Get[model.Node](t, "nodes", id)
	if err != nil {
		return err
	}
	if model.Now()-n.LastSeen < 30000 {
		return fail(409, "在线主机不能删除，请先停止该主机的 Agent")
	}
	deps, err := store.List[model.Deployment](t, "deployments")
	if err != nil {
		return err
	}
	for _, d := range deps {
		if d.NodeID == id {
			if err = store.Delete(t, "deployments", model.NodeKey(id, d.AppID)); err != nil {
				return err
			}
		}
	}
	ops, err := store.Query[model.Operation](t, "operations", store.Filter{Owner: id, States: []string{"active", "needs_attention"}})
	if err != nil {
		return err
	}
	for _, o := range ops {
		o.Result = "failed"
		o.Stage = "finished"
		o.Error = "离线主机已被手动删除，远端执行结果未知"
		o.FinishedAt = model.Now()
		if err = store.Put(t, "operations", o.ID, o); err != nil {
			return err
		}
	}
	for _, kind := range []string{"nodes", "inventories", "restored_nodes"} {
		if err = store.Delete(t, kind, id); err != nil {
			return err
		}
	}
	if err = store.Put(t, "removed_nodes", id, true); err != nil {
		return err
	}
	return nil
}
func (s *Server) deleteOfflineNodes(w http.ResponseWriter, r *http.Request) error {
	if r.URL.Query().Get("confirm") != "true" {
		return fail(400, "必须确认删除所有离线设备")
	}
	s.batchMu.Lock()
	defer s.batchMu.Unlock()
	count := 0
	err := s.DB.Update(func(t *store.Tx) error {
		nodes, err := store.List[model.Node](t, "nodes")
		if err != nil {
			return err
		}
		for _, n := range nodes {
			if model.Now()-n.LastSeen >= 30000 {
				if err = removeOfflineNode(t, n.ID); err != nil {
					return err
				}
				count++
			}
		}
		return audit(t, actor(r).ID, "delete_offline_nodes", strconv.Itoa(count))
	})
	if err != nil {
		return err
	}
	jsonOut(w, 200, map[string]int{"deleted": count})
	return nil
}
