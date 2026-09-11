package editor

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestCommand(t *testing.T) {
	for _, tc := range []struct {
		input string
		want  []string
	}{
		{`code --wait`, []string{"code", "--wait"}},
		{`'/path with spaces/editor' "a b" ''`, []string{"/path with spaces/editor", "a b", ""}},
		{`editor $(touch /tmp/not-run); $HOME *.md`, []string{"editor", "$(touch", "/tmp/not-run);", "$HOME", "*.md"}},
		{`a\ b c\ d`, []string{"a b", "c d"}},
	} {
		got, err := Command(tc.input)
		if err != nil || !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("Command(%q) = %q, %v", tc.input, got, err)
		}
	}
	for _, value := range []string{"", "''", "editor 'broken", "editor " + string('\\'), "editor\nnext"} {
		if _, err := Command(value); err == nil {
			t.Fatalf("accepted %q", value)
		}
	}
}

func TestEditorProcess(t *testing.T) {
	mode := os.Getenv("TT_EDITOR_HELPER_MODE")
	if mode == "" {
		return
	}
	path := os.Args[len(os.Args)-1]
	if info, err := os.Stat(filepath.Dir(path)); err != nil || info.Mode().Perm() != 0700 {
		os.Exit(20)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0600 {
		os.Exit(21)
	}
	switch mode {
	case "unchanged":
	case "wait":
		time.Sleep(time.Minute)
	case "edit", "fail":
		if os.WriteFile(path, []byte("body\n\n  exact \n"), 0600) != nil {
			os.Exit(22)
		}
		if mode == "fail" {
			os.Exit(3)
		}
	case "symlink":
		if os.Remove(path) != nil || os.Symlink("missing", path) != nil {
			os.Exit(23)
		}
	case "fifo":
		if os.Remove(path) != nil || unix.Mkfifo(path, 0600) != nil {
			os.Exit(24)
		}
	case "large":
		if os.Truncate(path, MaxSize+1) != nil {
			os.Exit(25)
		}
	case "atomic":
		if os.WriteFile(path+".new", []byte("atomic\n"), 0644) != nil || os.Rename(path+".new", path) != nil {
			os.Exit(26)
		}
	case "delete":
		if os.Remove(path) != nil {
			os.Exit(27)
		}
	default:
		os.Exit(28)
	}
	os.Exit(0)
}

func TestOpenEditorDraftLifecycle(t *testing.T) {
	for _, mode := range []string{"unchanged", "edit", "fail", "symlink", "fifo", "large", "atomic", "delete"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("TT_EDITOR_HELPER_MODE", mode)
			t.Setenv("VISUAL", strconv.Quote(os.Args[0])+" -test.run=^TestEditorProcess$ --")
			t.Setenv("EDITOR", "must-not-run")
			draft, err := Open(context.Background(), []byte("initial\n"), strings.NewReader(""), io.Discard, io.Discard)
			wantErr := mode != "unchanged" && mode != "edit" && mode != "atomic"
			if (err != nil) != wantErr || draft == nil {
				t.Fatalf("Open = %#v, %v", draft, err)
			}
			t.Cleanup(func() { _ = draft.Discard() })
			if _, statErr := os.Stat(draft.dir); statErr != nil {
				t.Fatal("draft directory was removed before caller decided")
			}
			if mode == "unchanged" && draft.Changed {
				t.Fatal("unchanged marked changed")
			}
			if mode == "edit" && string(draft.Data) != "body\n\n  exact \n" {
				t.Fatalf("body = %q", draft.Data)
			}
			if mode == "fail" {
				data, _ := os.ReadFile(draft.Path)
				if string(data) != "body\n\n  exact \n" {
					t.Fatal("failed editor draft not preserved")
				}
			}
			if mode == "atomic" {
				info, _ := os.Stat(draft.Path)
				if string(draft.Data) != "atomic\n" || info.Mode().Perm() != 0600 {
					t.Fatal("atomic save not read or protected")
				}
			}
		})
	}
}

func TestEditorSelectionAndCancellation(t *testing.T) {
	t.Setenv("VISUAL", "")
	t.Setenv("EDITOR", strconv.Quote(os.Args[0])+" -test.run=^TestEditorProcess$ --")
	t.Setenv("TT_EDITOR_HELPER_MODE", "unchanged")
	draft, err := Open(context.Background(), []byte("initial"), nil, io.Discard, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	_ = draft.Discard()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if draft, err := Open(ctx, nil, nil, io.Discard, io.Discard); err == nil || draft != nil {
		t.Fatal("cancelled context opened editor")
	}
	t.Setenv("TT_EDITOR_HELPER_MODE", "wait")
	ctx, cancel = context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	draft, err = Open(ctx, []byte("recover this"), nil, io.Discard, io.Discard)
	if err == nil || draft == nil {
		t.Fatal("running editor cancellation lost its draft")
	}
	defer draft.Discard()
	data, readErr := os.ReadFile(draft.Path)
	if readErr != nil || string(data) != "recover this" {
		t.Fatal("cancelled draft differs")
	}
}
