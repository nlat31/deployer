package model

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strings"
	"time"
)

const Protocol = 3

var namePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,95}$`)
var envPattern = regexp.MustCompile(`^[A-Z_][A-Z0-9_]{0,95}$`)

func ValidName(s string) bool { return namePattern.MatchString(s) && s != "." && s != ".." }
func ID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}
func Now() int64                      { return time.Now().UnixMilli() }
func Key(app, version string) string  { return app + "/" + version }
func NodeKey(node, app string) string { return node + "/" + app }

type Health struct {
	Type           string `json:"type" yaml:"type"`
	Target         string `json:"target" yaml:"target"`
	TimeoutSeconds int    `json:"timeout_seconds" yaml:"timeout_seconds"`
	StableSeconds  int    `json:"stable_seconds" yaml:"stable_seconds"`
}

func (h Health) Normalize() (Health, error) {
	if h.Type == "" {
		h.Type = "process"
	}
	if h.TimeoutSeconds == 0 {
		h.TimeoutSeconds = 60
	}
	if h.StableSeconds == 0 {
		h.StableSeconds = 10
	}
	if h.TimeoutSeconds < 1 || h.TimeoutSeconds > 1800 || h.StableSeconds < 1 || h.StableSeconds > h.TimeoutSeconds {
		return h, fmt.Errorf("健康检查时间必须在 1 至 1800 秒内，稳定期不能超过超时")
	}
	switch h.Type {
	case "process":
	case "http":
		u, e := url.Parse(h.Target)
		if e != nil || u.Scheme != "http" || u.Hostname() == "" || u.User != nil {
			return h, fmt.Errorf("HTTP 探测必须使用本机 http URL")
		}
		if !loopback(u.Hostname()) {
			return h, fmt.Errorf("探测地址仅允许回环地址")
		}
	case "tcp":
		host, _, e := net.SplitHostPort(h.Target)
		if e != nil || !loopback(host) {
			return h, fmt.Errorf("TCP 探测必须使用本机 host:port")
		}
	default:
		return h, fmt.Errorf("不支持的健康检查类型")
	}
	return h, nil
}
func loopback(s string) bool { ip := net.ParseIP(s); return ip != nil && ip.IsLoopback() }
func ValidEnv(env map[string]string) error {
	total := 0
	for k, v := range env {
		total += len(k) + len(v)
	}
	if total > 32<<10 {
		return fmt.Errorf("环境配置总大小不能超过 32 KiB")
	}
	if len(env) > 100 {
		return fmt.Errorf("环境变量过多")
	}
	for k, v := range env {
		if !envPattern.MatchString(k) || strings.HasPrefix(k, "APP_") || strings.HasPrefix(k, "DEPLOY_") || strings.HasPrefix(k, "DEPLOYER_") || k == "PATH" || k == "BASH_ENV" || k == "ENV" || strings.HasPrefix(k, "LD_") || strings.ContainsAny(v, "\x00\r\n") || len(v) > 16384 {
			return fmt.Errorf("不允许的环境变量 %q", k)
		}
	}
	return nil
}

type App struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Health      Health `json:"health"`
	CreatedAt   int64  `json:"created_at"`
}
type Version struct {
	AppID     string  `json:"app_id"`
	Version   string  `json:"version"`
	OS        string  `json:"os"`
	Arch      string  `json:"arch"`
	Digest    string  `json:"digest"`
	Size      int64   `json:"size"`
	Status    string  `json:"status"`
	CreatedAt int64   `json:"created_at"`
	Health    *Health `json:"health,omitempty"`
}
type Manifest struct {
	AppID   string  `yaml:"app_id"`
	Version string  `yaml:"version"`
	OS      string  `yaml:"os"`
	Arch    string  `yaml:"arch"`
	Health  *Health `yaml:"health,omitempty"`
}
type Node struct {
	IP                 string `json:"ip"`
	ID                 string `json:"id"`
	Name               string `json:"name"`
	OS                 string `json:"os"`
	Arch               string `json:"arch"`
	LastSeen           int64  `json:"last_seen"`
	FreeBytes          uint64 `json:"free_bytes"`
	CertificateExpires int64  `json:"certificate_expires"`
}
type Config struct {
	ID           string            `json:"id"`
	Env          map[string]string `json:"env,omitempty"`
	Health       Health            `json:"health"`
	AutoRollback bool              `json:"auto_rollback"`
}
type Deployment struct {
	NodeID          string `json:"node_id"`
	AppID           string `json:"app_id"`
	Current         string `json:"current"`
	Previous        string `json:"previous"`
	DesiredVersion  string `json:"desired_version"`
	DesiredState    string `json:"desired_state"`
	RunningVersion  string `json:"running_version"`
	RunState        string `json:"run_state"`
	Health          string `json:"health"`
	Revision        int64  `json:"revision"`
	ObservedAt      int64  `json:"observed_at"`
	Error           string `json:"error"`
	Config          Config `json:"config"`
	PreviousConfig  Config `json:"previous_config"`
	RestartFailures int    `json:"restart_failures"`
	NextRestartAt   int64  `json:"next_restart_at"`
	UnhealthySince  int64  `json:"unhealthy_since"`
	HealthySince    int64  `json:"healthy_since"`
}
type Installation struct {
	AppID       string `json:"app_id"`
	Version     string `json:"version"`
	Digest      string `json:"digest"`
	Initialized bool   `json:"initialized"`
	DataEpoch   int64  `json:"data_epoch"`
	DataBytes   int64  `json:"data_bytes"`
	InstalledAt int64  `json:"installed_at"`
}
type Operation struct {
	Skipped          bool       `json:"skipped,omitempty"`
	ID               string     `json:"id"`
	NodeID           string     `json:"node_id"`
	AppID            string     `json:"app_id"`
	Action           string     `json:"action"`
	Version          string     `json:"version"`
	Artifact         *Version   `json:"artifact,omitempty"`
	IdempotencyKey   string     `json:"idempotency_key"`
	RequestHash      string     `json:"request_hash"`
	ExpectedRevision int64      `json:"expected_revision"`
	Revision         int64      `json:"revision"`
	Stage            string     `json:"stage"`
	Result           string     `json:"result"`
	Error            string     `json:"error"`
	CreatedAt        int64      `json:"created_at"`
	UpdatedAt        int64      `json:"updated_at"`
	FinishedAt       int64      `json:"finished_at"`
	Before           Deployment `json:"before"`
	Config           Config     `json:"config"`
	Start            bool       `json:"start"`
	DataEpoch        int64      `json:"data_epoch"`
	Acked            bool       `json:"acked"`
	Actor            string     `json:"actor"`
	Archived         bool       `json:"archived,omitempty"`
}

func (o Operation) StorageIndex() (string, string, string) {
	state := o.Result
	if state == "" {
		state = "active"
	}
	if o.Archived {
		state = "archived"
	}
	return o.NodeID, o.AppID, state
}

func (o Operation) Terminal() bool { return o.Result != "" }

type Event struct {
	OperationID string `json:"operation_id"`
	Seq         int64  `json:"seq"`
	Time        int64  `json:"time"`
	Stage       string `json:"stage"`
	Message     string `json:"message"`
}
type OperationRequest struct {
	SkipIfDeployed   bool              `json:"skip_if_deployed,omitempty"`
	Action           string            `json:"action"`
	Version          string            `json:"version"`
	ExpectedRevision int64             `json:"expected_revision"`
	IdempotencyKey   string            `json:"idempotency_key"`
	Start            *bool             `json:"start,omitempty"`
	Env              map[string]string `json:"env,omitempty"`
	Health           *Health           `json:"health,omitempty"`
	AutoRollback     *bool             `json:"auto_rollback,omitempty"`
	Confirm          bool              `json:"confirm"`
}
type Poll struct {
	Name          string          `json:"name"`
	OS            string          `json:"os"`
	Arch          string          `json:"arch"`
	Protocol      int             `json:"protocol"`
	Deployments   []Deployment    `json:"deployments"`
	Installations []Installation  `json:"installations"`
	Operations    []Operation     `json:"operations"`
	Events        []Event         `json:"events"`
	FreeBytes     uint64          `json:"free_bytes"`
	Rejected      []TaskRejection `json:"rejected"`
}
type TaskRejection struct {
	ID            string `json:"id"`
	Error         string `json:"error"`
	LocalRevision int64  `json:"local_revision"`
}
type PollResponse struct {
	Tasks        []Operation      `json:"tasks"`
	Acknowledged []string         `json:"acknowledged"`
	EventAcks    map[string]int64 `json:"event_acks"`
}
type User struct {
	ID              string   `json:"id"`
	Name            string   `json:"name"`
	Role            string   `json:"role"`
	PasswordHash    string   `json:"password_hash,omitempty"`
	Disabled        bool     `json:"disabled"`
	AuthVersion     int64    `json:"auth_version"`
	TOTPEnabled     bool     `json:"totp_enabled"`
	TOTPSecret      string   `json:"totp_secret,omitempty"`
	TOTPLastCounter int64    `json:"totp_last_counter,omitempty"`
	RecoveryHashes  []string `json:"recovery_hashes,omitempty"`
}
type Session struct {
	UserID      string `json:"user_id"`
	CSRF        string `json:"csrf"`
	ExpiresAt   int64  `json:"expires_at"`
	AuthVersion int64  `json:"auth_version"`
}

func PublicUser(u User) User {
	u.PasswordHash = ""
	u.TOTPSecret = ""
	u.TOTPLastCounter = 0
	u.RecoveryHashes = nil
	return u
}

type Audit struct {
	ID     string `json:"id"`
	Time   int64  `json:"time"`
	Actor  string `json:"actor"`
	Action string `json:"action"`
	Target string `json:"target"`
}

func PublicConfig(c Config) Config { c.Env = nil; return c }
func PublicDeployment(d Deployment) Deployment {
	d.Config = PublicConfig(d.Config)
	d.PreviousConfig = PublicConfig(d.PreviousConfig)
	return d
}
func PublicOperation(o Operation) Operation {
	o.Config = PublicConfig(o.Config)
	o.Before = PublicDeployment(o.Before)
	o.RequestHash = ""
	return o
}
