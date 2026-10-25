package controller

import (
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"deployer/internal/model"
	"deployer/internal/selfupdate"
	"deployer/internal/store"
)

const agentRolloutConcurrency = 20

type AgentRollout struct {
	Digest    string          `json:"digest"`
	Nodes     []string        `json:"nodes"`
	Index     int             `json:"index"`
	Paused    bool            `json:"paused"`
	Error     string          `json:"error,omitempty"`
	Active    map[string]bool `json:"active"`
	Completed map[string]bool `json:"completed"`
}

// Keep Index as the completed prefix for old readers. Completed also records
// hosts that finish out of order. Persist reservations before offering updates.
func (r *AgentRollout) normalize() {
	if r.Completed == nil {
		r.Completed = map[string]bool{}
		for i, id := range r.Nodes {
			if i < r.Index {
				r.Completed[id] = true
			}
		}
		// Preserve the one host already authorized by the former serial rollout.
		if r.Index >= 0 && r.Index < len(r.Nodes) {
			if r.Active == nil {
				r.Active = map[string]bool{}
			}
			r.Active[r.Nodes[r.Index]] = true
		}
	}
	if r.Active == nil {
		r.Active = map[string]bool{}
	}
}

func (r AgentRollout) includes(id string) bool {
	for _, node := range r.Nodes {
		if node == id {
			return true
		}
	}
	return false
}

