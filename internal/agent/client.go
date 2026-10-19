package agent

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"deployer/internal/launcher"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"deployer/internal/identity"
	"deployer/internal/model"
	"deployer/internal/packages"
	"deployer/internal/store"
	"deployer/internal/supervisor"
)

type Identity struct {
	NodeID     string `json:"node_id"`
	Controller string `json:"controller"`
}
type TLSFiles struct{ CA, Cert, Key string }
type Client struct {
	Name       string
	Root       string
	DB         *store.Store
	HTTP       *http.Client
	Identity   Identity
	Engine     *Engine
	workers    sync.WaitGroup
	gate       sync.RWMutex
	updating   atomic.Bool
	nextUpdate time.Time
	lastUpdate string
	running    sync.Map
}

func Open(root, server string, files TLSFiles, sup supervisor.Interface) (*Client, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	u, err := url.Parse(server)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, fmt.Errorf("控制端必须为完整的 HTTPS 根地址")
	}
	if u.Scheme != "https" {
		return nil, fmt.Errorf("Agent 必须使用 HTTPS + mTLS")
	}
	pair, err := tls.LoadX509KeyPair(files.Cert, files.Key)
	if err != nil {
		return nil, fmt.Errorf("客户端证书: %w", err)
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return nil, err
	}
	nodeID, err := identity.NodeID(leaf)
	if err != nil {
		return nil, err
	}
	getPair, err := identity.ReloadPair(files.Cert, files.Key, nodeID)
	if err != nil {
		return nil, err
	}
	roots, err := identity.Trust(files.CA)
	if err != nil {
		return nil, fmt.Errorf("服务端 CA: %w", err)
	}
	for _, dir := range []string{"", "apps", "cache", "staging", "state"} {
		mode := os.FileMode(0700)
		if dir == "" || dir == "apps" {
			mode = 0755
		}
		if err = os.MkdirAll(filepath.Join(abs, dir), mode); err != nil {
			return nil, err
		}
		if err = os.Chmod(filepath.Join(abs, dir), mode); err != nil {
			return nil, err
		}
	}
	db, err := store.Open(filepath.Join(abs, "state", "agent.db"))
	if err != nil {
		return nil, err
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots, GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) { return getPair() }}
	transport.DialTLSContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		pool, err := identity.Trust(files.CA)
		if err != nil {
			return nil, err
		}
		config := transport.TLSClientConfig.Clone()
		config.RootCAs = pool
		config.ServerName = u.Hostname()
		dialer := tls.Dialer{NetDialer: &net.Dialer{Timeout: 10 * time.Second}, Config: config}
		return dialer.DialContext(ctx, network, addr)
	}
	c := &Client{Root: abs, DB: db, HTTP: &http.Client{Transport: transport, Timeout: 40 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	err = db.Update(func(t *store.Tx) error {
		id, er := store.Get[Identity](t, "settings", "identity")
		if errors.Is(er, store.ErrNotFound) {
			id = Identity{NodeID: nodeID, Controller: strings.TrimRight(server, "/")}
			if er = store.Put(t, "settings", "identity", id); er != nil {
				return er
			}
		} else if er != nil {
			return er
		}
		if id.NodeID != nodeID {
			return fmt.Errorf("此数据目录与客户端证书的主机身份不匹配")
		}
		if id.Controller != strings.TrimRight(server, "/") {
			return fmt.Errorf("此数据目录已绑定其他控制端")
		}
		c.Identity = id
		return nil
	})
	if err != nil {
		db.Close()
		return nil, err
	}
	c.Name, _ = os.Hostname()
	if c.Name == "" {
		c.Name = nodeID
	}
	c.Engine = NewEngine(abs, c.Identity.NodeID, db, sup, c.download)
	return c, nil
}
func (c *Client) Close() error { return c.DB.Close() }
func (c *Client) request(ctx context.Context, path string, in, out any) error {
	b, err := json.Marshal(in)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", c.Identity.Controller+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("控制端返回 %d: %s", resp.StatusCode, b)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(out)
}
func (c *Client) download(ctx context.Context, v model.Version, dest string) error {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(c.Root, &stat); err != nil {
		return err
	}
	if stat.Bavail*uint64(stat.Bsize) < uint64(v.Size)+64<<20 {
		return fmt.Errorf("磁盘剩余空间不足")
	}
	req, err := http.NewRequestWithContext(ctx, "GET", c.Identity.Controller+"/agent/packages/"+v.Digest, nil)
	if err != nil {
		return err
	}
	client := *c.HTTP
	client.Timeout = 10 * time.Minute
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("包下载返回 %d", resp.StatusCode)
	}
	f, err := os.OpenFile(dest, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	n, err := io.Copy(f, io.LimitReader(resp.Body, packages.MaxArchive+1))
	if err == nil && n != v.Size {
		err = fmt.Errorf("包大小不匹配")
	}
	if err == nil {
		err = f.Sync()
	}
	ce := f.Close()
	if err == nil {
		err = ce
	}
	return err
}
func (c *Client) poll(ctx context.Context) error {
	p := model.Poll{AgentUpdate: c.updateStatus(), Draining: launcher.Draining(), Trial: launcher.Trial(), Protocol: model.Protocol, Name: c.Name, OS: runtime.GOOS, Arch: runtime.GOARCH}
	var st syscall.Statfs_t
	if syscall.Statfs(c.Root, &st) == nil {
		p.FreeBytes = st.Bavail * uint64(st.Bsize)
	}
	err := c.DB.View(func(t *store.Tx) error {
		var er error
		p.Deployments, er = store.List[model.Deployment](t, "deployments")
		if er != nil {
			return er
		}
		p.Rejected, er = store.Query[model.TaskRejection](t, "rejections", store.Filter{Limit: 100})
		if er != nil {
			return er
		}
		p.Installations, er = store.List[model.Installation](t, "installations")
		if er != nil {
			return er
		}
		ops, er := store.Query[model.Operation](t, "operations", store.Filter{States: []string{"active"}, Limit: 100})
		if er != nil {
			return er
		}
		if len(ops) < 100 {
			terminal, err := store.Query[model.Operation](t, "outbox", store.Filter{Limit: 100 - len(ops), Asc: true})
			if err != nil {
				return err
			}
			ops = append(ops, terminal...)
		}
		for _, o := range ops {
			if len(p.Operations) >= 100 {
				break
			}
			report := model.PublicOperation(o)
			report.Before = model.Deployment{}
			report.Config = model.Config{}
			p.Operations = append(p.Operations, report)
		}
		pending, er := store.Query[string](t, "event_pending", store.Filter{Limit: 100, Asc: true})
		if er != nil {
			return er
		}
		for _, id := range pending {
			if len(p.Events) >= 200 {
				break
			}
			after, er := store.Get[int64](t, "event_acks", id)
			if er != nil && !errors.Is(er, store.ErrNotFound) {
				return er
			}
			ev, er := store.Events[model.Event](t, id, after, 200-len(p.Events))
			if er != nil {
				return er
			}
			p.Events = append(p.Events, ev...)
		}

		return nil
	})
	if err != nil {
		return err
	}
	var response model.PollResponse
	if err = c.request(ctx, "/agent/poll", p, &response); err != nil {
		return err
	}
	err = c.DB.Update(func(t *store.Tx) error {
		for _, id := range response.Acknowledged {
			o, er := store.Get[model.Operation](t, "operations", id)
			if errors.Is(er, store.ErrNotFound) {
				if er = store.Delete(t, "rejections", id); er != nil {
					return er
				}
				continue
			}
			if er != nil {
				return er
			}
			if o.Terminal() {
				o.Acked = true
				if er = store.Put(t, "operations", id, o); er != nil {
					return er
				}
				if er = store.Delete(t, "outbox", id); er != nil {
					return er
				}
			}
		}
		for id, n := range response.EventAcks {
			last, er := store.LastSeq(t, id)
			if er != nil {
				return er
			}
			if n >= last {
				if er = store.Delete(t, "event_pending", id); er != nil {
					return er
				}
			}

			if er := store.Put(t, "event_acks", id, n); er != nil {
				return er
			}
			if er := store.TrimEvents(t, id, 2000); er != nil {
				return er
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	c.scheduleUpdate(ctx, response.Update)
	if launcher.Draining() || launcher.Trial() {
		return nil
	}
	for _, task := range response.Tasks {
		if _, loaded := c.running.LoadOrStore(task.ID, true); loaded {
			continue
		}
		c.gate.RLock()
		c.workers.Add(1)
		go c.executeTask(ctx, task)
	}
	return nil
}
func (c *Client) executeTask(ctx context.Context, task model.Operation) {
	defer c.workers.Done()
	defer c.gate.RUnlock()
	defer c.running.Delete(task.ID)
	defer func() {
		if p := recover(); p != nil {
			_ = c.Engine.markAttention(task.ID, fmt.Errorf("panic: %v", p))
		}
	}()
	fresh, err := c.Engine.Accept(task)
	if err != nil {
		log.Printf("任务 %s 拒绝: %v", task.ID, err)
		_ = c.DB.Update(func(t *store.Tx) error {
			d, _ := store.Get[model.Deployment](t, "deployments", task.AppID)
			return store.Put(t, "rejections", task.ID, model.TaskRejection{ID: task.ID, Error: err.Error(), LocalRevision: d.Revision})
		})
		return
	}
	var saved model.Operation
	if err = c.DB.View(func(t *store.Tx) error {
		var er error
		saved, er = store.Get[model.Operation](t, "operations", task.ID)
		return er
	}); err != nil {
		return
	}
	if saved.Terminal() {
		return
	}
	if fresh {
		err = c.Engine.Run(ctx, saved.ID)
	} else {
		err = c.Engine.RecoverOperation(ctx, saved)
	}
	if err != nil && ctx.Err() == nil {
		log.Printf("任务 %s 恢复: %v", saved.ID, err)
		_ = c.DB.View(func(t *store.Tx) error {
			var er error
			saved, er = store.Get[model.Operation](t, "operations", saved.ID)
			return er
		})
		if next := c.Engine.RecoverOperation(ctx, saved); next != nil && ctx.Err() == nil {
			_ = c.Engine.markAttention(saved.ID, next)
		}
	}
}

func (c *Client) Run(ctx context.Context) error {
	if err := c.Engine.Recover(ctx); err != nil {
		return err
	}
	if err := c.Engine.Boot(ctx); err != nil {
		return err
	}
	defer c.workers.Wait()
	c.workers.Add(1)
	go func() {
		defer c.workers.Done()
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			c.gate.RLock()
			if !launcher.Draining() && !launcher.Trial() {
				if err := c.Engine.Observe(ctx); err != nil && ctx.Err() == nil {
					log.Printf("状态核对: %v", err)
				}
			}
			c.gate.RUnlock()
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	c.workers.Add(1)
	go func() {
		defer c.workers.Done()
		ticker := time.NewTicker(time.Hour)
		defer ticker.Stop()
		for {
			c.gate.RLock()
			if !launcher.Draining() && !launcher.Trial() {
				if err := c.maintain(); err != nil && ctx.Err() == nil {
					log.Printf("状态维护: %v", err)
				}
			}
			c.gate.RUnlock()
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()

	failures := 0
	for {
		if ctx.Err() != nil {
			return nil
		}

		if launcher.Draining() {
			c.gate.Lock()
			err := launcher.Pulse("drained")
			c.gate.Unlock()
			if err != nil {
				return err
			}
		} else {
			phase := "ready"
			if launcher.Trial() {
				if e := c.validateTrial(ctx); e != nil {
					phase = "unhealthy"
					log.Printf("trial validation: %v", e)
				}
			}
			if err := launcher.Pulse(phase); err != nil {
				return err
			}
		}
		if err := c.poll(ctx); err != nil {
			// Reconnect after authentication/transport failure so renewed
			// certificates replace the identity on a long-lived TLS session.
			c.HTTP.CloseIdleConnections()
			if ctx.Err() != nil {
				return nil
			}
			failures++
			log.Printf("控制端连接: %v", err)
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(time.Duration(1<<min(failures, 5)) * time.Second):
			}
		} else {
			failures = 0
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(time.Second):
			}
		}
	}
}
