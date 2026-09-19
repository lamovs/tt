package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/movsar/tt/internal/model"
)

type TaskExtensionsPreview struct {
	Task model.Task
	Edit model.TaskEdit
}

func hasTaskExtensions(edit model.TaskEdit) bool {
	return edit.ParentId != nil || edit.ColumnId != nil || edit.EstimatedDuration != nil || edit.EstimatedPomo != nil
}

func taskExtensionsOnly(edit model.TaskEdit) model.TaskEdit {
	var out model.TaskEdit
	if edit.ParentId != nil {
		out.ParentId = model.Ptr(*edit.ParentId)
	}
	if edit.ColumnId != nil {
		out.ColumnId = model.Ptr(*edit.ColumnId)
	}
	if edit.EstimatedDuration != nil {
		out.EstimatedDuration = model.Ptr(*edit.EstimatedDuration)
	}
	if edit.EstimatedPomo != nil {
		out.EstimatedPomo = model.Ptr(*edit.EstimatedPomo)
	}
	return out
}

func onlyTaskExtensions(edit model.TaskEdit) bool {
	edit.ParentId, edit.ColumnId, edit.EstimatedDuration, edit.EstimatedPomo = nil, nil, nil, nil
	return edit.IsEmpty()
}

func extensionEditOfTask(task model.Task) model.TaskEdit {
	var edit model.TaskEdit
	if task.ParentId != "" {
		edit.ParentId = model.Ptr(task.ParentId)
	}
	if task.ColumnId != "" {
		edit.ColumnId = model.Ptr(task.ColumnId)
	}
	if task.EstimatedDuration != 0 {
		edit.EstimatedDuration = model.Ptr(task.EstimatedDuration)
	}
	if task.EstimatedPomo != 0 {
		edit.EstimatedPomo = model.Ptr(task.EstimatedPomo)
	}
	return edit
}

// ErrParentUnsynced reports a local parent a child may not be hung under
// yet: the create of that parent is no longer the only change queued for it,
// so which task the relationship would end up pointing at cannot be told
// here. tt batch reports it in words of its own, because inside a batch the
// create was queued moments ago and only another run can have taken it.
var ErrParentUnsynced = errors.New("sync the parent task before attaching a child")

// awaitsOnlyItsOwnCreate reports whether the outbox holds exactly one entry
// for id, and that entry is the pending create of id itself. A parent in that
// state is the one local parent a child may name: the queue sends the create
// before anything that points at it, committing it rewrites every queued
// reference to the local id, and the push validates the chain against the
// server once the parent has a server id. An inflight create is not enough -
// it can still be parked with its outcome unknown - and neither is a failed
// one or a second queued change, which would leave the identity of the parent
// unresolvable here.
//
// This is one guard of three on the same rule, and they are not one
// function. Hanging a child under a parent runs validateTaskExtensions below
// against the cache; pushing that relationship runs the same function again
// inside the transaction that claims the queued entry, in
// freezeFeatureSnapshot, and then reads the parent off the server in
// checkTaskParent in internal/sync; completing the parent runs
// validateTaskClosure, which is this rule read from the other end. A change
// to what a parent of a child may be has to be made in all three at once: a
// rule only one of them holds is a change tt accepts locally and parks on
// the push, or the other way round - and a closed parent with a queued child
// relationship parks it on every retry until the next pull drops it.
func awaitsOnlyItsOwnCreate(ctx context.Context, q execer, id string) (bool, error) {
	var queued, pendingCreate int
	err := q.QueryRowContext(ctx,
		`SELECT (SELECT count(*) FROM outbox WHERE task_id=?),
		        (SELECT count(*) FROM outbox WHERE task_id=? AND op=? AND state=?)`,
		id, id, OpTaskCreate, string(OutboxPending)).Scan(&queued, &pendingCreate)
	if err != nil {
		return false, fmt.Errorf("read the queued changes of %s: %w", id, err)
	}
	return queued == 1 && pendingCreate == 1, nil
}

// parentChainLimit is how far up a chain of parents either end of the rule
// walks: the attach refuses to build one deeper, and the closure reads no
// further than the attach would have let it grow.
const parentChainLimit = 1000

