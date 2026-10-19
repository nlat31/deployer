package packages

import (
	"archive/tar"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"

	"deployer/internal/model"
)

func archive(t *testing.T, extra []*tar.Header, missing bool) string {
	t.Helper()
	f, e := os.CreateTemp(t.TempDir(), "*.tar.gz")
	if e != nil {
		t.Fatal(e)
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	scripts := []string{"setup.sh", "start.sh", "stop.sh", "clean.sh"}
	if missing {
		scripts = scripts[:3]
	}
	for _, s := range scripts {
		b := []byte("#!/bin/bash\nexit 0\n")
		if e = tw.WriteHeader(&tar.Header{Name: s, Mode: 0755, Size: int64(len(b))}); e != nil {
			t.Fatal(e)
		}
		tw.Write(b)
	}
	for _, h := range extra {
		if e = tw.WriteHeader(h); e != nil {
			t.Fatal(e)
		}
	}
	tw.Close()
	gz.Close()
	f.Close()
	return f.Name()
}
func TestExtractRejectsUnsafeAndIncompletePackages(t *testing.T) {
	for name, headers := range map[string][]*tar.Header{"traversal": {{Name: "../outside", Mode: 0644}}, "absolute": {{Name: "/outside", Mode: 0644}}, "symlink": {{Name: "escape", Typeflag: tar.TypeSymlink, Linkname: "/tmp"}}, "hardlink": {{Name: "escape", Typeflag: tar.TypeLink, Linkname: "setup.sh"}}, "duplicate": {{Name: "setup.sh", Mode: 0644}}, "device": {{Name: "dev", Typeflag: tar.TypeChar}}} {
		t.Run(name, func(t *testing.T) {
			dest := filepath.Join(t.TempDir(), "out")
			_, e := Extract(archive(t, headers, false), dest, model.Version{AppID: "demo", Version: "1", OS: "linux", Arch: "amd64"})
			if e == nil {
				t.Fatal("unsafe package accepted")
			}
			if _, e = os.Stat(dest); !os.IsNotExist(e) {
				t.Fatal("partial extraction retained")
			}
		})
	}
	if _, e := Extract(archive(t, nil, true), filepath.Join(t.TempDir(), "out"), model.Version{AppID: "demo", Version: "1"}); e == nil {
		t.Fatal("missing script accepted")
	}
}
func TestExtractComplete(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "out")
	if _, e := Extract(archive(t, nil, false), dest, model.Version{AppID: "demo", Version: "1"}); e != nil {
		t.Fatal(e)
	}
	for _, s := range []string{"setup.sh", "start.sh", "stop.sh", "clean.sh"} {
		st, e := os.Stat(filepath.Join(dest, s))
		if e != nil || st.Mode()&0111 == 0 {
			t.Fatalf("invalid script %s", s)
		}
	}
}
