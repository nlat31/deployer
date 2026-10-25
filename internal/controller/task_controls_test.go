package controller

import (
	"bytes"
	"deployer/internal/model"
	"deployer/internal/store"
	"encoding/json"
	"net/http"
	"testing"
)

func featureBrowser(t *testing.T) (*Server, *authBrowser) {
	t.Helper()
	s := newAuthServer(t, t.TempDir())
	t.Cleanup(func() { s.Close() })
	seedListDeploy(t, s)
	b := &authBrowser{t: t, s: s}
	b.login(DefaultAdminPassword, 200)
	return s, b
}
func TestDeleteAllOfflineDevicesIsIdempotent(t *testing.T) {
	s, b := featureBrowser(t)
	if err := s.DB.Update(func(tx *store.Tx) error {
		n, err := store.Get[model.Node](tx, "nodes", "n2")
		if err != nil {
			return err
		}
		n.LastSeen = model.Now() - 31000
		return store.Put(tx, "nodes", "n2", n)
	}); err != nil {
		t.Fatal(err)
	}
	b.call("DELETE", "/nodes/offline", nil, 400)
	if result := b.call("DELETE", "/nodes/offline?confirm=true", nil, 200); result["deleted"] != float64(1) {
		t.Fatal(result)
	}
	if result := b.call("DELETE", "/nodes/offline?confirm=true", nil, 200); result["deleted"] != float64(0) {
		t.Fatal(result)
	}
	if err := s.DB.View(func(tx *store.Tx) error { _, err := store.Get[model.Node](tx, "nodes", "n1"); return err }); err != nil {
		t.Fatal("online device removed", err)
	}
}
func TestBatchDetachContinuesAfterDispatchFailure(t *testing.T) {
	s, b := featureBrowser(t)
	result := b.call("POST", "/batches", map[string]any{"app_id": "a", "nodes": []string{"n2", "n1"}, "request": model.OperationRequest{Action: "detach", Confirm: true}}, 202)
	id := result["id"].(string)
	if err := s.DB.Update(func(tx *store.Tx) error { return store.Delete(tx, "nodes", "n2") }); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := s.advanceBatches(); err != nil {
			t.Fatal(err)
		}
	}
	var batch Batch
	if err := s.DB.View(func(tx *store.Tx) error { var err error; batch, err = store.Get[Batch](tx, "batches", id); return err }); err != nil {
		t.Fatal(err)
	}
	if batch.Index != 1 || batch.Failed != 1 || batch.CurrentOperation == "" {
		t.Fatal(batch)
	}
	if err := s.DB.Update(func(tx *store.Tx) error {
		op, err := store.Get[model.Operation](tx, "operations", batch.CurrentOperation)
		if err != nil {
			return err
		}
		if op.Action != "detach" || op.NodeID != "n1" {
			t.Fatal(op)
		}
		op.Result = "succeeded"
		return store.Put(tx, "operations", op.ID, op)
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.advanceBatches(); err != nil {
		t.Fatal(err)
	}
	s.DB.View(func(tx *store.Tx) error { batch, _ = store.Get[Batch](tx, "batches", id); return nil })
	if batch.State != "completed_with_errors" || batch.Index != 2 || batch.Failures["n2"] == "" {
		t.Fatal(batch)
	}
}
func TestCancelQueuedOperationRestoresReservedRevision(t *testing.T) {
	s, b := featureBrowser(t)
	var op model.Operation
	if err := s.enqueue("n1", "a", model.OperationRequest{Action: "deploy", Version: "2", IdempotencyKey: "cancel-test", ExpectedRevision: 7}, "admin", &op); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		b.call("POST", "/operations/"+op.ID+"/cancel", nil, 200)
	}
	s.DB.View(func(tx *store.Tx) error {
		saved, _ := store.Get[model.Operation](tx, "operations", op.ID)
		d, _ := store.Get[model.Deployment](tx, "deployments", model.NodeKey("n1", "a"))
		if saved.Result != "cancelled" || d.Revision != 7 || d.Current != "1" {
			t.Fatal(saved, d)
		}
		return nil
	})
}
func TestCancelBatchStopsDispatchAndSignalsCurrent(t *testing.T) {
	s, b := featureBrowser(t)
	result := b.call("POST", "/batches", map[string]any{"app_id": "a", "nodes": []string{"n1", "n2"}, "request": model.OperationRequest{Action: "deploy", Version: "2"}}, 202)
	id := result["id"].(string)
	if err := s.advanceBatches(); err != nil {
		t.Fatal(err)
	}
	var batch Batch
	s.DB.Update(func(tx *store.Tx) error {
		batch, _ = store.Get[Batch](tx, "batches", id)
		op, _ := store.Get[model.Operation](tx, "operations", batch.CurrentOperation)
		op.Stage = "setting_up"
		return store.Put(tx, "operations", op.ID, op)
	})
	b.call("POST", "/batches/"+id+"/cancel", nil, 200)
	if err := s.advanceBatches(); err != nil {
		t.Fatal(err)
	}
	s.DB.View(func(tx *store.Tx) error {
		batch, _ = store.Get[Batch](tx, "batches", id)
		op, _ := store.Get[model.Operation](tx, "operations", batch.CurrentOperation)
		ops, _ := store.List[model.Operation](tx, "operations")
		if batch.State != "cancelled" || !op.CancelRequested || op.Terminal() || len(ops) != 1 {
			t.Fatal(batch, op, len(ops))
		}
		return nil
	})
}

