package install

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type Installer struct {
	Platform   string
	Home       string
	Executable string
	Resources  string
	Env        []string
	Run        func(string, ...string) error
}

func Resources(executable string) (string, error) {
	real, err := filepath.EvalSymlinks(executable)
	if err != nil {
		return "", err
	}
	for _, dir := range []string{
		filepath.Join(filepath.Dir(real), "share", "tt"),
		filepath.Join(filepath.Dir(real), "..", "share", "tt"),
	} {
		if info, err := os.Stat(filepath.Join(dir, "integrations")); err == nil && info.IsDir() {
			return filepath.Clean(dir), nil
		}
	}
	return "", errors.New("installation files are missing; install a release archive or the Homebrew package")
}

func (i Installer) Install() (string, error) {
	if !filepath.IsAbs(i.Home) || !filepath.IsAbs(i.Executable) || strings.ContainsAny(i.Home, "\x00\r\n") {
		return "", errors.New("installation requires absolute home and executable paths")
	}
	root := filepath.Join(i.Home, ".local", "lib", "tt")
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", err
	}
	stage, err := os.MkdirTemp(root, ".install-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(stage)
	if i.Platform == "darwin" {
		if err := i.Run("/usr/bin/ditto", "--rsrc", "--extattr", i.Executable, filepath.Join(stage, "tt")); err != nil {
			return "", err
		}
	} else if err := copyFile(i.Executable, filepath.Join(stage, "tt"), 0o755); err != nil {
		return "", err
	}
	if err := i.copyResources(i.Resources, filepath.Join(stage, "share", "tt")); err != nil {
		return "", err
	}
	digest, err := treeDigest(stage)
	if err != nil {
		return "", err
	}
	bundle := filepath.Join(root, digest)
	if _, err := os.Lstat(bundle); errors.Is(err, fs.ErrNotExist) {
		if err := os.Rename(stage, bundle); err != nil {
			return "", err
		}
	} else if err != nil {
		return "", err
	} else if existing, err := treeDigest(bundle); err != nil || existing != digest {
		return "", errors.New("an existing installation bundle was modified; nothing replaced")
	}
	target := filepath.Join(i.Home, ".local", "bin", "tt")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return "", err
	}
	a, errA := os.Stat(target)
	b, errB := os.Stat(filepath.Join(bundle, "tt"))
	if errA == nil && errB == nil && os.SameFile(a, b) {
		return target, nil
	}
	linkStage, err := os.MkdirTemp(filepath.Dir(target), ".tt-link-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(linkStage)
	link := filepath.Join(linkStage, "tt")
	if err := os.Symlink(filepath.Join(bundle, "tt"), link); err != nil {
		return "", err
	}
	if err := replace(link, target); err != nil {
		return "", err
	}
	return target, nil
}

func (i Installer) Background(remove bool) error {
	if !filepath.IsAbs(i.Home) || !filepath.IsAbs(i.Executable) || strings.ContainsAny(i.Home+i.Executable, "\x00\r\n") {
		return errors.New("background service requires an absolute executable path without line breaks")
	}
	if i.Platform == "darwin" {
		return i.launchAgent(remove)
	}
	if i.Platform == "linux" {
		return i.systemd(remove)
	}
	return errors.New("background setup supports macOS and Linux only")
}

func (i Installer) launchAgent(remove bool) error {
	const label = "com.movsar.tt.background"
	path := filepath.Join(i.Home, "Library", "LaunchAgents", label+".plist")
	domain := fmt.Sprintf("gui/%d", os.Getuid())
	if remove {
		return i.removeService(path, func() error { return i.Run("/bin/launchctl", "bootout", domain, path) })
	}
	var env strings.Builder
	for _, entry := range i.serviceEnv() {
		key, value, _ := strings.Cut(entry, "=")
		env.WriteString("<key>" + xmlText(key) + "</key><string>" + xmlText(value) + "</string>")
	}
	content := "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n" +
		"<!DOCTYPE plist PUBLIC \"-//Apple//DTD PLIST 1.0//EN\" \"http://www.apple.com/DTDs/PropertyList-1.0.dtd\">\n" +
		"<plist version=\"1.0\"><dict><key>Label</key><string>" + label + "</string>" +
		"<key>ProgramArguments</key><array><string>" + xmlText(i.Executable) + "</string>" +
		"<string>auto</string><string>run</string><string>--quiet</string></array>" +
		"<key>EnvironmentVariables</key><dict>" + env.String() + "</dict>" +
		"<key>RunAtLoad</key><true/><key>StartInterval</key><integer>60</integer>" +
		"<key>ProcessType</key><string>Background</string>" +
		"<key>LimitLoadToSessionType</key><string>Aqua</string>" +
		"<key>StandardOutPath</key><string>/dev/null</string>" +
		"<key>StandardErrorPath</key><string>/dev/null</string></dict></plist>\n"
	if err := writeBacked(path, []byte(content), 0o600); err != nil {
		return err
	}
	_ = i.Run("/bin/launchctl", "bootout", domain+"/"+label)
	if err := i.Run("/bin/launchctl", "bootstrap", domain, path); err != nil {
		return errors.New("service file was saved, but launchctl could not start it; run tt setup background in a macOS desktop session")
	}
	return nil
}

