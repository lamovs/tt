package focus

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/movsar/tt/internal/store"
	"github.com/movsar/tt/internal/webapi"
)

type topicFocusClient struct {
	fingerprint string
	posts, gets int
	created     *webapi.FocusCreate
	lostReply   bool
}

func TestTopicCredentialRenewalReadsBackWithoutRepeatingUncertainPOST(t *testing.T) {
	st, target := topicFocusFixture(t)
	ctx := context.Background()
	old, next := strings.Repeat("a", 64), strings.Repeat("b", 64)
	if _, err := st.DB().ExecContext(ctx, "UPDATE focus_sessions SET topic_credential=? WHERE id=?", old, target.Session.ID); err != nil {
		t.Fatal(err)
	}
	target, _ = st.ReadFocusTarget(ctx, target.Session.ID)
	client := &topicFocusClient{fingerprint: old, lostReply: true}
	factory := func() (TopicClient, error) { return client, nil }
	first := UploadSelected(ctx, st, nil, target, false, false, factory)
	if first.Held != 1 || client.posts != 1 {
		t.Fatalf("first %+v", first)
	}
	before, err := st.FocusUpload(ctx, target.Session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.RecoverBrowserCredential(ctx, old, next); err != nil {
		t.Fatal(err)
	}
	client.fingerprint, client.lostReply = next, false
	fresh, _ := st.ReadFocusTarget(ctx, target.Session.ID)
	if fresh.Session.TopicCredential != old {
		t.Fatal("frozen credential was rewritten")
	}
	second := UploadSelected(ctx, st, nil, fresh, false, false, factory)
	after, err := st.FocusUpload(ctx, target.Session.ID)
	if err != nil || second.Uploaded != 1 || client.posts != 1 || string(before.Request) != string(after.Request) {
		t.Fatalf("renewal replayed or rewrote request: %+v %v", second, err)
	}
}

func (c *topicFocusClient) CredentialFingerprint() string { return c.fingerprint }
func (c *topicFocusClient) ListTopics(context.Context) ([]webapi.Topic, error) {
	return nil, nil
}
func (c *topicFocusClient) CreateTopicFocus(_ context.Context, request webapi.FocusCreate) (webapi.BatchResponse, error) {
	c.posts++
	copy := request
	copy.Tasks = append([]webapi.FocusTopic(nil), request.Tasks...)
	c.created = &copy
	if c.lostReply {
		return webapi.BatchResponse{}, errors.New("synthetic lost reply")
	}
	return webapi.BatchResponse{ID2Etag: map[string]string{request.ID: "etag"}}, nil
}
func (c *topicFocusClient) GetTopicFocus(_ context.Context, id string, focusType int) (webapi.FocusCreate, error) {
	c.gets++
	if c.created == nil || c.created.ID != id || c.created.Type != focusType {
		return webapi.FocusCreate{}, errors.New("not found")
	}
	return *c.created, nil
}

func topicFocusFixture(t *testing.T) (*store.Store, store.FocusTarget) {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	now := time.Date(2026, 9, 11, 12, 0, 0, 123000000, time.UTC)
	started, err := st.StartTimer(ctx, store.TimerStartOptions{FocusType: 1, TopicID: "work-id", TopicName: "Work", TopicCredential: "browser-a"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.StopTimer(ctx, now.Add(5*time.Second)); err != nil {
		t.Fatal(err)
	}
	target, err := st.ReadFocusTarget(ctx, started.State.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	return st, target
}

func TestTopicFocusUploadConfirmsExactFrozenRelation(t *testing.T) {
	st, target := topicFocusFixture(t)
	client := &topicFocusClient{fingerprint: "browser-a"}
	result := UploadSelected(context.Background(), st, nil, target, false, false, func() (TopicClient, error) { return client, nil })
	if result.Uploaded != 1 || result.Held != 0 || client.posts != 1 || client.gets != 1 {
		t.Fatalf("result %+v, posts=%d gets=%d", result, client.posts, client.gets)
	}
	if client.created == nil || len(client.created.Tasks) != 1 || client.created.Tasks[0].TimerID != "work-id" || client.created.Tasks[0].TimerName != "Work" {
		t.Fatalf("created %+v", client.created)
	}
	confirmed, err := st.ReadFocusTarget(context.Background(), target.Session.ID)
	if err != nil || confirmed.Session.SyncedAt == nil || confirmed.Upload == nil || confirmed.Upload.Phase != "confirmed" {
		t.Fatalf("confirmed %+v, %v", confirmed, err)
	}
}

func TestTopicFocusLostReplyOnlyReadsBackAndCredentialMismatchNeverSends(t *testing.T) {
	st, target := topicFocusFixture(t)
	client := &topicFocusClient{fingerprint: "browser-a", lostReply: true}
	factory := func() (TopicClient, error) { return client, nil }
	first := UploadSelected(context.Background(), st, nil, target, false, false, factory)
	if first.Held != 1 || client.posts != 1 {
		t.Fatalf("first %+v, posts=%d", first, client.posts)
	}
	client.lostReply = false
	fresh, _ := st.ReadFocusTarget(context.Background(), target.Session.ID)
	second := UploadSelected(context.Background(), st, nil, fresh, false, false, factory)
	if second.Uploaded != 1 || client.posts != 1 || client.gets == 0 {
		t.Fatalf("second %+v, posts=%d gets=%d", second, client.posts, client.gets)
	}

	st2, target2 := topicFocusFixture(t)
	wrong := &topicFocusClient{fingerprint: "browser-b"}
	result := UploadSelected(context.Background(), st2, nil, target2, false, false, func() (TopicClient, error) { return wrong, nil })
	if result.Held != 1 || wrong.posts != 0 || wrong.gets != 0 {
		t.Fatalf("mismatch %+v, posts=%d gets=%d", result, wrong.posts, wrong.gets)
	}
}
