package image

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestExtractTar(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	write := func(h *tar.Header, body string) {
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if body != "" {
			tw.Write([]byte(body))
		}
	}
	write(&tar.Header{Name: "etc/", Typeflag: tar.TypeDir, Mode: 0o755}, "")
	write(&tar.Header{Name: "etc/hello", Typeflag: tar.TypeReg, Mode: 0o644, Size: 5}, "world")
	write(&tar.Header{Name: "etc/link", Typeflag: tar.TypeSymlink, Linkname: "hello"}, "")
	// A device node must be skipped, not fail extraction.
	write(&tar.Header{Name: "dev/null", Typeflag: tar.TypeChar, Devmajor: 1, Devminor: 3, Mode: 0o666}, "")
	tw.Close()

	dst := t.TempDir()
	if err := extractTar(&buf, dst); err != nil {
		t.Fatalf("extractTar: %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(dst, "etc/hello")); err != nil || string(b) != "world" {
		t.Errorf("etc/hello = %q, %v", b, err)
	}
	if target, err := os.Readlink(filepath.Join(dst, "etc/link")); err != nil || target != "hello" {
		t.Errorf("symlink = %q, %v", target, err)
	}
	if _, err := os.Lstat(filepath.Join(dst, "dev/null")); !os.IsNotExist(err) {
		t.Errorf("device node should have been skipped")
	}
}

func TestSeedDirCopiesAndExcludes(t *testing.T) {
	src := t.TempDir()
	mustWrite(t, filepath.Join(src, "root/a.txt"), "A")
	mustWrite(t, filepath.Join(src, "usr/bin/tool"), "bin")
	mustWrite(t, filepath.Join(src, "proc/fake"), "should-be-excluded")
	if err := os.Symlink("a.txt", filepath.Join(src, "root/link")); err != nil {
		t.Fatal(err)
	}

	dst := t.TempDir()
	exclude := map[string]bool{filepath.Join(src, "proc"): true}
	var lastFiles int
	var lastBytes int64
	prog := func(files int, bytes int64) { lastFiles, lastBytes = files, bytes }
	if err := SeedDir(src, dst, exclude, prog); err != nil {
		t.Fatalf("SeedDir: %v", err)
	}
	// Two regular files copied (root/a.txt + usr/bin/tool; proc/fake excluded,
	// the symlink doesn't count). Bytes = len("A") + len("bin") = 4.
	if lastFiles != 2 {
		t.Errorf("progress files = %d, want 2", lastFiles)
	}
	if lastBytes != 4 {
		t.Errorf("progress bytes = %d, want 4", lastBytes)
	}

	if b, err := os.ReadFile(filepath.Join(dst, "root/a.txt")); err != nil || string(b) != "A" {
		t.Errorf("root/a.txt = %q, %v", b, err)
	}
	if b, err := os.ReadFile(filepath.Join(dst, "usr/bin/tool")); err != nil || string(b) != "bin" {
		t.Errorf("usr/bin/tool = %q, %v", b, err)
	}
	if target, err := os.Readlink(filepath.Join(dst, "root/link")); err != nil || target != "a.txt" {
		t.Errorf("symlink = %q, %v", target, err)
	}
	if _, err := os.Stat(filepath.Join(dst, "proc")); !os.IsNotExist(err) {
		t.Errorf("excluded /proc should not be copied")
	}
}

func mustWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
