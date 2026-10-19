package controller

import (
	"deployer/internal/model"
	"deployer/internal/store"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func seedListDeploy(t *testing.T, s *Server) {
	t.Helper()
	h, _ := (model.Health{}).Normalize()
	err := s.DB.Update(func(tx *store.Tx) error {
		if e := store.Put(tx, "apps", "a", model.App{ID: "a", Name: "app", Health: h}); e != nil {
			return e
		}
		for _, ver := range []string{"1", "2"} {
			if e := store.Put(tx, "versions", model.Key("a", ver), model.Version{AppID: "a", Version: ver, OS: "linux", Arch: "amd64", Status: "available"}); e != nil {
				return e
			}
		}
		for _, id := range []string{"n1", "n2"} {
			if e := store.Put(tx, "nodes", id, model.Node{ID: id, Name: id, OS: "linux", Arch: "amd64", LastSeen: model.Now()}); e != nil {
				return e
			}
		}
		return store.Put(tx, "deployments", model.NodeKey("n1", "a"), model.Deployment{NodeID: "n1", AppID: "a", Current: "1", DesiredVersion: "1", DesiredState: "stopped", RunState: "stopped", Revision: 7, Config: model.Config{ID: "keep-config"}})
	})
	if err != nil {
		t.Fatal(err)
	}
}
func TestListDeploySkipsWithoutMutatingHost(t *testing.T) {
	s := newAuthServer(t, t.TempDir())
	defer s.Close()
	seedListDeploy(t, s)
	req := model.OperationRequest{Action: "deploy", Version: "1", IdempotencyKey: "first", SkipIfDeployed: true, Env: map[string]string{"USER_VALUE": "ignored"}}
	var op, replay model.Operation
	if e := s.enqueue("n1", "a", req, "admin", &op); e != nil {
		t.Fatal(e)
	}
	if !op.Skipped || !op.Terminal() || op.Revision != 7 {
		t.Fatal(op)
	}
	if e := s.enqueue("n1", "a", req, "admin", &replay); e != nil || op.ID != replay.ID {
		t.Fatal("skip replay", e)
	}
	if e := s.DB.View(func(tx *store.Tx) error {
		d, e := store.Get[model.Deployment](tx, "deployments", model.NodeKey("n1", "a"))
		if e != nil {
			return e
		}
		if d.Revision != 7 || d.DesiredState != "stopped" || d.Config.ID != "keep-config" {
			t.Fatal("skip changed deployment", d)
		}
		active, e := store.Query[model.Operation](tx, "operations", store.Filter{States: []string{"active"}})
		if len(active) != 0 {
			t.Fatal("skip dispatched work")
		}
		return e
	}); e != nil {
		t.Fatal(e)
	}
	req.Version = "2"
	req.IdempotencyKey = "new-version"
	req.ExpectedRevision = 7
	if e := s.enqueue("n1", "a", req, "admin", &op); e != nil || op.Skipped || op.Revision != 8 {
		t.Fatal("new version was skipped", op, e)
	}
	req.Version = "1"
	req.IdempotencyKey = "conflicting"
	if e := s.enqueue("n1", "a", req, "admin", &op); e == nil {
		t.Fatal("skip bypassed active operation")
	}
}
func TestBatchSkipsExistingAndDeploysMissing(t *testing.T) {
	s := newAuthServer(t, t.TempDir())
	defer s.Close()
	seedListDeploy(t, s)
	b := Batch{ID: model.ID(), AppID: "a", Nodes: []string{"n1", "n2"}, Actor: "admin", Request: model.OperationRequest{Action: "deploy", Version: "1", SkipIfDeployed: true}, State: "running"}
	if e := s.DB.Update(func(tx *store.Tx) error { return store.Put(tx, "batches", b.ID, b) }); e != nil {
		t.Fatal(e)
	}
	for i := 0; i < 2; i++ {
		if e := s.advanceBatches(); e != nil {
			t.Fatal(e)
		}
	}
	if e := s.DB.Update(func(tx *store.Tx) error {
		var e error
		b, e = store.Get[Batch](tx, "batches", b.ID)
		if e != nil {
			return e
		}
		if b.Skipped != 1 || b.Index != 1 {
			t.Fatal(b)
		}
		op, e := store.Get[model.Operation](tx, "operations", b.CurrentOperation)
		if e != nil {
			return e
		}
		if op.NodeID != "n2" || op.Skipped {
			t.Fatal("wrong node dispatched", op)
		}
		op.Result = "succeeded"
		return store.Put(tx, "operations", op.ID, op)
	}); e != nil {
		t.Fatal(e)
	}
	if e := s.advanceBatches(); e != nil {
		t.Fatal(e)
	}
	if e := s.DB.View(func(tx *store.Tx) error {
		b, e := store.Get[Batch](tx, "batches", b.ID)
		if e == nil && (b.State != "succeeded" || b.Skipped != 1 || b.Index != 2) {
			t.Fatal(b)
		}
		return e
	}); e != nil {
		t.Fatal(e)
	}
}
func TestDeleteAppAndLastVersionRemoveCards(t *testing.T) {
	for _, mode := range []string{"app", "last-version", "recovery"} {
		t.Run(mode, func(t *testing.T) {
			s := newAuthServer(t, t.TempDir())
			defer s.Close()
			b := &authBrowser{t: t, s: s}
			b.login(DefaultAdminPassword, 200)
			digest := "abcdef"
			archive := filepath.Join(s.Root, "archives", digest+".tar.gz")
			if e := os.WriteFile(archive, []byte("shared archive"), 0600); e != nil {
				t.Fatal(e)
			}
			if e := s.DB.Update(func(tx *store.Tx) error {
				for _, app := range []string{"a", "other"} {
					if e := store.Put(tx, "apps", app, model.App{ID: app}); e != nil {
						return e
					}
					status := "available"
					if mode == "recovery" && app == "a" {
						status = "deleting"
					}
					if e := store.Put(tx, "versions", model.Key(app, "1"), model.Version{AppID: app, Version: "1", Status: status, Digest: digest}); e != nil {
						return e
					}
				}
				return nil
			}); e != nil {
				t.Fatal(e)
			}
			switch mode {
			case "app":
				b.call("DELETE", "/apps/a?confirm=true", nil, 200)
			case "last-version":
				b.call("DELETE", "/apps/a/versions/1?confirm=true", nil, 200)
			case "recovery":
				if e := s.recoverImports(); e != nil {
					t.Fatal(e)
				}
			}
			if e := s.DB.View(func(tx *store.Tx) error {
				_, e := store.Get[model.App](tx, "apps", "a")
				if !errors.Is(e, store.ErrNotFound) {
					t.Fatal("deleted card remains", e)
				}
				v, e := store.Get[model.Version](tx, "versions", "a/1")
				if !errors.Is(e, store.ErrNotFound) {
					t.Fatal("version not deleted", v, e)
				}
				_, e = store.Get[model.App](tx, "apps", "other")
				return e
			}); e != nil {
				t.Fatal(e)
			}
			if _, e := os.Stat(archive); e != nil {
				t.Fatal("shared archive removed", e)
			}
			// Recreating an app with a new version must not be removed by recovery.
			b.call("POST", "/apps", map[string]any{"id": "a", "name": "recreated"}, 200)
			if e := s.recoverImports(); e != nil {
				t.Fatal(e)
			}
			if e := s.DB.View(func(tx *store.Tx) error { _, e := store.Get[model.App](tx, "apps", "a"); return e }); e != nil {
				t.Fatal("recreated app disappeared", e)
			}
		})
	}
}
func TestFailedCurrentVersionCanBeDeployedAgain(t *testing.T) {
	s := newAuthServer(t, t.TempDir())
	defer s.Close()
	seedListDeploy(t, s)
	if err := s.DB.Update(func(tx *store.Tx) error {
		d, err := store.Get[model.Deployment](tx, "deployments", model.NodeKey("n1", "a"))
		if err != nil {
			return err
		}
		d.Health = "unhealthy"
		d.Error = "component checksum mismatch"
		return store.Put(tx, "deployments", model.NodeKey("n1", "a"), d)
	}); err != nil {
		t.Fatal(err)
	}
	var op model.Operation
	err := s.enqueue("n1", "a", model.OperationRequest{Action: "deploy", Version: "1", ExpectedRevision: 7, IdempotencyKey: "retry-failed", SkipIfDeployed: true}, "admin", &op)
	if err != nil || op.Skipped || op.Terminal() || op.Revision != 8 {
		t.Fatal("failed target was silently skipped", op, err)
	}
}

func TestDeletedAppTombstonesDoNotExhaustNodeLimit(t *testing.T) {
	s := newAuthServer(t, t.TempDir())
	defer s.Close()
	seedListDeploy(t, s)
	if err := s.DB.Update(func(tx *store.Tx) error {
		for i := 0; i < 256; i++ {
			id := fmt.Sprintf("deleted-%d", i)
			if err := store.Put(tx, "deployments", model.NodeKey("n2", id), model.Deployment{NodeID: "n2", AppID: id, RunState: "stopped"}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var op model.Operation
	if err := s.enqueue("n2", "a", model.OperationRequest{Action: "deploy", Version: "1", IdempotencyKey: "new-app"}, "admin", &op); err != nil {
		t.Fatal(err)
	}
}
func TestAppRollbackActionsAreRejected(t *testing.T) {
	s := newAuthServer(t, t.TempDir())
	defer s.Close()
	seedListDeploy(t, s)
	for _, action := range []string{"rollback", "clear_rollback"} {
		var op model.Operation
		if err := s.enqueue("n1", "a", model.OperationRequest{Action: action, ExpectedRevision: 7, IdempotencyKey: action}, "admin", &op); err == nil {
			t.Fatalf("accepted removed action %s", action)
		}
	}
}
