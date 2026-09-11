package tui

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/movsar/tt/internal/app"
	"github.com/movsar/tt/internal/store"
)

type fakeResourceActions struct {
	listing                              app.ResourceListing
	queries                              []app.ResourceQuery
	remote                               []bool
	prepares, applies, cancels, recovers int
	mutation                             store.EntityMutation
	operations                           []store.EntityOperation
	seq, revision                        int64
	err                                  error
}

func (f *fakeResourceActions) List(_ context.Context, q app.ResourceQuery, remote bool) (app.ResourceListing, error) {
	f.queries = append(f.queries, q)
	f.remote = append(f.remote, remote)
	return f.listing, f.err
}
func (f *fakeResourceActions) Prepare(_ context.Context, m store.EntityMutation) (app.ResourcePreview, error) {
	f.prepares++
	f.mutation = m
	if m.Ref.Key == "" {
		m.Ref.Key = "local:new"
	}
	return app.ResourcePreview{ID: "preview-1", Preview: store.EntityMutationPreview{Mutation: m}, Undo: "cancel before sending"}, f.err
}
func (f *fakeResourceActions) Apply(_ context.Context, id, kind, action string) (store.EntityMutationOutcome, error) {
	f.applies++
	entity := store.ResourceEntity{Ref: store.EntityRef{Kind: kind, Key: "local:new"}, Data: f.mutation.Patch, Dirty: true, Revision: 1}
	f.listing.Entities = []store.ResourceEntity{entity}
	return store.EntityMutationOutcome{Entity: entity, OperationSeq: 17}, f.err
}
func (f *fakeResourceActions) Queue(context.Context) ([]store.EntityOperation, error) {
	return f.operations, f.err
}
func (f *fakeResourceActions) Cancel(_ context.Context, seq, revision int64) error {
	f.cancels++
	f.seq, f.revision = seq, revision
	return f.err
}
func (f *fakeResourceActions) Recover(_ context.Context, seq, revision int64) error {
	f.recovers++
	f.seq, f.revision = seq, revision
	return f.err
}

func resourceModel(t *testing.T, kind string, entities ...store.ResourceEntity) (browserModel, *fakeResourceActions) {
	t.Helper()
	service := &fakeResourceActions{listing: app.ResourceListing{Entities: entities, Meta: app.ResultMeta{Source: "local", Completeness: "unknown"}}}
	m := newModel(context.Background(), &fakeQueries{}, Options{Resources: service})
	t.Cleanup(m.cancelLoad)
	m.loading = false
	m, cmd := m.openResources(app.ResourceQuery{Kind: kind})
	if len(service.queries) != 0 {
		t.Fatal("opening executed I/O before the command")
	}
	m = settle(t, m, cmd)
	return m, service
}

func TestResourceWorkspaceLocalDefaultExplicitRefreshAndStableIdentity(t *testing.T) {
	entity := store.ResourceEntity{Ref: store.EntityRef{Kind: "habit", Key: "h1"}, ServerID: "h1", Data: json.RawMessage(`{"id":"h1","name":"Same"}`)}
	m, service := resourceModel(t, "habit", entity)
	if len(service.remote) != 1 || service.remote[0] {
		t.Fatal("workspace opened remotely")
	}
	if m.resource.key != "h1" {
		t.Fatal("missing stable initial identity")
	}
	calls := len(service.queries)
	for i := 0; i < 20; i++ {
		_ = m.View()
	}
	if len(service.queries) != calls {
		t.Fatal("View performed I/O")
	}
	next, cmd := press(m, 'R', 0)
	if len(service.queries) != calls {
		t.Fatal("key handling executed I/O")
	}
	m = settle(t, next, cmd)
	if !service.remote[len(service.remote)-1] {
		t.Fatal("explicit remote refresh was not sent")
	}
	service.listing.Entities = []store.ResourceEntity{{Ref: store.EntityRef{Kind: "habit", Key: "h2"}, ServerID: "h2", Data: json.RawMessage(`{"id":"h2","name":"Same"}`)}}
	m, cmd = press(m, 'r', 0)
	m = settle(t, m, cmd)
	if m.resource.key != "" || !m.resource.lost {
		t.Fatal("lost resource was replaced by same-name row")
	}
	m, _ = press(m, 'j', 0)
	if m.resource.key != "h2" {
		t.Fatal("explicit selection did not select new row")
	}
}

