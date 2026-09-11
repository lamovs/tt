//go:build unix

package main

import "github.com/movsar/tt/internal/store"

func acquireSyncLock(st *store.Store) (func() error, error) {
	lock, err := store.AcquireLock(store.LockPath(st.Path()))
	if err != nil {
		return nil, err
	}
	return lock.Release, nil
}
