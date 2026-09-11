package focus

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/movsar/tt/internal/api"
	"github.com/movsar/tt/internal/model"
	"github.com/movsar/tt/internal/store"
)

type focusServer struct {
	mu       sync.Mutex
	posts    int
	status   int
	mismatch bool
	records  []map[string]any
	bodies   []string
}

func (f *focusServer) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if r.Method == http.MethodPost {
		f.posts++
		var request api.FocusCreate
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			panic(err)
		}
		raw, _ := json.Marshal(request)
		f.bodies = append(f.bodies, string(raw))
		if f.status >= 400 && f.status < 500 {
			w.WriteHeader(f.status)
			return
		}
		start, _ := time.Parse(stamp, request.StartTime)
		end, _ := time.Parse(stamp, request.EndTime)
		record := map[string]any{"id": fmt.Sprintf("remote-%d", f.posts), "type": request.Type, "startTime": request.StartTime, "endTime": request.EndTime, "duration": end.Sub(start).Milliseconds() - request.PauseDuration*1000, "pauseDuration": request.PauseDuration, "note": request.Note, "tasks": []any{}}
		if request.TaskID != "" {
			record["tasks"] = []any{map[string]any{"taskId": request.TaskID}}
		} else {
			record["tasks"] = []any{map[string]any{"startTime": request.StartTime, "endTime": request.EndTime}}
			record["relationType"] = []int{0}
		}
		f.records = append(f.records, record)
		if f.status != 0 {
			w.WriteHeader(f.status)
			return
		}
		json.NewEncoder(w).Encode(record)
		return
	}
	if r.URL.Path == "/open/v1/focus" {
		json.NewEncoder(w).Encode(append([]map[string]any{}, f.records...))
		return
	}
	for _, record := range f.records {
		if r.URL.Path == "/open/v1/focus/"+record["id"].(string) {
			copy := map[string]any{}
			for k, v := range record {
				copy[k] = v
			}
			if f.mismatch {
				copy["duration"] = int64(999000)
			}
			json.NewEncoder(w).Encode(copy)
			return
		}
	}
	w.WriteHeader(http.StatusNotFound)
}