func validateTaskExtensions(ctx context.Context, q execer, task model.Task, edit model.TaskEdit) error {
	if edit.EstimatedDuration != nil || edit.EstimatedPomo != nil {
		if _, err := model.ParseTaskEstimates(task.FocusSummaries); err != nil {
			return fmt.Errorf("cannot edit task estimates: %w", err)
		}
	}
	if edit.EstimatedDuration != nil && *edit.EstimatedDuration < 0 {
		return errors.New("estimated duration must be a nonnegative number of seconds")
	}
	if edit.EstimatedPomo != nil && (*edit.EstimatedPomo < 0 || *edit.EstimatedPomo > 60) {
		return errors.New("estimated Pomodoros must be between 0 and 60")
	}
	if edit.ParentId != nil && *edit.ParentId != "" {
		id := *edit.ParentId
		seen := map[string]bool{task.Id: true}
		for id != "" {
			if seen[id] {
				return errors.New("parent relationship would create a task cycle")
			}
			seen[id] = true
			parent, err := loadTask(ctx, q, id)
			if err != nil {
				return fmt.Errorf("parent task must be cached before attaching a child: %w", err)
			}
			if parent.ProjectId != task.ProjectId {
				return errors.New("parent and child must belong to the same project")
			}
			if parent.Status != model.TaskOpen {
				return errors.New("parent task must be open")
			}
			if IsLocalID(id) {
				awaiting, err := awaitsOnlyItsOwnCreate(ctx, q, id)
				if err != nil {
					return err
				}
				if !awaiting {
					return ErrParentUnsynced
				}
			} else {
				var pending int
				if err := q.QueryRowContext(ctx, `SELECT count(*) FROM outbox WHERE task_id=?`, id).Scan(&pending); err != nil {
					return err
				}
				if pending != 0 {
					return errors.New("sync or recover changes to the parent chain before attaching a child")
				}
			}
			if len(seen) > parentChainLimit {
				return errors.New("parent relationship exceeds the supported depth")
			}
			id = parent.ParentId
		}
	}
	if edit.ColumnId != nil {
		if !columnOnlyEdit(edit) {
			return errors.New("move a Kanban card separately from other task edits")
		}
		_, err := validateTaskColumn(ctx, q, task, *edit.ColumnId)
		return err
	}
	return nil
}

// ErrChildLinkUnsent reports a task that cannot be completed yet: the queue
// still holds a relationship that hangs another task under it, or under a
// task somewhere below it. Closing it first parks that relationship on the
// push, where the freeze reads the closed ancestor out of the cache, and
// parks it again on every retry, where the server reads the chain closed as
// well; the next pull then writes the empty parent the server returns over
// the child and nothing is left of the relationship.
var ErrChildLinkUnsent = errors.New("a child task is queued to be hung under this task")

// ErrChildLinkParked is the same refusal once the queue has parked the entry
// that carries the relationship. tt sync claims pending entries alone, so a
// refusal that named it there would send the reader round a loop that changes
// nothing: tt sync --retry-failed is what puts a parked entry back in line,
// and tt sync --drop-parked is what throws it away when the server keeps
// rejecting it. The second command is the way out of the state this guard
// would otherwise make permanent - a relationship that fails every time it is
// sent would leave the task above it closed off for good.
var ErrChildLinkParked = fmt.Errorf("%w, and the queue has parked that relationship", ErrChildLinkUnsent)

// childLinkRefusal names the task the relationship hangs, which is not the
// task being closed and not always a task the cache still shows under it, and
// the command that moves the entry carrying it, which is not the same command
// in every state.
func childLinkRefusal(link queuedChildLink) error {
	if link.state == OutboxFailed {
		return fmt.Errorf("%w (%s); run tt sync --retry-failed to send it again, or tt sync --drop-parked to throw it away if it keeps being rejected",
			ErrChildLinkParked, link.child)
	}
	return fmt.Errorf("%w (%s); run tt sync first", ErrChildLinkUnsent, link.child)
}

// queuedChildLink is one relationship the queue still holds: the entry that
// carries it, the task it hangs, the task it names as the parent, and the
// state the entry is in.
type queuedChildLink struct {
	seq    int64
	child  string
	parent string
	state  OutboxState
}