func (s *Server) releasePath(d string) string { return filepath.Join(s.Root, "agent-releases", d) }
func (s *Server) release(d string) (selfupdate.Manifest, error) {
	var m selfupdate.Manifest
	if !selfupdate.ValidDigest(d) {
		return m, fail(400, "无效摘要")
	}
	if e := selfupdate.Load(filepath.Join(s.releasePath(d), "manifest.json"), &m); e != nil {
		return m, e
	}
	key, e := selfupdate.ReadKey(s.UpdatePublicKey)
	if e != nil {
		return m, e
	}
	if e = m.Verify(key); e != nil {
		return m, e
	}
	if m.Digest != d {
		return m, fmt.Errorf("release digest mismatch")
	}
	return m, nil
}
func (s *Server) uploadAgentRelease(w http.ResponseWriter, r *http.Request) error {
	key, e := selfupdate.ReadKey(s.UpdatePublicKey)
	if e != nil {
		return fail(409, "未配置发布公钥")
	}
	r.Body = http.MaxBytesReader(w, r.Body, selfupdate.MaxBinary+(1<<20))
	if e = r.ParseMultipartForm(1 << 20); e != nil {
		return fail(400, "无效发布包")
	}
	defer r.MultipartForm.RemoveAll()
	var m selfupdate.Manifest
	if e = json.Unmarshal([]byte(r.FormValue("manifest")), &m); e != nil {
		return fail(400, "无效 manifest")
	}
	if e = m.Verify(key); e != nil {
		return fail(400, e.Error())
	}
	src, _, e := r.FormFile("binary")
	if e != nil {
		return fail(400, "缺少 binary")
	}
	defer src.Close()
	s.packagesMu.Lock()
	defer s.packagesMu.Unlock()
	releaseKey := m.Version + "/" + m.OS + "/" + m.Arch
	if e = s.DB.View(func(t *store.Tx) error {
		old, e := store.Get[string](t, "agent_release_versions", releaseKey)
		if errors.Is(e, store.ErrNotFound) {
			return nil
		}
		if e != nil {
			return e
		}
		if old != m.Digest {
			return fail(409, "版本已发布，不能覆盖")
		}
		return nil
	}); e != nil {
		return e
	}

	if e = os.MkdirAll(filepath.Join(s.Root, "agent-releases"), 0700); e != nil {
		return e
	}
	stage, e := os.MkdirTemp(filepath.Join(s.Root, "agent-releases"), ".stage-")
	if e != nil {
		return e
	}
	defer os.RemoveAll(stage)
	f, e := os.OpenFile(filepath.Join(stage, "binary"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		return e
	}
	n, e := io.Copy(f, io.LimitReader(src, m.Size+1))
	if e == nil && n != m.Size {
		e = fmt.Errorf("size mismatch")
	}
	if e == nil {
		e = f.Sync()
	}
	ce := f.Close()
	if e == nil {
		e = ce
	}
	if e != nil {
		return fail(400, e.Error())
	}
	if e = m.CheckFile(f.Name()); e != nil {
		return fail(400, e.Error())
	}
	if e = selfupdate.Save(filepath.Join(stage, "manifest.json"), m); e != nil {
		return e
	}
	if old, err := s.release(m.Digest); err == nil {
		if string(old.Payload()) != string(m.Payload()) {
			return fail(409, "不可覆盖已发布内容")
		}
	} else {
		if e = os.Rename(stage, s.releasePath(m.Digest)); e != nil {
			return e
		}
		if e = selfupdate.SyncDir(filepath.Dir(stage)); e != nil {
			return e
		}
	}
	if e = s.DB.Update(func(t *store.Tx) error {
		if e := store.Put(t, "agent_release_versions", releaseKey, m.Digest); e != nil {
			return e
		}
		return audit(t, "admin", "agent_release", m.Digest)
	}); e != nil {
		return e
	}
	jsonOut(w, 200, m)
	return nil
}
func (s *Server) setAgentRollout(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Digest string   `json:"digest"`
		Nodes  []string `json:"nodes"`
		Paused bool     `json:"paused"`
	}
	if e := decode(w, r, &in); e != nil {
		return e
	}
	e := s.DB.Update(func(t *store.Tx) error {
		old, err := store.Get[AgentRollout](t, "settings", "agent_rollout")
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
		old.normalize()
		if in.Digest == "" {
			if err != nil {
				return fail(404, "没有更新批次")
			}
			if old.Error != "" && !in.Paused {
				return fail(409, "失败批次不能直接恢复，请检查后创建新批次")
			}
			old.Paused = in.Paused
		} else {
			if err == nil && old.Index < len(old.Nodes) && !old.Paused {
				return fail(409, "请先暂停当前批次")
			}
			m, e := s.release(in.Digest)
			if e != nil {
				return fail(400, "发布不存在或签名无效")
			}
			if len(in.Nodes) == 0 || len(in.Nodes) > 100 {
				return fail(400, "每批选择 1 至 100 台主机")
			}
			seen := map[string]bool{}
			for _, id := range in.Nodes {
				if seen[id] {
					return fail(400, "主机重复")
				}
				seen[id] = true
				n, e := store.Get[model.Node](t, "nodes", id)
				if e != nil || n.Arch != m.Arch || n.OS != m.OS || !n.AgentUpdate.Capable {
					return fail(409, "主机平台或更新能力不匹配: "+id)
				}
			}
			old = AgentRollout{Digest: in.Digest, Nodes: in.Nodes, Paused: in.Paused, Active: map[string]bool{}, Completed: map[string]bool{}}
		}
		if e := store.Put(t, "settings", "agent_rollout", old); e != nil {
			return e
		}
		return audit(t, "admin", "agent_rollout", old.Digest)
	})
	if e != nil {
		return e
	}
	return s.agentRollout(w, r)
}
func (s *Server) agentRollout(w http.ResponseWriter, r *http.Request) error {
	var result AgentRollout
	e := s.DB.View(func(t *store.Tx) error {
		var e error
		result, e = store.Get[AgentRollout](t, "settings", "agent_rollout")
		if errors.Is(e, store.ErrNotFound) {
			return nil
		}
		return e
	})
	if e != nil {
		return e
	}
	result.normalize()
	jsonOut(w, 200, result)
	return nil
}
func (s *Server) planAgentUpdate(t *store.Tx, n model.Node, p model.Poll, out *model.PollResponse) error {
	roll, e := store.Get[AgentRollout](t, "settings", "agent_rollout")
	if errors.Is(e, store.ErrNotFound) {
		return nil
	}
	if e != nil {
		return e
	}
	if !roll.includes(n.ID) {
		return nil
	}
	roll.normalize()
	if roll.Completed[n.ID] {
		return nil
	}
	if p.AgentUpdate.Target == roll.Digest && p.AgentUpdate.Phase == "failed" {
		roll.Paused = true
		roll.Error = n.ID + ": " + p.AgentUpdate.Error
		return store.Put(t, "settings", "agent_rollout", roll)
	}
	if p.AgentUpdate.Digest == roll.Digest && p.AgentUpdate.Phase == "committed" && !p.Trial {
		roll.Completed[n.ID] = true
		delete(roll.Active, n.ID)
		for roll.Index < len(roll.Nodes) && roll.Completed[roll.Nodes[roll.Index]] {
			roll.Index++
		}
		return store.Put(t, "settings", "agent_rollout", roll)
	}
	if roll.Paused || !p.AgentUpdate.Capable || p.Trial || p.Draining || (!roll.Active[n.ID] && len(roll.Active) >= agentRolloutConcurrency) {
		return nil
	}
	m, e := s.release(roll.Digest)
	if e != nil {
		roll.Paused = true
		roll.Error = "发布不可用，已暂停更新: " + e.Error()
		return store.Put(t, "settings", "agent_rollout", roll)
	}
	roll.Active[n.ID] = true
	if e = store.Put(t, "settings", "agent_rollout", roll); e != nil {
		return e
	}
	out.Update = &m
	return nil
}
func (s *Server) downloadAgentRelease(w http.ResponseWriter, r *http.Request) error {
	d := r.PathValue("digest")
	if !selfupdate.ValidDigest(d) {
		return fail(400, "无效摘要")
	}
	allowed := false
	e := s.DB.View(func(t *store.Tx) error {
		roll, e := store.Get[AgentRollout](t, "settings", "agent_rollout")
		if errors.Is(e, store.ErrNotFound) {
			return nil
		}
		if e != nil {
			return e
		}
		roll.normalize()
		allowed = !roll.Paused && roll.Active[agentNode(r).ID] && roll.includes(agentNode(r).ID) && roll.Digest == d
		return nil
	})
	if e != nil {
		return e
	}
	if !allowed {
		return fail(403, "没有此版本的更新授权")
	}
	path := filepath.Join(s.releasePath(d), "binary")
	w.Header().Add("Vary", "Accept-Encoding")
	// Default Go transports transparently decompress gzip before the existing
	// signed size/digest verification. Ranges keep the original representation.
	if r.Header.Get("Range") == "" && acceptsAgentGzip(r.Header.Get("Accept-Encoding")) {
		path, e = s.compressedAgentRelease(path)
		if e != nil {
			return e
		}
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Content-Type", "application/octet-stream")
	}
	http.ServeFile(w, r, path)
	return nil
}

