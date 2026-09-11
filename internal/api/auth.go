package api

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func TokenPath() (string, error) {
	dataHome := os.Getenv("XDG_DATA_HOME")
	if dataHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("ticktick: resolve home directory: %w", err)
		}
		dataHome = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(dataHome, "ticktick", "token"), nil
}

type TokenInfo struct {
	Value        string
	Path         string
	ResolvedPath string
	PathVerified bool
	Mode         os.FileMode
	Insecure     bool
}

func LoadToken() (TokenInfo, error) {
	path, err := TokenPath()
	if err != nil {
		return TokenInfo{}, err
	}
	return LoadTokenFrom(path)
}

func LoadTokenFrom(path string) (TokenInfo, error) {
	file, err := ReadRegularFile(path)
	if err != nil {
		return TokenInfo{}, fmt.Errorf("ticktick: read token file %s: %w", path, err)
	}
	return TokenInfo{
		Value:        strings.TrimSpace(string(file.Data)),
		Path:         path,
		ResolvedPath: file.ResolvedPath,
		PathVerified: file.PathVerified,
		Mode:         file.Mode,
		Insecure:     file.Mode&0o077 != 0,
	}, nil
}
