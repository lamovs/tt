package focus

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/movsar/tt/internal/store"
)

func TestSelectedFocusUploadNeverWidensAndRejectsStaleRetry(t *testing.T) {
	st, id, server, client := focusFixture(t, 1)
	ctx := context.Background()
	target, err := st.ReadFocusTarget(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Add(-time.Minute)
	later, err := st.StartTimer(ctx, store.TimerStartOptions{FocusType: 1}, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = st.StopTimer(ctx, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	server.status = 400
	if result := UploadSelected(ctx, st, client, target, false, false); result.Held != 1 {
		t.Fatalf("rejection %+v", result)
	}
	rejected, err := st.ReadFocusTarget(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if result := UploadSelected(ctx, st, client, rejected, false, true); result.Held != 1 {
		t.Fatalf("second rejection %+v", result)
	}
	if server.posts != 2 {
		t.Fatalf("posts %d", server.posts)
	}

	if result := UploadSelected(ctx, st, client, rejected, false, true); result.Held != 1 || server.posts != 2 {
		t.Fatalf("stale retry sent %+v posts=%d", result, server.posts)
	}
	fresh, _ := st.ReadFocusTarget(ctx, id)
	server.status = 0
	if result := UploadSelected(ctx, st, client, fresh, false, true); result.Uploaded != 1 {
		t.Fatalf("retry %+v", result)
	}
	if server.posts != 3 || server.bodies[0] != server.bodies[1] || server.bodies[1] != server.bodies[2] {
		t.Fatal("changed frozen request")
	}
	kept, err := st.TimerSessionByID(ctx, later.State.SessionID)
	if err != nil || kept.SyncedAt != nil {
		t.Fatalf("late session joined upload %+v %v", kept, err)
	}
}

func TestSelectedUncertainFocusOnlyReadsAndConcurrentSendIsOnce(t *testing.T) {
	for _, test := range []struct {
		name     string
		lost     bool
		attempts int
	}{{"concurrent", false, 4}, {"uncertain", true, 1}, {"uncertain-concurrent", true, 4}} {
		t.Run(test.name, func(t *testing.T) {
			st, id, server, client := focusFixture(t, 0)
			ctx := context.Background()
			target, err := st.ReadFocusTarget(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if test.lost {
				server.status = 500
			}
			var wg sync.WaitGroup
			for range test.attempts {
				wg.Add(1)
				go func() { defer wg.Done(); UploadSelected(ctx, st, client, target, false, false) }()
			}
			wg.Wait()
			server.mu.Lock()
			posts := server.posts
			server.status = 0
			server.mu.Unlock()
			if posts != 1 {
				t.Fatalf("posts=%d", posts)
			}
			if test.lost {
				held, err := st.ReadFocusTarget(ctx, id)
				if err != nil {
					t.Fatal(err)
				}
				if err = ValidateTarget(held, false, true); err == nil {
					t.Fatal("uncertain retry allowed")
				}
				result := UploadSelected(ctx, st, client, held, false, false)
				if held.Session.SyncedAt == nil {
					if result.Uploaded != 1 || result.Held != 0 {
						t.Fatalf("read back %+v", result)
					}
				} else if test.attempts == 1 || result.Uploaded != 0 || result.Held != 1 {

					t.Fatalf("already confirmed %+v", result)
				}
				confirmed, err := st.ReadFocusTarget(ctx, id)
				if err != nil || confirmed.Session.SyncedAt == nil || confirmed.Upload == nil || confirmed.Upload.Phase != "confirmed" {
					t.Fatalf("missing durable confirmation %+v %v", confirmed, err)
				}
				if server.posts != 1 {
					t.Fatal("uncertain POST replay")
				}
			}
		})
	}
}
