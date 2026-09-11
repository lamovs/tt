package focus

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/movsar/tt/internal/api"
	"github.com/movsar/tt/internal/store"
)

type noteBaselineClient struct {
	Client
	before func()
}

func (c noteBaselineClient) GetFocuses(ctx context.Context, from, to string, kind api.FocusType) ([]api.Focus, error) {
	c.before()
	return c.Client.GetFocuses(ctx, from, to, kind)
}

func TestAutomaticFocusUploadFencesNoteDuringBaseline(t *testing.T) {
	st, id, server, client := focusFixture(t, 1)
	ctx := context.Background()
	changed := false
	wrapped := noteBaselineClient{Client: client, before: func() {
		if changed {
			return
		}
		changed = true
		if _, err := st.DB().ExecContext(ctx, "UPDATE focus_sessions SET note=? WHERE id=?", "changed during baseline", id); err != nil {
			t.Fatal(err)
		}
	}}
	result := Upload(ctx, st, wrapped, false, false)
	if result.Held != 1 || result.Uploaded != 0 || server.posts != 0 {
		t.Fatalf("stale note was sent: %+v posts=%d", result, server.posts)
	}
	if _, err := st.FocusUpload(ctx, id); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("stale request was armed: %v", err)
	}
	result = Upload(ctx, st, client, false, false)
	if result.Uploaded != 1 || server.posts != 1 {
		t.Fatalf("fresh note did not upload: %+v posts=%d", result, server.posts)
	}
	var request api.FocusCreate
	if err := json.Unmarshal([]byte(server.bodies[0]), &request); err != nil || request.Note != "changed during baseline" {
		t.Fatalf("wrong note: %+v %v", request, err)
	}
}

func TestPendingNoteReviewNeverReachesFocusAPI(t *testing.T) {
	st, id, server, client := focusFixture(t, 1)
	ctx := context.Background()
	if _, err := st.DB().ExecContext(ctx, "UPDATE focus_sessions SET note_review_pending=1 WHERE id=?", id); err != nil {
		t.Fatal(err)
	}
	reads := 0
	wrapped := noteBaselineClient{Client: client, before: func() { reads++ }}
	result := Upload(ctx, st, wrapped, false, false)
	if result.Uploaded != 0 || reads != 0 || server.posts != 0 {
		t.Fatalf("pending review reached provider: %+v reads=%d posts=%d", result, reads, server.posts)
	}
	target, err := st.ReadFocusTarget(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	result = UploadSelected(ctx, st, wrapped, target, false, false)
	if result.Held != 1 || reads != 0 || server.posts != 0 {
		t.Fatalf("selected pending review reached provider: %+v reads=%d posts=%d", result, reads, server.posts)
	}
}

func TestResolvedCompletionNoteUsesExistingFocusUpload(t *testing.T) {
	for _, kind := range []int{0, 1} {
		st, _, server, client := focusFixture(t, kind)
		ctx := context.Background()
		if result := Upload(ctx, st, client, false, false); result.Uploaded != 1 {
			t.Fatalf("initial fixture did not settle: %+v", result)
		}
		now := time.Date(2026, 9, 10, 10, 0, 0, 0, time.UTC)
		options := store.TimerStartOptions{FocusType: kind, Note: "start thought", ReviewNote: true}
		if kind == 0 {
			options.Planned = 25 * time.Minute
		}
		started, err := st.StartTimer(ctx, options, now)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.StopTimer(ctx, now.Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
		target, err := st.ReadFocusTarget(ctx, started.State.SessionID)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.ResolveFocusNote(ctx, target, "completion thought", false); err != nil {
			t.Fatal(err)
		}
		result := Upload(ctx, st, client, false, false)
		if result.Uploaded != 1 || server.posts != 2 {
			t.Fatalf("resolved note did not upload: %+v posts=%d", result, server.posts)
		}
		var request api.FocusCreate
		if err := json.Unmarshal([]byte(server.bodies[1]), &request); err != nil || request.Note != "start thought\n\ncompletion thought" || int(request.Type) != kind {
			t.Fatalf("wrong completed note: %+v %v", request, err)
		}
	}
}
