package controller

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"deployer/internal/identity"
	"deployer/internal/model"
	"deployer/internal/packages"
	"deployer/internal/store"
	"golang.org/x/crypto/bcrypt"
)

type Server struct {
	issuer       *identity.Issuer
	enrollMu     sync.Mutex
	enrollMinute int64
	enrollCount  int

	UpdatePublicKey string
	ClientCA        string
	Root            string
	DB              *store.Store
	mux             *http.ServeMux
	packagesMu      sync.RWMutex
	loginMu         sync.Mutex
	attempts        map[string][]int64
	cookieSecure    bool
	batchMu         sync.Mutex
	done            chan struct{}
	workers         sync.WaitGroup
}
type apiError struct {
	status  int
	message string
}

func (e apiError) Error() string      { return e.message }
func fail(code int, msg string) error { return apiError{code, msg} }
func hash(s string) string            { b := sha256.Sum256([]byte(s)); return hex.EncodeToString(b[:]) }
func jsonOut(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func decode(w http.ResponseWriter, r *http.Request, v any) error { return decodeLimit(w, r, v, 4<<20) }
func decodeLimit(w http.ResponseWriter, r *http.Request, v any, limit int64) error {
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	d := json.NewDecoder(r.Body)
	// Heartbeats are extensible within one protocol version so an older Agent
	// can reconnect and receive its update. Operator requests remain strict.
	if _, heartbeat := v.(*model.Poll); !heartbeat {
		d.DisallowUnknownFields()
	}
	if e := d.Decode(v); e != nil {
		return fail(400, "无效请求: "+e.Error())
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return fail(400, "请求包含多余内容")
	}
	return nil
}

func New(root string, secure bool, ui http.Handler) (*Server, error) {
	abs, e := filepath.Abs(root)
	if e != nil {
		return nil, e
	}
	for _, dir := range []string{"state", "packages", "archives", "staging"} {
		if e = os.MkdirAll(filepath.Join(abs, dir), 0700); e != nil {
			return nil, e
		}
	}
	db, e := store.Open(filepath.Join(abs, "state", "controller.db"))
	if e != nil {
		return nil, e
	}
	s := &Server{Root: abs, DB: db, mux: http.NewServeMux(), attempts: map[string][]int64{}, cookieSecure: secure, done: make(chan struct{})}
	e = db.Update(func(t *store.Tx) error {
		users, e := store.List[model.User](t, "users")
		if e != nil {
			return e
		}
		if len(users) > 0 {
			return nil
		}
		b, e := bcrypt.GenerateFromPassword([]byte(DefaultAdminPassword), bcrypt.DefaultCost)
		if e != nil {
			return e
		}
		return store.Put(t, "users", "admin", model.User{ID: "admin", Name: "管理员", Role: "admin", PasswordHash: string(b), AuthVersion: 1})
	})
	if e != nil {
		db.Close()
		return nil, e
	}
	if e = s.recoverImports(); e != nil {
		db.Close()
		return nil, e
	}
	s.mux.HandleFunc("POST /api/login", s.wrap(s.login))
	s.mux.HandleFunc("POST /api/login/mfa", s.wrap(s.loginMFA))
	s.mux.HandleFunc("GET /api/account", s.authorize("viewer", s.account))
	s.mux.HandleFunc("POST /api/account/password", s.authorize("viewer", s.changePassword))
	s.mux.HandleFunc("POST /api/account/totp/setup", s.authorize("viewer", s.setupTOTP))
	s.mux.HandleFunc("POST /api/account/totp/confirm", s.authorize("viewer", s.confirmTOTP))
	s.mux.HandleFunc("POST /api/account/totp/disable", s.authorize("viewer", s.disableTOTP))
	s.mux.HandleFunc("POST /api/account/recovery-codes", s.authorize("viewer", s.regenerateRecovery))
	s.mux.HandleFunc("GET /api/session", s.authorize("viewer", s.session))
	s.mux.HandleFunc("POST /api/logout", s.authorize("viewer", s.logout))
	s.mux.HandleFunc("GET /api/snapshot", s.authorize("viewer", s.snapshot))
	s.mux.HandleFunc("DELETE /api/apps/{app}", s.authorize("admin", s.deleteApp))
	s.mux.HandleFunc("POST /api/apps", s.authorize("operator", s.saveApp))
	s.mux.HandleFunc("POST /api/apps/{app}/versions", s.authorize("operator", s.upload))
	s.mux.HandleFunc("DELETE /api/apps/{app}/versions/{version}", s.authorize("admin", s.deleteVersion))
	s.mux.HandleFunc("POST /api/nodes/{node}/apps/{app}/operations", s.authorize("operator", s.createOperation))
	s.mux.HandleFunc("POST /api/batches", s.authorize("operator", s.createBatch))
	s.mux.HandleFunc("GET /api/operations/{id}", s.authorize("viewer", s.operation))
	s.mux.HandleFunc("GET /api/operations/{id}/events", s.authorize("viewer", s.events))
	s.mux.HandleFunc("DELETE /api/nodes/{node}", s.authorize("admin", s.deleteNode))
	s.mux.HandleFunc("POST /api/users", s.authorize("admin", s.saveUser))
	s.mux.HandleFunc("GET /api/users", s.authorize("admin", s.users))
	s.mux.HandleFunc("POST /enroll", s.wrap(s.enroll))
	s.mux.HandleFunc("GET /api/agent-releases", s.authorize("viewer", s.agentReleases))
	s.mux.HandleFunc("POST /api/agent-releases", s.authorize("admin", s.uploadAgentRelease))
	s.mux.HandleFunc("GET /api/agent-rollout", s.authorize("viewer", s.agentRollout))
	s.mux.HandleFunc("POST /api/agent-rollout", s.authorize("admin", s.setAgentRollout))
	s.mux.HandleFunc("GET /agent/releases/{digest}", s.agentAuth(s.downloadAgentRelease))
	s.mux.HandleFunc("POST /agent/poll", s.agentAuth(s.poll))
	s.mux.HandleFunc("GET /agent/packages/{digest}", s.agentAuth(s.download))
	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		jsonOut(w, 200, map[string]any{"ok": true, "protocol": model.Protocol})
	})
	s.mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		jsonOut(w, 404, map[string]string{"error": "接口不存在"})
	})
	s.mux.Handle("/", ui)
	s.workers.Add(1)
	go func() {
		defer s.workers.Done()
		lastMaintenance := time.Time{}
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-s.done:
				return
			case <-ticker.C:
				if time.Since(lastMaintenance) > time.Hour {
					if err := s.maintain(); err != nil {
						log.Printf("自动维护: %v", err)
					}
					lastMaintenance = time.Now()
				}
				if err := s.advanceBatches(); err != nil {
					log.Printf("批量任务: %v", err)
				}
			}
		}
	}()
	return s, nil
}
func (s *Server) Close() error { close(s.done); s.workers.Wait(); return s.DB.Close() }
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Referrer-Policy", "same-origin")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; connect-src 'self'; img-src 'self' data:; frame-ancestors 'none'")
	s.mux.ServeHTTP(w, r)
}
func (s *Server) wrap(fn func(http.ResponseWriter, *http.Request) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if e := fn(w, r); e != nil {
			code := 500
			msg := "内部错误"
			var ae apiError
			if errors.As(e, &ae) {
				code = ae.status
				msg = ae.message
			} else if errors.Is(e, store.ErrNotFound) {
				code = 404
				msg = "记录不存在"
			} else {
				log.Printf("%s %s: %v", r.Method, r.URL.Path, e)
			}
			jsonOut(w, code, map[string]string{"error": msg})
		}
	}
}
func audit(t *store.Tx, user, action, target string) error {
	a := model.Audit{ID: model.ID(), Time: model.Now(), Actor: user, Action: action, Target: target}
	return store.Put(t, "audit", a.ID, a)
}

