package model

import "encoding/json"

func Ptr[T any](v T) *T { return &v }

type EditTime struct {
	Time
}

func NewEditTime(t Time) *EditTime { return &EditTime{t} }

func (t EditTime) MarshalJSON() ([]byte, error) { return json.Marshal(t.Time.String()) }

func (t *EditTime) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	v, err := ParseTime(s)
	if err != nil {
		return err
	}
	t.Time = v
	return nil
}

type EditList[T any] []T

func NewEditList[T any](v []T) *EditList[T] {
	l := EditList[T](v)
	return &l
}

func (l EditList[T]) MarshalJSON() ([]byte, error) {
	if l == nil {
		return []byte("[]"), nil
	}
	return json.Marshal([]T(l))
}

type TaskEdit struct {
	Title         *string           `json:"title,omitempty"`
	Content       *string           `json:"content,omitempty"`
	Priority      *Priority         `json:"priority,omitempty"`
	Status        *TaskStatus       `json:"status,omitempty"`
	DueDate       *EditTime         `json:"due_date,omitempty"`
	StartDate     *EditTime         `json:"start_date,omitempty"`
	CompletedTime *EditTime         `json:"completed_time,omitempty"`
	IsAllDay      *bool             `json:"is_all_day,omitempty"`
	TimeZone      *string           `json:"time_zone,omitempty"`
	RepeatFlag    *string           `json:"repeat_flag,omitempty"`
	Reminders     *EditList[string] `json:"reminders,omitempty"`
	Tags          *EditList[string] `json:"tags,omitempty"`
	Kind          *string           `json:"kind,omitempty"`
	SortOrder     *int64            `json:"sort_order,omitempty"`
	Items         *EditList[Item]   `json:"items,omitempty"`

	ProjectId         *string `json:"project_id,omitempty"`
	ParentId          *string `json:"parent_id,omitempty"`
	ColumnId          *string `json:"column_id,omitempty"`
	EstimatedDuration *int64  `json:"estimated_duration,omitempty"`
	EstimatedPomo     *int    `json:"estimated_pomo,omitempty"`
}

func (e TaskEdit) IsEmpty() bool {
	return e == TaskEdit{}
}