func (i Installer) systemd(remove bool) error {
	config := filepath.Join(i.Home, ".config")
	for _, item := range i.Env {
		if value, ok := strings.CutPrefix(item, "XDG_CONFIG_HOME="); ok && filepath.IsAbs(value) {
			config = value
		}
	}
	dir := filepath.Join(config, "systemd", "user")
	service := filepath.Join(dir, "tt-background.service")
	timer := filepath.Join(dir, "tt-background.timer")
	if remove {
		if err := i.Run("systemctl", "--user", "disable", "--now", "tt-background.timer"); err != nil {
			return errors.New("could not stop the timer; service files were not removed")
		}
		if err := i.Run("systemctl", "--user", "stop", "tt-background.service"); err != nil {
			return errors.New("could not stop background work; service files were not removed")
		}
		for _, path := range []string{service, timer} {
			if err := preserve(path); err != nil {
				return err
			}
		}
		return i.Run("systemctl", "--user", "daemon-reload")
	}
	var env strings.Builder
	for _, entry := range i.serviceEnv() {
		env.WriteString("Environment=" + systemdQuote(entry) + "\n")
	}
	content := "[Unit]\nDescription=tt automatic sync and reminders\n\n[Service]\nType=oneshot\n" +
		"ExecStart=" + strings.ReplaceAll(systemdQuote(i.Executable), "$", "$$") + " auto run --quiet\n" + env.String()
	if err := writeBacked(service, []byte(content), 0o600); err != nil {
		return err
	}
	content = "[Unit]\nDescription=Run tt background work every minute\n\n[Timer]\n" +
		"OnBootSec=1min\nOnCalendar=*-*-* *:*:00\nAccuracySec=1s\nPersistent=true\n\n[Install]\nWantedBy=timers.target\n"
	if err := writeBacked(timer, []byte(content), 0o600); err != nil {
		return err
	}
	if err := i.Run("systemctl", "--user", "daemon-reload"); err != nil {
		return errors.New("service files were saved, but systemctl could not reload them")
	}
	if err := i.Run("systemctl", "--user", "enable", "--now", "tt-background.timer"); err != nil {
		return errors.New("service files were saved, but the user timer could not start; a systemd user session is required")
	}
	return nil
}

