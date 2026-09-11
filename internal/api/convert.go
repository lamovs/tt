package api

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"

	"github.com/movsar/tt/internal/model"
)

type Warning struct {
	TaskID string `json:"task_id,omitempty"`
	ItemID string `json:"item_id,omitempty"`
	Field  string `json:"field"`
	Value  string `json:"value"`
	Detail string `json:"detail"`
}

func (w Warning) String() string {
	var b strings.Builder
	if w.TaskID != "" {
		fmt.Fprintf(&b, "task %s", w.TaskID)
		if w.ItemID != "" {
			fmt.Fprintf(&b, " item %s", w.ItemID)
		}
		b.WriteString(": ")
	}
	fmt.Fprintf(&b, "%s %q: %s", w.Field, w.Value, w.Detail)
	return b.String()
}

type conv struct {
	taskID   string
	itemID   string
	warnings []Warning
}

func (c *conv) warn(field, value, detail string) {
	c.warnings = append(c.warnings, Warning{
		TaskID: c.taskID, ItemID: c.itemID, Field: field, Value: value, Detail: detail,
	})
}

func (c *conv) priority(n int) model.Priority {
	p, err := model.PriorityFromWire(n)
	if err != nil {
		c.warn("priority", strconv.Itoa(n), err.Error()+"; using none")
		return model.PriorityNone
	}
	return p
}

func (c *conv) taskStatus(n int) model.TaskStatus {
	s, err := model.TaskStatusFromWire(n)
	if err != nil {
		c.warn("status", strconv.Itoa(n), err.Error()+"; using done")
		return model.TaskDone
	}
	return s
}

func (c *conv) itemStatus(n int) model.ItemStatus {
	s, err := model.ItemStatusFromWire(n)
	if err != nil {
		c.warn("status", strconv.Itoa(n), err.Error()+"; using done")
		return model.ItemDone
	}
	return s
}

func (c *conv) time(field, s string) model.Time {
	v, err := model.ParseTime(s)
	if err != nil {
		c.warn(field, s, err.Error()+"; using no date")
		return model.Time{}
	}
	return v
}

func TaskToModel(t Task) (model.Task, []Warning) {
	c := conv{taskID: t.ID}
	out := model.Task{
		ParentId:       t.ParentID,
		ChildIds:       copyStrings(t.ChildIDs),
		ColumnId:       t.ColumnID,
		ColumnName:     t.ColumnName,
		FocusSummaries: append([]byte(nil), t.FocusSummaries...),
		Id:             t.ID,
		ProjectId:      t.ProjectID,
		Title:          t.Title,
		Content:        t.Content,
		Status:         c.taskStatus(t.Status),
		Priority:       c.priority(t.Priority),
		DueDate:        c.time("dueDate", t.DueDate),
		StartDate:      c.time("startDate", t.StartDate),
		IsAllDay:       t.IsAllDay,
		TimeZone:       t.TimeZone,
		RepeatFlag:     t.RepeatFlag,
		Reminders:      copyStrings(t.Reminders),
		Tags:           copyStrings(t.Tags),
		Kind:           t.Kind,
		SortOrder:      t.SortOrder,
		CreatedTime:    c.time("createdTime", t.CreatedTime),
		ModifiedTime:   c.time("modifiedTime", t.ModifiedTime),
		CompletedTime:  c.time("completedTime", t.CompletedTime),
	}
	if bytes.Equal(bytes.TrimSpace(out.FocusSummaries), []byte("null")) {
		out.FocusSummaries = nil
	}
	estimates, err := model.ParseTaskEstimates(t.FocusSummaries)
	if err != nil {
		c.warn("focusSummaries", string(t.FocusSummaries), err.Error()+"; estimates are unavailable")
	} else {
		if estimates.EstimatedDuration != nil {
			out.EstimatedDuration = *estimates.EstimatedDuration
		}
		if estimates.EstimatedPomo != nil {
			out.EstimatedPomo = *estimates.EstimatedPomo
		}
	}
	for _, it := range t.Items {
		c.itemID = it.ID
		out.Items = append(out.Items, model.Item{
			Id:            it.ID,
			Title:         it.Title,
			Status:        c.itemStatus(it.Status),
			SortOrder:     it.SortOrder,
			StartDate:     c.time("startDate", it.StartDate),
			IsAllDay:      it.IsAllDay,
			TimeZone:      it.TimeZone,
			CompletedTime: c.time("completedTime", it.CompletedTime),
		})
	}
	c.itemID = ""
	return out, c.warnings
}

func TasksToModel(ts []Task) ([]model.Task, []Warning) {
	out := make([]model.Task, 0, len(ts))
	var warnings []Warning
	for _, t := range ts {
		m, w := TaskToModel(t)
		out = append(out, m)
		warnings = append(warnings, w...)
	}
	return out, warnings
}

