package model

import (
	"encoding/json"
	"time"
)

type Project struct {
	Id         string
	Name       string
	Kind       string
	SortOrder  int64
	Closed     bool
	Color      string
	GroupId    string
	ViewMode   string
	Permission string
}

type Task struct {
	Id                string
	ProjectId         string
	Title             string
	Content           string
	Status            TaskStatus
	Priority          Priority
	DueDate           Time
	StartDate         Time
	IsAllDay          bool
	TimeZone          string
	RepeatFlag        string
	Reminders         []string
	Tags              []string
	Kind              string
	SortOrder         int64
	CreatedTime       Time
	ModifiedTime      Time
	CompletedTime     Time
	Items             []Item
	ParentId          string          `json:"ParentId,omitempty"`
	ChildIds          []string        `json:"ChildIds,omitempty"`
	ColumnId          string          `json:"ColumnId,omitempty"`
	ColumnName        string          `json:"ColumnName,omitempty"`
	EstimatedDuration int64           `json:"EstimatedDuration,omitempty"`
	EstimatedPomo     int             `json:"EstimatedPomo,omitempty"`
	FocusSummaries    json.RawMessage `json:"FocusSummaries,omitempty"`
}

type Item struct {
	Key           string `json:"-"`
	Id            string
	Title         string
	Status        ItemStatus
	SortOrder     int64
	StartDate     Time
	IsAllDay      bool
	TimeZone      string
	CompletedTime Time
}

type FocusSession struct {
	Id         string
	TaskId     string
	Kind       SessionKind
	StartedAt  time.Time
	EndedAt    time.Time
	PlannedSec int
	PauseSec   int
	Outcome    SessionOutcome
	Note       string
	SyncedAt   time.Time
}