func TestResourceGenerationFencesLateCollectionAndPreview(t *testing.T) {
	m, service := resourceModel(t, "folder")
	m, late := m.loadResources(false)
	m, current := m.openResources(app.ResourceQuery{Kind: "tag"})
	m = settle(t, m, current)
	m = settle(t, m, late)
	if m.resource.query.Kind != "tag" {
		t.Fatal("late collection changed workspace")
	}
	m, _ = press(m, 'a', 0)
	m.resource.form.fields[0].input.set("tag")
	m, cmd := press(m, 's', tea.ModCtrl)
	msg := cmd().(resourcePrepared)
	stale := msg
	stale.target = "different"
	before := service.applies
	m, _ = step(m, stale)
	if m.resource.preview != nil || service.applies != before {
		t.Fatal("stale target preview accepted")
	}
	m, _ = step(m, msg)
	if m.resource.preview == nil {
		t.Fatal("current preview missing")
	}
}

func TestResourceFormQueuesExactlyReviewedPatchOnce(t *testing.T) {
	m, service := resourceModel(t, "habit", store.ResourceEntity{Ref: store.EntityRef{Kind: "habit", Key: "h"}, ServerID: "h", Revision: 3, Data: json.RawMessage(`{"id":"h","name":"Read","goal":5,"future":true}`)})
	m, _ = press(m, 'e', 0)
	m.resource.form.fields[2].input.set("0")
	m, cmd := press(m, 's', tea.ModCtrl)
	if service.prepares != 0 || service.applies != 0 {
		t.Fatal("form key wrote synchronously")
	}
	msg := cmd()
	_ = cmd()
	if service.prepares != 1 || string(service.mutation.Patch) != `{"goal":0}` || service.mutation.Ref.Key != "h" {
		t.Fatalf("preview=%+v calls=%d", service.mutation, service.prepares)
	}
	m, _ = step(m, msg)
	m, apply := press(m, tea.KeyEnter, 0)
	if service.applies != 0 {
		t.Fatal("confirmation wrote synchronously")
	}
	result := apply()
	_ = apply()
	if service.applies != 1 {
		t.Fatal("repeated command re-applied mutation")
	}
	m, load := step(m, result)
	m = settle(t, m, load)
	if m.resource.form != nil || m.resource.preview != nil || !strings.Contains(m.notice, "17") {
		t.Fatal("local queue outcome missing")
	}
}

func TestResourceFormHelpAndInterruptProtectDraft(t *testing.T) {
	m, _ := resourceModel(t, "folder")
	m, _ = press(m, 'a', 0)
	m, _ = press(m, 'q', 0)
	m, _ = press(m, '?', 0)
	if string(m.resource.form.fields[0].input.value) != "q?" || m.help.ShowAll {
		t.Fatal("form shortcuts escaped text input")
	}
	m, _ = press(m, tea.KeyF1, 0)
	next, cmd := press(m, 'q', 0)
	if cmd != nil || next.resource.form == nil {
		t.Fatal("help quit discarded a resource draft")
	}
	m, _ = press(next, tea.KeyEscape, 0)
	m, cmd = press(m, 'c', tea.ModCtrl)
	if cmd != nil || !m.resource.form.discard || !m.resource.form.quitAfter {
		t.Fatal("interrupt bypassed unsaved choice")
	}
	m, _ = press(m, tea.KeyEscape, 0)
	if m.resource.form.discard {
		t.Fatal("could not return to draft")
	}
}