func focusFixture(t *testing.T, kind int) (*store.Store, string, *focusServer, *api.Client) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cache.db")
	st, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	now := time.Date(2026, 9, 9, 12, 0, 0, 123000000, time.UTC)
	options := store.TimerStartOptions{FocusType: kind, Note: "exact  note\nsecond line"}
	if kind == 0 {
		options.Planned = 25 * time.Minute
	}
	started, err := st.StartTimer(context.Background(), options, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.PauseTimer(context.Background(), now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ResumeTimer(context.Background(), now.Add(4500*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if _, err := st.StopTimer(context.Background(), now.Add(8*time.Second)); err != nil {
		t.Fatal(err)
	}
	fake := &focusServer{}
	server := httptest.NewServer(http.HandlerFunc(fake.serve))
	t.Cleanup(server.Close)
	client := api.NewClient("synthetic-token", "test", api.WithBaseURL(server.URL), api.WithMaxRetries(1))
	return st, started.State.SessionID, fake, client
}

func TestFocusUploadBothTypesKeepsMeasuredUnits(t *testing.T) {
	for _, kind := range []int{0, 1} {
		t.Run(fmt.Sprint(kind), func(t *testing.T) {
			st, id, fake, client := focusFixture(t, kind)
			result := Upload(context.Background(), st, client, false, false)
			if result.Uploaded != 1 || result.Held != 0 {
				t.Fatalf("upload: %+v", result)
			}
			row, err := st.FocusUpload(context.Background(), id)
			if err != nil || row.Phase != "confirmed" {
				t.Fatalf("row %+v %v", row, err)
			}
			if len(row.Response) == 0 || len(row.Confirmation) == 0 {
				t.Fatal("both POST and GET provenance must be retained")
			}
			var request api.FocusCreate
			json.Unmarshal(row.Request, &request)
			if request.Duration != 5 || request.PauseDuration != 2 || request.Type != api.FocusType(kind) || request.StartTime != "2026-09-09T12:00:00.123+0000" {
				t.Fatalf("wrong conversion %+v", request)
			}
			if result = Upload(context.Background(), st, client, false, false); result.Uploaded != 0 || fake.posts != 1 {
				t.Fatalf("repeat %+v posts=%d", result, fake.posts)
			}
			session, _ := st.TimerSessionByID(context.Background(), id)
			if session.SyncedAt == nil || session.PauseDuration != 2500*time.Millisecond || session.ActiveDuration != 5500*time.Millisecond {
				t.Fatalf("local measurement changed %+v", session)
			}
		})
	}
}

func TestFocusUploadLostReplyRestartOnlyReads(t *testing.T) {
	st, id, fake, client := focusFixture(t, 1)
	fake.status = 500
	first := Upload(context.Background(), st, client, false, false)
	if first.Held != 1 || fake.posts != 1 {
		t.Fatalf("first %+v posts=%d", first, fake.posts)
	}
	before, _ := st.FocusUpload(context.Background(), id)
	path := st.Path()
	st.Close()
	reopened, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	fake.status = 0
	second := Upload(context.Background(), reopened, client, false, false)
	after, _ := reopened.FocusUpload(context.Background(), id)
	if second.Uploaded != 1 || fake.posts != 1 || string(before.Request) != string(after.Request) {
		t.Fatalf("recovery %+v posts=%d", second, fake.posts)
	}
}

func TestFocusUploadAmbiguityAndNoMatchNeverRepost(t *testing.T) {
	for _, count := range []int{0, 2} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			st, _, fake, client := focusFixture(t, 1)
			fake.status = 500
			Upload(context.Background(), st, client, false, false)
			if count == 0 {
				fake.records = nil
			} else {
				second := map[string]any{}
				for k, v := range fake.records[0] {
					second[k] = v
				}
				second["id"] = "another"
				fake.records = append(fake.records, second)
			}
			fake.status = 0
			result := Upload(context.Background(), st, client, false, true)
			if result.Held != 1 || fake.posts != 1 {
				t.Fatalf("unsafe retry %+v posts=%d", result, fake.posts)
			}
		})
	}
}

func TestFocusUploadDefinitiveRejectionNeedsExplicitRetry(t *testing.T) {
	st, id, fake, client := focusFixture(t, 0)
	fake.status = 400
	Upload(context.Background(), st, client, false, false)
	row, _ := st.FocusUpload(context.Background(), id)
	if row.Phase != "rejected" {
		t.Fatal(row.Phase)
	}
	fake.status = 0
	if result := Upload(context.Background(), st, client, false, false); result.Held != 1 || fake.posts != 1 {
		t.Fatalf("implicit retry %+v", result)
	}
	if result := Upload(context.Background(), st, client, false, true); result.Uploaded != 1 || fake.posts != 2 || fake.bodies[0] != fake.bodies[1] {
		t.Fatalf("explicit retry %+v", result)
	}
}

func TestFocusUploadAtomicSettlementFailureKeepsAcceptedIntent(t *testing.T) {
	st, id, fake, client := focusFixture(t, 1)
	_, err := st.DB().Exec(`CREATE TRIGGER refuse_focus_sync BEFORE UPDATE OF synced_at ON focus_sessions BEGIN SELECT RAISE(ABORT, 'synthetic failure'); END;`)
	if err != nil {
		t.Fatal(err)
	}
	result := Upload(context.Background(), st, client, false, false)
	row, _ := st.FocusUpload(context.Background(), id)
	session, _ := st.TimerSessionByID(context.Background(), id)
	if result.Held != 1 || row.Phase != "accepted" || session.SyncedAt != nil {
		t.Fatalf("partial settlement %+v %+v", result, row)
	}
	st.DB().Exec(`DROP TRIGGER refuse_focus_sync`)
	if result = Upload(context.Background(), st, client, false, false); result.Uploaded != 1 || fake.posts != 1 {
		t.Fatalf("settlement retry %+v posts=%d", result, fake.posts)
	}
}

