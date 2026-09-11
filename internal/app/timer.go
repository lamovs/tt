package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/movsar/tt/internal/api"
	"github.com/movsar/tt/internal/config"
	"github.com/movsar/tt/internal/focus"
	"github.com/movsar/tt/internal/store"
)

func CompleteTimer(ctx context.Context, st *store.Store, session store.TimerSession, notify func() error, upload func() error) []error {
	if session.Outcome != "done" {
		return nil
	}
	var problems []error
	claimed, err := st.ClaimTimerNotification(ctx, session.ID)
	if err != nil {
		problems = append(problems, err)
	}
	if claimed && notify != nil {
		if err := notify(); err != nil {
			problems = append(problems, err)
		}
	}
	if upload != nil {
		if err := upload(); err != nil {
			problems = append(problems, err)
		}
	}
	return problems
}

type TimerData struct {
	Active                       store.TimerSnapshot
	TaskTitle                    string
	History                      []store.FocusTarget
	NoteReviews                  []store.FocusTarget
	DefaultDuration              time.Duration
	UploadEnabled, UploadAborted bool
	IndicatorEnabled             bool
	Topics                       []store.FocusTopic
}

type TimerOutcome struct {
	Result   store.TimerResult
	Problems []error
}

type Timers struct {
	store  *store.Store
	cfg    config.Config
	client func() (*api.Client, error)
	topics focus.TopicClientFactory
	watch  func(string) error
	notify func(context.Context, store.TimerSession) error
	wake   func() error
}

func NewTimers(st *store.Store, cfg config.Config, client func() (*api.Client, error), watch func(string) error, notify func(context.Context, store.TimerSession) error) *Timers {
	return &Timers{store: st, cfg: cfg, client: client, watch: watch, notify: notify}
}

func (t *Timers) WithCompletionWake(wake func() error) *Timers {
	t.wake = wake
	return t
}

func (t *Timers) WithTopicClient(client focus.TopicClientFactory) *Timers {
	t.topics = client
	return t
}

func (t *Timers) RefreshTopics(ctx context.Context) (int, error) {
	if t.topics == nil {
		return 0, errors.New("Timer topics need Browser authorization; run tt login web --same-account")
	}
	client, err := t.topics()
	if err != nil {
		return 0, err
	}
	topics, err := client.ListTopics(ctx)
	if err != nil {
		return 0, err
	}
	rows := make([]store.FocusTopic, 0, len(topics))
	for _, topic := range topics {
		rows = append(rows, store.FocusTopic{ID: topic.ID, Name: topic.Name, Type: topic.Type, Status: topic.Status,
			SortOrder: topic.SortOrder, PomodoroTime: topic.PomodoroTime, Raw: topic.Raw})
	}
	if err := t.store.ReplaceFocusTopics(ctx, rows, client.CredentialFingerprint(), time.Now()); err != nil {
		return 0, err
	}
	return len(rows), nil
}

func (t *Timers) Read(ctx context.Context, now time.Time) (TimerData, error) {
	out := TimerData{DefaultDuration: t.cfg.Timer.Focus.Duration(), UploadEnabled: t.cfg.FocusUpload.Enabled, UploadAborted: t.cfg.Timer.UploadAborted, IndicatorEnabled: t.cfg.Timer.Indicator}
	var err error
	out.Active, err = t.store.ReadTimer(ctx, now)
	if err != nil {
		return out, err
	}
	out.TaskTitle = out.Active.TaskTitle
	topics, err := t.store.FocusTopics(ctx)
	if err != nil {
		return out, err
	}
	for _, topic := range topics {
		if topic.Status == 0 {
			out.Topics = append(out.Topics, topic)
		}
	}
	history, err := t.store.TimerHistory(ctx, 20)
	if err != nil {
		return out, err
	}
	for _, session := range history {
		target, err := t.store.ReadFocusTarget(ctx, session.ID)
		if err != nil {
			return out, err
		}
		out.History = append(out.History, target)
	}
	out.NoteReviews, err = t.store.PendingFocusNoteReviews(ctx)
	if err != nil {
		return out, err
	}
	return out, nil
}

