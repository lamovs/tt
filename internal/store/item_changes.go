package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"time"

	"github.com/movsar/tt/internal/model"
)

type ItemAction string

const (
	ItemAdd     ItemAction = "add"
	ItemRename  ItemAction = "rename"
	ItemSetDone ItemAction = "set done"
	ItemRemove  ItemAction = "remove"
	ItemMove    ItemAction = "move"
)

type ItemChange struct {
	Action                     ItemAction
	Key, DestinationKey, Title string
	Done                       bool
}

func (s *Store) ChangeTaskItemIfUnchanged(ctx context.Context, original model.Task, change ItemChange) (TaskMutationOutcome, error) {
	return s.changeTaskItem(ctx, original.Id, &original, change, 0, 0)
}

func (s *Store) changeTaskItem(ctx context.Context, taskID string, original *model.Task, change ItemChange, position, destination int) (TaskMutationOutcome, error) {
	if (change.Action == ItemAdd || change.Action == ItemRename) && strings.TrimSpace(change.Title) == "" {
		return TaskMutationOutcome{}, fmt.Errorf("%s checklist item: title is required", change.Action)
	}
	return s.editTaskWithTxOutcome(ctx, taskID, OpTaskUpdate, TaskUpdatedKind,
		func(ctx context.Context, tx *sql.Tx, cur model.Task) (model.TaskEdit, []string, error) {
			if original != nil {
				if !reflect.DeepEqual(cur, *original) {
					return model.TaskEdit{}, nil, ErrTaskChanged
				}
				if change.Action != ItemAdd {
					position = itemPosition(cur.Items, change.Key)
				}
				if change.Action == ItemMove {
					destination = itemPosition(cur.Items, change.DestinationKey)
				}
			}
			items := append([]model.Item(nil), cur.Items...)
			if change.Action == ItemAdd {
				key, err := newItemKey(ctx, tx, cur.Id)
				if err != nil {
					return model.TaskEdit{}, nil, err
				}
				rank := int64(1)
				for _, item := range items {
					if item.SortOrder == math.MaxInt64 {
						for i := range items {
							items[i].SortOrder = int64(i + 1)
						}
						rank = int64(len(items) + 1)
						break
					}
					if item.SortOrder >= rank {
						rank = item.SortOrder + 1
					}
				}
				items = append(items, model.Item{Key: key, Title: change.Title, SortOrder: rank})
				e := model.TaskEdit{Items: model.NewEditList(items)}
				if cur.Kind == "" || cur.Kind == "TEXT" {
					e.Kind = model.Ptr("CHECKLIST")
				}
				return e, []string{key}, nil
			}
			if change.Action == ItemMove && (position < 1 || position > len(items) || destination < 1 || destination > len(items)) {
				return model.TaskEdit{}, nil, fmt.Errorf("move checklist item: position must be between 1 and %d", len(items))
			}
			if position < 1 || position > len(items) {
				return model.TaskEdit{}, nil, fmt.Errorf("checklist item position must be between 1 and %d", len(items))
			}
			i := position - 1
			switch change.Action {
			case ItemRename:
				items[i].Title = change.Title
			case ItemSetDone:
				if change.Done {
					if !items[i].Status.Done() || items[i].CompletedTime.IsZero() {
						items[i].Status, items[i].CompletedTime = model.ItemDone, model.NewTime(time.Now())
					}
				} else {
					items[i].Status, items[i].CompletedTime = model.ItemOpen, model.Time{}
				}
			case ItemRemove:
				items = append(items[:i], items[i+1:]...)
			case ItemMove:
				if position != destination {
					item := items[i]
					items = append(items[:i], items[i+1:]...)
					at := destination - 1
					items = append(items, model.Item{})
					copy(items[at+1:], items[at:])
					items[at] = item
					for i := range items {
						items[i].SortOrder = int64(i + 1)
					}
				}
			default:
				return model.TaskEdit{}, nil, errors.New("unknown checklist action")
			}
			return model.TaskEdit{Items: model.NewEditList(items)}, nil, nil
		})
}

func itemPosition(items []model.Item, key string) int {
	position := 0
	if key == "" {
		return 0
	}
	for i, item := range items {
		if item.Key == key {
			if position != 0 {
				return 0
			}
			position = i + 1
		}
	}
	return position
}

type ChecklistState struct {
	Refusal         string
	Uncertain       bool
	RecoveryPending bool
}

func (s *Store) ChecklistStateFor(ctx context.Context, original model.Task) (ChecklistState, error) {
	var out ChecklistState
	err := s.Tx(ctx, func(tx *sql.Tx) error {
		current, err := loadTask(ctx, tx, original.Id)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(current, original) {
			return ErrTaskChanged
		}
		if _, _, err = validateItemReplacement(ctx, tx, current, current.Items, nil); err != nil {
			if !errors.Is(err, ErrUnsafeChecklist) {
				return err
			}
			out.Refusal = err.Error()
		}
		identities, err := itemIdentities(ctx, tx, original.Id)
		if err != nil {
			return err
		}
		for _, identity := range identities {
			out.Uncertain = out.Uncertain || identity.State == ItemUncertain
		}
		control, _, err := itemIdentity(ctx, tx, original.Id, "")
		out.RecoveryPending = control.State == RecoveryPending
		return err
	})
	return out, err
}