func TestFocusUploadWrongReadbackDoesNotSettle(t *testing.T) {
	st, id, fake, client := focusFixture(t, 1)
	fake.mismatch = true
	result := Upload(context.Background(), st, client, false, false)
	row, _ := st.FocusUpload(context.Background(), id)
	if result.Held != 1 || row.Phase != "accepted" {
		t.Fatalf("mismatch %+v %+v", result, row)
	}
	fake.mismatch = false
	if result = Upload(context.Background(), st, client, false, false); result.Uploaded != 1 || fake.posts != 1 {
		t.Fatalf("retry %+v", result)
	}
}

func TestConcurrentFocusUploadsAuthorizeOnePost(t *testing.T) {
	st, id, fake, client := focusFixture(t, 1)
	other, err := store.Open(context.Background(), st.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	start := make(chan struct{})
	results := make(chan Result, 2)
	for _, handle := range []*store.Store{st, other} {
		go func(handle *store.Store) {
			<-start
			results <- Upload(context.Background(), handle, client, false, false)
		}(handle)
	}
	close(start)
	<-results
	<-results
	Upload(context.Background(), st, client, false, false)
	row, err := st.FocusUpload(context.Background(), id)
	if err != nil || row.Phase != "confirmed" || fake.posts != 1 {
		t.Fatalf("concurrent row=%+v err=%v posts=%d", row, err, fake.posts)
	}
}

func TestFocusUploadLocalTaskWaitsForPromotion(t *testing.T) {
	st, id, fake, client := focusFixture(t, 1)
	ctx := context.Background()
	if err := st.ReplaceProjects(ctx, []model.Project{{Id: "p1", Name: "Test"}}); err != nil {
		t.Fatal(err)
	}
	created, err := st.CreateTask(ctx, model.Task{ProjectId: "p1", Title: "local task"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`UPDATE focus_sessions SET task_id=? WHERE id=?`, created.Id, id); err != nil {
		t.Fatal(err)
	}
	if result := Upload(ctx, st, client, false, false); result.Held != 1 || fake.posts != 0 {
		t.Fatalf("local task %+v", result)
	}

	if err := st.ReplaceLocalID(ctx, created.Id, "server-task"); err != nil {
		t.Fatal(err)
	}
	if result := Upload(ctx, st, client, false, false); result.Uploaded != 1 || fake.posts != 1 {
		t.Fatalf("promoted task %+v", result)
	}
}

func TestFocusConfirmationRequiresExplicitFieldsAndFreshID(t *testing.T) {
	request := api.FocusCreate{Type: 0, StartTime: "2026-09-09T12:00:00.000+0000", EndTime: "2026-09-09T12:00:03.000+0000", Duration: 3}
	good := `{"id":"remote","type":0,"startTime":"2026-09-09T12:00:00.000+0000","endTime":"2026-09-09T12:00:03.000+0000","duration":3000,"pauseDuration":0}`
	for _, change := range []string{"none", "type", "pauseDuration", "duration", "old", "null-note"} {
		raw := good
		switch change {
		case "type":
			raw = strings.Replace(raw, `"type":0,`, "", 1)
		case "pauseDuration":
			raw = strings.Replace(raw, `,"pauseDuration":0`, "", 1)
		case "duration":
			raw = strings.Replace(raw, `"duration":3000`, `"duration":3`, 1)
		case "null-note":
			raw = strings.TrimSuffix(raw, "}") + `,"note":null}`
		}
		var remote api.Focus
		if err := json.Unmarshal([]byte(raw), &remote); err != nil {
			t.Fatal(err)
		}
		prior := []string{}
		if change == "old" {
			prior = []string{"remote"}
		}
		if err := confirm(request, prior, remote); (err == nil) != (change == "none") {
			t.Fatalf("%s confirmation %v", change, err)
		}
	}
}
