package app

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/movsar/tt/internal/model"
	"github.com/movsar/tt/internal/store"
	"github.com/movsar/tt/internal/taskdoc"
)

func TestDocumentEditorProcess(t *testing.T) {
	if os.Getenv("TT_DOCUMENT_HELPER") != "1" {
		return
	}
	path := os.Args[len(os.Args)-1]
	initial, err := os.ReadFile(path)
	if err != nil {
		os.Exit(2)
	}
	if err = os.WriteFile(os.Getenv("TT_DOCUMENT_CAPTURE"), initial, 0600); err != nil {
		os.Exit(3)
	}
	if ready := os.Getenv("TT_DOCUMENT_READY"); ready != "" {
		if os.WriteFile(ready, []byte("ready"), 0600) != nil {
			os.Exit(4)
		}
		for {
			if _, err := os.Stat(ready + ".release"); err == nil {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	if replacement := os.Getenv("TT_DOCUMENT_REPLACEMENT"); replacement != "" {
		data, err := os.ReadFile(replacement)
		if err != nil {
			os.Exit(5)
		}

		if os.WriteFile(path+".new", data, 0644) != nil || os.Rename(path+".new", path) != nil {
			os.Exit(6)
		}
	}
	if os.Getenv("TT_DOCUMENT_FAIL") == "1" {
		os.Exit(7)
	}
	os.Exit(0)
}

func documentEditor(t *testing.T, replacement []byte) (capture, directory string) {
	t.Helper()
	directory = t.TempDir()
	capture = filepath.Join(directory, "capture.md")
	t.Setenv("VISUAL", strconv.Quote(os.Args[0])+" -test.run=^TestDocumentEditorProcess$ --")
	t.Setenv("EDITOR", "missing-editor-never-used")
	t.Setenv("TT_DOCUMENT_HELPER", "1")
	t.Setenv("TT_DOCUMENT_CAPTURE", capture)
	t.Setenv("TT_DOCUMENT_READY", "")
	t.Setenv("TT_DOCUMENT_FAIL", "")
	t.Setenv("TT_DOCUMENT_REPLACEMENT", "")
	if replacement != nil {
		path := filepath.Join(directory, "replacement.md")
		if err := os.WriteFile(path, replacement, 0600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("TT_DOCUMENT_REPLACEMENT", path)
	}
	return capture, directory
}

func documentBytes(t *testing.T, task model.Task) []byte {
	t.Helper()
	data, err := taskdoc.Encode(task, model.HintsNone)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestDocumentCreationExactBodyAndFailures(t *testing.T) {
	for _, mode := range []string{"note", "text", "unchanged", "comments", "invalid", "failed", "identity", "kind"} {
		t.Run(mode, func(t *testing.T) {
			a, st := actionStore(t)
			seed := model.Task{ProjectId: "work", Kind: "TEXT"}
			next := seed
			next.Title, next.Kind, next.Content = "Saved note", "NOTE", "\r\n# Note\r\n  exact  \n- [x] body only\n\n"
			data := documentBytes(t, next)
			switch mode {
			case "text":
				next.Kind = "TEXT"
				data = documentBytes(t, next)
			case "unchanged":
				data = nil
			case "comments":
				data = bytes.Replace(documentBytes(t, seed), []byte("+++\n"), []byte("+++\n# only a comment\n"), 1)
			case "invalid":
				data = []byte("invalid document")
			case "identity":
				next.Id = "foreign"
				data = documentBytes(t, next)
			case "kind":
				next.Kind = "CHECKLIST"
				data = documentBytes(t, next)
			}
			_, dir := documentEditor(t, data)
			if mode == "failed" {
				t.Setenv("TT_DOCUMENT_FAIL", "1")
			}
			before := dumpCache(t, st)
			result, err := a.Document(context.Background(), DocumentRequest{Seed: seed}, nil, io.Discard, io.Discard)
			success := mode == "note" || mode == "text"
			canceled := mode == "unchanged" || mode == "comments"
			if success {
				if err != nil || !result.Outcome.Changed || result.DraftPath != "" {
					t.Fatalf("result %+v %v", result, err)
				}
				got := readTask(t, st, result.Outcome.Task.Id)
				if got.Kind != next.Kind || got.Content != next.Content || len(got.Items) != 0 {
					t.Fatalf("lost body/kind: %+v", got)
				}
				undo, err := a.PreviewUndo(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				if _, err = a.Undo(context.Background(), undo); err != nil {
					t.Fatal(err)
				}
				if tasks, err := st.Tasks(context.Background(), store.TaskFilter{}); err != nil || len(tasks) != 0 {
					t.Fatal("global undo did not remove creation")
				}
			} else {
				if before != dumpCache(t, st) || result.Outcome.Changed {
					t.Fatal("non-save wrote the cache")
				}
				if canceled {
					if err != nil || !result.Canceled || result.DraftPath != "" {
						t.Fatalf("cancel %+v %v", result, err)
					}
				} else {
					if err == nil || result.DraftPath == "" {
						t.Fatalf("failed draft lost: %+v %v", result, err)
					}
					defer os.RemoveAll(filepath.Dir(result.DraftPath))
					got, err := os.ReadFile(result.DraftPath)
					if err != nil || !bytes.Equal(got, data) {
						t.Fatal("recovery bytes lost")
					}
					info, err := os.Stat(filepath.Dir(result.DraftPath))
					if err != nil || info.Mode().Perm() != 0700 {
						t.Fatal("draft directory not private")
					}
				}
			}
			_ = dir
		})
	}
}

func TestDocumentPreservesChecklistScheduleRawAndUnsavedForm(t *testing.T) {
	a, st := actionStore(t)
	ctx := context.Background()
	task, err := st.CreateTask(ctx, model.Task{ProjectId: "work", Title: "Initial", Kind: "CHECKLIST",
		Content: "old\r\n", Items: []model.Item{{Title: "keep", SortOrder: 17}}})
	if err != nil {
		t.Fatal(err)
	}
	stamp := "2026-09-09T12:13:14.123+09:00"
	raw := `{"vendor":{"exact":" keep  spaces "},"repeatFrom":2}`
	if _, err = st.DB().Exec(`UPDATE tasks SET due_date=?,start_date=?,time_zone=?,repeat_flag=?,reminders=?,raw=? WHERE id=?`,
		stamp, stamp, "Asia/Tokyo", "unsupported repeat", `["legacy raw","legacy raw"]`, raw, task.Id); err != nil {
		t.Fatal(err)
	}
	original := readTask(t, st, task.Id)
	seed := original
	seed.Title = "Unsaved form title"
	next := seed
	next.Content = "\n# Edited\r\n  exact  \n- [ ] remains body"
	documentEditor(t, documentBytes(t, next))
	result, err := a.Document(ctx, DocumentRequest{Original: &original, Seed: seed}, nil, io.Discard, io.Discard)
	if err != nil || !result.Outcome.Changed {
		t.Fatalf("edit %+v %v", result, err)
	}
	got := readTask(t, st, task.Id)
	expected := original
	expected.Title, expected.Content, expected.ModifiedTime = next.Title, next.Content, got.ModifiedTime
	if !reflect.DeepEqual(got, expected) {
		t.Fatalf("untouched fields changed\ngot %+v\nwant %+v", got, expected)
	}
	var gotRaw string
	if err = st.DB().QueryRow(`SELECT raw FROM tasks WHERE id=?`, task.Id).Scan(&gotRaw); err != nil || gotRaw != raw {
		t.Fatal("raw changed")
	}
	undo, err := a.PreviewUndo(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = a.Undo(ctx, undo); err != nil {
		t.Fatal(err)
	}
	got = readTask(t, st, task.Id)
	if got.Title != original.Title || got.Content != original.Content {
		t.Fatal("undo did not restore original form fields")
	}
}

func TestDocumentConcurrentWritePromotionAndCancellation(t *testing.T) {
	for _, mode := range []string{"write", "comments", "promotion", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			a, st := actionStore(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			created, err := a.Create(ctx, Draft{ProjectID: "work", Title: "Original"})
			if err != nil {
				t.Fatal(err)
			}
			original := created.Task
			next := original
			next.Title = "Editor change"
			data := documentBytes(t, next)
			if mode == "comments" {
				data = bytes.Replace(documentBytes(t, original), []byte("+++\n"), []byte("+++\n# comment only\n"), 1)
			}
			_, dir := documentEditor(t, data)
			ready := filepath.Join(dir, "ready")
			t.Setenv("TT_DOCUMENT_READY", ready)
			type response struct {
				result DocumentResult
				err    error
			}
			done := make(chan response, 1)
			go func() {
				result, err := a.Document(ctx, DocumentRequest{Original: &original, Seed: original}, nil, io.Discard, io.Discard)
				done <- response{result, err}
			}()
			until := time.Now().Add(10 * time.Second)
			for {
				if _, err = os.Stat(ready); err == nil {
					break
				}
				if time.Now().After(until) {
					t.Fatal("editor did not start")
				}
				time.Sleep(5 * time.Millisecond)
			}
			other, err := store.Open(context.Background(), st.Path())
			if err != nil {
				t.Fatal(err)
			}
			defer other.Close()

			writeCtx, stop := context.WithTimeout(context.Background(), time.Second)
			defer stop()
			if _, err = other.CreateTask(writeCtx, model.Task{ProjectId: "work", Title: "Concurrent writer", Kind: "TEXT"}); err != nil {
				t.Fatalf("editor held DB lock: %v", err)
			}
			switch mode {
			case "write", "comments":
				_, err = other.UpdateTask(writeCtx, original.Id, model.TaskEdit{Priority: model.Ptr(model.PriorityHigh)})
			case "promotion":
				err = other.ReplaceLocalID(writeCtx, original.Id, "promoted-id")
			case "cancel":
				cancel()
			}
			if err != nil {
				t.Fatal(err)
			}
			before := dumpCache(t, st)
			if mode != "cancel" {
				if err = os.WriteFile(ready+".release", nil, 0600); err != nil {
					t.Fatal(err)
				}
			}
			reply := <-done
			if reply.err == nil || reply.result.DraftPath == "" || reply.result.Outcome.Changed {
				t.Fatalf("stale editor %+v %v", reply.result, reply.err)
			}
			defer os.RemoveAll(filepath.Dir(reply.result.DraftPath))
			if before != dumpCache(t, st) {
				t.Fatal("editor wrote after conflict/cancel")
			}
			if (mode == "write" || mode == "comments") && !errors.Is(reply.err, store.ErrTaskChanged) {
				t.Fatal(reply.err)
			}
		})
	}
}
