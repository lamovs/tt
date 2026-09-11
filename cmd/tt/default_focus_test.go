package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/movsar/tt/internal/cli"
	"github.com/movsar/tt/internal/config"
	"github.com/movsar/tt/internal/model"
	"github.com/movsar/tt/internal/store"
)

func focusConfigCommand(t *testing.T, input io.Reader, args ...string) (int, string, string) {
	t.Helper()
	var out, diagnostic bytes.Buffer
	code := run(context.Background(), append([]string{"config", "default-focus"}, args...), input, &out, &diagnostic)
	return code, out.String(), diagnostic.String()
}

func TestDefaultFocusCLIPickerBackupAndNoop(t *testing.T) {
	isolate(t)
	id := seedFeatureCommandTask(t, model.Task{Title: "Focus target"})
	st, err := store.Open(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	task, _ := st.Task(context.Background(), id)
	original := "# owner\ndefault_project = 'id:" + task.ProjectId + "'\n[timer]\nfocus = '40m'\n"
	path := writeConfig(t, original)
	previous := canAsk
	canAsk = func(*invocation) bool { return true }
	t.Cleanup(func() { canAsk = previous })
	code, out, diagnostic := focusConfigCommand(t, strings.NewReader("\n1\n"))
	if code != exitOK || !strings.Contains(out, "task:"+id) || !strings.Contains(diagnostic, "Focus target") {
		t.Fatalf("%d %s %s", code, out, diagnostic)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "default_focus = 'task:"+id+"'\n"+original {
		t.Fatalf("config: %q", got)
	}
	backup, _ := os.ReadFile(path + ".bak")
	if string(backup) != original {
		t.Fatalf("backup: %q", backup)
	}
	if code, out, diagnostic := focusConfigCommand(t, nil, "task:"+id); code != exitOK || !strings.Contains(out, "unchanged") {
		t.Fatalf("%d %s %s", code, out, diagnostic)
	}
	if _, err := os.Stat(path + ".bak.1"); !os.IsNotExist(err) {
		t.Fatal("noop wrote backup")
	}
	if code, _, diagnostic := focusConfigCommand(t, strings.NewReader("q\n")); code != exitOK {
		t.Fatal(diagnostic)
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(got, after) {
		t.Fatal("cancel changed config")
	}
}

func TestDefaultFocusCLINoneNeedsNoCacheAndInvalidSyntaxCreatesNothing(t *testing.T) {
	for _, arg := range []string{"task:", "timer:x", "id:x", "none", "task:missing"} {
		t.Run(arg, func(t *testing.T) {
			isolate(t)
			code, _, diagnostic := focusConfigCommand(t, nil, arg)
			if arg == "none" {
				if code != exitOK {
					t.Fatal(diagnostic)
				}
			} else if code == exitOK {
				t.Fatal("invalid target accepted")
			}
			path, _ := store.DefaultPath()
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatal("picker created cache")
			}
			path, _ = config.Path()
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatal("noop or refusal wrote config")
			}
		})
	}
}

func TestDefaultFocusCLIStartsBothModesAndExplicitOverrides(t *testing.T) {
	for _, verb := range []string{"timer", "pomodoro"} {
		for _, destination := range []string{"default", "explicit", "none", "missing"} {
			t.Run(verb+"/"+destination, func(t *testing.T) {
				isolate(t)
				id := seedFeatureCommandTask(t, model.Task{Title: "Default"})
				cfg := config.Default()
				cfg.DefaultFocus.TaskID = id
				args := []string{"start"}
				want := id
				if destination != "default" {
					cfg.DefaultFocus.TaskID = "missing"
				}
				if destination == "explicit" {
					args = append(args, "1")
				}
				if destination == "none" {
					args = append(args, "--none")
					want = ""
				}
				runtime := timerRuntime{now: time.Now, watch: func(string) error { return nil }}
				inv, _, diagnostic := timerTestInvocation(verb, args, cfg)
				code := cmdTimerWithRuntime(inv, runtime)
				st, err := store.Open(context.Background(), "")
				if err != nil {
					t.Fatal(err)
				}
				defer st.Close()
				state, err := st.ReadTimer(context.Background(), time.Now())
				if destination == "missing" {
					if code == exitOK || state.State != nil || !strings.Contains(diagnostic.String(), "default_focus") {
						t.Fatalf("%d %+v %s", code, state, diagnostic)
					}
				} else if code != exitOK || err != nil || state.State == nil || state.State.TaskID != want {
					t.Fatalf("%d %+v %s %v", code, state, diagnostic, err)
				}
			})
		}
	}
	for _, args := range [][]string{{"start", "1", "--none"}, {"start", "--none", "--none"}, {"pause", "--none"}} {
		inv, _, _ := timerTestInvocation("timer", args, config.Default())
		if _, code := parseTimerArguments(inv); code != exitUsage {
			t.Fatalf("accepted %v", args)
		}
	}
}

func TestDefaultFocusUIPreviewRefusesChangedConfigTaskAndSymlink(t *testing.T) {
	for _, change := range []string{"config", "task", "closed", "symlink", "cancel", "none"} {
		t.Run(change, func(t *testing.T) {
			isolate(t)
			id := seedFeatureCommandTask(t, model.Task{Title: "Target"})
			path := writeConfig(t, "# owner\n")
			s := &uiSystem{output: io.Discard}
			defer s.close()
			if _, err := s.Settings(context.Background()); err != nil {
				t.Fatal(err)
			}
			ref := config.FocusReference{TaskID: id}
			if change == "none" {
				ref = config.FocusReference{}
			}
			preview, err := s.PrepareDefaultFocus(context.Background(), ref)
			if err != nil {
				t.Fatal(err)
			}
			switch change {
			case "config":
				err = os.WriteFile(path, []byte("# concurrent owner\n"), 0600)
			case "task":
				_, err = s.store.DB().Exec("UPDATE tasks SET title='Changed'")
			case "closed":
				_, err = s.store.DB().Exec("UPDATE projects SET closed=1")
			case "symlink":
				other := filepath.Join(filepath.Dir(path), "other.toml")
				if err = os.WriteFile(other, []byte("# other\n"), 0600); err != nil {
					t.Fatal(err)
				}
				if err = os.Remove(path); err != nil {
					t.Fatal(err)
				}
				err = os.Symlink(other, path)
			}
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if change == "cancel" {
				cancel()
			}
			_, err = preview.Apply(ctx)
			if change == "none" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil {
				t.Fatal("stale write accepted")
			}
			if _, err := os.Stat(path + ".bak"); !os.IsNotExist(err) {
				t.Fatal("refusal or noop made backup")
			}
		})
	}
}

func TestDefaultFocusDoctorLocalEvidence(t *testing.T) {
	isolate(t)
	id := seedFeatureCommandTask(t, model.Task{Title: "Target"})
	writeConfig(t, "default_focus = 'task:"+id+"'\n")
	clause, _, warn := defaultFocusVerdict(config.FocusReference{TaskID: id})
	if warn || !strings.Contains(clause, "matches one open task") {
		t.Fatal(clause)
	}
	clause, notes, warn := defaultFocusVerdict(config.FocusReference{TaskID: "missing"})
	if !warn || !strings.Contains(clause, "cache") || !strings.Contains(strings.Join(notes, " "), "server removal is not confirmed") {
		t.Fatalf("%s %v", clause, notes)
	}
	result := checkConfig(doctorCache{})
	if !strings.Contains(result.summary, "default_focus matches") {
		t.Fatal(result.summary)
	}
}

func TestDefaultFocusOutputEscapesNamesAndPaths(t *testing.T) {
	isolate(t)
	previous := canAsk
	canAsk = func(*invocation) bool { return true }
	t.Cleanup(func() { canAsk = previous })
	hostile := "\x1b[31m\n\u202e" + strings.Repeat("x", 200)
	id := seedFeatureCommandTask(t, model.Task{Title: hostile})
	seedProjects(t, model.Project{Id: "p1", Name: hostile})
	writeConfig(t, "default_project = 'id:p1'\n")
	code, out, diagnostic := focusConfigCommand(t, strings.NewReader("\n1\n"))
	if code != exitOK {
		t.Fatalf("%d %s %s", code, out, diagnostic)
	}
	_, notes, _ := defaultFocusVerdict(config.FocusReference{TaskID: id})
	for _, text := range append([]string{diagnostic}, notes...) {
		if strings.ContainsAny(text, "\x1b\u202e") {
			t.Fatalf("raw control: %q", text)
		}
		for _, line := range strings.Split(text, "\n") {
			if cli.DisplayWidth(line) > cli.Width {
				t.Fatalf("line too wide: %q", line)
			}
		}
	}
	path := filepath.Join(t.TempDir(), "unsafe\x1b\u202e")
	t.Setenv("XDG_CONFIG_HOME", path)
	if err := os.MkdirAll(filepath.Join(path, "tt"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "tt", "config.toml"), []byte("# owner\n"), 0600); err != nil {
		t.Fatal(err)
	}
	code, out, diagnostic = focusConfigCommand(t, nil, "task:"+id)
	if code != exitOK || strings.ContainsAny(out+diagnostic, "\x1b\u202e") {
		t.Fatalf("unsafe saved backup path: %d %q %q", code, out, diagnostic)
	}
	if err := os.WriteFile(filepath.Join(path, "tt", "config.toml"), []byte("unknown=1"), 0600); err != nil {
		t.Fatal(err)
	}
	code, out, diagnostic = focusConfigCommand(t, nil, "none")
	if code == exitOK || strings.ContainsAny(out+diagnostic, "\x1b\u202e") {
		t.Fatalf("unsafe error path: %d %q %q", code, out, diagnostic)
	}
}