func (t *Timers) ReviewNote(ctx context.Context, target store.FocusTarget, addition string, skip bool) (store.TimerSession, error) {
	if err := ctx.Err(); err != nil {
		return store.TimerSession{}, err
	}
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	session, err := t.store.ResolveFocusNote(writeCtx, target, addition, skip)
	cancel()
	if err != nil {
		return store.TimerSession{}, err
	}
	if !t.cfg.FocusUpload.Enabled {
		return session, nil
	}
	if t.wake != nil {
		err = t.wake()
	} else {
		uploadCtx, stop := context.WithTimeout(context.WithoutCancel(ctx), 45*time.Second)
		result, _, uploadErr := UploadPendingFocus(uploadCtx, t.store, t.client, t.cfg.Timer.UploadAborted, false, t.topics)
		stop()
		err = errors.Join(append(result.Errors, uploadErr)...)
	}
	if err != nil {
		return session, fmt.Errorf("completion note saved; focus upload remains pending: %w", err)
	}
	return session, nil
}

func (t *Timers) BindTask(ctx context.Context, guard store.TimerGuard, id string) (store.TimerGuard, error) {
	return t.store.GuardTimerTask(ctx, guard, id)
}

func (t *Timers) BindDefault(ctx context.Context, guard store.TimerGuard) (store.TimerGuard, error) {
	return BindDefaultFocus(ctx, t.store, guard, t.cfg.DefaultFocus)
}

func (t *Timers) Control(ctx context.Context, action string, options store.TimerStartOptions, guard *store.TimerGuard) (TimerOutcome, error) {
	var out TimerOutcome
	if err := ctx.Err(); err != nil {
		return out, err
	}
	if action == "start" && options.TopicID != "" {
		if t.topics == nil {
			return out, errors.New("Timer topic start needs Browser authorization; run tt login web --same-account")
		}
		topic, err := t.store.ResolveFocusTopic(ctx, options.TopicID)
		if err != nil {
			return out, err
		}
		client, err := t.topics()
		if err != nil {
			return out, err
		}
		if topic.Status != 0 || topic.Name != options.TopicName || topic.CredentialFingerprint != options.TopicCredential || client.CredentialFingerprint() != options.TopicCredential {
			return out, errors.New("Timer topic or Browser credential changed; refresh topics and review the destination again")
		}
	}

	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	result, err := t.store.ControlTimer(writeCtx, action, options, guard, time.Now())
	cancel()
	out.Result = result
	if err != nil {
		return out, err
	}
	if result.Completed != nil {

		effectsCtx, stop := context.WithTimeout(context.WithoutCancel(ctx), 45*time.Second)
		out.Problems = t.complete(effectsCtx, *result.Completed)
		stop()
	}
	if state := result.State; state != nil && state.FocusType == 0 && (action == "start" || action == "resume" || action == "status") && t.watch != nil {
		if err := t.watch(state.SessionID); err != nil {
			out.Problems = append(out.Problems, fmt.Errorf("session saved; countdown watcher unavailable; refresh to recover: %w", err))
		}
	}
	return out, nil
}

func (t *Timers) complete(ctx context.Context, session store.TimerSession) []error {
	var notify, upload func() error
	if t.cfg.Timer.OnEnd != "" && t.notify != nil {
		notify = func() error { return t.notify(ctx, session) }
	}
	if t.cfg.FocusUpload.Enabled {
		if t.wake != nil {
			upload = t.wake
		} else {
			upload = func() error {
				result, _, err := UploadPendingFocus(ctx, t.store, t.client, t.cfg.Timer.UploadAborted, false, t.topics)
				return errors.Join(append(result.Errors, err)...)
			}
		}
	}
	return CompleteTimer(ctx, t.store, session, notify, upload)
}

func (t *Timers) Upload(ctx context.Context, target store.FocusTarget, retry bool) (focus.Result, error) {
	if err := focus.ValidateTarget(target, t.cfg.Timer.UploadAborted, retry); err != nil {
		return focus.Result{}, err
	}
	if err := t.store.CheckFocusTarget(ctx, target); err != nil {
		return focus.Result{}, err
	}
	client, err := t.client()
	if err != nil {
		return focus.Result{}, err
	}
	return focus.UploadSelected(ctx, t.store, client, target, t.cfg.Timer.UploadAborted, retry, t.topics), nil
}