func (s *Server) snapshot(w http.ResponseWriter, r *http.Request) error {
	out := map[string]any{}
	e := s.DB.View(func(t *store.Tx) error {
		apps, e := store.List[model.App](t, "apps")
		if e != nil {
			return e
		}
		out["apps"] = apps
		versions, e := store.List[model.Version](t, "versions")
		if e != nil {
			return e
		}
		out["versions"] = versions
		nodes, e := store.List[model.Node](t, "nodes")
		if e != nil {
			return e
		}
		out["nodes"] = nodes
		deps, e := store.List[model.Deployment](t, "deployments")
		if e != nil {
			return e
		}
		for i := range deps {
			deps[i] = model.PublicDeployment(deps[i])
		}
		out["deployments"] = deps
		ops, e := store.Query[model.Operation](t, "operations", store.Filter{States: []string{"active", "needs_attention"}})
		if e != nil {
			return e
		}
		recent, e := store.Query[model.Operation](t, "operations", store.Filter{Limit: 200, States: []string{"succeeded", "failed", "failed_recovered", "failed_recovery"}})
		if e != nil {
			return e
		}
		ops = append(ops, recent...)
		sort.Slice(ops, func(i, j int) bool { return ops[i].CreatedAt > ops[j].CreatedAt })
		for i := range ops {
			ops[i] = model.PublicOperation(ops[i])
		}
		out["operations"] = ops
		batches, e := store.Query[Batch](t, "batches", store.Filter{Limit: 100})
		if e != nil {
			return e
		}
		for i := range batches {
			batches[i].Request.Env = nil
			if batches[i].Prepared != nil {
				batches[i].Prepared.Env = nil
			}
		}
		out["batches"] = batches
		inv := map[string][]model.Installation{}
		for _, n := range nodes {
			v, e := store.Get[[]model.Installation](t, "inventories", n.ID)
			if e != nil && !errors.Is(e, store.ErrNotFound) {
				return e
			}
			inv[n.ID] = v
		}
		out["inventories"] = inv
		if actor(r).Role == "admin" {
			aud, e := store.Query[model.Audit](t, "audit", store.Filter{Limit: 100})
			if e != nil {
				return e
			}
			sort.Slice(aud, func(i, j int) bool { return aud[i].Time > aud[j].Time })
			if len(aud) > 100 {
				aud = aud[:100]
			}
			out["audit"] = aud
		}
		return nil
	})
	if e != nil {
		return e
	}
	out["now"] = model.Now()
	jsonOut(w, 200, out)
	return nil
}
func (s *Server) saveApp(w http.ResponseWriter, r *http.Request) error {
	var a model.App
	if e := decode(w, r, &a); e != nil {
		return e
	}
	if !model.ValidName(a.ID) || len(a.Name) > 200 || len(a.Description) > 4000 {
		return fail(400, "App 标识或描述无效")
	}
	h, e := a.Health.Normalize()
	if e != nil {
		return fail(400, e.Error())
	}
	a.Health = h
	a.CreatedAt = model.Now()
	if a.Name == "" {
		a.Name = a.ID
	}
	e = s.DB.Update(func(t *store.Tx) error {
		old, e := store.Get[model.App](t, "apps", a.ID)
		if e == nil {
			a.CreatedAt = old.CreatedAt
		} else if !errors.Is(e, store.ErrNotFound) {
			return e
		}
		if e = store.Put(t, "apps", a.ID, a); e != nil {
			return e
		}
		return audit(t, actor(r).ID, "save_app", a.ID)
	})
	if e != nil {
		return e
	}
	jsonOut(w, 200, a)
	return nil
}

