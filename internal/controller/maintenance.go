package controller

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"deployer/internal/identity"
	"deployer/internal/model"
	"deployer/internal/store"
)

func (s *Server) maintain() error {
	if err := s.DB.Maintain(); err != nil {
		return err
	}
	s.packagesMu.Lock()
	defer s.packagesMu.Unlock()
	root := filepath.Join(s.Root, "backups")
	if err := os.MkdirAll(root, 0700); err != nil {
		return err
	}
	name := time.Now().UTC().Format("2006-01-02")
	target := filepath.Join(root, name)
	if _, err := os.Stat(target); os.IsNotExist(err) {
		stage, err := os.MkdirTemp(root, ".backup-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(stage)
		if err = s.DB.Backup(filepath.Join(stage, "controller.db")); err != nil {
			return err
		}
		key, err := os.ReadFile(filepath.Join(s.Root, "state", "master.key"))
		if err != nil {
			return err
		}
		if err = store.AtomicWrite(filepath.Join(stage, "master.key"), key, 0600); err != nil {
			return err
		}
		if err = s.backupIssuer(stage); err != nil {
			return err
		}
		if err = os.Mkdir(filepath.Join(stage, "archives"), 0700); err != nil {
			return err
		}
		var vs []model.Version
		if err = s.DB.View(func(t *store.Tx) error { var err error; vs, err = store.List[model.Version](t, "versions"); return err }); err != nil {
			return err
		}
		seen := map[string]bool{}
		for _, v := range vs {
			if v.Status != "available" || seen[v.Digest] {
				continue
			}
			seen[v.Digest] = true
			source := filepath.Join(s.Root, "archives", v.Digest+".tar.gz")
			if err = os.Link(source, filepath.Join(stage, "archives", v.Digest+".tar.gz")); err != nil {
				return fmt.Errorf("备份包 %s: %w", v.Digest, err)
			}
		}
		if err = store.SyncDir(filepath.Join(stage, "archives")); err != nil {
			return err
		}
		if err = store.SyncDir(stage); err != nil {
			return err
		}
		if err = os.Rename(stage, target); err != nil {
			return err
		}
		if err = store.SyncDir(root); err != nil {
			return err
		}
	}
	if err := s.backupIssuer(target); err != nil {
		return err
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	names := []string{}
	for _, entry := range entries {
		if entry.IsDir() {
			if _, err := time.Parse("2006-01-02", entry.Name()); err == nil {
				names = append(names, entry.Name())
			} else if strings.HasPrefix(entry.Name(), ".backup-") {
				if info, e := entry.Info(); e == nil && time.Since(info.ModTime()) > 24*time.Hour {
					if e = os.RemoveAll(filepath.Join(root, entry.Name())); e != nil {
						return e
					}
				}
			}
		}
	}
	sort.Strings(names)
	for len(names) > 7 {
		if err = os.RemoveAll(filepath.Join(root, names[0])); err != nil {
			return err
		}
		names = names[1:]
	}
	return nil
}

// The online issuer must survive restoring a database snapshot, otherwise
// already-enrolled Agents would no longer chain to a trusted client CA.
func (s *Server) backupIssuer(target string) error {
	source := filepath.Join(s.Root, "state", "agent-issuer")
	if _, err := os.Stat(source); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	unlock, err := identity.IdentityLock(filepath.Join(source, "issuer.lock"))
	if err != nil {
		return err
	}
	defer unlock()
	destination := filepath.Join(target, "agent-issuer")
	if err = os.MkdirAll(destination, 0700); err != nil {
		return err
	}
	for _, name := range []string{"key.pem", "ca.pem"} {
		data, e := os.ReadFile(filepath.Join(source, name))
		if e != nil {
			return e
		}
		if e = store.AtomicWrite(filepath.Join(destination, name), data, 0600); e != nil {
			return e
		}
	}
	return store.SyncDir(destination)
}