func acceptsAgentGzip(header string) bool {
	for _, item := range strings.Split(header, ",") {
		parts := strings.Split(item, ";")
		if !strings.EqualFold(strings.TrimSpace(parts[0]), "gzip") {
			continue
		}
		for _, parameter := range parts[1:] {
			key, value, _ := strings.Cut(strings.TrimSpace(parameter), "=")
			if strings.EqualFold(key, "q") {
				q, err := strconv.ParseFloat(value, 64)
				if err != nil || !(q > 0 && q <= 1) {
					return false
				}
			}
		}
		return true
	}
	return false
}

// Releases are immutable. Build one atomic on-disk representation for all
// clients, rather than recompressing a binary on every download/heartbeat.
func (s *Server) compressedAgentRelease(path string) (string, error) {
	s.packagesMu.Lock()
	defer s.packagesMu.Unlock()
	target := path + ".gz"
	if _, err := os.Stat(target); err == nil {
		return target, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	source, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer source.Close()
	f, err := os.CreateTemp(filepath.Dir(path), ".gzip-")
	if err != nil {
		return "", err
	}
	defer os.Remove(f.Name())
	z := gzip.NewWriter(f)
	_, err = io.Copy(z, source)
	if ce := z.Close(); err == nil {
		err = ce
	}
	if err == nil {
		err = f.Sync()
	}
	if ce := f.Close(); err == nil {
		err = ce
	}
	if err == nil {
		err = os.Rename(f.Name(), target)
	}
	if err == nil {
		err = selfupdate.SyncDir(filepath.Dir(path))
	}
	return target, err
}

// Only indexed, signature-verified releases are offered for rollout.
func (s *Server) agentReleases(w http.ResponseWriter, r *http.Request) error {
	result := []selfupdate.Manifest{}
	err := s.DB.View(func(t *store.Tx) error {
		digests, e := store.List[string](t, "agent_release_versions")
		if e != nil {
			return e
		}
		seen := map[string]bool{}
		for _, d := range digests {
			if seen[d] {
				continue
			}
			seen[d] = true
			m, e := s.release(d)
			if e != nil {
				continue
			}
			result = append(result, m)
		}
		return nil
	})
	if err != nil {
		return err
	}
	jsonOut(w, 200, result)
	return nil
}
