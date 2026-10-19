package controller

import (
	"deployer/internal/model"
	"deployer/internal/store"
	"errors"
	"testing"
)

func TestDeletionOfflineFailureRetryAndInventoryBarrier(t *testing.T) {
	s := newAuthServer(t, t.TempDir())
	defer s.Close()
	seedListDeploy(t, s)
	b := &authBrowser{t: t, s: s}
	b.login(DefaultAdminPassword, 200)
	s.DB.Update(func(tx *store.Tx) error {
		n, _ := store.Get[model.Node](tx, "nodes", "n1")
		n.LastSeen = 0
		store.Put(tx, "nodes", "n1", n)
		return store.Put(tx, "inventories", "n1", []model.Installation{{AppID: "a", Version: "1"}})
	})
	b.call("DELETE", "/apps/a/versions/1?confirm=true", nil, 202)
	b.call("DELETE", "/apps/a/versions/1?confirm=true", nil, 202)
	b.call("DELETE", "/nodes/n1?confirm=true", nil, 409)
	var job Deletion
	s.DB.View(func(tx *store.Tx) error {
		job, _ = store.Get[Deletion](tx, "deletions", "a/1")
		if len(job.Operations) != 1 {
			t.Fatal(job)
		}
		return nil
	})
	if err := s.recoverImports(); err != nil {
		t.Fatal(err)
	}
	s.DB.Update(func(tx *store.Tx) error {
		o, _ := store.Get[model.Operation](tx, "operations", job.Operations[0])
		o.Result = "failed"
		o.Error = "stop failed"
		store.Put(tx, "operations", o.ID, o)
		return s.advanceDeletions(tx)
	})
	s.DB.View(func(tx *store.Tx) error {
		j, _ := store.Get[Deletion](tx, "deletions", "a/1")
		if j.State != "failed" || j.Error == "" {
			t.Fatal(j)
		}
		return nil
	})
	b.call("DELETE", "/apps/a/versions/1?confirm=true", nil, 202)
	s.DB.Update(func(tx *store.Tx) error {
		j, _ := store.Get[Deletion](tx, "deletions", "a/1")
		if j.Operations[0] == job.Operations[0] {
			t.Fatal("retry did not issue new operation")
		}
		o, _ := store.Get[model.Operation](tx, "operations", j.Operations[0])
		o.Result = "succeeded"
		store.Put(tx, "operations", o.ID, o)
		return s.advanceDeletions(tx)
	})
	s.DB.View(func(tx *store.Tx) error {
		v, _ := store.Get[model.Version](tx, "versions", "a/1")
		if v.Status != "removing_clients" {
			t.Fatal("deleted before fresh inventory", v)
		}
		return nil
	})
	s.DB.Update(func(tx *store.Tx) error {
		store.Put(tx, "inventories", "n1", []model.Installation{})
		return s.advanceDeletions(tx)
	})
	if err := s.recoverImports(); err != nil {
		t.Fatal(err)
	}
	s.DB.View(func(tx *store.Tx) error {
		v, _ := store.Get[model.Version](tx, "versions", "a/1")
		j, _ := store.Get[Deletion](tx, "deletions", "a/1")
		if v.Status != "deleted" || j.State != "completed" {
			t.Fatal(v, j)
		}
		_, err := store.Get[model.App](tx, "apps", "a")
		if errors.Is(err, store.ErrNotFound) {
			t.Fatal("other version lost")
		}
		return err
	})
}
