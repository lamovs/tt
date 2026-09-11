package sync

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/movsar/tt/internal/api"
	"github.com/movsar/tt/internal/model"
	"github.com/movsar/tt/internal/store"
)

const LastSyncKey = "last_sync"

const maxLease = time.Hour

const (
	maxResultErrors   = 50
	maxResultWarnings = 50
	maxResultUnsent   = 50
)

type Options struct {
	AllowProjectDrop bool
}

func leaseFor(maxCall time.Duration) time.Duration {
	if maxCall <= 0 || maxCall > maxLease/2 {
		return maxLease
	}
	return 2 * maxCall
}

type Result struct {
	Pushed   int
	Requeued int

	Failed int

	ParkedUnsent int
	Projects     int
	Pulled       int
	Skipped      int
	Deleted      int
	Kept         int

	CompletedDropped int

	Warnings []api.Warning

	Errors []error

	Unsent []error

	warningsDropped int
	errorsDropped   int
	unsentDropped   int
}

func (r Result) WarningCount() int {
	list, dropped := kept(r.Warnings, r.warningsDropped)
	return len(list) + dropped
}

func (r Result) ErrorCount() int {
	list, dropped := kept(r.Errors, r.errorsDropped)
	return len(list) + dropped
}

func (r Result) UnsentCount() int {
	list, dropped := kept(r.Unsent, r.unsentDropped)
	return len(list) + dropped
}

func (r *Result) merge(other Result) {
	r.Pushed += other.Pushed
	r.Requeued += other.Requeued
	r.Failed += other.Failed
	r.ParkedUnsent += other.ParkedUnsent
	r.Projects += other.Projects
	r.Pulled += other.Pulled
	r.Skipped += other.Skipped
	r.Deleted += other.Deleted
	r.Kept += other.Kept
	r.CompletedDropped += other.CompletedDropped

	warnings, warningsDropped := kept(other.Warnings, other.warningsDropped)
	errs, errsDropped := kept(other.Errors, other.errorsDropped)
	unsent, unsentDropped := kept(other.Unsent, other.unsentDropped)
	for _, w := range warnings {
		r.addWarning(w)
	}
	for _, err := range errs {
		r.addError(err)
	}
	for _, err := range unsent {
		r.addUnsent(err)
	}
	r.dropWarnings(warningsDropped)
	r.dropErrors(errsDropped)
	r.dropUnsent(unsentDropped)
}

func kept[T any](list []T, dropped int) ([]T, int) {
	if dropped == 0 {
		return list, 0
	}
	return list[:len(list)-1], dropped
}

func (r *Result) addError(err error) {
	if len(r.Errors)+r.errorsDropped < maxResultErrors {
		r.Errors = append(r.Errors, err)
		return
	}
	r.dropErrors(1)
}

func (r *Result) dropErrors(n int) {
	if n <= 0 {
		return
	}
	if r.errorsDropped == 0 && len(r.Errors) == maxResultErrors {

		n++
	}
	r.errorsDropped += n
	summary := fmt.Errorf("and %d more failure(s), not listed", r.errorsDropped)
	if len(r.Errors) < maxResultErrors {
		r.Errors = append(r.Errors, summary)
		return
	}
	r.Errors[len(r.Errors)-1] = summary
}

func (r *Result) addUnsent(err error) {
	if len(r.Unsent)+r.unsentDropped < maxResultUnsent {
		r.Unsent = append(r.Unsent, err)
		return
	}
	r.dropUnsent(1)
}

func (r *Result) dropUnsent(n int) {
	if n <= 0 {
		return
	}
	if r.unsentDropped == 0 && len(r.Unsent) == maxResultUnsent {
		n++
	}
	r.unsentDropped += n
	summary := fmt.Errorf("and %d more entry(s) nothing was sent for, not listed", r.unsentDropped)
	if len(r.Unsent) < maxResultUnsent {
		r.Unsent = append(r.Unsent, summary)
		return
	}
	r.Unsent[len(r.Unsent)-1] = summary
}

func (r *Result) addWarning(w api.Warning) {
	if len(r.Warnings)+r.warningsDropped < maxResultWarnings {
		r.Warnings = append(r.Warnings, w)
		return
	}
	r.dropWarnings(1)
}

func (r *Result) dropWarnings(n int) {
	if n <= 0 {
		return
	}
	if r.warningsDropped == 0 && len(r.Warnings) == maxResultWarnings {
		n++
	}
	r.warningsDropped += n
	summary := api.Warning{
		Field:  "warnings",
		Value:  strconv.Itoa(r.warningsDropped),
		Detail: "more fields could not be read, not listed",
	}
	if len(r.Warnings) < maxResultWarnings {
		r.Warnings = append(r.Warnings, summary)
		return
	}
	r.Warnings[len(r.Warnings)-1] = summary
}

type Syncer struct {
	store *store.Store
	api   *api.Client
	opts  Options
	lease time.Duration
}

func New(st *store.Store, client *api.Client, opts Options) *Syncer {
	return &Syncer{
		store: st,
		api:   client,
		opts:  opts,
		lease: leaseFor(client.MaxCallDuration()),
	}
}

func (s *Syncer) Run(ctx context.Context) (Result, error) {
	return s.RunWithProgress(ctx, nil)
}

func (s *Syncer) RunWithProgress(ctx context.Context, phase func(string)) (Result, error) {
	if phase != nil {
		phase("Sending task queue")
	}
	res, err := s.Push(ctx)
	if err != nil {
		return res, err
	}
	if phase != nil {
		phase("Reading remote tasks")
	}
	pull, err := s.Pull(ctx)
	res.merge(pull)
	return res, err
}

func (s *Syncer) LastSync(ctx context.Context) (time.Time, bool, error) {
	v, ok, err := s.store.Meta(ctx, LastSyncKey)
	if err != nil || !ok {
		return time.Time{}, false, err
	}
	t, err := model.ParseStoreTime(v)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("%s: %w", LastSyncKey, err)
	}
	return t.UTC(), true, nil
}

func (s *Syncer) Due(ctx context.Context, interval time.Duration) (bool, error) {
	if interval <= 0 {
		return true, nil
	}
	last, ok, err := s.LastSync(ctx)
	if err != nil {
		return false, err
	}
	if !ok {
		return true, nil
	}
	return time.Since(last) >= interval, nil
}

func stamp(t time.Time) string {
	return model.NewTime(t).StoreString()
}
