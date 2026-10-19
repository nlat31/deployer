package packages

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"

	"deployer/internal/model"
	"gopkg.in/yaml.v3"
)

const MaxArchive = 512 << 20
const MaxExpanded = 2 << 30
const MaxFiles = 20000

func Digest(file string) (string, error) {
	f, e := os.Open(file)
	if e != nil {
		return "", e
	}
	defer f.Close()
	h := sha256.New()
	if _, e = io.Copy(h, f); e != nil {
		return "", e
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Extract only publishes regular files and directories into a freshly created directory.
// Rejecting links also makes the same extractor safe on the controller and the Agent.
func Extract(archive, dest string, expected model.Version) (*model.Manifest, error) {
	if !model.ValidName(expected.AppID) || !model.ValidName(expected.Version) {
		return nil, fmt.Errorf("非法 App 或版本标识")
	}
	if e := os.Mkdir(dest, 0755); e != nil {
		return nil, e
	}
	ok := false
	defer func() {
		if !ok {
			os.RemoveAll(dest)
		}
	}()
	f, e := os.Open(archive)
	if e != nil {
		return nil, e
	}
	defer f.Close()
	gz, e := gzip.NewReader(f)
	if e != nil {
		return nil, e
	}
	defer gz.Close()
	gz.Multistream(false)
	tr := tar.NewReader(gz)
	seen := map[string]bool{}
	scripts := map[string]bool{}
	var total int64
	count := 0
	for {
		h, e := tr.Next()
		if e == io.EOF {
			break
		}
		if e != nil {
			return nil, e
		}
		count++
		if count > MaxFiles {
			return nil, fmt.Errorf("包内文件数量超限")
		}
		name := strings.TrimPrefix(h.Name, "./")
		name = strings.TrimSuffix(name, "/")
		if name == "" || name == "." {
			if h.Typeflag == tar.TypeDir {
				continue
			}
			return nil, fmt.Errorf("非法根目录文件")
		}
		if strings.ContainsAny(name, "\\\x00") || strings.HasPrefix(name, "/") || path.Clean(name) != name || name == ".." || strings.HasPrefix(name, "../") || seen[name] {
			return nil, fmt.Errorf("非法或重复路径 %q", h.Name)
		}
		seen[name] = true
		target := filepath.Join(dest, filepath.FromSlash(name))
		switch h.Typeflag {
		case tar.TypeDir:
			if e = os.MkdirAll(target, 0755); e != nil {
				return nil, e
			}
		case tar.TypeReg, tar.TypeRegA:
			if h.Size < 0 || h.Size > MaxExpanded-total {
				return nil, fmt.Errorf("包展开大小超限")
			}
			total += h.Size
			var disk syscall.Statfs_t
			if e = syscall.Statfs(dest, &disk); e != nil {
				return nil, e
			}
			if disk.Bavail*uint64(disk.Bsize) < uint64(h.Size)+(64<<20) {
				return nil, fmt.Errorf("磁盘空间不足，保留至少 64 MiB 空闲空间")
			}
			if e = os.MkdirAll(filepath.Dir(target), 0755); e != nil {
				return nil, e
			}
			mode := os.FileMode(0644)
			if h.Mode&0111 != 0 {
				mode = 0755
			}
			out, e := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
			if e != nil {
				return nil, e
			}
			_, e = io.CopyN(out, tr, h.Size)
			if e == nil {
				e = out.Sync()
			}
			ce := out.Close()
			if e == nil {
				e = ce
			}
			if e != nil {
				return nil, e
			}
			for _, s := range []string{"setup.sh", "start.sh", "stop.sh", "clean.sh"} {
				if name == s {
					scripts[s] = true
					if e = os.Chmod(target, 0755); e != nil {
						return nil, e
					}
				}
			}
		default:
			return nil, fmt.Errorf("包中只允许普通文件和目录: %q", h.Name)
		}
	}
	// Read through gzip EOF to validate its checksum even if tar ended earlier.
	trailing, e := io.Copy(io.Discard, io.LimitReader(gz, (1<<20)+1))
	if e != nil {
		return nil, e
	}
	if trailing > 1<<20 {
		return nil, fmt.Errorf("压缩包包含过量尾随数据")
	}
	for _, s := range []string{"setup.sh", "start.sh", "stop.sh", "clean.sh"} {
		if !scripts[s] {
			return nil, fmt.Errorf("缺少根目录脚本 %s", s)
		}
	}
	var manifest *model.Manifest
	if b, e := os.ReadFile(filepath.Join(dest, "app.yaml")); e == nil {
		if len(b) > 65536 {
			return nil, fmt.Errorf("app.yaml 过大")
		}
		var m model.Manifest
		dec := yaml.NewDecoder(strings.NewReader(string(b)))
		dec.KnownFields(true)
		if e = dec.Decode(&m); e != nil {
			return nil, e
		}
		if m.AppID != expected.AppID || m.Version != expected.Version || m.OS != expected.OS || m.Arch != expected.Arch {
			return nil, fmt.Errorf("app.yaml 与上传元数据不一致")
		}
		if m.Health != nil {
			h, e := m.Health.Normalize()
			if e != nil {
				return nil, e
			}
			m.Health = &h
		}
		manifest = &m
	} else if !os.IsNotExist(e) {
		return nil, e
	}
	for _, reserved := range []string{"data", "logs", "run", "config"} {
		if st, e := os.Stat(filepath.Join(dest, reserved)); e == nil && !st.IsDir() {
			return nil, fmt.Errorf("%s 必须是目录", reserved)
		}
	}
	ok = true
	return manifest, nil
}

func DirBytes(root string) (int64, error) {
	var size int64
	e := filepath.WalkDir(root, func(p string, d os.DirEntry, e error) error {
		if os.IsNotExist(e) {
			return nil
		}
		if e != nil {
			return e
		}
		if d.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("目录中存在符号链接: %s", p)
		}
		if !d.IsDir() {
			s, e := d.Info()
			if e != nil {
				return e
			}
			if !s.Mode().IsRegular() {
				return fmt.Errorf("存在非普通文件")
			}
			size += s.Size()
		}
		return nil
	})
	return size, e
}
func PlainDir(root string) error {
	st, e := os.Lstat(root)
	if e != nil {
		return e
	}
	if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("不是普通目录: %s", root)
	}
	return nil
}