func (i Installer) removeService(path string, stop func() error) error {
	if _, err := os.Lstat(path); errors.Is(err, fs.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	if err := stop(); err != nil {
		return errors.New("could not stop background work; the service file was not removed")
	}
	return preserve(path)
}

func (i Installer) serviceEnv() []string {
	env := map[string]string{
		"HOME": i.Home,
		"PATH": filepath.Join(i.Home, ".local", "bin") + ":" + filepath.Dir(i.Executable) + ":/opt/homebrew/bin:/home/linuxbrew/.linuxbrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin",
	}
	for _, entry := range i.Env {
		key, value, _ := strings.Cut(entry, "=")
		switch key {
		case "XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_CACHE_HOME", "XDG_STATE_HOME", "XDG_RUNTIME_DIR", "XDG_CONFIG_DIRS", "XDG_DATA_DIRS", "DBUS_SESSION_BUS_ADDRESS", "DISPLAY", "WAYLAND_DISPLAY":
			if value != "" && !strings.ContainsAny(value, "\x00\r\n") {
				env[key] = value
			}
		}
	}
	var result []string
	for key, value := range env {
		result = append(result, key+"="+value)
	}
	sort.Strings(result)
	return result
}

func (i Installer) Notifier() error {
	if i.Platform != "darwin" {
		return errors.New("the bundled notifier supports macOS only; Linux uses notify-send")
	}
	if !filepath.IsAbs(i.Home) || strings.ContainsAny(i.Home, "\x00\r\n") {
		return errors.New("notification setup requires an absolute home path without line breaks")
	}
	source := filepath.Join(i.Resources, "TT Notifier.app")
	if err := i.Run("/usr/bin/codesign", "--verify", "--deep", "--strict", source); err != nil {
		return errors.New("the bundled notifier is missing or its signature is invalid")
	}
	root := filepath.Join(i.Home, ".local", "share", "tt")
	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
	}
	stage, err := os.MkdirTemp(root, ".notifier-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	app := filepath.Join(stage, "TT Notifier.app")
	if err := i.copyResources(source, app); err != nil {
		return err
	}
	target := filepath.Join(root, "TT Notifier.app")
	if err := replace(app, target); err != nil {
		return err
	}
	launcher, err := os.ReadFile(filepath.Join(i.Resources, "integrations", "macos", "tt-notify"))
	if err != nil {
		return err
	}
	if err := writeBacked(filepath.Join(i.Home, ".local", "bin", "tt-notify"), launcher, 0o755); err != nil {
		return err
	}
	if err := i.Run("/System/Library/Frameworks/CoreServices.framework/Frameworks/LaunchServices.framework/Support/lsregister", "-f", target); err != nil {
		return errors.New("the notifier was installed, but macOS registration failed")
	}
	return nil
}

func systemdQuote(s string) string {
	r := strings.NewReplacer("\\", "\\\\", "\"", "\\\"", "%", "%%", "\n", "\\n", "\r", "\\r", "\t", "\\t")
	return "\"" + r.Replace(s) + "\""
}

func xmlText(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

func preserve(path string) error {
	_, err := backup(path)
	return err
}

func backup(path string) (string, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if info.IsDir() && (filepath.Base(path) != "TT Notifier.app" || !notifierBundle(path)) {
		return "", errors.New("refusing to replace an unrelated directory")
	}
	dir, err := os.MkdirTemp(filepath.Dir(path), ".tt-backup-")
	if err != nil {
		return "", err
	}
	target := filepath.Join(dir, filepath.Base(path))
	return target, os.Rename(path, target)
}

func replace(source, target string) error {
	previous, err := backup(target)
	if err != nil {
		return err
	}
	if err := os.Rename(source, target); err != nil {
		if previous != "" {
			return errors.Join(err, os.Rename(previous, target))
		}
		return err
	}
	return nil
}

func notifierBundle(path string) bool {
	content, err := os.ReadFile(filepath.Join(path, "Contents", "Info.plist"))
	if err != nil {
		return false
	}
	d := xml.NewDecoder(bytes.NewReader(content))
	identifier := false
	for {
		token, err := d.Token()
		if err != nil {
			return false
		}
		start, ok := token.(xml.StartElement)
		if !ok {
			continue
		}
		if start.Name.Local == "key" {
			var key string
			if d.DecodeElement(&key, &start) != nil {
				return false
			}
			identifier = key == "CFBundleIdentifier"
		} else if start.Name.Local == "string" && identifier {
			var value string
			if d.DecodeElement(&value, &start) != nil {
				return false
			}
			return value == "com.movsar.tt.notifier" || value == "com.movsar.tt.focus-notifier"
		}
	}
}

func (i Installer) copyResources(source, target string) error {
	if _, err := treeDigest(source); err != nil {
		return err
	}
	if i.Platform == "darwin" {
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return i.Run("/usr/bin/ditto", "--rsrc", "--extattr", source, target)
	}
	return copyTree(source, target)
}

func writeBacked(path string, content []byte, mode fs.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if info, err := os.Lstat(path); err == nil && info.Mode().IsRegular() {
		if old, err := os.ReadFile(path); err == nil && bytes.Equal(old, content) && info.Mode().Perm() == mode {
			return nil
		}
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tt-write-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	_, writeErr := tmp.Write(content)
	modeErr := tmp.Chmod(mode)
	closeErr := tmp.Close()
	if err := errors.Join(writeErr, modeErr, closeErr); err != nil {
		return err
	}
	return replace(tmp.Name(), path)
}

func copyFile(source, target string, mode fs.FileMode) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	return errors.Join(copyErr, out.Close())
}

func copyTree(source, target string) error {
	return filepath.WalkDir(source, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		dest := filepath.Join(target, rel)
		if entry.IsDir() {
			return os.MkdirAll(dest, 0o755)
		}
		if !entry.Type().IsRegular() {
			return errors.New("installation resources must contain only regular files and directories")
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		mode := fs.FileMode(0o644)
		if info.Mode()&0o111 != 0 {
			mode = 0o755
		}
		return copyFile(path, dest, mode)
	})
}

func treeDigest(root string) (string, error) {
	h := sha256.New()
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		if !entry.Type().IsRegular() {
			return errors.New("installation bundle contains a non-regular file")
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		fmt.Fprintf(h, "%s\x00%d\x00%d\x00", rel, info.Mode().Perm(), info.Size())
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		_, readErr := io.Copy(h, f)
		return errors.Join(readErr, f.Close())
	})
	return hex.EncodeToString(h.Sum(nil)), err
}
