package controller

import (
	"deployer/internal/model"
	"deployer/internal/store"
	"errors"
	"testing"
)

func TestRepositoryDeletionIsImmediateAndDerivedFromHeartbeat(t *testing.T) {
	s := newAuthServer(t, t.TempDir())
	defer s.Close()
	seedListDeploy(t, s)
	b := &authBrowser{t: t, s: s}
	b.login(DefaultAdminPassword, 200)
	p := model.Poll{Apps: []string{"a", "orphan"}, Installations: []model.Installation{{AppID: "a", Version: "1"}, {AppID: "a", Version: "2"}}, Deployments: []model.Deployment{{AppID: "a", Current: "1"}}}
	b.call("DELETE", "/apps/a/versions/1?confirm=true", nil, 200)
	check := func(want int) {
		t.Helper()
		if err := s.DB.View(func(tx *store.Tx) error {
			_, err := store.Get[model.Version](tx, "versions", "a/1")
			if !errors.Is(err, store.ErrNotFound) {
				t.Fatal("repository version retained", err)
			}
			ops, err := store.List[model.Operation](tx, "operations")
			if err != nil {
				return err
			}
			if len(ops) != 0 {
				t.Fatal("deletion created tasks", ops)
			}
			out, err := deletedFor(tx, p)
			if err != nil {
				return err
			}
			if len(out) != want {
				t.Fatal(out)
			}
			if out[0].AppID != "a" || out[0].Version != "1" {
				t.Fatal(out)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	check(2)
	check(2) // lost response/offline reconnect repeats without a stored job
	b.call("DELETE", "/apps/a?confirm=true", nil, 200)
	if err := s.DB.View(func(tx *store.Tx) error {
		out, err := deletedFor(tx, p)
		if len(out) != 2 || out[0].Version != "" {
			t.Fatal(out)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	b.call("DELETE", "/apps/a?confirm=true", nil, 200)
}
