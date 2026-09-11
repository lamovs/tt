package main

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

func archiveFiles(platform string) map[string]int64 {
	files := map[string]int64{
		"tt":                                  0o755,
		"share/tt/integrations/herdr/tt.toml": 0o644,
		"share/tt/integrations/tmux/tt.conf":  0o644,
	}
	if platform == "darwin" {
		files["share/tt/integrations/macos/tt-notify"] = 0o755
		root := "share/tt/TT Notifier.app/Contents/"
		files[root+"Info.plist"] = 0o644
		files[root+"MacOS/TTNotifier"] = 0o755
		files[root+"Resources/tt.icns"] = 0o644
		files[root+"_CodeSignature/CodeResources"] = 0o644
	}
	return files
}

func verifyArchives(dir, version, checksums string) error {
	sums := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(checksums), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 {
			sums[fields[1]] = fields[0]
		}
	}
	for _, platform := range []string{"darwin", "linux"} {
		for _, arch := range []string{"arm64", "amd64"} {
			name := "tt_" + version + "_" + platform + "_" + arch + ".tar.gz"
			if err := verifyArchive(filepath.Join(dir, name), sums[name], platform); err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
		}
	}
	return nil
}

func verifyArchive(path, expected, platform string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, f); err != nil {
		return err
	}
	if hex.EncodeToString(hash.Sum(nil)) != expected {
		return errors.New("checksum mismatch")
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()
	reader := tar.NewReader(gz)
	remaining := archiveFiles(platform)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		mode, allowed := remaining[header.Name]
		if !allowed || header.Typeflag != tar.TypeReg || header.Mode != mode || header.Size <= 0 || header.Size > 64<<20 || header.Uid != 0 || header.Gid != 0 {
			return errors.New("unexpected archive entry, ownership or permissions")
		}
		delete(remaining, header.Name)
		if _, err := io.Copy(io.Discard, reader); err != nil {
			return err
		}
	}
	if len(remaining) != 0 {
		return errors.New("required installation files are missing")
	}
	return nil
}
