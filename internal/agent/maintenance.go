package agent

import (
	"os"
	"path/filepath"
	"strings"
	"time"

	"deployer/internal/model"
	"deployer/internal/store"
)

func (c *Client) maintain() error {
	if err := c.DB.Maintain(); err != nil {
		return err
	}
	c.Engine.cacheMu.Lock()
	defer c.Engine.cacheMu.Unlock()
	pinned := map[string]bool{}
	active := map[string]bool{}
	if err := c.DB.View(func(t *store.Tx) error {
		installed, err := store.List[model.Installation](t, "installations")
		if err != nil {
			return err
		}
		for _, i := range installed {
			pinned[i.Digest] = true
		}
		ops, err := store.Query[model.Operation](t, "operations", store.Filter{States: []string{"active", "needs_attention"}})
		if err != nil {
			return err
		}
		for _, o := range ops {
			active[o.ID] = true
			if o.Artifact != nil {
				pinned[o.Artifact.Digest] = true
			}
		}
		return nil
	}); err != nil {
		return err
	}
	files, err := os.ReadDir(filepath.Join(c.Root, "cache"))
	if err != nil {
		return err
	}
	for _, file := range files {
		if !file.Type().IsRegular() {
			continue
		}
		name := file.Name()
		digest := strings.SplitN(name, ".", 2)[0]
		if len(digest) != 64 || strings.Trim(digest, "0123456789abcdef") != "" || pinned[digest] {
			continue
		}
		info, err := file.Info()
		if err != nil {
			return err
		}
		if time.Since(info.ModTime()) > 7*24*time.Hour {
			if err = os.Remove(filepath.Join(c.Root, "cache", name)); err != nil {
				return err
			}
		}
	}
	files, err = os.ReadDir(filepath.Join(c.Root, "staging"))
	if err != nil {
		return err
	}
	for _, file := range files {
		if active[file.Name()] || !model.ValidName(file.Name()) {
			continue
		}
		info, err := file.Info()
		if err != nil {
			return err
		}
		if time.Since(info.ModTime()) > 24*time.Hour {
			if err = os.RemoveAll(filepath.Join(c.Root, "staging", file.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}