func TestCancelBatchFindsOperationAfterDispatchBookkeepingCrash(t *testing.T) {
	s, b := featureBrowser(t)
	result := b.call("POST", "/batches", map[string]any{"app_id": "a", "nodes": []string{"n1", "n2"}, "request": model.OperationRequest{Action: "deploy", Version: "2"}}, 202)
	id := result["id"].(string)
	if err := s.advanceBatches(); err != nil {
		t.Fatal(err)
	}
	var opID string
	if err := s.DB.Update(func(tx *store.Tx) error {
		batch, err := store.Get[Batch](tx, "batches", id)
		if err != nil {
			return err
		}
		opID = batch.CurrentOperation
		batch.CurrentOperation = ""
		batch.Operations = nil
		return store.Put(tx, "batches", id, batch)
	}); err != nil {
		t.Fatal(err)
	}
	b.call("POST", "/batches/"+id+"/cancel", nil, 200)
	s.DB.View(func(tx *store.Tx) error {
		op, _ := store.Get[model.Operation](tx, "operations", opID)
		if op.Result != "cancelled" {
			t.Fatal("orphan operation still dispatchable", op)
		}
		return nil
	})
}
func TestTaskControlsRequireRoleAndCSRF(t *testing.T) {
	_, b := featureBrowser(t)
	csrf := b.csrf
	b.csrf = ""
	b.call("DELETE", "/nodes/offline?confirm=true", nil, 403)
	b.call("POST", "/operations/nonexistent/cancel", nil, 403)
	b.csrf = csrf
	b.call("POST", "/users", map[string]any{"id": "reader", "name": "reader", "role": "viewer", "password": "reader-password"}, 200)
	b.call("POST", "/login", map[string]string{"username": "reader", "password": "reader-password"}, 200)
	b.call("DELETE", "/nodes/offline?confirm=true", nil, 403)
	b.call("POST", "/operations/nonexistent/cancel", nil, 403)
	b.call("POST", "/batches/nonexistent/cancel", nil, 403)
}

func TestCancellationIsNotStarvedByDispatchLimitOrAgentDrain(t *testing.T) {
	s, pki, ts := newMTLSTestServer(t)
	cert := pki.Node(t, "cancel-node")
	client := certClient(t, pki, &cert)
	pollRequest(t, client, ts, model.Poll{}, 200)
	if err := s.DB.Update(func(tx *store.Tx) error {
		for i, id := range []string{"normal-one", "normal-two", "cancel-target"} {
			op := model.Operation{ID: id, NodeID: "cancel-node", AppID: "a", Stage: "dispatched", CreatedAt: int64(i + 1), CancelRequested: i == 2}
			if err := store.Put(tx, "operations", id, op); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, draining := range []bool{false, true} {
		raw, _ := json.Marshal(model.Poll{Protocol: model.Protocol, Name: "cancel-node", OS: "linux", Arch: "amd64", Draining: draining})
		response, err := client.Post(ts.URL+"/agent/poll", "application/json", bytes.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		var out model.PollResponse
		err = json.NewDecoder(response.Body).Decode(&out)
		response.Body.Close()
		if err != nil || response.StatusCode != http.StatusOK || len(out.Cancelled) != 1 || out.Cancelled[0].ID != "cancel-target" {
			t.Fatal("cancellation starved", out, err)
		}
		if draining && len(out.Tasks) != 0 {
			t.Fatal("draining agent received new work")
		}
	}
}
