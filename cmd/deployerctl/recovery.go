package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"deployer/internal/controller"
	"deployer/internal/model"
	"deployer/internal/packages"
	"deployer/internal/store"
	"golang.org/x/crypto/bcrypt"
)

func resetAdmin(args []string) error {
	f := flag.NewFlagSet("reset-admin", flag.ContinueOnError)
	data := f.String("data", "", "已停止的控制端数据目录")
	confirm := f.Bool("confirm", false, "确认重置管理员密码和验证码绑定")
	if err := f.Parse(args); err != nil {
		return err
	}
	if *data == "" || !*confirm {
		return fmt.Errorf("需要 --data 和 --confirm；先停止控制端服务")
	}
	path := filepath.Join(*data, "state", "controller.db")
	if _, err := os.Stat(path); err != nil {
		return err
	}
	db, err := store.Open(path)
	if err != nil {
		return err
	}
	defer db.Close()
	password, err := bcrypt.GenerateFromPassword([]byte(controller.DefaultAdminPassword), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	return db.Update(func(t *store.Tx) error {
		u, err := store.Get[model.User](t, "users", "admin")
		if err != nil {
			return err
		}
		u.PasswordHash = string(password)
		u.Role = "admin"
		u.Disabled = false
		u.TOTPEnabled = false
		u.TOTPSecret = ""
		u.TOTPLastCounter = 0
		u.RecoveryHashes = nil
		u.AuthVersion++
		if err = store.Put(t, "users", u.ID, u); err != nil {
			return err
		}
		a := model.Audit{ID: model.ID(), Time: model.Now(), Actor: "local-recovery", Action: "reset_admin", Target: "admin"}
		return store.Put(t, "audit", a.ID, a)
	})
}
func copyRegular(source, target string) error {
	info, err := os.Lstat(source)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("备份包含非普通文件")
	}
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, in)
	if err == nil {
		err = out.Sync()
	}
	closeErr := out.Close()
	if err != nil {
		return err
	}
	return closeErr
}
func restoreBackup(args []string) error {
	f := flag.NewFlagSet("restore-backup", flag.ContinueOnError)
	data := f.String("data", "", "新的数据目录，必须不存在")
	backup := f.String("backup", "", "当前结构的自动备份目录")
	if err := f.Parse(args); err != nil {
		return err
	}
	if *data == "" || *backup == "" {
		return fmt.Errorf("需要 --data 和 --backup")
	}
	root, err := filepath.Abs(*data)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(root), 0700); err != nil {
		return err
	}
	if err = os.Mkdir(root, 0700); err != nil {
		return fmt.Errorf("目标目录必须不存在: %w", err)
	}
	ok := false
	defer func() {
		if !ok {
			os.RemoveAll(root)
		}
	}()
	for _, d := range []string{"state", "archives", "packages"} {
		if err = os.Mkdir(filepath.Join(root, d), 0700); err != nil {
			return err
		}
	}
	for _, name := range []string{"controller.db", "master.key"} {
		if err = copyRegular(filepath.Join(*backup, name), filepath.Join(root, "state", name)); err != nil {
			return err
		}
	}
	issuerBackup := filepath.Join(*backup, "agent-issuer")
	if _, e := os.Stat(issuerBackup); e == nil {
		destination := filepath.Join(root, "state", "agent-issuer")
		if err = os.Mkdir(destination, 0700); err != nil {
			return err
		}
		for _, name := range []string{"key.pem", "ca.pem"} {
			if err = copyRegular(filepath.Join(issuerBackup, name), filepath.Join(destination, name)); err != nil {
				return err
			}
		}
	} else if !os.IsNotExist(e) {
		return e
	}
	db, err := store.Open(filepath.Join(root, "state", "controller.db"))
	if err != nil {
		return err
	}
	defer db.Close()
	var versions []model.Version
	if err = db.View(func(t *store.Tx) error { var e error; versions, e = store.List[model.Version](t, "versions"); return e }); err != nil {
		return err
	}
	for _, v := range versions {
		if v.Status != "available" {
			continue
		}
		if !model.ValidName(v.AppID) || !model.ValidName(v.Version) || (len(v.Digest) != 64 || strings.Trim(v.Digest, "0123456789abcdef") != "") {
			return fmt.Errorf("无效版本元数据")
		}
		archive := filepath.Join(root, "archives", v.Digest+".tar.gz")
		if _, e := os.Stat(archive); os.IsNotExist(e) {
			if err = copyRegular(filepath.Join(*backup, "archives", v.Digest+".tar.gz"), archive); err != nil {
				return err
			}
		}
		digest, e := packages.Digest(archive)
		if e != nil || digest != v.Digest {
			return fmt.Errorf("备份包校验失败: %s", v.Version)
		}
		if err = os.MkdirAll(filepath.Join(root, "packages", v.AppID), 0700); err != nil {
			return err
		}
		if _, err = packages.Extract(archive, filepath.Join(root, "packages", v.AppID, v.Version), v); err != nil {
			return err
		}
	}
	// Quarantine snapshot-era tasks on each existing node's first reconnect.
	if err = db.Update(func(t *store.Tx) error {
		if err := store.Put(t, "settings", "backup_restored", true); err != nil {
			return err
		}
		nodes, err := store.List[model.Node](t, "nodes")
		if err != nil {
			return err
		}
		for _, n := range nodes {
			if err = store.Put(t, "restored_nodes", n.ID, true); err != nil {
				return err
			}
		}
		batches, err := store.Query[controller.Batch](t, "batches", store.Filter{States: []string{"running"}})
		if err != nil {
			return err
		}
		for _, b := range batches {
			b.State = "failed"
			b.Error = "控制端已从备份恢复，批次需要重新创建"
			if err = store.Put(t, "batches", b.ID, b); err != nil {
				return err
			}
		}

		return nil
	}); err != nil {
		return err
	}
	ok = true
	return store.SyncDir(root)
}