// childLinkCandidateSQL selects the queued entries a relationship can be read
// out of at all: the three operations taskExtensionParent knows - every other
// one answers false on its first switch - and, of those, the payloads that
// carry an extension edit, which is the same condition taskExtensionParent
// reads off the decoded metadata. Narrowing here and not over the whole queue
// keeps a closure from decoding every queued change of every task, which a
// batch pays for once per task it closes.
const childLinkCandidateSQL = `SELECT seq, task_id, op, payload, state FROM outbox
	 WHERE op IN ('` + OpTaskCreate + `','` + OpTaskUpdate + `','` + OpTaskMove + `')
	   AND json_valid(payload) AND json_extract(payload, '$._tt.fields.extensions') = 1`

// scanChildLinks reads the relationships out of the rows a candidate query
// answered with, dropping the entries that carry an extension edit of some
// other kind. The ids are taken as they stand, because replaceLocalID
// rewrites the queued payloads in the same transaction that gives a task its
// server id, so no queued entry names a task by an id the cache has already
// replaced.
func scanChildLinks(rows *sql.Rows) ([]queuedChildLink, error) {
	defer rows.Close()
	var out []queuedChildLink
	for rows.Next() {
		var (
			link    queuedChildLink
			taskID  sql.NullString
			op      string
			payload []byte
			state   string
		)
		if err := rows.Scan(&link.seq, &taskID, &op, &payload, &state); err != nil {
			return nil, err
		}
		parent, hangs, err := taskExtensionParent(
			OutboxItem{OutboxEntry: OutboxEntry{Op: op, TaskID: taskID.String, Payload: payload}}, true)
		if err != nil {
			return nil, err
		}
		if !hangs {
			continue
		}
		link.child, link.parent, link.state = taskID.String, parent, OutboxState(state)
		out = append(out, link)
	}
	return out, rows.Err()
}

// unsentChildLinks reads every relationship the queue still holds, oldest
// first. Every unsent state counts, not pending alone: a parked relationship
// returns to the queue with tt sync --retry-failed. Every phase counts too,
// unlike the push, which asks about a relationship it is about to send: an
// armed request is one the store cannot place - it may be on the server and
// it may be about to come back rejected, and tt sync is what settles that
// either way.
func unsentChildLinks(ctx context.Context, q execer) ([]queuedChildLink, error) {
	rows, err := q.QueryContext(ctx, childLinkCandidateSQL+` AND state IN (?,?,?) ORDER BY seq`,
		string(OutboxPending), string(OutboxInflight), string(OutboxFailed))
	if err != nil {
		return nil, err
	}
	return scanChildLinks(rows)
}

// validateTaskClosure refuses to close a task while the queue still holds an
// unsent relationship hung under it or under any task below it. It is the
// closing end of the rule awaitsOnlyItsOwnCreate above describes, and it
// reads the chain to the same depth the attaching end does: validateTaskExtensions
// walks every ancestor of the parent a child names and wants each one open,
// the push walks the same chain again, so closing a grandparent strands a
// queued relationship exactly the way closing the parent does.
//
// The chain above a relationship is read out of the cache. A relationship the
// cache does not hold - the one an undo took back out of it, the one queued
// against a task whose own parent is still queued - is a starting point of
// its own, so the walk above any one of them runs over cached parents alone:
// the topmost queued edge of any path is itself in this list.
func validateTaskClosure(ctx context.Context, q execer, task model.Task, edit model.TaskEdit) error {
	// The attach refuses every parent that is not open, so the closing end
	// reads the status the same way round and not the one status that
	// closes a task today.
	if edit.Status == nil || *edit.Status == model.TaskOpen {
		return nil
	}
	links, err := unsentChildLinks(ctx, q)
	if err != nil {
		return fmt.Errorf("read the queued children of %s: %w", task.Id, err)
	}
	// cleared remembers the ids a walk has already passed without meeting
	// this task, so the chains of many queued relationships cost one walk
	// between them and not one walk each.
	cleared := map[string]bool{}
	for _, link := range links {
		hangs, err := chainHoldsAncestor(ctx, q, link.parent, task.Id, cleared)
		if err != nil {
			return fmt.Errorf("read the queued children of %s: %w", task.Id, err)
		}
		if hangs {
			return childLinkRefusal(link)
		}
	}
	return nil
}

