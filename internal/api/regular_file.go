package api

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

var ErrNotRegular = errors.New("not a regular file")

var ErrUnusableLink = errors.New("symbolic link target cannot be opened")

var ErrNotDirectory = errors.New("not a directory")

type RegularFile struct {
	Data         []byte
	Mode         os.FileMode
	ResolvedPath string
	PathVerified bool
}

type DirectoryInfo struct {
	Mode         os.FileMode
	ResolvedPath string
	PathVerified bool
}

func ReadRegularFile(path string) (RegularFile, error) {
	f, err := openNonblocking(path)
	if err != nil {
		if li, lerr := os.Lstat(path); lerr == nil && li.Mode()&os.ModeSymlink != 0 {
			return RegularFile{}, fmt.Errorf("%w: %w", ErrUnusableLink, err)
		}
		return RegularFile{}, err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return RegularFile{}, err
	}
	if !info.Mode().IsRegular() {
		return RegularFile{}, fmt.Errorf("%s: %w", path, ErrNotRegular)
	}
	raw, err := io.ReadAll(f)
	if err != nil {
		return RegularFile{}, err
	}
	resolved, verified := resolvedOpenedPath(path, info)
	return RegularFile{Data: raw, Mode: info.Mode().Perm(), ResolvedPath: resolved, PathVerified: verified}, nil
}

func InspectDirectory(path string) (DirectoryInfo, error) {
	f, err := openDirectoryNonblocking(path)
	if err != nil {
		return DirectoryInfo{}, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return DirectoryInfo{}, err
	}
	if !info.IsDir() {
		return DirectoryInfo{}, fmt.Errorf("%s: %w", path, ErrNotDirectory)
	}
	resolved, verified := resolvedOpenedPath(path, info)
	return DirectoryInfo{Mode: info.Mode().Perm(), ResolvedPath: resolved, PathVerified: verified}, nil
}

func resolvedOpenedPath(path string, opened os.FileInfo) (string, bool) {
	target, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", false
	}
	current, err := os.Stat(target)
	if err != nil || !os.SameFile(opened, current) {
		return "", false
	}
	return target, true
}
