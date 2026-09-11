package app

import (
	"context"
	"errors"
	"github.com/movsar/tt/internal/cli"
	"github.com/movsar/tt/internal/schedule"
	"io"
	"reflect"

	"github.com/movsar/tt/internal/editor"
	"github.com/movsar/tt/internal/model"
	"github.com/movsar/tt/internal/store"
	"github.com/movsar/tt/internal/taskdoc"
)

type DocumentRequest struct {
	Original *model.Task
	Seed     model.Task
}

type DocumentResult struct {
	Outcome      store.TaskMutationOutcome
	Canceled     bool
	DraftPath    string
	CleanupError error
}

func (a *Actions) Document(ctx context.Context, request DocumentRequest, input io.Reader, output, diagnostic io.Writer) (result DocumentResult, err error) {
	seed := request.Seed
	if seed.ProjectId == "" {
		return result, errors.New("choose a cached list before opening the editor")
	}
	if request.Original != nil {
		original := request.Original
		if seed.Id != original.Id || seed.ProjectId != original.ProjectId || seed.Kind != original.Kind {
			return result, errors.New("id, project_id and existing kind are read-only")
		}
	} else if seed.Id != "" || (seed.Kind != "TEXT" && seed.Kind != "NOTE") {
		return result, errors.New("new documents need an empty id and TEXT or NOTE kind")
	}
	initial, err := taskdoc.Encode(seed, a.config.EditorHints)
	if err != nil {
		return result, err
	}
	draft, err := editor.Open(ctx, initial, input, output, diagnostic)
	if draft != nil {
		result.DraftPath = draft.Path
	}
	if err != nil {
		return result, err
	}
	defer func() {
		if err == nil {
			result.CleanupError = draft.Discard()
			if result.CleanupError == nil {
				result.DraftPath = ""
			}
		}
	}()
	if !draft.Changed {
		result.Canceled = true
		return result, nil
	}
	unchanged := func() (DocumentResult, error) {

		if request.Original != nil {
			if _, err := a.store.UpdateTaskIfUnchanged(ctx, *request.Original, model.TaskEdit{}); err != nil {
				return result, err
			}
		}
		result.Canceled = true
		return result, nil
	}
	fields, body, err := taskdoc.Decode(draft.Data)
	if err != nil {
		return result, err
	}
	if reflect.DeepEqual(fields, taskdoc.FieldsOf(seed)) && body == seed.Content {
		return unchanged()
	}
	baseline := seed
	if request.Original != nil {
		baseline = *request.Original
	}

	next, edit, err := taskdoc.Apply(baseline, draft.Data, request.Original == nil)
	if err != nil {
		return result, err
	}
	if reflect.DeepEqual(next, seed) {
		return unchanged()
	}
	if request.Original == nil {
		result.Outcome.Task, err = a.store.CreateTask(ctx, next)
		result.Outcome.Changed = err == nil
	} else if schedule.ReviewsInterval(*request.Original, edit) {
		p, prepareErr := a.store.PrepareInterval(ctx, request.Original, edit, model.Task{})
		if prepareErr != nil {
			return result, prepareErr
		}
		if err = cli.ReviewInterval(ctx, p, input, diagnostic, cli.IsTerminal(input) && cli.IsTerminal(diagnostic), false); err != nil {
			return result, err
		}
		result.Outcome, err = a.store.ApplyInterval(ctx, p, true)
	} else {
		result.Outcome, err = a.store.UpdateTaskIfUnchanged(ctx, *request.Original, edit)
	}
	return result, err
}
