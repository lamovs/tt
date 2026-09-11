//go:build unix

package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestInspectRefusesSpecialSidecarsWithoutBlocking(t *testing.T) {
	for _, suffix := range []string{"-wal", "-shm"} {
		t.Run(suffix, func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "cache.db")
			s, err := Open(ctx, path)
			if err != nil {
				t.Fatal(err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			os.Remove(path + suffix)
			if err := syscall.Mkfifo(path+suffix, 0o600); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() {
				_, err := Inspect(ctx, path)
				done <- err
			}()
			select {
			case err := <-done:
				if !errors.Is(err, ErrUnusableSidecar) {
					t.Fatalf("Inspect error = %v, want ErrUnusableSidecar", err)
				}
			case <-time.After(time.Second):
				t.Fatal("Inspect blocked on sidecar")
			}
		})
	}
}
