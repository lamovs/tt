package install

import (
	"encoding/xml"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func put(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(content)
}

func fixture(t *testing.T) Installer {
	t.Helper()
	root := t.TempDir()
	i := Installer{Platform: "linux", Home: filepath.Join(root, "user space"), Executable: filepath.Join(root, "release", "tt"), Resources: filepath.Join(root, "release", "share", "tt")}
	put(t, i.Executable, "binary one", 0o755)
	put(t, filepath.Join(i.Resources, "integrations", "tmux", "tt.conf"), "set -g status-interval 1\n", 0o644)
	i.Run = func(string, ...string) error { t.Fatal("unexpected process execution"); return nil }
	return i
}

func TestInstallAndUpdateKeepDataAndOldBinary(t *testing.T) {
	i := fixture(t)
	data := filepath.Join(i.Home, ".local", "share", "ticktick", "token")
	put(t, data, "private fixture", 0o600)
	target := filepath.Join(i.Home, ".local", "bin", "tt")
	put(t, target, "previous executable", 0o755)
	got, err := i.Install()
	if err != nil || got != target {
		t.Fatalf("install: %q, %v", got, err)
	}
	if read(t, target) != "binary one" || read(t, data) != "private fixture" {
		t.Fatal("unexpected installed binary or data mutation")
	}
	backups, _ := filepath.Glob(filepath.Join(filepath.Dir(target), ".tt-backup-*", "tt"))
	if len(backups) != 1 || read(t, backups[0]) != "previous executable" {
		t.Fatal("previous executable was not retained")
	}
	first, _ := os.Readlink(target)
	if _, err := i.Install(); err != nil {
		t.Fatal(err)
	}
	backups, _ = filepath.Glob(filepath.Join(filepath.Dir(target), ".tt-backup-*", "tt"))
	if len(backups) != 1 {
		t.Fatal("reinstalling the same bundle should be a no-op")
	}
	put(t, i.Executable, "binary two", 0o755)
	if _, err := i.Install(); err != nil {
		t.Fatal(err)
	}
	if read(t, target) != "binary two" || read(t, first) != "binary one" {
		t.Fatal("update did not preserve the old bundle")
	}
	resources, err := Resources(target)
	if err != nil || read(t, filepath.Join(resources, "integrations", "tmux", "tt.conf")) == "" {
		t.Fatalf("installed resources: %v", err)
	}
	if _, err := os.Stat(filepath.Join(i.Home, ".zshrc")); !os.IsNotExist(err) {
		t.Fatal("installation wrote shell configuration")
	}
}

func TestInstallRefusesUnsafeResourcesWithoutReplacingBinary(t *testing.T) {
	i := fixture(t)
	target := filepath.Join(i.Home, ".local", "bin", "tt")
	put(t, target, "old", 0o755)
	if err := os.Symlink(i.Executable, filepath.Join(i.Resources, "link")); err != nil {
		t.Fatal(err)
	}
	if _, err := i.Install(); err == nil {
		t.Fatal("accepted a symbolic link in resources")
	}
	if read(t, target) != "old" {
		t.Fatal("old executable changed after a failed install")
	}
}

func TestInstallRefusesModifiedBundle(t *testing.T) {
	i := fixture(t)
	target, err := i.Install()
	if err != nil {
		t.Fatal(err)
	}
	put(t, target, "modified", 0o755)
	if _, err := i.Install(); err == nil {
		t.Fatal("overwrote modified bundle")
	}
}

func TestResourcesFindsHomebrewAndArchiveLayouts(t *testing.T) {
	for _, bin := range []string{"tt", "bin/tt"} {
		t.Run(bin, func(t *testing.T) {
			root := t.TempDir()
			executable := filepath.Join(root, bin)
			put(t, executable, "binary", 0o755)
			put(t, filepath.Join(root, "share", "tt", "integrations", "example"), "resource", 0o644)
			link := filepath.Join(t.TempDir(), "tt")
			if err := os.Symlink(executable, link); err != nil {
				t.Fatal(err)
			}
			got, err := Resources(link)
			want, _ := filepath.EvalSymlinks(filepath.Join(root, "share", "tt"))
			if err != nil || got != want {
				t.Fatalf("resources: %q, %v", got, err)
			}
		})
	}
}

func TestBackgroundUsesSelectedExecutableAndXDGWithoutSecrets(t *testing.T) {
	for _, platform := range []string{"linux", "darwin"} {
		t.Run(platform, func(t *testing.T) {
			i := fixture(t)
			i.Platform = platform
			i.Executable = filepath.Join(i.Home, "brew % $ &", "bin", "tt")
			i.Env = []string{"XDG_CONFIG_HOME=" + filepath.Join(i.Home, "config"), "XDG_DATA_HOME=" + filepath.Join(i.Home, "custom data"), "TT_TOKEN=private", "GITHUB_TOKEN=private", "PATH=/untrusted", "DISPLAY=:1"}
			var calls []string
			i.Run = func(name string, args ...string) error {
				calls = append(calls, name+" "+strings.Join(args, " "))
				return nil
			}
			if err := i.Background(false); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(i.Home, "config", "systemd", "user", "tt-background.service")
			if platform == "darwin" {
				path = filepath.Join(i.Home, "Library", "LaunchAgents", "com.movsar.tt.background.plist")
			}
			content := read(t, path)
			if strings.Contains(content, "private") || strings.Contains(content, "TOKEN") || strings.Contains(content, "/untrusted") {
				t.Fatal("service persisted a secret or arbitrary PATH")
			}
			if !strings.Contains(content, "XDG_DATA_HOME") || !strings.Contains(content, "custom data") {
				t.Fatal("service lost the selected data location")
			}
			if platform == "linux" {
				if !strings.Contains(content, "brew %% $$ &") || !strings.Contains(content, "Environment=\"PATH=") {
					t.Fatal("incorrect systemd quoting")
				}
			} else {
				decoder := xml.NewDecoder(strings.NewReader(content))
				for {
					_, err := decoder.Token()
					if errors.Is(err, io.EOF) {
						break
					}
					if err != nil {
						t.Fatal(err)
					}
				}
				if !strings.Contains(content, "brew % $ &amp;") {
					t.Fatal("incorrect plist escaping")
				}
			}
			if len(calls) != 2 {
				t.Fatalf("unexpected activation calls: %v", calls)
			}
			if err := i.Background(true); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Lstat(path); !os.IsNotExist(err) {
				t.Fatal("service was not removed")
			}
			backups, _ := filepath.Glob(filepath.Join(filepath.Dir(path), ".tt-backup-*", filepath.Base(path)))
			if len(backups) != 1 || read(t, backups[0]) != content {
				t.Fatal("service backup missing")
			}
		})
	}
}

func TestFailedBackgroundStopKeepsService(t *testing.T) {
	i := fixture(t)
	i.Platform = "darwin"
	path := filepath.Join(i.Home, "Library", "LaunchAgents", "com.movsar.tt.background.plist")
	put(t, path, "existing service", 0o600)
	i.Run = func(string, ...string) error { return errors.New("unavailable") }
	if err := i.Background(true); err == nil || read(t, path) != "existing service" {
		t.Fatal("failed stop removed the service")
	}
}

func TestReplaceRestoresPreviousFileOnFailure(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "tt")
	put(t, path, "old", 0o755)
	if err := replace(filepath.Join(root, "missing"), path); err == nil || read(t, path) != "old" {
		t.Fatal("failed replacement did not restore previous file")
	}
}

