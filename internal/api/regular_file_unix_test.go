//go:build unix

package api

import (
	"errors"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestReadRegularFileRefusesFIFOWithoutBlocking(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	for name, path := range map[string]string{"direct": fifo, "symlink": filepath.Join(dir, "link")} {
		t.Run(name, func(t *testing.T) {
			if name == "symlink" {
				if err := syscall.Symlink(fifo, path); err != nil {
					t.Fatal(err)
				}
			}
			done := make(chan error, 1)
			go func() {
				_, err := ReadRegularFile(path)
				done <- err
			}()
			select {
			case err := <-done:
				if !errors.Is(err, ErrNotRegular) {
					t.Fatalf("error = %v, want ErrNotRegular", err)
				}
			case <-time.After(time.Second):
				t.Fatal("credential FIFO read blocked")
			}
		})
	}
}
