package store

import (
	"errors"
	"fmt"
	"os"
	"strconv"
)

var ErrLocked = errors.New("another tt process holds the lock")

type Lock struct {
	f *os.File
}

func LockPath(dbPath string) string { return dbPath + ".lock" }

func AcquireLock(path string) (*Lock, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, filePerm)
	if err != nil {
		return nil, fmt.Errorf("open lock %s: %w", path, err)
	}
	if err := lockFile(f); err != nil {
		f.Close()
		if errors.Is(err, ErrLocked) {
			return nil, err
		}
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}

	if err := f.Truncate(0); err == nil {
		f.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0)
	}
	return &Lock{f: f}, nil
}

func (l *Lock) Release() error {
	if l == nil || l.f == nil {
		return nil
	}
	err := unlockFile(l.f)
	if cerr := l.f.Close(); err == nil {
		err = cerr
	}
	l.f = nil
	return err
}
