package sync

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/movsar/tt/internal/api"
	"github.com/movsar/tt/internal/model"
	"github.com/movsar/tt/internal/store"
)

func (s *Syncer) sendVersioned(ctx context.Context, item store.OutboxItem, send store.FeatureSend, post bool) (string, pushOutcome, error) {
	taskID, projectID, addressed := send.FeatureAddress()
	if post {
		var err error
		if send.Task != nil {
			var created *api.RawTask
			body := featureCreateBody(send)
			created, err = s.api.CreateTaskRaw(ctx, body)
			if err == nil {
				taskID, projectID = created.Task.ID, created.Task.ProjectID
				if projectID == "" {
					projectID = send.ProjectID
				}
				evidence := store.FeatureConfirmation{TaskID: taskID, ProjectID: projectID, Raw: created.Raw}
				if recordErr := s.store.RecordFeatureEvidence(settleContext(ctx), item, store.FeatureAccepted, evidence); recordErr != nil {
					return "", pushAbort, fmt.Errorf("outbox %d: record accepted create: %w", item.Seq, recordErr)
				}
				addressed = true
			}
		} else {
			err = s.api.UpdateTask(ctx, featureUpdateBody(send))
		}
		if err != nil {
			out := classify(item.Op, err)
			if definitelyRejected(err) {
				if rejectErr := s.store.RejectFeature(settleContext(ctx), item); rejectErr != nil {
					return "", pushAbort, fmt.Errorf("outbox %d: record rejected feature request: %w", item.Seq, rejectErr)
				}
			}
			return "", out, fmt.Errorf("outbox %d: send versioned %s of %s: %w", item.Seq, item.Op, item.TaskID, err)
		}
	}

	if !addressed {
		return "", pushPark, fmt.Errorf("outbox %d: versioned create of %s was armed without a recorded server id; no second POST is allowed", item.Seq, item.TaskID)
	}
	if out, err := s.renewBefore(ctx, item, "the read that confirms the versioned mutation"); err != nil {
		return "", out, err
	}
	got, err := s.api.GetTaskRaw(ctx, projectID, taskID)
	if err != nil {
		out := classify(item.Op, err)
		if out == pushAbort {
			return "", out, fmt.Errorf("outbox %d: confirm versioned %s: %w", item.Seq, item.Op, err)
		}
		if statusIs(err, http.StatusNotFound) {
			out = pushPark
		}
		return "", out, fmt.Errorf("outbox %d: confirm versioned %s of %s by read only: %w", item.Seq, item.Op, item.TaskID, err)
	}
	confirmation, err := store.ConfirmFeatureResponse(send, taskID, projectID, got.Raw)
	if err == nil && send.Edit != nil {
		err = confirmScheduleEdit(got.Task, *send.Edit)
	}
	if err == nil && send.Edit != nil && hasColumnAssignment(send.Edit, &send.Metadata) {
		err = checkColumnTaskStatus(got.Raw)
		if err == nil && got.Task.Status != 0 {
			err = errors.New("column move read-back found a completed task; review current server state")
		}
	}
	if err != nil {
		evidence := store.FeatureConfirmation{TaskID: taskID, ProjectID: projectID, Raw: got.Raw}
		if recordErr := s.store.RecordFeatureEvidence(settleContext(ctx), item, store.FeatureMismatch, evidence); recordErr != nil {
			return "", pushAbort, fmt.Errorf("outbox %d: record mismatching confirmation: %w", item.Seq, recordErr)
		}
		return "", pushPark, fmt.Errorf("outbox %d: versioned %s of %s was not confirmed: %w", item.Seq, item.Op, item.TaskID, err)
	}
	if err := s.store.RecordFeatureEvidence(settleContext(ctx), item, store.FeatureAccepted, confirmation); err != nil {
		return "", pushAbort, fmt.Errorf("outbox %d: record confirmed response: %w", item.Seq, err)
	}
	if err := s.store.SettleFeature(settleContext(ctx), item, confirmation); err != nil {
		return "", pushPark, fmt.Errorf("outbox %d: settle confirmed versioned %s: %w", item.Seq, item.Op, err)
	}
	return taskID, pushSettled, nil
}