func (s *Server) upload(w http.ResponseWriter, r *http.Request) error {
	app := r.PathValue("app")
	ver := r.URL.Query().Get("version")
	platform := r.URL.Query().Get("arch")
	if !model.ValidName(app) || !model.ValidName(ver) || (platform != "amd64" && platform != "arm64") {
		return fail(400, "需要有效的 app、version 和 arch（amd64/arm64）")
	}
	s.packagesMu.Lock()
	defer s.packagesMu.Unlock()
	if e := s.recoverImports(); e != nil {
		return e
	}
	var exists model.Version
	if e := s.DB.View(func(t *store.Tx) error {
		if _, e := store.Get[model.App](t, "apps", app); e != nil {
			return e
		}
		v, e := store.Get[model.Version](t, "versions", model.Key(app, ver))
		if e == nil {
			exists = v
		} else if !errors.Is(e, store.ErrNotFound) {
			return e
		}
		return nil
	}); e != nil {
		return e
	}
	f, e := os.CreateTemp(filepath.Join(s.Root, "staging"), "upload-")
	if e != nil {
		return e
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	r.Body = http.MaxBytesReader(w, r.Body, packages.MaxArchive)
	size, e := io.Copy(f, r.Body)
	if e == nil {
		e = f.Sync()
	}
	f.Close()
	if e != nil {
		return fail(400, "包上传失败或超过 512 MiB 限制")
	}
	digest, e := packages.Digest(tmp)
	if e != nil {
		return e
	}
	if exists.Version != "" {
		if exists.Digest != digest {
			return fail(409, "该版本已使用过不同的包，请更换版本号")
		}
		if exists.Status == "available" {
			jsonOut(w, 200, exists)
			return nil
		}
		if exists.Status == "deleted" {
			return fail(409, "版本已删除，请发布新的版本号")
		}
	}
	v := model.Version{AppID: app, Version: ver, OS: "linux", Arch: platform, Digest: digest, Size: size, Status: "importing", CreatedAt: model.Now()}
	dir := filepath.Join(s.Root, "staging", model.ID())
	defer os.RemoveAll(dir)
	manifest, e := packages.Extract(tmp, dir, v)
	if e != nil {
		return fail(400, e.Error())
	}
	if manifest != nil {
		v.Health = manifest.Health
	}
	if e = s.DB.Update(func(t *store.Tx) error { return store.Put(t, "versions", model.Key(app, ver), v) }); e != nil {
		return e
	}
	if e = os.MkdirAll(filepath.Join(s.Root, "packages", app), 0700); e != nil {
		return e
	}
	target := filepath.Join(s.Root, "packages", app, ver)
	if _, e = os.Stat(target); e == nil {
		return fail(409, "版本目录已存在，导入需恢复")
	}
	if e = os.Rename(dir, target); e != nil {
		return e
	}
	if e = store.SyncDir(filepath.Dir(target)); e != nil {
		return e
	}
	archive := filepath.Join(s.Root, "archives", digest+".tar.gz")
	if e = os.Rename(tmp, archive); e != nil {
		return e
	}
	if e = store.SyncDir(filepath.Dir(archive)); e != nil {
		return e
	}
	v.Status = "available"
	e = s.DB.Update(func(t *store.Tx) error {
		if e := store.Put(t, "versions", model.Key(app, ver), v); e != nil {
			return e
		}
		return audit(t, actor(r).ID, "upload_version", model.Key(app, ver))
	})
	if e != nil {
		return e
	}
	jsonOut(w, 201, v)
	return nil
}
func (s *Server) recoverImports() error {
	return s.DB.Update(func(t *store.Tx) error {
		vs, err := store.List[model.Version](t, "versions")
		if err != nil {
			return err
		}
		retained := map[string]bool{}
		digests := map[string]bool{}
		removedApps := map[string]bool{}
		for _, v := range vs {
			key := model.Key(v.AppID, v.Version)
			if v.Status == "deleted" || v.Status == "deleting" || v.Status == "removing_clients" {
				if err = store.Delete(t, "versions", key); err != nil {
					return err
				}
				removedApps[v.AppID] = true
				continue
			}
			if v.Status == "importing" {
				d, e := packages.Digest(filepath.Join(s.Root, "archives", v.Digest+".tar.gz"))
				_, de := os.Stat(filepath.Join(s.Root, "packages", v.AppID, v.Version))
				if e == nil && de == nil && d == v.Digest {
					v.Status = "available"
				} else {
					v.Status = "failed"
				}
				if err = store.Put(t, "versions", key, v); err != nil {
					return err
				}
			}
			if v.Status != "failed" {
				retained[key] = true
				digests[v.Digest+".tar.gz"] = true
			}
		}
		for app := range removedApps {
			if err = removeEmptyApp(t, app); err != nil {
				return err
			}
		}
		apps, err := os.ReadDir(filepath.Join(s.Root, "packages"))
		if err != nil {
			return err
		}
		for _, app := range apps {
			path := filepath.Join(s.Root, "packages", app.Name())
			if !app.IsDir() {
				continue
			}
			versions, err := os.ReadDir(path)
			if err != nil {
				return err
			}
			for _, v := range versions {
				if !retained[model.Key(app.Name(), v.Name())] {
					if err = os.RemoveAll(filepath.Join(path, v.Name())); err != nil {
						return err
					}
				}
			}
			entries, err := os.ReadDir(path)
			if err != nil {
				return err
			}
			if len(entries) == 0 {
				if err = os.Remove(path); err != nil {
					return err
				}
			}
		}
		archives, err := os.ReadDir(filepath.Join(s.Root, "archives"))
		if err != nil {
			return err
		}
		for _, a := range archives {
			if !digests[a.Name()] {
				if err = os.RemoveAll(filepath.Join(s.Root, "archives", a.Name())); err != nil {
					return err
				}
			}
		}
		// Discard legacy deletion records; repository absence now drives cleanup.
		records, err := store.List[struct {
			ID string `json:"id"`
		}](t, "deletions")
		if err != nil {
			return err
		}
		for _, r := range records {
			if err = store.Delete(t, "deletions", r.ID); err != nil {
				return err
			}
		}
		return nil
	})
}
func (s *Server) deleteVersion(w http.ResponseWriter, r *http.Request) error {
	return s.requestDeletion(w, r, r.PathValue("version"))
}

func (s *Server) createOperation(w http.ResponseWriter, r *http.Request) error {
	var req model.OperationRequest
	if e := decode(w, r, &req); e != nil {
		return e
	}
	var op model.Operation
	if err := s.enqueue(r.PathValue("node"), r.PathValue("app"), req, actor(r).ID, &op); err != nil {
		return err
	}
	jsonOut(w, 202, model.PublicOperation(op))
	return nil
}

func (s *Server) enqueue(node, app string, req model.OperationRequest, actorID string, output *model.Operation) error {
	if !model.ValidName(node) || !model.ValidName(app) || !model.ValidName(req.IdempotencyKey) {
		return fail(400, "需要有效的节点、App 和幂等键")
	}
	if e := model.ValidEnv(req.Env); e != nil {
		return fail(400, e.Error())
	}
	body, _ := json.Marshal(req)
	requestHash := hash(string(body))
	var op model.Operation
	e := s.DB.Update(func(t *store.Tx) error {
		requestID := hash(node + "/" + app + "/" + req.IdempotencyKey)
		prior, e := store.Get[string](t, "operation_requests", requestID)
		if e == nil {
			old, e := store.Get[model.Operation](t, "operations", prior)
			if e != nil {
				return e
			}
			if old.RequestHash != requestHash {
				return fail(409, "同一幂等键用于不同请求")
			}
			op = old
			return nil
		}
		if !errors.Is(e, store.ErrNotFound) {
			return e
		}
		ops, e := store.Query[model.Operation](t, "operations", store.Filter{Owner: node, Scope: app, States: []string{"active", "needs_attention"}})
		if e != nil {
			return e
		}

		n, e := store.Get[model.Node](t, "nodes", node)
		if e != nil {
			return e
		}
		if model.Now()-n.LastSeen > 30000 {
			return fail(409, "节点离线或已撤销，请等待节点恢复")
		}
		a, e := store.Get[model.App](t, "apps", app)
		if e != nil {
			return e
		}
		d, e := store.Get[model.Deployment](t, "deployments", model.NodeKey(node, app))
		if errors.Is(e, store.ErrNotFound) {
			existing, err := store.List[model.Deployment](t, "deployments")
			if err != nil {
				return err
			}
			managed := map[string]bool{}
			for _, item := range existing {
				if item.NodeID == node && (item.Current != "" || item.DesiredVersion != "" || item.RunningVersion != "") {
					managed[item.AppID] = true
				}
			}
			items, err := store.Get[[]model.Installation](t, "inventories", node)
			if err != nil && !errors.Is(err, store.ErrNotFound) {
				return err
			}
			for _, item := range items {
				managed[item.AppID] = true
			}
			if len(managed) >= 256 && !managed[app] {
				return fail(409, "每台主机最多管理 256 个应用")
			}
			d = model.Deployment{NodeID: node, AppID: app, DesiredState: "stopped", RunState: "stopped"}
		} else if e != nil {
			return e
		}
		for _, x := range ops {
			if x.NodeID == node && x.AppID == app && (!x.Terminal() || (x.Result == "needs_attention" && req.Action != "reconcile")) {
				return fail(409, "已有执行中或需要现场核对的任务")
			}
		}
		if req.SkipIfDeployed && (req.Action == "deploy" || req.Action == "switch" || req.Action == "install") {
			v, e := store.Get[model.Version](t, "versions", model.Key(app, req.Version))
			if e != nil {
				return e
			}
			if v.Status != "available" || v.OS != n.OS || v.Arch != n.Arch {
				return fail(409, "目标版本不可用或平台不匹配")
			}
			skip := d.Current == req.Version && req.Version != "" && d.Error == "" && d.Health != "unhealthy"
			if req.Action == "install" {
				skip = false
				items, e := store.Get[[]model.Installation](t, "inventories", node)
				if e != nil && !errors.Is(e, store.ErrNotFound) {
					return e
				}
				for _, i := range items {
					if i.AppID == app && i.Version == req.Version && i.Initialized {
						skip = true
					}
				}
			}
			if skip {
				now := model.Now()
				op = model.Operation{ID: model.ID(), NodeID: node, AppID: app, Action: req.Action, Version: req.Version, IdempotencyKey: req.IdempotencyKey, RequestHash: requestHash, ExpectedRevision: d.Revision, Revision: d.Revision, Stage: "finished", Result: "succeeded", Skipped: true, CreatedAt: now, UpdatedAt: now, FinishedAt: now, Before: d, Actor: actorID, Config: d.Config}
				if e = store.Put(t, "operations", op.ID, op); e != nil {
					return e
				}
				if e = store.Put(t, "operation_requests", requestID, op.ID); e != nil {
					return e
				}
				return audit(t, actorID, "deploy_skipped", node+"/"+app+"/"+req.Version)
			}
		}
		if d.Revision != req.ExpectedRevision {
			return fail(409, "部署状态已变化，请刷新后重试")
		}
		op = model.Operation{ID: model.ID(), NodeID: node, AppID: app, Action: req.Action, Version: req.Version, IdempotencyKey: req.IdempotencyKey, RequestHash: requestHash, ExpectedRevision: d.Revision, Revision: d.Revision + 1, Stage: "queued", CreatedAt: model.Now(), UpdatedAt: model.Now(), Before: d, Actor: actorID, Config: d.Config, Start: d.DesiredState == "running"}
		if op.Config.ID == "" {
			op.Config = model.Config{ID: model.ID(), Health: a.Health}
		}
		if req.Health == nil && (req.Action == "deploy" || req.Action == "switch" || req.Action == "install") {
			op.Config.Health = a.Health
			op.Config.ID = model.ID()
		}
		if req.Env != nil {
			op.Config.Env = req.Env
			op.Config.ID = model.ID()
		}
		if req.Health != nil {
			h, e := req.Health.Normalize()
			if e != nil {
				return fail(400, e.Error())
			}
			op.Config.Health = h
			op.Config.ID = model.ID()
		}

		if req.Start != nil {
			op.Start = *req.Start
		}
		switch req.Action {
		case "install", "deploy", "switch":
			if !model.ValidName(op.Version) {
				return fail(400, "请选择目标版本")
			}
			if req.Action == "deploy" {
				op.Start = true
			}
		case "start", "stop", "restart", "logs":
			if d.Current == "" {
				return fail(409, "尚无当前版本")
			}
			if op.Version != "" && op.Version != d.Current {
				return fail(409, "此操作只能作用于当前版本")
			}
			op.Version = d.Current
			op.Start = req.Action != "stop"
		case "clean_data", "remove_version":
			if !req.Confirm {
				return fail(400, "必须明确确认清理或删除")
			}
			if !model.ValidName(op.Version) {
				return fail(400, "请选择目标版本")
			}
			if req.Action == "clean_data" && op.Version == d.Current && d.DesiredState != "stopped" {
				return fail(409, "请先停止当前版本")
			}
			is, e := store.Get[[]model.Installation](t, "inventories", node)
			if e != nil {
				return e
			}
			found := false
			for _, i := range is {
				if i.AppID == app && i.Version == op.Version {
					op.DataEpoch = i.DataEpoch
					found = true
				}
			}
			if !found {
				return fail(404, "主机未安装此版本")
			}
		case "detach", "reconcile":
			if !req.Confirm {
				return fail(400, "请确认解除部署")
			}
			op.Version = d.Current
			op.Start = false
		default:
			return fail(400, "不支持的操作")
		}
		if req.Action == "install" || req.Action == "deploy" || req.Action == "switch" || req.Action == "start" || req.Action == "restart" {
			v, e := store.Get[model.Version](t, "versions", model.Key(app, op.Version))
			if e != nil {
				return e
			}
			if v.Status != "available" {
				return fail(409, "仓库版本不可用")
			}
			if v.OS != n.OS || v.Arch != n.Arch {
				return fail(409, "版本与主机平台不匹配")
			}
			op.Artifact = &v
			if req.Health == nil && v.Health != nil && (req.Action == "deploy" || req.Action == "switch" || req.Action == "install") {
				op.Config.Health = *v.Health
				op.Config.ID = model.ID()
			}
		}
		d.Revision = op.Revision
		switch req.Action {
		case "deploy", "switch", "start", "restart":
			d.DesiredVersion = op.Version
			d.DesiredState = "stopped"
			if op.Start {
				d.DesiredState = "running"
			}
		case "stop", "detach", "reconcile":
			d.DesiredState = "stopped"
		}
		if e = store.Put(t, "deployments", model.NodeKey(node, app), d); e != nil {
			return e
		}
		if e = store.Put(t, "operations", op.ID, op); e != nil {
			return e
		}
		if e = store.Put(t, "operation_requests", requestID, op.ID); e != nil {
			return e
		}
		return audit(t, actorID, req.Action, node+"/"+app+"/"+op.Version)
	})
	if e != nil {
		return e
	}
	*output = op
	return nil
}
func (s *Server) operation(w http.ResponseWriter, r *http.Request) error {
	var o model.Operation
	e := s.DB.View(func(t *store.Tx) error {
		var e error
		o, e = store.Get[model.Operation](t, "operations", r.PathValue("id"))
		return e
	})
	if e != nil {
		return e
	}
	jsonOut(w, 200, model.PublicOperation(o))
	return nil
}
func (s *Server) events(w http.ResponseWriter, r *http.Request) error {
	id := r.PathValue("id")
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	if h := r.Header.Get("Last-Event-ID"); h != "" {
		after, _ = strconv.ParseInt(h, 10, 64)
	}
	stream := r.URL.Query().Get("stream") == "true"
	var list []model.Event
	get := func() error {
		return s.DB.View(func(t *store.Tx) error {
			if _, e := store.Get[model.Operation](t, "operations", id); e != nil {
				return e
			}
			var e error
			list, e = store.Events[model.Event](t, id, after, 500)
			return e
		})
	}
	if e := get(); e != nil {
		return e
	}
	if !stream {
		jsonOut(w, 200, list)
		return nil
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("X-Accel-Buffering", "no")
	flusher, ok := w.(http.Flusher)
	if !ok {
		return fail(500, "不支持流式响应")
	}
	deadline := time.NewTimer(25 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		for _, ev := range list {
			b, _ := json.Marshal(ev)
			fmt.Fprintf(w, "id: %d\ndata: %s\n\n", ev.Seq, b)
			after = ev.Seq
		}
		fmt.Fprint(w, ": heartbeat\n\n")
		flusher.Flush()
		select {
		case <-r.Context().Done():
			return nil
		case <-deadline.C:
			return nil
		case <-ticker.C:
			if e := get(); e != nil {
				return nil
			}
		}
	}
}

func (s *Server) poll(w http.ResponseWriter, r *http.Request) error {
	var p model.Poll
	if e := decodeLimit(w, r, &p, 16<<20); e != nil {
		return e
	}
	if p.Protocol != model.Protocol {
		return fail(409, "协议版本不一致")
	}
	if len(p.AgentUpdate.Version) > 96 || len(p.AgentUpdate.Digest) > 64 || len(p.AgentUpdate.Target) > 64 || len(p.AgentUpdate.Error) > 4096 || len(p.AgentUpdate.Phase) > 32 {
		return fail(400, "更新状态超限")
	}
	if len(p.Events) > 200 || len(p.Operations) > 100 || len(p.Apps) > 10000 || len(p.Installations) > 10000 || len(p.Deployments) > 10000 || len(p.Rejected) > 100 {
		return fail(400, "状态批次超限")
	}
	if len(p.Name) > 200 || strings.TrimSpace(p.Name) == "" || p.OS != "linux" || (p.Arch != "amd64" && p.Arch != "arm64") {
		return fail(400, "无效主机心跳")
	}
	n := agentNode(r)
	// Use the authenticated TCP peer, never self-reported or forwarded headers.
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return fail(400, "无效心跳来源地址")
	}
	address, err := netip.ParseAddr(host)
	if err != nil {
		return fail(400, "无效心跳来源 IP")
	}
	n.IP = address.Unmap().String()
	out := model.PollResponse{Tasks: []model.Operation{}, Acknowledged: []string{}, EventAcks: map[string]int64{}}
	e := s.DB.Update(func(t *store.Tx) error {
		currentNode, e := store.Get[model.Node](t, "nodes", n.ID)
		fresh := errors.Is(e, store.ErrNotFound)
		if e != nil && !fresh {
			return e
		}
		if !fresh && (currentNode.OS != p.OS || currentNode.Arch != p.Arch) {
			return fail(409, "同一主机身份的平台不能改变")
		}
		n.Name = p.Name
		n.OS = p.OS
		n.Arch = p.Arch
		n.LastSeen = model.Now()
		n.FreeBytes = p.FreeBytes
		n.AgentUpdate = p.AgentUpdate
		if err := store.Put(t, "nodes", n.ID, n); err != nil {
			return err
		}
		if fresh {
			if err := audit(t, "agent:"+n.ID, "heartbeat_register", n.ID); err != nil {
				return err
			}
			_, removedErr := store.Get[bool](t, "removed_nodes", n.ID)
			restored, restoreErr := store.Get[bool](t, "settings", "backup_restored")
			if removedErr != nil && !errors.Is(removedErr, store.ErrNotFound) {
				return removedErr
			}
			if restoreErr != nil && !errors.Is(restoreErr, store.ErrNotFound) {
				return restoreErr
			}
			if removedErr == nil || restored {
				if err := store.Put(t, "restored_nodes", n.ID, true); err != nil {
					return err
				}
				if err := store.Delete(t, "removed_nodes", n.ID); err != nil {
					return err
				}
			}
		}
		restored, err := s.recoverRestoredNode(t, n, p)
		if err != nil {
			return err
		}
		for _, d := range p.Deployments {
			if !model.ValidName(d.AppID) {
				return fail(400, "无效部署标识")
			}
			old, e := store.Get[model.Deployment](t, "deployments", model.NodeKey(n.ID, d.AppID))
			if errors.Is(e, store.ErrNotFound) {
				old = d
			} else if e != nil {
				return e
			}
			if d.Revision > old.Revision {
				return fail(409, "未知部署修订")
			}
			if d.Revision < old.Revision {
				continue
			}
			d.NodeID = n.ID
			d.ObservedAt = model.Now()
			if e = store.Put(t, "deployments", model.NodeKey(n.ID, d.AppID), d); e != nil {
				return e
			}
		}
		for _, i := range p.Installations {
			if !model.ValidName(i.AppID) || !model.ValidName(i.Version) {
				return fail(400, "无效安装记录")
			}
		}
		if e := store.Put(t, "inventories", n.ID, p.Installations); e != nil {
			return e
		}
		for _, rejection := range p.Rejected {
			if len(rejection.Error) > 4096 {
				return fail(400, "任务拒绝原因过长")
			}
			op, err := store.Get[model.Operation](t, "operations", rejection.ID)
			if errors.Is(err, store.ErrNotFound) && restored {
				out.Acknowledged = append(out.Acknowledged, rejection.ID)
				continue
			}
			if err != nil || op.NodeID != n.ID {
				return fail(403, "拒绝报告不属于此节点")
			}
			if !op.Terminal() {
				op.Result = "needs_attention"
				op.Stage = "finished"
				op.Error = "Agent 拒绝任务: " + rejection.Error
				op.UpdatedAt = model.Now()
				op.FinishedAt = op.UpdatedAt
				if err = store.Put(t, "operations", op.ID, op); err != nil {
					return err
				}
				d, err := store.Get[model.Deployment](t, "deployments", model.NodeKey(n.ID, op.AppID))
				if err != nil {
					return err
				}
				d.RunState = "unknown"
				d.Health = "unknown"
				d.DesiredState = "stopped"
				d.Error = op.Error
				if err = store.Put(t, "deployments", model.NodeKey(n.ID, op.AppID), d); err != nil {
					return err
				}
			}
			out.Acknowledged = append(out.Acknowledged, op.ID)
		}
		for _, report := range p.Operations {
			op, e := store.Get[model.Operation](t, "operations", report.ID)
			if errors.Is(e, store.ErrNotFound) && restored {
				d, err := store.Get[model.Deployment](t, "deployments", model.NodeKey(n.ID, report.AppID))
				if err != nil || report.Revision > d.Revision || !model.ValidName(report.ID) {
					return fail(400, "无效恢复任务")
				}
				op = model.Operation{ID: report.ID, NodeID: n.ID, AppID: report.AppID, Action: report.Action, Version: report.Version, Revision: report.Revision, Stage: "finished", Result: "needs_attention", Error: "备份之后执行的任务，需核对现场状态", CreatedAt: model.Now(), FinishedAt: model.Now(), Actor: "backup-recovery"}
				if e = store.Put(t, "operations", op.ID, op); e != nil {
					return e
				}
			}
			if e != nil || op.NodeID != n.ID {
				return fail(403, "任务不属于此节点")
			}
			if !op.Terminal() || (op.Result == "needs_attention" && report.Result == "failed") {
				op.Stage = report.Stage
				op.Result = report.Result
				op.Error = report.Error
				op.UpdatedAt = model.Now()
				if op.Terminal() {
					op.FinishedAt = model.Now()
				}
				if e = store.Put(t, "operations", op.ID, op); e != nil {
					return e
				}
			}
			if op.Action == "reconcile" && op.Result == "succeeded" {
				blocked, err := store.Query[model.Operation](t, "operations", store.Filter{Owner: n.ID, Scope: op.AppID, States: []string{"needs_attention"}})
				if err != nil {
					return err
				}
				for _, prior := range blocked {
					if prior.Revision >= op.Revision {
						continue
					}
					prior.Result = "failed"
					prior.Stage = "finished"
					prior.Error += "；已通过停止并解除阻塞完成核对"
					prior.FinishedAt = model.Now()
					if err = store.Put(t, "operations", prior.ID, prior); err != nil {
						return err
					}
				}
			}
			if op.Terminal() {
				out.Acknowledged = append(out.Acknowledged, op.ID)
			}
		}
		for _, ev := range p.Events {
			op, e := store.Get[model.Operation](t, "operations", ev.OperationID)
			if errors.Is(e, store.ErrNotFound) && restored && ev.Seq > 0 {
				if ev.Seq > out.EventAcks[ev.OperationID] {
					out.EventAcks[ev.OperationID] = ev.Seq
				}
				continue
			}
			if e != nil || op.NodeID != n.ID || ev.Seq < 1 || len(ev.Message) > 16384 {
				return fail(400, "无效日志事件")
			}
			if e = store.AddEvent(t, ev.OperationID, ev.Seq, ev); e != nil {
				return e
			}
			if ev.Seq > out.EventAcks[ev.OperationID] {
				out.EventAcks[ev.OperationID] = ev.Seq
			}
			if e = store.TrimEvents(t, ev.OperationID, 2000); e != nil {
				return e
			}
		}
		out.Deleted, err = deletedFor(t, p)
		if err != nil {
			return err
		}
		if e := s.planAgentUpdate(t, n, p, &out); e != nil {
			return e
		}
		if p.Draining || p.Trial {
			return nil
		}
		ops, e := store.Query[model.Operation](t, "operations", store.Filter{Owner: n.ID, States: []string{"active"}, Asc: true})
		if e != nil {
			return e
		}
		sort.Slice(ops, func(i, j int) bool { return ops[i].CreatedAt < ops[j].CreatedAt })
		for _, op := range ops {
			if op.NodeID != n.ID || op.Terminal() {
				continue
			}
			if len(out.Tasks) >= 2 {
				break
			}
			if op.Stage == "queued" {
				op.Stage = "dispatched"
				op.UpdatedAt = model.Now()
				if e = store.Put(t, "operations", op.ID, op); e != nil {
					return e
				}
			}
			out.Tasks = append(out.Tasks, op)
		}
		return nil
	})
	if e != nil {
		return e
	}
	if len(out.Tasks) == 0 {
		select {
		case <-time.After(2 * time.Second):
		case <-r.Context().Done():
			return nil
		}
	}
	jsonOut(w, 200, out)
	return nil
}
func (s *Server) download(w http.ResponseWriter, r *http.Request) error {
	digest := r.PathValue("digest")
	if len(digest) != 64 || strings.Trim(digest, "0123456789abcdef") != "" {
		return fail(400, "非法摘要")
	}
	s.packagesMu.RLock()
	defer s.packagesMu.RUnlock()
	n := agentNode(r)
	allowed := false
	e := s.DB.View(func(t *store.Tx) error {
		ops, e := store.Query[model.Operation](t, "operations", store.Filter{Owner: n.ID, States: []string{"active"}, Asc: true})
		if e != nil {
			return e
		}
		for _, op := range ops {
			if op.NodeID == n.ID && !op.Terminal() && op.Artifact != nil && op.Artifact.Digest == digest {
				allowed = true
			}
		}
		return nil
	})
	if e != nil {
		return e
	}
	if !allowed {
		return fail(403, "没有下载此包的有效任务")
	}
	w.Header().Set("Content-Type", "application/gzip")
	http.ServeFile(w, r, filepath.Join(s.Root, "archives", digest+".tar.gz"))
	return nil
}
