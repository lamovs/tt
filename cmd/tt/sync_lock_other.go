//go:build !unix

package main

import "github.com/movsar/tt/internal/store"

func acquireSyncLock(*store.Store) (func() error, error) {
	return func() error { return nil }, nil
}
