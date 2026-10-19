package controller

import (
	"net/http"
	"testing"

	"deployer/internal/model"
	"deployer/internal/store"
)

func TestBatchStopsAfterFailureAndRecoversDispatch(t *testing.T) {
	s, e := New(t.TempDir(), false, http.NotFoundHandler())
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	health, _ := (model.Health{}).Normalize()
	e = s.DB.Update(func(tx *store.Tx) error {
		if e := store.Put(tx, "apps", "a", model.App{ID: "a", Health: health}); e != nil {
			return e
		}
		if e := store.Put(tx, "versions", "a/1", model.Version{AppID: "a", Version: "1", OS: "linux", Arch: "amd64", Status: "available"}); e != nil {
			return e
		}
		for _, id := range []string{"n1", "n2"} {
			if e := store.Put(tx, "nodes", id, model.Node{ID: id, OS: "linux", Arch: "amd64", LastSeen: model.Now()}); e != nil {
				return e
			}
		}
		return nil
	})
	if e != nil {
		t.Fatal(e)
	}
	b := Batch{ID: model.ID(), AppID: "a", Nodes: []string{"n1", "n2"}, Actor: "admin", Request: model.OperationRequest{Action: "deploy", Version: "1"}, State: "running"}
	s.DB.Update(func(tx *store.Tx) error { return store.Put(tx, "batches", b.ID, b) })
	if e = s.advanceBatches(); e != nil {
		t.Fatal(e)
	}
	var first model.Operation
	s.DB.View(func(tx *store.Tx) error {
		b, _ = store.Get[Batch](tx, "batches", b.ID)
		first, _ = store.Get[model.Operation](tx, "operations", b.CurrentOperation)
		return nil
	})
	if first.NodeID != "n1" {
		t.Fatal(first)
	}
	// Crash after enqueue but before saving the task ID: the persisted request
	// must reproduce the exact same operation despite the reserved revision.
	b.CurrentOperation = ""
	b.Operations = nil
	s.DB.Update(func(tx *store.Tx) error { return store.Put(tx, "batches", b.ID, b) })
	if e = s.advanceBatches(); e != nil {
		t.Fatal(e)
	}
	s.DB.View(func(tx *store.Tx) error {
		b, _ = store.Get[Batch](tx, "batches", b.ID)
		ops, _ := store.List[model.Operation](tx, "operations")
		if len(ops) != 1 || b.CurrentOperation != first.ID {
			t.Fatal("duplicate batch dispatch")
		}
		return nil
	})
	first.Result = "failed"
	s.DB.Update(func(tx *store.Tx) error { return store.Put(tx, "operations", first.ID, first) })
	if e = s.advanceBatches(); e != nil {
		t.Fatal(e)
	}
	s.DB.View(func(tx *store.Tx) error {
		b, _ = store.Get[Batch](tx, "batches", b.ID)
		ops, _ := store.List[model.Operation](tx, "operations")
		if b.State != "failed" || len(ops) != 1 {
			t.Fatal("batch continued after failure")
		}
		return nil
	})
}
