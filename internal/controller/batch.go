package controller

import (
	"errors"
	"net/http"
	"strconv"

	"deployer/internal/model"
	"deployer/internal/store"
)

type Batch struct {
	Failed           int                     `json:"failed"`
	Failures         map[string]string       `json:"failures,omitempty"`
	Skipped          int                     `json:"skipped"`
	ID               string                  `json:"id"`
	AppID            string                  `json:"app_id"`
	Nodes            []string                `json:"nodes"`
	Request          model.OperationRequest  `json:"request"`
	Prepared         *model.OperationRequest `json:"prepared,omitempty"`
	Actor            string                  `json:"actor"`
	Index            int                     `json:"index"`
	CurrentOperation string                  `json:"current_operation"`
	Operations       []string                `json:"operations"`
	State            string                  `json:"state"`
	Error            string                  `json:"error"`
	CreatedAt        int64                   `json:"created_at"`
}

func (b Batch) StorageIndex() (string, string, string) { return b.Actor, b.AppID, b.State }

func (s *Server) createBatch(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		AppID   string                 `json:"app_id"`
		Nodes   []string               `json:"nodes"`
		Request model.OperationRequest `json:"request"`
	}
	if e := decode(w, r, &in); e != nil {
		return e
	}
	if len(in.Nodes) < 1 || len(in.Nodes) > 100 || !model.ValidName(in.AppID) || (in.Request.Action != "detach" && !model.ValidName(in.Request.Version)) || (in.Request.Action != "deploy" && in.Request.Action != "install" && in.Request.Action != "switch" && in.Request.Action != "detach") {
		return fail(400, "批量操作需要 1 至 100 台设备和有效操作")
	}
	if e := model.ValidEnv(in.Request.Env); e != nil {
		return fail(400, e.Error())
	}
	if in.Request.Health != nil {
		h, e := in.Request.Health.Normalize()
		if e != nil {
			return fail(400, e.Error())
		}
		in.Request.Health = &h
	}
	b := Batch{ID: model.ID(), AppID: in.AppID, Nodes: in.Nodes, Request: in.Request, Actor: actor(r).ID, State: "running", CreatedAt: model.Now(), Operations: []string{}}
	e := s.DB.Update(func(t *store.Tx) error {
		v := model.Version{}
		var e error
		if b.Request.Action != "detach" {
			v, e = store.Get[model.Version](t, "versions", model.Key(b.AppID, b.Request.Version))
		} else if !b.Request.Confirm {
			return fail(400, "请确认批量解除部署")
		}
		if e != nil {
			return e
		}
		if b.Request.Action != "detach" && v.Status != "available" {
			return fail(409, "版本不可用")
		}
		seen := map[string]bool{}
		for _, id := range b.Nodes {
			if seen[id] {
				return fail(400, "目标主机重复")
			}
			seen[id] = true
			n, e := store.Get[model.Node](t, "nodes", id)
			if e != nil {
				return e
			}
			if model.Now()-n.LastSeen > 30000 || (b.Request.Action != "detach" && (n.Arch != v.Arch || n.OS != v.OS)) {
				return fail(409, "存在离线或平台不匹配的主机")
			}
		}
		if e = store.Put(t, "batches", b.ID, b); e != nil {
			return e
		}
		return audit(t, actor(r).ID, "create_batch", b.ID)
	})
	if e != nil {
		return e
	}
	b.Request.Env = nil
	jsonOut(w, 202, b)
	return nil
}
func (s *Server) advanceBatches() error {
	s.batchMu.Lock()
	defer s.batchMu.Unlock()
	var batches []Batch
	if e := s.DB.View(func(t *store.Tx) error {
		var e error
		batches, e = store.Query[Batch](t, "batches", store.Filter{States: []string{"running"}})
		return e
	}); e != nil {
		return e
	}
	for _, b := range batches {
		if b.State != "running" {
			continue
		}
		if b.CurrentOperation != "" {
			var op model.Operation
			if e := s.DB.View(func(t *store.Tx) error {
				var e error
				op, e = store.Get[model.Operation](t, "operations", b.CurrentOperation)
				return e
			}); e != nil {
				return e
			}
			if !op.Terminal() {
				continue
			}
			if op.Result != "succeeded" {
				b.recordFailure(op.NodeID, op.Error)
			} else if op.Skipped {
				b.Skipped++
			}
			b.Index++
			b.CurrentOperation = ""
			b.Prepared = nil
			if b.Index == len(b.Nodes) {
				b.complete()
			}

			if e := s.DB.Update(func(t *store.Tx) error { return store.Put(t, "batches", b.ID, b) }); e != nil {
				return e
			}
			if b.State != "running" {
				continue
			}
		}
		if b.Prepared == nil {
			req := b.Request
			req.IdempotencyKey = b.ID + "-" + strconv.Itoa(b.Index)
			if e := s.DB.View(func(t *store.Tx) error {
				d, e := store.Get[model.Deployment](t, "deployments", model.NodeKey(b.Nodes[b.Index], b.AppID))
				if e != nil && !errors.Is(e, store.ErrNotFound) {
					return e
				}
				req.ExpectedRevision = d.Revision
				return nil
			}); e != nil {
				return e
			}
			b.Prepared = &req
			if e := s.DB.Update(func(t *store.Tx) error { return store.Put(t, "batches", b.ID, b) }); e != nil {
				return e
			}
		}
		var op model.Operation
		e := s.enqueue(b.Nodes[b.Index], b.AppID, *b.Prepared, b.Actor, &op)
		if e != nil {
			b.recordFailure(b.Nodes[b.Index], e.Error())
			b.Index++
			b.Prepared = nil
			if b.Index == len(b.Nodes) {
				b.complete()
			}
		} else {
			b.CurrentOperation = op.ID
			b.Operations = append(b.Operations, op.ID)
		}
		if e = s.DB.Update(func(t *store.Tx) error { return store.Put(t, "batches", b.ID, b) }); e != nil {
			return e
		}
	}
	return nil
}