// chainHoldsAncestor reports whether target is the parent a relationship
// names or one of the ancestors above it, walking the cached chain the way
// validateTaskExtensions walks it and stopping at the same depth.
func chainHoldsAncestor(ctx context.Context, q execer, from, target string, cleared map[string]bool) (bool, error) {
	var walked []string
	seen := map[string]bool{}
	complete := true
	for id := from; id != ""; {
		if id == target {
			return true, nil
		}
		// A chain that turns back on itself reaches nothing the walk has
		// not read already, and one an earlier walk cleared reaches
		// nothing this one would meet either.
		if cleared[id] || seen[id] {
			break
		}
		// Deeper than the attach guard would ever have let a chain grow,
		// what stands above is left unread - and unread is not cleared.
		if len(walked) >= parentChainLimit {
			complete = false
			break
		}
		seen[id] = true
		walked = append(walked, id)
		parent, cached, err := cachedParentID(ctx, q, id)
		if err != nil {
			return false, err
		}
		// A parent the cache no longer holds ends the chain: nothing above
		// it can be read here, and the task being closed is read out of the
		// cache before this runs.
		if !cached {
			break
		}
		id = parent
	}
	if complete {
		for _, id := range walked {
			cleared[id] = true
		}
	}
	return false, nil
}

// cachedParentID reads the parent the cache holds for a task and says whether
// the cache holds the task at all. It reads the one column the walk needs,
// because a closure walks this for every relationship the queue holds.
func cachedParentID(ctx context.Context, q execer, id string) (string, bool, error) {
	var parent string
	err := q.QueryRowContext(ctx, `SELECT parent_id FROM tasks WHERE id = ?`, id).Scan(&parent)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("read the parent of %s: %w", id, err)
	}
	return parent, true, nil
}

func (s *Store) PreviewTaskExtensions(ctx context.Context, original model.Task, edit model.TaskEdit) (TaskExtensionsPreview, error) {
	if !hasTaskExtensions(edit) || !onlyTaskExtensions(edit) {
		return TaskExtensionsPreview{}, errors.New("expected a task extension edit")
	}
	edit = taskExtensionsOnly(edit)
	err := s.Tx(ctx, func(tx *sql.Tx) error {
		current, err := loadTask(ctx, tx, original.Id)
		if err != nil {
			return err
		}
		if !samePulledTask(current, original) {
			return ErrTaskChanged
		}
		return validateTaskExtensions(ctx, tx, current, edit)
	})
	return TaskExtensionsPreview{Task: original, Edit: edit}, err
}

func (s *Store) ApplyTaskExtensions(ctx context.Context, preview TaskExtensionsPreview) (TaskMutationOutcome, error) {
	if !hasTaskExtensions(preview.Edit) || !onlyTaskExtensions(preview.Edit) {
		return TaskMutationOutcome{}, errors.New("expected a task extension edit")
	}
	return s.UpdateTaskIfUnchanged(ctx, preview.Task, taskExtensionsOnly(preview.Edit))
}

func confirmTaskExtensions(root map[string]json.RawMessage, edit model.TaskEdit) error {
	return matchTaskExtensions(root, edit, false)
}

func matchTaskExtensions(root map[string]json.RawMessage, edit model.TaskEdit, allowAbsent bool) error {
	for _, field := range []struct {
		name  string
		value *string
	}{{"parentId", edit.ParentId}, {"columnId", edit.ColumnId}} {
		if field.value != nil {
			if err := exactRawString(root, field.name, *field.value); err != nil {
				return err
			}
		}
	}
	var estimates model.TaskEstimateFields
	if edit.EstimatedDuration != nil || edit.EstimatedPomo != nil {
		var err error
		estimates, err = model.ParseTaskEstimates(root["focusSummaries"])
		if err != nil {
			return err
		}
	}
	if edit.EstimatedDuration != nil {
		if estimates.EstimatedDuration == nil && allowAbsent && *edit.EstimatedDuration == 0 {
			estimates.EstimatedDuration = model.Ptr(int64(0))
		}
		if estimates.EstimatedDuration == nil || *estimates.EstimatedDuration != *edit.EstimatedDuration {
			return errors.New("estimated duration was not confirmed")
		}
	}
	if edit.EstimatedPomo != nil {
		if estimates.EstimatedPomo == nil && allowAbsent && *edit.EstimatedPomo == 0 {
			estimates.EstimatedPomo = model.Ptr(0)
		}
		if estimates.EstimatedPomo == nil || *estimates.EstimatedPomo != *edit.EstimatedPomo {
			return errors.New("estimated Pomodoros were not confirmed")
		}
	}
	return nil
}