func ProjectToModel(p Project) model.Project {
	return model.Project{
		Color:      p.Color,
		GroupId:    p.GroupID,
		ViewMode:   p.ViewMode,
		Permission: p.Permission,
		Id:         p.ID,
		Name:       p.Name,
		Kind:       p.Kind,
		SortOrder:  p.SortOrder,
		Closed:     p.Closed,
	}
}

func ProjectsToModel(ps []Project) []model.Project {
	out := make([]model.Project, 0, len(ps))
	for _, p := range ps {
		out = append(out, ProjectToModel(p))
	}
	return out
}

func TaskCreateFrom(t model.Task) TaskCreate {
	out := TaskCreate{
		ParentID:   t.ParentId,
		ProjectID:  t.ProjectId,
		Title:      t.Title,
		Content:    t.Content,
		Status:     t.Status.Wire(),
		Priority:   t.Priority.Wire(),
		DueDate:    t.DueDate.String(),
		StartDate:  t.StartDate.String(),
		IsAllDay:   t.IsAllDay,
		TimeZone:   t.TimeZone,
		RepeatFlag: t.RepeatFlag,
		Reminders:  copyStrings(t.Reminders),
		Tags:       copyStrings(t.Tags),
		Kind:       t.Kind,
		SortOrder:  t.SortOrder,
	}
	if t.EstimatedDuration != 0 || t.EstimatedPomo != 0 {
		out.FocusSummaries = []WritableFocusSummary{{EstimatedDuration: Ptr(t.EstimatedDuration), EstimatedPomo: Ptr(t.EstimatedPomo)}}
	}
	for _, it := range t.Items {
		out.Items = append(out.Items, itemFromModel(it))
	}
	return out
}

func TaskUpdateFrom(taskID, projectID string, e model.TaskEdit) TaskUpdate {
	u := TaskUpdate{ID: taskID, ProjectID: projectID}
	if e.ColumnId != nil {
		u.ColumnID = Ptr(*e.ColumnId)
	}
	if e.ParentId != nil {
		u.ParentID = Ptr(*e.ParentId)
	}
	if e.EstimatedDuration != nil || e.EstimatedPomo != nil {
		summary := WritableFocusSummary{}
		if e.EstimatedDuration != nil {
			summary.EstimatedDuration = Ptr(*e.EstimatedDuration)
		}
		if e.EstimatedPomo != nil {
			summary.EstimatedPomo = Ptr(*e.EstimatedPomo)
		}
		u.FocusSummaries = Ptr([]WritableFocusSummary{summary})
	}
	if e.Title != nil {
		u.Title = Ptr(*e.Title)
	}
	if e.Content != nil {
		u.Content = Ptr(*e.Content)
	}
	if e.Priority != nil {
		u.Priority = Ptr(e.Priority.Wire())
	}
	if e.Status != nil {
		u.Status = Ptr(e.Status.Wire())
	}
	if e.DueDate != nil {
		u.DueDate = Ptr(scheduleDateFromEdit(e.DueDate))
	}
	if e.StartDate != nil {
		u.StartDate = Ptr(scheduleDateFromEdit(e.StartDate))
	}
	if e.CompletedTime != nil {
		u.CompletedTime = Ptr(e.CompletedTime.String())
	}
	if e.IsAllDay != nil {
		u.IsAllDay = Ptr(*e.IsAllDay)
	}
	if e.TimeZone != nil {
		u.TimeZone = Ptr(*e.TimeZone)
	}
	if e.RepeatFlag != nil {
		u.RepeatFlag = Ptr(*e.RepeatFlag)
	}
	if e.Reminders != nil {
		u.Reminders = Ptr(copyList(*e.Reminders))
	}
	if e.Tags != nil {
		u.Tags = Ptr(copyList(*e.Tags))
	}
	if e.Kind != nil {
		u.Kind = Ptr(*e.Kind)
	}
	if e.SortOrder != nil {
		u.SortOrder = Ptr(*e.SortOrder)
	}
	if e.Items != nil {
		items := make([]WritableChecklistItem, 0, len(*e.Items))
		for _, it := range *e.Items {
			items = append(items, itemFromModel(it))
		}
		u.Items = Ptr(items)
	}
	return u
}

func scheduleDateFromEdit(value *model.EditTime) string {
	if value.IsZero() {
		return "1970-01-01T00:00:00.000+0000"
	}
	return value.String()
}

func itemFromModel(it model.Item) WritableChecklistItem {
	return WritableChecklistItem{
		ID:            it.Id,
		Title:         it.Title,
		Status:        it.Status.Wire(),
		SortOrder:     it.SortOrder,
		StartDate:     it.StartDate.String(),
		IsAllDay:      it.IsAllDay,
		TimeZone:      it.TimeZone,
		CompletedTime: it.CompletedTime.String(),
	}
}

func copyStrings(v []string) []string {
	if len(v) == 0 {
		return nil
	}
	return append([]string(nil), v...)
}

func copyList(v []string) []string {
	out := make([]string, len(v))
	copy(out, v)
	return out
}