func settleContext(ctx context.Context) context.Context {
	return context.WithoutCancel(ctx)
}

func featureCreateBody(send store.FeatureSend) api.TaskCreate {
	task := *send.Task
	applyFrozenTask(&task, send.Metadata.Snapshot)
	body := api.TaskCreateFrom(task)
	body.ItemsPresent = send.Metadata.Fields.Items
	body.AllDayPresent = send.Metadata.Fields.Interval
	return body
}

func featureUpdateBody(send store.FeatureSend) api.TaskUpdate {
	edit := *send.Edit
	applyFrozenEdit(&edit, send.Metadata.Snapshot)
	return api.TaskUpdateFrom(send.TaskID, send.ProjectID, edit)
}

func applyFrozenTask(task *model.Task, snapshot *store.FeatureSnapshot) {
	if snapshot.Extensions != nil {
		e := snapshot.Extensions
		if e.ParentId != nil {
			task.ParentId = *e.ParentId
		}
		if e.ColumnId != nil {
			task.ColumnId = *e.ColumnId
		}
		if e.EstimatedDuration != nil {
			task.EstimatedDuration = *e.EstimatedDuration
		}
		if e.EstimatedPomo != nil {
			task.EstimatedPomo = *e.EstimatedPomo
		}
	}
	if snapshot.Interval != nil {
		task.StartDate, task.DueDate = snapshot.Interval.Start, snapshot.Interval.End
		task.IsAllDay, task.TimeZone = false, snapshot.Interval.Zone
	}
	if snapshot.Items != nil {
		task.Items = modelItems(*snapshot.Items)
	}
	if snapshot.RepeatFlag != nil {
		task.RepeatFlag = *snapshot.RepeatFlag
	}
	if snapshot.Reminders != nil {
		task.Reminders = append([]string(nil), (*snapshot.Reminders)...)
	}
	if snapshot.Kind != nil {
		task.Kind = *snapshot.Kind
	}
}

func applyFrozenEdit(edit *model.TaskEdit, snapshot *store.FeatureSnapshot) {
	if snapshot.Extensions != nil {
		e := snapshot.Extensions
		edit.ParentId, edit.ColumnId = e.ParentId, e.ColumnId
		edit.EstimatedDuration, edit.EstimatedPomo = e.EstimatedDuration, e.EstimatedPomo
	}
	if snapshot.Interval != nil {
		edit.StartDate, edit.DueDate = model.NewEditTime(snapshot.Interval.Start), model.NewEditTime(snapshot.Interval.End)
		edit.IsAllDay, edit.TimeZone = model.Ptr(false), model.Ptr(snapshot.Interval.Zone)
	}
	if snapshot.Items != nil {
		items := model.EditList[model.Item](modelItems(*snapshot.Items))
		edit.Items = &items
	}
	if snapshot.RepeatFlag != nil {
		edit.RepeatFlag = model.Ptr(*snapshot.RepeatFlag)
	}
	if snapshot.Reminders != nil {
		reminders := model.EditList[string](append([]string(nil), (*snapshot.Reminders)...))
		edit.Reminders = &reminders
	}
	if snapshot.Kind != nil {
		edit.Kind = model.Ptr(*snapshot.Kind)
	}
}

func modelItems(items []store.FeatureItemSnapshot) []model.Item {
	out := make([]model.Item, len(items))
	for i, item := range items {
		start, _ := model.ParseTime(item.StartDate)
		completed, _ := model.ParseTime(item.CompletedTime)
		out[i] = model.Item{
			Key: item.Key, Id: item.ID, Title: item.Title, Status: model.ItemStatus(item.Status),
			SortOrder: item.SortOrder, StartDate: start, IsAllDay: item.IsAllDay,
			TimeZone: item.TimeZone, CompletedTime: completed,
		}
	}
	return out
}

func definitelyRejected(err error) bool {
	var guard *api.RequestGuardError
	if errors.As(err, &guard) {
		return true
	}
	var status *api.StatusError
	return errors.As(err, &status) && status.StatusCode >= 400 && status.StatusCode < 500 && status.StatusCode != http.StatusRequestTimeout
}