func TestResourceQueueConfirmationFreezesSequenceAndRevision(t *testing.T) {
	m, service := resourceModel(t, "folder")
	service.operations = []store.EntityOperation{{Item: store.OutboxItem{Seq: 21, State: store.OutboxFailed}, Mutation: store.EntityMutation{Ref: store.EntityRef{Kind: "folder", Key: "g"}}, Revision: 4, Phase: "uncertain"}}
	m, cmd := press(m, 'Q', 0)
	m = settle(t, m, cmd)
	m, _ = press(m, 'f', 0)
	if m.resource.queue.confirmation.seq != 21 || m.resource.queue.confirmation.revision != 4 {
		t.Fatal("missing exact operation preview")
	}
	m.resource.queue.operations[0].Revision = 9
	m, cmd = press(m, tea.KeyEnter, 0)
	msg := cmd()
	_ = cmd()
	if service.recovers != 1 || service.seq != 21 || service.revision != 4 {
		t.Fatal("recovery retargeted or repeated")
	}
	m, load := step(m, msg)
	m = settle(t, m, load)
	service.err = store.ErrEntityUncertain
	m, _ = press(m, 'c', 0)
	m, cmd = press(m, tea.KeyEnter, 0)
	m = settle(t, m, cmd)
	if !strings.Contains(m.resource.err, "read-only") || m.busy {
		t.Fatal("unsafe cancellation refusal hidden")
	}
}

func TestResourceWorkspaceEscapesAndFitsEveryViewport(t *testing.T) {
	raw, _ := json.Marshal(map[string]any{"id": "g", "name": "title\x1b]52;c;payload\a", "future": strings.Repeat("界", 80)})
	m, service := resourceModel(t, "folder", store.ResourceEntity{Ref: store.EntityRef{Kind: "folder", Key: "g"}, ServerID: "g", Data: raw})
	for _, size := range [][2]int{{32, 10}, {80, 24}, {160, 40}} {
		m.width, m.height = size[0], size[1]
		for _, detail := range []bool{false, true} {
			m.resource.detail = detail
			view := m.View().Content
			if strings.Contains(view, "\x1b]52") || strings.ContainsRune(view, '\a') {
				t.Fatal("resource emitted raw terminal control")
			}
			lines := strings.Split(view, "\n")
			if len(lines) > m.height {
				t.Fatal("height exceeded")
			}
			for _, line := range lines {
				if ansi.StringWidth(line) != m.width {
					t.Fatalf("width=%d want%d line=%q", ansi.StringWidth(line), m.width, line)
				}
			}
		}
	}
	if len(service.queries) != 1 {
		t.Fatal("render made resource reads")
	}
}

func TestCheckinBaselineRefreshIsExplicitAndDated(t *testing.T) {
	m, service := resourceModel(t, "habit", store.ResourceEntity{Ref: store.EntityRef{Kind: "habit", Key: "h"}, ServerID: "h", Data: json.RawMessage(`{"id":"h","name":"Habit","goal":8}`)})
	m, _ = press(m, 'c', 0)
	m.resource.form.fields[0].input.set("20260911")
	m.resource.form.fields[1].input.set("0")
	before := len(service.queries)
	m, cmd := press(m, 'r', tea.ModCtrl)
	if len(service.queries) != before {
		t.Fatal("baseline key sent synchronously")
	}
	m = settle(t, m, cmd)
	q := service.queries[len(service.queries)-1]
	if q.Kind != "checkin" || q.HabitID != "h" || q.From != "20260911" || q.To != "20260911" || !service.remote[len(service.remote)-1] {
		t.Fatalf("baseline query: %+v", q)
	}
	if service.prepares != 0 || service.applies != 0 {
		t.Fatal("baseline refresh created check-in")
	}
	mutation, err := m.resource.form.mutation()
	if err != nil || mutation.Ref.Key != "h/20260911" || !strings.Contains(string(mutation.Patch), `"value":0`) {
		t.Fatalf("dated setter=%+v err=%v", mutation, err)
	}
}

func TestResourceSyncFailureRemainsVisible(t *testing.T) {
	m := newModel(context.Background(), nil, Options{})
	t.Cleanup(m.cancelLoad)
	m.sync = &syncPanel{result: app.SyncOutcome{Resources: app.ResourceSyncResult{Confirmed: 1, Failed: 1, Errors: []string{"resource refused"}}}}
	if !strings.Contains(m.sync.summary(), "partially") {
		t.Fatal("resource failure reported as clean sync")
	}
	if !strings.Contains(strings.Join(m.syncLines(), "\n"), "resource refused") {
		t.Fatal("resource error hidden")
	}
}
