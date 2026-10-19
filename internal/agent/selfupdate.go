package agent

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"deployer/internal/launcher"
	"deployer/internal/model"
	"deployer/internal/selfupdate"
	"deployer/internal/store"
	"deployer/internal/supervisor"
)

func (c *Client) updateStatus() selfupdate.Status {
	s := selfupdate.Status{Version: selfupdate.Version}
	root := os.Getenv("DEPLOYER_INSTALL_ROOT")
	if root == "" {
		return s
	}
	r := launcher.Runtime{Root: root, Role: "agent"}
	state, e := r.Status()
	if e != nil {
		return s
	}
	s.Digest = state.Current
	s.Target = state.Target
	s.Phase = state.Phase
	s.Error = state.Error
	key, e := selfupdate.ReadKey(r.Key())
	s.Capable = e == nil && len(key) == 32
	var download selfupdate.Status
	if selfupdate.Load(filepath.Join(root, "updater", "agent-download.json"), &download) == nil && download.Target != state.Current && state.Phase != "trial" && !(state.Phase == "failed" && state.Target == download.Target) {
		s.Target = download.Target
		s.Phase = download.Phase
		s.Error = download.Error
	}

	return s
}
func (c *Client) fetchUpdate(ctx context.Context, m selfupdate.Manifest) error {
	root := os.Getenv("DEPLOYER_INSTALL_ROOT")
	if root == "" {
		return fmt.Errorf("automatic updates require launcher")
	}
	r := launcher.Runtime{Root: root, Role: "agent"}
	key, e := selfupdate.ReadKey(r.Key())
	if e != nil {
		return e
	}
	if e = m.Verify(key); e != nil {
		return e
	}
	if e = m.Platform(); e != nil {
		return e
	}
	state, e := r.Status()
	if e != nil {
		return e
	}
	if state.Current == m.Digest {
		return nil
	}
	for _, d := range state.Failed {
		if d == m.Digest {
			return nil
		}
	}
	path := r.Binary(m.Digest)
	if m.CheckFile(path) != nil {
		if e = os.MkdirAll(filepath.Dir(path), 0700); e != nil {
			return e
		}
		req, e := http.NewRequestWithContext(ctx, "GET", c.Identity.Controller+"/agent/releases/"+m.Digest, nil)
		if e != nil {
			return e
		}
		client := *c.HTTP
		client.Timeout = 10 * time.Minute
		resp, e := client.Do(req)
		if e != nil {
			return e
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			return fmt.Errorf("update download returned %d", resp.StatusCode)
		}
		f, e := os.CreateTemp(filepath.Dir(path), ".download-")
		if e != nil {
			return e
		}
		defer os.Remove(f.Name())
		n, e := io.Copy(f, io.LimitReader(resp.Body, m.Size+1))
		if e == nil && n != m.Size {
			e = fmt.Errorf("update size mismatch")
		}
		if e == nil {
			e = f.Sync()
		}
		ce := f.Close()
		if e == nil {
			e = ce
		}
		if e != nil {
			return e
		}
		if e = m.CheckFile(f.Name()); e != nil {
			return e
		}
		if e = os.Chmod(f.Name(), 0700); e != nil {
			return e
		}
		if e = os.Rename(f.Name(), path); e != nil {
			return e
		}
		if e = selfupdate.SyncDir(filepath.Dir(path)); e != nil {
			return e
		}
	}
	return selfupdate.Save(filepath.Join(root, "updater", "agent-request.json"), m)
}
func (c *Client) scheduleUpdate(ctx context.Context, m *selfupdate.Manifest) {
	if os.Getenv("DEPLOYER_INSTALL_ROOT") == "" || m == nil || launcher.Draining() || launcher.Trial() || (m.Digest == c.lastUpdate && time.Now().Before(c.nextUpdate)) || !c.updating.CompareAndSwap(false, true) {
		return
	}
	c.nextUpdate = time.Now().Add(time.Minute)
	c.lastUpdate = m.Digest
	c.workers.Add(1)
	go func() {
		defer c.workers.Done()
		defer c.updating.Store(false)
		path := filepath.Join(os.Getenv("DEPLOYER_INSTALL_ROOT"), "updater", "agent-download.json")
		status := selfupdate.Status{Target: m.Digest, Phase: "downloading"}
		if e := selfupdate.Save(path, status); e != nil {
			log.Printf("Agent update state: %v", e)
			return
		}
		if e := c.fetchUpdate(ctx, *m); e != nil {
			status.Phase = "download_retry"
			status.Error = e.Error()
			if len(status.Error) > 4096 {
				status.Error = status.Error[:4096]
			}
			log.Printf("Agent update: %v", e)
		} else {
			status.Phase = "staged"
		}
		if e := selfupdate.Save(path, status); e != nil {
			log.Printf("Agent update state: %v", e)
		}
	}()
}

// Trial validation reads persisted process identities without healing applications.
func (c *Client) validateTrial(ctx context.Context) error {
	var deps []model.Deployment
	if e := c.DB.View(func(t *store.Tx) error {
		var e error
		deps, e = store.List[model.Deployment](t, "deployments")
		return e
	}); e != nil {
		return e
	}
	for _, d := range deps {
		if d.Current == "" {
			continue
		}
		sp := supervisor.Spec{Root: c.Root, AppID: d.AppID, Version: d.Current, Env: d.Config.Env}
		st, e := c.Engine.Supervisor.Status(ctx, sp)
		if e != nil {
			return e
		}
		if d.DesiredState == "running" && !st.Running {
			return fmt.Errorf("cannot adopt running application %s", d.AppID)
		}
	}
	return nil
}
