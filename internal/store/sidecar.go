package store

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
)

var ErrUnusableSidecar = errors.New("cache sidecar is not usable")

func preflightSidecars(path string) error {
	if err := preflightSidecar(path+"-wal", true); err != nil {
		return err
	}
	return preflightSidecar(path+"-shm", false)
}

func preflightSidecar(path string, wal bool) error {
	f, err := openSidecarNonblocking(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("%w: open %s: %w", ErrUnusableSidecar, path, err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return fmt.Errorf("%w: inspect %s: %w", ErrUnusableSidecar, path, err)
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("%w: %s is not a regular file", ErrUnusableSidecar, path)
	}
	if !wal || fi.Size() == 0 {
		return nil
	}
	if fi.Size() < 32 {
		return fmt.Errorf("%w: %s has a truncated WAL header", ErrUnusableSidecar, path)
	}
	header := make([]byte, 32)
	if _, err := io.ReadFull(f, header); err != nil {
		return fmt.Errorf("%w: read %s header: %w", ErrUnusableSidecar, path, err)
	}
	magic := binary.BigEndian.Uint32(header[0:4])
	if magic != 0x377f0682 && magic != 0x377f0683 {
		return fmt.Errorf("%w: %s has an invalid WAL header", ErrUnusableSidecar, path)
	}
	pageSize := int64(binary.BigEndian.Uint32(header[8:12]))
	if pageSize < 512 || pageSize > 65536 || pageSize&(pageSize-1) != 0 {
		return fmt.Errorf("%w: %s has an invalid WAL page size", ErrUnusableSidecar, path)
	}
	return nil
}
