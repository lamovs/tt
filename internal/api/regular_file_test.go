package api

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestReadRegularFileModesAndSymlinks(t *testing.T) {
	for _, mode := range []os.FileMode{0o500, 0o600, 0o700, 0o640} {
		t.Run(mode.String(), func(t *testing.T) {
			dir := t.TempDir()
			target := filepath.Join(dir, "target")
			if err := os.WriteFile(target, []byte("value"), mode); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(target, mode); err != nil {
				t.Fatal(err)
			}
			link := filepath.Join(dir, "link")
			if err := os.Symlink(target, link); err != nil {
				t.Fatal(err)
			}
			got, err := ReadRegularFile(link)
			if err != nil {
				t.Fatalf("ReadRegularFile: %v", err)
			}
			resolvedTarget, err := filepath.EvalSymlinks(target)
			if err != nil {
				t.Fatal(err)
			}
			if string(got.Data) != "value" || got.Mode != mode || got.ResolvedPath != resolvedTarget || !got.PathVerified {
				t.Errorf("read = {%q %04o %q}, want value, %04o, %q", got.Data, got.Mode, got.ResolvedPath, mode, resolvedTarget)
			}
		})
	}
}

func TestReadRegularFileDistinguishesUnusableLinks(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing-link")
	if err := os.Symlink(filepath.Join(dir, "absent"), missing); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadRegularFile(missing); !errors.Is(err, ErrUnusableLink) || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("dangling link error = %v, want unusable-link and not-exist identities", err)
	}

	loop := filepath.Join(dir, "loop")
	if err := os.Symlink(loop, loop); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadRegularFile(loop); !errors.Is(err, ErrUnusableLink) {
		t.Fatalf("loop error = %v, want unusable-link identity", err)
	}
}

func TestResolvedOpenedPathRefusesANameThatWasReplacedAfterOpen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "credential")
	if err := os.WriteFile(path, []byte("opened"), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path, path+".opened"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("replacement"), 0o777); err != nil {
		t.Fatal(err)
	}
	if resolved, verified := resolvedOpenedPath(path, opened); verified || resolved != "" {
		t.Fatalf("resolved path = %q, verified = %v; want unknown attribution", resolved, verified)
	}
}

func TestLoadTokenPermissionBoundaryIsGroupAndOtherBits(t *testing.T) {
	for _, tc := range []struct {
		mode     os.FileMode
		insecure bool
	}{
		{0o500, false},
		{0o600, false},
		{0o700, false},
		{0o610, true},
		{0o604, true},
	} {
		dir := t.TempDir()
		path := filepath.Join(dir, "token")
		if err := os.WriteFile(path, []byte("token"), tc.mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, tc.mode); err != nil {
			t.Fatal(err)
		}
		info, err := LoadTokenFrom(path)
		if err != nil {
			t.Fatalf("mode %04o: %v", tc.mode, err)
		}
		if info.Insecure != tc.insecure {
			t.Errorf("mode %04o: Insecure = %v, want %v", tc.mode, info.Insecure, tc.insecure)
		}
	}
}