func TestNotifierPreservesPreviousAppAndNeverStartsBackground(t *testing.T) {
	i := fixture(t)
	i.Platform = "darwin"
	app := filepath.Join(i.Resources, "TT Notifier.app")
	put(t, filepath.Join(app, "Contents", "Info.plist"), `<plist><dict><key>CFBundleIdentifier</key><string>com.movsar.tt.focus-notifier</string></dict></plist>`, 0o644)
	put(t, filepath.Join(app, "Contents", "MacOS", "TTNotifier"), "notifier", 0o755)
	put(t, filepath.Join(i.Resources, "integrations", "macos", "tt-notify"), "launcher", 0o755)
	i.Run = func(name string, args ...string) error {
		switch filepath.Base(name) {
		case "codesign", "lsregister":
			return nil
		case "ditto":
			if len(args) != 4 || args[0] != "--rsrc" || args[1] != "--extattr" {
				t.Fatal("copy must retain extended attributes")
			}
			return copyTree(args[2], args[3])
		default:
			t.Fatalf("unexpected external command: %s", name)
			return nil
		}
	}
	for range 2 {
		if err := i.Notifier(); err != nil {
			t.Fatal(err)
		}
	}
	backups, _ := filepath.Glob(filepath.Join(i.Home, ".local", "share", "tt", ".tt-backup-*", "TT Notifier.app"))
	if len(backups) != 1 {
		t.Fatal("previous notifier app was not kept")
	}
	if _, err := os.Stat(filepath.Join(i.Home, "Library", "LaunchAgents")); !os.IsNotExist(err) {
		t.Fatal("notification setup configured background sync")
	}
}

func TestNotifierRejectsInvalidSignatureAndUnrelatedTarget(t *testing.T) {
	i := fixture(t)
	i.Platform = "darwin"
	i.Run = func(string, ...string) error { return errors.New("invalid signature") }
	if err := i.Notifier(); err == nil {
		t.Fatal("accepted an invalid signature")
	}
	target := filepath.Join(t.TempDir(), "TT Notifier.app")
	put(t, filepath.Join(target, "personal"), "keep", 0o600)
	if err := preserve(target); err == nil || read(t, filepath.Join(target, "personal")) != "keep" {
		t.Fatal("replaced an unrelated app")
	}
}
