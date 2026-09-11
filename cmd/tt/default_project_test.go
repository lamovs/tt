package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/movsar/tt/internal/cli"
	"github.com/movsar/tt/internal/config"
	"github.com/movsar/tt/internal/model"
	"github.com/movsar/tt/internal/store"
)

func defaultListCommand(t *testing.T, input io.Reader, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := run(context.Background(), append([]string{"config", "default-project"}, args...), input, &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestDefaultListPickerAndStableCreation(t *testing.T) {
	isolate(t)
	previous := canAsk
	canAsk = func(*invocation) bool { return true }
	t.Cleanup(func() { canAsk = previous })
	seedProjects(t, model.Project{Id: "p1", Name: "Same", SortOrder: 1}, model.Project{Id: "p2", Name: "Same", SortOrder: 2}, model.Project{Id: "p3", Name: "Closed", Closed: true})
	original := "# owner\n default_project = 'Old' # keep\n[timer]\nfocus = '40m'\n"
	path := writeConfig(t, original)
	code, out, errOut := defaultListCommand(t, strings.NewReader("2\n"))
	if code != exitOK || !strings.Contains(out, "[id:p2]") || !strings.Contains(errOut, "[id:p1]") || strings.Contains(errOut, "Closed") {
		t.Fatalf("%d %s %s", code, out, errOut)
	}
	got, _ := os.ReadFile(path)
	if string(got) != strings.Replace(original, "'Old'", "'id:p2'", 1) {
		t.Fatalf("config changed unexpectedly: %q", got)
	}
	backup, err := os.ReadFile(path + ".bak")
	if err != nil || string(backup) != original {
		t.Fatalf("backup: %q %v", backup, err)
	}
	info, _ := os.Stat(path)
	if code, out, errOut := defaultListCommand(t, nil, "id:p2"); code != exitOK || !strings.Contains(out, "unchanged") {
		t.Fatalf("noop: %d %s %s", code, out, errOut)
	}
	after, _ := os.Stat(path)
	if !after.ModTime().Equal(info.ModTime()) {
		t.Fatal("noop rewrote config")
	}
	if _, err := os.Lstat(path + ".bak.1"); !os.IsNotExist(err) {
		t.Fatal("noop created another backup")
	}
	seedProjects(t, model.Project{Id: "p1", Name: "Other"}, model.Project{Id: "p2", Name: "Renamed"})
	for _, args := range [][]string{{"add", "Default"}, {"add", "Explicit", "-P", "Other"}} {
		var out, errOut bytes.Buffer
		if code := run(context.Background(), args, nil, &out, &errOut); code != exitOK {
			t.Fatalf("%v: %d %s", args, code, errOut.String())
		}
	}
	for _, task := range cachedTasks(t) {
		want := "p2"
		if task.Title == "Explicit" {
			want = "p1"
		}
		if task.ProjectId != want {
			t.Errorf("%s: project %s want %s", task.Title, task.ProjectId, want)
		}
	}
}

func TestDefaultListRefusalsLeaveConfigAlone(t *testing.T) {
	for _, tc := range []struct {
		name, answer, arg string
		terminal          bool
	}{
		{"cancel", "\n", "", true}, {"eof", "1", "", true},
		{"bad number", "9\n", "", true}, {"pipe", "1\n", "", false},
		{"missing", "", "id:gone", false}, {"closed", "", "id:closed", false},
		{"unknown kind", "", "id:unknown", false}, {"syntax", "", "id:", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolate(t)
			previous := canAsk
			canAsk = func(*invocation) bool { return tc.terminal }
			t.Cleanup(func() { canAsk = previous })
			seedProjects(t, model.Project{Id: "p1", Name: "Work"}, model.Project{Id: "closed", Closed: true}, model.Project{Id: "unknown", Kind: "OTHER"})
			path := writeConfig(t, "default_project = 'Work'\n")
			var args []string
			if tc.arg != "" {
				args = []string{tc.arg}
			}
			code, _, _ := defaultListCommand(t, strings.NewReader(tc.answer), args...)
			if tc.name != "cancel" && tc.name != "eof" && code == exitOK {
				t.Fatal("invalid selection succeeded")
			}
			got, _ := os.ReadFile(path)
			if string(got) != "default_project = 'Work'\n" {
				t.Fatalf("config changed: %q", got)
			}
			if _, err := os.Lstat(path + ".bak"); !os.IsNotExist(err) {
				t.Fatal("unexpected backup")
			}
		})
	}
}

type defaultListChangingReader struct {
	change func()
	answer *strings.Reader
}

func (r *defaultListChangingReader) Read(p []byte) (int, error) {
	if r.change != nil {
		r.change()
		r.change = nil
	}
	return r.answer.Read(p)
}

func TestDefaultListRejectsChangesWhileChoosing(t *testing.T) {
	for _, target := range []string{"config", "list"} {
		t.Run(target, func(t *testing.T) {
			isolate(t)
			previous := canAsk
			canAsk = func(*invocation) bool { return true }
			t.Cleanup(func() { canAsk = previous })
			seedProjects(t, model.Project{Id: "p1", Name: "Work"})
			path := writeConfig(t, "default_project = 'Work'\n")
			reader := &defaultListChangingReader{answer: strings.NewReader("1\n"), change: func() {
				if target == "config" {
					_ = os.WriteFile(path, []byte("default_project = 'Other'\n"), 0o600)
				} else {
					seedProjects(t, model.Project{Id: "p1", Name: "Work", Closed: true})
				}
			}}
			if code, out, errOut := defaultListCommand(t, reader); code == exitOK {
				t.Fatalf("accepted concurrent %s change: %s %s", target, out, errOut)
			}
			if _, err := os.Lstat(path + ".bak"); !os.IsNotExist(err) {
				t.Fatal("unexpected backup")
			}
		})
	}
}

func TestDefaultListMissingCacheAndNewConfig(t *testing.T) {
	isolate(t)
	path, _ := config.Path()
	cache, _ := store.DefaultPath()
	if code, _, _ := defaultListCommand(t, nil, "id:p1"); code == exitOK {
		t.Fatal("accepted absent cache")
	}
	for _, name := range []string{path, cache} {
		if _, err := os.Lstat(name); !os.IsNotExist(err) {
			t.Fatalf("created %s", name)
		}
	}
	seedProjects(t, model.Project{Id: "p1", Name: "Work"})
	if code, out, errOut := defaultListCommand(t, nil, "id:p1"); code != exitOK {
		t.Fatalf("%d %s %s", code, out, errOut)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "default_project = 'id:p1'\n" {
		t.Fatalf("new config: %q", got)
	}
	files, _ := filepath.Glob(path + ".bak*")
	if len(files) != 0 {
		t.Fatal("new config should not have a backup")
	}
}

func TestDoctorDefaultListIDAndUsability(t *testing.T) {
	projects := []model.Project{{Id: "p1", Name: "Renamed"}, {Id: "p2", Name: "Closed", Closed: true}, {Id: "p3", Name: "Unsupported", Kind: "OTHER"}}
	cache := doctorCache{read: true, lists: 3, names: []string{"Renamed", "Closed", "Unsupported"}, projects: projects}
	for _, value := range []string{"id:p1", "name:Renamed", "id:p2", "Closed", "id:gone", "Unsupported"} {
		_, notes, warn := defaultProjectVerdict(cache, value, originSilentFile)
		want := value != "id:p1" && value != "name:Renamed"
		if warn != want {
			t.Errorf("%s: warn %v notes %v", value, warn, notes)
		}
	}
	clause, _, warn := defaultProjectVerdict(doctorCache{read: true}, "id:p1", originSilentFile)
	if clause != "default_project not checked" || warn {
		t.Fatalf("empty cache: %s %v", clause, warn)
	}
}

func TestDefaultListOutputEscapesNamesAndPaths(t *testing.T) {
	isolate(t)
	previous := canAsk
	canAsk = func(*invocation) bool { return true }
	t.Cleanup(func() { canAsk = previous })
	hostile := "\x1b[31m\n\u202e" + strings.Repeat("x", 200)
	seedProjects(t, model.Project{Id: "p1", Name: hostile}, model.Project{Id: "bad\x1bID", Name: "Bad ID"})
	code, out, errOut := defaultListCommand(t, strings.NewReader("1\n"))
	if code != exitOK {
		t.Fatalf("%d %s %s", code, out, errOut)
	}
	for _, text := range []string{out, errOut} {
		for _, raw := range []string{"\x1b", "\u202e", "Bad ID"} {
			if strings.Contains(text, raw) {
				t.Errorf("unsafe output: %q", text)
			}
		}
		for _, line := range strings.Split(text, "\n") {
			if cli.DisplayWidth(line) > cli.Width {
				t.Errorf("line exceeds width: %q", line)
			}
		}
	}
	cache := doctorCache{read: true, lists: 1, names: []string{hostile}, projects: []model.Project{{Id: "p1", Name: hostile}}}
	_, notes, _ := defaultProjectVerdict(cache, "id:p1", originSilentFile)
	for _, line := range notes {
		if strings.ContainsAny(line, "\x1b\u202e") || cli.DisplayWidth(line) > cli.Width {
			t.Errorf("unsafe doctor output: %q", line)
		}
	}
	var errOutBuf bytes.Buffer
	inv := &invocation{stderr: &errOutBuf}
	defaultProjectFailure(inv, &os.PathError{Op: "open", Path: hostile, Err: os.ErrPermission})
	if strings.ContainsAny(errOutBuf.String(), "\x1b\u202e") {
		t.Fatal("raw error path")
	}
}

func TestDefaultListNewConfigNeverReplacesConcurrentFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	err := writeNewDefaultConfig(path, []byte("default_project = 'id:p1'\n"), func() error {
		return os.WriteFile(path, []byte("# concurrent owner file\n"), 0o600)
	})
	if err == nil {
		t.Fatal("replaced a concurrent file")
	}
	got, _ := os.ReadFile(path)
	if string(got) != "# concurrent owner file\n" {
		t.Fatalf("%q", got)
	}
}

func TestDefaultListPromptCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	cancel()
	if _, err := readDefaultProjectAnswer(ctx, reader); err != context.Canceled {
		t.Fatalf("%v", err)
	}
}
