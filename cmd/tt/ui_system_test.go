package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/movsar/tt/internal/api"
	"github.com/movsar/tt/internal/config"
)

func TestUISettingsOnlyReadValidatedConfiguration(t *testing.T) {
	isolate(t)
	s := &uiSystem{output: io.Discard}
	defer s.close()
	before := config.Default()
	r, err := s.Settings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	path, _ := config.Path()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("settings initialized config")
	}
	if !strings.Contains(strings.Join(r.Lines, "\n"), "using defaults") || r.Actions == nil {
		t.Fatal("missing effective defaults")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("default_project = \"Work\"\n[timer]\nfocus = \"40m\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r, err = s.Settings(context.Background())
	if err != nil || r.DefaultProject != "Work" {
		t.Fatal("valid reload failed", err)
	}
	if err := os.WriteFile(path, []byte("unknown = true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r, err = s.Settings(context.Background())
	if err == nil || r.Actions != nil {
		t.Fatal("invalid config supplied services")
	}
	if !reflect.DeepEqual(before, config.Default()) {
		t.Fatal("defaults mutated")
	}
}

func TestUIInitPreviewRefusesConcurrentFileAndSymlink(t *testing.T) {
	for _, link := range []bool{false, true} {
		t.Run(map[bool]string{false: "file", true: "symlink"}[link], func(t *testing.T) {
			isolate(t)
			s := &uiSystem{}
			p, err := s.Prepare(context.Background(), "config init")
			if err != nil {
				t.Fatal(err)
			}
			path, _ := config.Path()
			if _, err := os.Lstat(path); !os.IsNotExist(err) {
				t.Fatal("preview wrote")
			}
			os.MkdirAll(filepath.Dir(path), 0o700)
			original := []byte("# another process\n")
			if link {
				target := filepath.Join(t.TempDir(), "other")
				os.WriteFile(target, original, 0o600)
				os.Symlink(target, path)
			} else {
				os.WriteFile(path, original, 0o600)
			}
			if _, err := p.Apply(context.Background()); err == nil {
				t.Fatal("replaced concurrent entry")
			}
			after, _ := os.ReadFile(path)
			if string(after) != string(original) {
				t.Fatal("lost concurrent config")
			}
		})
	}
}

func TestUINotificationFixAndTestHaveExactIndependentAuthority(t *testing.T) {
	isolate(t)
	s := &uiSystem{}
	path, _ := config.Path()
	os.MkdirAll(filepath.Dir(path), 0o700)
	marker := filepath.Join(t.TempDir(), "runs")
	dir := notifyPrograms(t, "custom-recorder")
	os.WriteFile(filepath.Join(dir, "custom-recorder"), []byte("#!/bin/sh\nprintf %s \"$2\" >> "+marker+"\n"), 0o700)
	original := "default_project = \"Work\"\n[timer]\nfocus = \"40m\"\non_end = 'custom-recorder -- \"{task}\"'\n"

	os.WriteFile(path, []byte(original), 0o600)
	p, err := s.Prepare(context.Background(), "notification fix")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("preview ran notifier")
	}
	if _, err := p.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	backup, _ := os.ReadFile(path + backupSuffix)
	if string(backup) != original {
		t.Fatal("backup not exact")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("fix ran notifier")
	}
	cfg, err := config.Load()
	if err != nil || cfg.DefaultProject != "Work" || cfg.Timer.Focus.String() != "40m" {
		t.Fatal("unrelated config changed", err)
	}
	p, err = s.Prepare(context.Background(), "notifier test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(marker)
	if string(data) != "Test task" {
		t.Fatal("wrong notifier payload")
	}
	p, err = s.Prepare(context.Background(), "notifier test")
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(path, []byte(original+"# changed\n"), 0o600)
	if _, err := p.Apply(context.Background()); err == nil {
		t.Fatal("stale notifier command ran")
	}
	data, _ = os.ReadFile(marker)
	if string(data) != "Test task" {
		t.Fatal("notifier repeated")
	}
}

func TestUISystemPrivateCredentialsAndNotifierOutput(t *testing.T) {
	isolate(t)
	s := &uiSystem{output: io.Discard}
	defer s.close()
	secret := "synthetic-private-value-123456"
	path, _ := api.TokenPath()
	if err := writeSecretFile(path, secret); err != nil {
		t.Fatal(err)
	}
	if err := saveCredentials(appCredentials{ClientSecret: secret + "-client", RefreshToken: secret + "-refresh"}); err != nil {
		t.Fatal(err)
	}
	configPath, _ := config.Path()
	os.MkdirAll(filepath.Dir(configPath), 0o700)
	os.WriteFile(configPath, []byte("[timer]\non_end = 'cat \"$XDG_DATA_HOME/ticktick/token\"'\n"), 0o600)
	r, err := s.Settings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(r.Lines, "\n"), secret) {
		t.Fatal("settings leaked token")
	}
	p, err := s.Prepare(context.Background(), "notifier test")
	if err != nil {
		t.Fatal(err)
	}
	text, err := p.Apply(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(text, secret) {
		t.Fatal("notifier output leaked token")
	}
	os.WriteFile(configPath, []byte("[timer]\non_end = 'printf "+secret+"'\n"), 0o600)
	r, err = s.Settings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(r.Lines, "\n"), secret) {
		t.Fatal("config leaked token")
	}
	if _, err := s.Prepare(context.Background(), "notifier test"); err == nil {
		t.Fatal("private preview accepted")
	}
}

func TestUIFixKeepsUnsupportedRepairsManual(t *testing.T) {
	isolate(t)
	s := &uiSystem{}
	path, _ := config.Path()
	os.MkdirAll(filepath.Dir(path), 0o700)
	for _, command := range []string{"echo {task} | cat", "sh -c \"echo {task}\""} {
		original := "[timer]\non_end = '" + command + "'\n"
		os.WriteFile(path, []byte(original), 0o600)
		if _, err := s.Prepare(context.Background(), "notification fix"); err == nil {
			t.Fatal("expanded repair scope")
		}
		after, _ := os.ReadFile(path)
		if string(after) != original {
			t.Fatal("unsupported source changed")
		}
	}
}
