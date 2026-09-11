package app

import (
	"context"
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/movsar/tt/internal/model"
	"github.com/movsar/tt/internal/store"
)

func ValidateItemTitle(title string) error {
	if strings.TrimSpace(title) == "" {
		return errors.New("item title is required")
	}
	if !utf8.ValidString(title) || strings.ContainsRune(title, 0) {
		return errors.New("item title must be UTF-8 without NUL")
	}
	if len(title) > 4<<20 {
		return errors.New("item title exceeds 4 MiB")
	}
	return nil
}

func (a *Actions) Checklist(ctx context.Context, original model.Task, change store.ItemChange) (store.TaskMutationOutcome, error) {
	if change.Action == store.ItemAdd || change.Action == store.ItemRename {
		if err := ValidateItemTitle(change.Title); err != nil {
			return store.TaskMutationOutcome{}, err
		}
	}
	return a.store.ChangeTaskItemIfUnchanged(ctx, original, change)
}

func (a *Actions) ChecklistState(ctx context.Context, original model.Task) (store.ChecklistState, error) {
	return a.store.ChecklistStateFor(ctx, original)
}
