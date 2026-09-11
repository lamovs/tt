//go:build unix

package main

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/movsar/tt/internal/api"
)

func TestCheckTokenEnvWithFIFODiskCredentialsCompletes(t *testing.T) {
	tests := []struct {
		name  string
		oauth bool
		link  bool
	}{
		{"token fifo", false, false},
		{"token symlink to fifo", false, true},
		{"oauth fifo", true, false},
		{"oauth symlink to fifo", true, true},
	}
	for _, tc := range tests {
		name, link := tc.name, tc.link
		t.Run(name, func(t *testing.T) {
			isolate(t)
			t.Setenv(tokenEnvVar, "selected-env-token")
			tokenPath, err := api.TokenPath()
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Dir(tokenPath), 0o700); err != nil {
				t.Fatal(err)
			}
			path := tokenPath
			if tc.oauth {
				path, err = credentialsPath()
				if err != nil {
					t.Fatal(err)
				}
			}
			fifo := path
			if link {
				fifo = path + ".fifo"
			}
			if err := syscall.Mkfifo(fifo, 0o600); err != nil {
				t.Fatal(err)
			}
			if link {
				if err := os.Symlink(fifo, path); err != nil {
					t.Fatal(err)
				}
			}
			done := make(chan checkResult, 1)
			go func() {
				result, _ := checkToken()
				done <- result
			}()
			select {
			case result := <-done:
				want := "unused saved token"
				if tc.oauth {
					want = "unused saved app credentials"
				}
				if result.status != statusWarn || !strings.Contains(collapsed(noteText(result)), want) {
					t.Fatalf("result = %v, want disk warning with environment selection", checkReport(result))
				}
			case <-time.After(time.Second):
				t.Fatal("doctor blocked on a FIFO credential")
			}
		})
	}
}

func TestCheckTokenRefusesFIFOCredentialDirectoryWithoutBlocking(t *testing.T) {
	for name, link := range map[string]bool{"fifo": false, "symlink-to-fifo": true} {
		t.Run(name, func(t *testing.T) {
			isolate(t)
			t.Setenv(tokenEnvVar, "selected-env-token")
			tokenPath, err := api.TokenPath()
			if err != nil {
				t.Fatal(err)
			}
			dir := filepath.Dir(tokenPath)
			if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
				t.Fatal(err)
			}
			fifo := dir
			if link {
				fifo = dir + ".fifo"
			}
			if err := syscall.Mkfifo(fifo, 0o600); err != nil {
				t.Fatal(err)
			}
			if link {
				if err := os.Symlink(fifo, dir); err != nil {
					t.Fatal(err)
				}
			}
			done := make(chan checkResult, 1)
			go func() {
				result, _ := checkToken()
				done <- result
			}()
			select {
			case result := <-done:
				if result.status != statusWarn || !strings.Contains(collapsed(noteText(result)), "directory for saved credentials") {
					t.Fatalf("result = %v, want directory warning", checkReport(result))
				}
			case <-time.After(time.Second):
				t.Fatal("doctor blocked on a FIFO credential directory")
			}
		})
	}
}