func TaskExtensionBaseline(item OutboxItem) (*model.TaskEdit, error) {
	if item.Op != OpTaskUpdate && item.Op != OpTaskMove {
		return nil, nil
	}
	_, metadata, err := DecodeTaskEditPayload(item.Payload)
	if err != nil {
		return nil, err
	}
	if metadata == nil || !metadata.Fields.Extensions || metadata.Phase != FeaturePrepared && metadata.Phase != FeatureRejected {
		return nil, nil
	}
	baseline := taskExtensionsOnly(*metadata.ExtensionBaseline)
	return &baseline, nil
}

// TaskExtensionParent reads the parent a queued relationship is about to hang
// its own task under: the two phases a send still goes out from, prepared and
// rejected, and nothing else. The push asks it this way round because an armed
// request has been sent already and the push does no more than read the parent
// back off the server.
func TaskExtensionParent(item OutboxItem) (string, bool, error) {
	return taskExtensionParent(item, false)
}

// taskExtensionParent answers the same question as TaskExtensionParent, and
// with anyPhase it drops the phase filter: a caller asking whether anything
// queued still hangs a child under a task - validateTaskClosure - counts an
// armed relationship too, because whether it reached the server is exactly
// what the store cannot tell.
func taskExtensionParent(item OutboxItem, anyPhase bool) (string, bool, error) {
	var parent *string
	var metadata *FeaturePayloadMetadata
	var err error
	switch item.Op {
	case OpTaskCreate:
		var task model.Task
		task, metadata, err = DecodeTaskPayload(item.Payload)
		if task.ParentId != "" {
			parent = model.Ptr(task.ParentId)
		}
	case OpTaskUpdate, OpTaskMove:
		var edit model.TaskEdit
		edit, metadata, err = DecodeTaskEditPayload(item.Payload)
		parent = edit.ParentId
	default:
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if metadata == nil || !metadata.Fields.Extensions || parent == nil || *parent == "" {
		return "", false, nil
	}
	if !anyPhase && metadata.Phase != FeaturePrepared && metadata.Phase != FeatureRejected {
		return "", false, nil
	}
	return *parent, true, nil
}

func TaskExtensionColumn(item OutboxItem) (string, bool, error) {
	if item.Op != OpTaskUpdate && item.Op != OpTaskMove {
		return "", false, nil
	}
	edit, metadata, err := DecodeTaskEditPayload(item.Payload)
	if err != nil {
		return "", false, err
	}
	if metadata == nil || !metadata.Fields.Extensions || metadata.Phase != FeaturePrepared && metadata.Phase != FeatureRejected || edit.ColumnId == nil {
		return "", false, nil
	}
	if !columnOnlyEdit(edit) || *edit.ColumnId == "" {
		return "", false, errors.New("invalid Kanban column mutation")
	}
	return *edit.ColumnId, true, nil
}

func CheckTaskExtensionBaseline(raw []byte, taskID, projectID string, baseline model.TaskEdit) error {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(raw, &root); err != nil {
		return err
	}
	if err := exactRawString(root, "id", taskID); err != nil {
		return err
	}
	if err := exactRawString(root, "projectId", projectID); err != nil {
		return err
	}
	for _, field := range []struct {
		name  string
		value *string
	}{{"parentId", baseline.ParentId}, {"columnId", baseline.ColumnId}} {
		if field.value == nil {
			continue
		}
		if _, present := root[field.name]; !present && *field.value == "" {
			root[field.name] = json.RawMessage(`""`)
		}
	}
	if err := matchTaskExtensions(root, baseline, true); err != nil {
		return fmt.Errorf("task extension conflict; review current server values: %w", err)
	}
	return nil
}

func (s *Store) RecordTaskRequestUnsent(ctx context.Context, item OutboxItem) error {
	result, err := s.db.ExecContext(ctx, `UPDATE outbox SET sent_at=NULL WHERE seq=? AND state='inflight' AND lease_token=?`, item.Seq, string(item.LeaseToken))
	if err != nil {
		return err
	}
	return affectedOne(ctx, s.db, result, item.Seq)
}
