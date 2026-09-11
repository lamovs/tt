package main

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

func makeArchive(t *testing.T, platform string, change func(map[string]int64)) (string, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "archive.tar.gz")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(f)
	w := tar.NewWriter(gz)
	files := archiveFiles(platform)
	change(files)
	for name, mode := range files {
		if err := w.WriteHeader(&tar.Header{Name: name, Mode: mode, Size: 4, Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte("data")); err != nil {
			t.Fatal(err)
		}
	}
	for _, closer := range []interface{ Close() error }{w, gz, f} {
		if err := closer.Close(); err != nil {
			t.Fatal(err)
		}
	}
	source, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(source)
	return path, hex.EncodeToString(hash[:])
}

func TestVerifyArchive(t *testing.T) {
	for _, platform := range []string{"darwin", "linux"} {
		path, digest := makeArchive(t, platform, func(map[string]int64) {})
		if err := verifyArchive(path, digest, platform); err != nil {
			t.Fatal(err)
		}
		if err := verifyArchive(path, "invalid", platform); err == nil {
			t.Fatal("accepted a mismatched checksum")
		}
	}
}

func TestVerifyArchiveRejectsMissingOrExtraFiles(t *testing.T) {
	for _, change := range []func(map[string]int64){
		func(files map[string]int64) { delete(files, "tt") },
		func(files map[string]int64) { files["../outside"] = 0o644 },
		func(files map[string]int64) { files["docs/private.md"] = 0o644 },
		func(files map[string]int64) { files["tt"] = 0o644 },
	} {
		path, digest := makeArchive(t, "linux", change)
		if err := verifyArchive(path, digest, "linux"); err == nil {
			t.Fatal("accepted an invalid archive")
		}
	}
}