func (b *Batch) recordFailure(node, message string) {
	if b.Failures == nil {
		b.Failures = map[string]string{}
	}
	if message == "" {
		message = "操作未成功"
	}
	b.Failures[node] = message
	b.Failed++
	b.Error = "部分设备操作失败，详见各设备失败原因"
}
func (b *Batch) complete() {
	if b.Failed > 0 {
		b.State = "completed_with_errors"
	} else {
		b.State = "succeeded"
	}
}
func (s *Server) cancelBatch(w http.ResponseWriter, r *http.Request) error {
	s.batchMu.Lock()
	defer s.batchMu.Unlock()
	err := s.DB.Update(func(t *store.Tx) error {
		b, err := store.Get[Batch](t, "batches", r.PathValue("id"))
		if err != nil {
			return err
		}
		if b.State != "running" {
			return nil
		}
		if b.CurrentOperation == "" && b.Prepared != nil && b.Index < len(b.Nodes) {
			key := hash(b.Nodes[b.Index] + "/" + b.AppID + "/" + b.Prepared.IdempotencyKey)
			id, lookupErr := store.Get[string](t, "operation_requests", key)
			if lookupErr == nil {
				b.CurrentOperation = id
			} else if !errors.Is(lookupErr, store.ErrNotFound) {
				return lookupErr
			}
		}
		if b.CurrentOperation != "" {
			if err = cancelOperationTx(t, b.CurrentOperation); err != nil {
				return err
			}
		}
		b.State = "cancelled"
		if err = store.Put(t, "batches", b.ID, b); err != nil {
			return err
		}
		return audit(t, actor(r).ID, "cancel_batch", b.ID)
	})
	if err != nil {
		return err
	}
	jsonOut(w, 200, map[string]bool{"ok": true})
	return nil
}
