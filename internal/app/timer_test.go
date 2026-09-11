package app

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/movsar/tt/internal/api"
	"github.com/movsar/tt/internal/config"
	"github.com/movsar/tt/internal/focus"
	"github.com/movsar/tt/internal/store"
	"github.com/movsar/tt/internal/webapi"
)

type timerTopicClient struct{ topics []webapi.Topic }

func (c *timerTopicClient) CredentialFingerprint() string { return "browser-a" }
func (c *timerTopicClient) ListTopics(context.Context) ([]webapi.Topic, error) {
	return c.topics, nil
}
func (c *timerTopicClient) CreateTopicFocus(context.Context, webapi.FocusCreate) (webapi.BatchResponse, error) {
	return webapi.BatchResponse{}, errors.New("write not expected")
}
func (c *timerTopicClient) GetTopicFocus(context.Context, string, int) (webapi.FocusCreate, error) {
	return webapi.FocusCreate{}, errors.New("read-back not expected")
}

func TestTimerServiceReadCompletionAndNotifierFailure(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg := config.Default()
	cfg.Timer.OnEnd = "synthetic"
	cfg.FocusUpload.Enabled = true
	var notified, watched, clients, wakes atomic.Int32
	timers := NewTimers(st, cfg, func() (*api.Client, error) { clients.Add(1); return nil, errors.New("no network") },
		func(string) error { watched.Add(1); return nil },
		func(context.Context, store.TimerSession) error {
			notified.Add(1)
			return errors.New("synthetic notifier failed")
		}).WithCompletionWake(func() error { wakes.Add(1); return nil })
	now := time.Now().Add(-time.Minute)
	started, err := st.StartTimer(ctx, store.TimerStartOptions{Planned: time.Second}, now)
	if err != nil {
		t.Fatal(err)
	}
	data, err := timers.Read(ctx, time.Now())
	if err != nil || data.Active.State == nil || notified.Load() != 0 || clients.Load() != 0 {
		t.Fatalf("read caused effects %+v %v", data, err)
	}
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := timers.Control(ctx, "status", store.TimerStartOptions{}, nil)
			if err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	history, err := st.TimerHistory(ctx, 20)
	if err != nil || len(history) != 1 || history[0].ID != started.State.SessionID || history[0].ActiveDuration != time.Second {
		t.Fatalf("completion %+v %v", history, err)
	}
	if notified.Load() != 1 || clients.Load() != 0 || wakes.Load() != 1 {
		t.Fatalf("effects notify=%d clients=%d wakes=%d", notified.Load(), clients.Load(), wakes.Load())
	}
	data, err = timers.Read(ctx, time.Now())
	if err != nil || len(data.History) != 1 || data.Active.State != nil {
		t.Fatalf("read %+v %v", data, err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err = timers.Control(canceled, "start", store.TimerStartOptions{FocusType: 1}, &data.Active.Guard); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation %v", err)
	}
	active, err := st.ReadTimer(ctx, time.Now())
	if err != nil || active.State != nil {
		t.Fatal("canceled start mutated")
	}
}

func TestTimerServiceRefreshesAndFiltersTopics(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	client := &timerTopicClient{topics: []webapi.Topic{
		{ID: "work-id", Name: "Work", Status: 0, Raw: json.RawMessage(`{"id":"work-id","name":"Work"}`)},
		{ID: "old-id", Name: "Old", Status: 1, Raw: json.RawMessage(`{"id":"old-id","name":"Old"}`)},
	}}
	timers := NewTimers(st, config.Default(), func() (*api.Client, error) { return nil, errors.New("Open API not expected") }, nil, nil).
		WithTopicClient(func() (focus.TopicClient, error) { return client, nil })
	count, err := timers.RefreshTopics(ctx)
	if err != nil || count != 2 {
		t.Fatalf("refresh count=%d err=%v", count, err)
	}
	data, err := timers.Read(ctx, time.Now())
	if err != nil || len(data.Topics) != 1 || data.Topics[0].ID != "work-id" || data.Topics[0].CredentialFingerprint != "browser-a" {
		t.Fatalf("topics %+v err=%v", data.Topics, err)
	}
}
