package controller

import (
	"errors"
	"net/http"
	"strconv"

	"deployer/internal/model"
	"deployer/internal/store"
)

type Batch struct {
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
	if len(in.Nodes) < 2 || len(in.Nodes) > 100 || !model.ValidName(in.AppID) || !model.ValidName(in.Request.Version) || (in.Request.Action != "deploy" && in.Request.Action != "install" && in.Request.Action != "switch") {
		return fail(400, "批量部署需要 2 至 100 台主机和有效版本")
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
		v, e := store.Get[model.Version](t, "versions", model.Key(b.AppID, b.Request.Version))
		if e != nil {
			return e
		}
		if v.Status != "available" {
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
			if model.Now()-n.LastSeen > 30000 || n.Arch != v.Arch || n.OS != v.OS {
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
				b.State = "failed"
				b.Error = "节点 " + op.NodeID + " 部署未成功，后续节点未派发"
			} else {
				if op.Skipped {
					b.Skipped++
				}
				b.Index++
				b.CurrentOperation = ""
				b.Prepared = nil
				if b.Index == len(b.Nodes) {
					b.State = "succeeded"
				}
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
			b.State = "failed"
			b.Error = e.Error()
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
