package api

import "encoding/json"

type Task struct {
	ID        string `json:"id,omitempty"`
	ProjectID string `json:"projectId"`
	Title     string `json:"title"`
	IsAllDay  bool   `json:"isAllDay,omitempty"`

	IsFloating    bool   `json:"isFloating,omitempty"`
	CreatedTime   string `json:"createdTime,omitempty"`
	ModifiedTime  string `json:"modifiedTime,omitempty"`
	CompletedTime string `json:"completedTime,omitempty"`
	Content       string `json:"content,omitempty"`

	Desc      string   `json:"desc,omitempty"`
	DueDate   string   `json:"dueDate,omitempty"`
	StartDate string   `json:"startDate,omitempty"`
	TimeZone  string   `json:"timeZone,omitempty"`
	Tags      []string `json:"tags,omitempty"`

	ColumnID       string          `json:"columnId,omitempty"`
	ColumnName     string          `json:"columnName,omitempty"`
	ParentID       string          `json:"parentId,omitempty"`
	ChildIDs       []string        `json:"childIds,omitempty"`
	FocusSummaries json.RawMessage `json:"focusSummaries,omitempty"`

	AssigneeUsername string `json:"assigneeUsername,omitempty"`

	Etag string `json:"etag,omitempty"`

	Etimestamp int64 `json:"etimestamp,omitempty"`

	Progress int `json:"progress,omitempty"`

	Items []ChecklistItem `json:"items,omitempty"`

	Priority   int      `json:"priority,omitempty"`
	Reminders  []string `json:"reminders,omitempty"`
	RepeatFlag string   `json:"repeatFlag,omitempty"`
	SortOrder  int64    `json:"sortOrder,omitempty"`

	Status int `json:"status,omitempty"`

	Kind string `json:"kind,omitempty"`
}

type ChecklistItem struct {
	ID    string `json:"id,omitempty"`
	Title string `json:"title"`

	Status        int    `json:"status,omitempty"`
	SortOrder     int64  `json:"sortOrder,omitempty"`
	StartDate     string `json:"startDate,omitempty"`
	IsAllDay      bool   `json:"isAllDay,omitempty"`
	TimeZone      string `json:"timeZone,omitempty"`
	CompletedTime string `json:"completedTime,omitempty"`
}

type WritableChecklistItem struct {
	ID            string `json:"id,omitempty"`
	Title         string `json:"title"`
	Status        int    `json:"status"`
	SortOrder     int64  `json:"sortOrder"`
	StartDate     string `json:"startDate"`
	IsAllDay      bool   `json:"isAllDay"`
	TimeZone      string `json:"timeZone"`
	CompletedTime string `json:"completedTime"`
}

type TaskCreate struct {
	ParentID       string                  `json:"parentId,omitempty"`
	FocusSummaries []WritableFocusSummary  `json:"focusSummaries,omitempty"`
	ProjectID      string                  `json:"projectId"`
	Title          string                  `json:"title"`
	Content        string                  `json:"content,omitempty"`
	DueDate        string                  `json:"dueDate,omitempty"`
	StartDate      string                  `json:"startDate,omitempty"`
	IsAllDay       bool                    `json:"isAllDay,omitempty"`
	TimeZone       string                  `json:"timeZone,omitempty"`
	Tags           []string                `json:"tags,omitempty"`
	Items          []WritableChecklistItem `json:"-"`

	ItemsPresent bool `json:"-"`

	AllDayPresent bool     `json:"-"`
	Priority      int      `json:"priority,omitempty"`
	Reminders     []string `json:"reminders,omitempty"`
	RepeatFlag    string   `json:"repeatFlag,omitempty"`
	SortOrder     int64    `json:"sortOrder,omitempty"`
	Status        int      `json:"status,omitempty"`
	Kind          string   `json:"kind,omitempty"`
}

type WritableFocusSummary struct {
	EstimatedDuration *int64 `json:"estimatedDuration,omitempty"`
	EstimatedPomo     *int   `json:"estimatedPomo,omitempty"`
}

func (t TaskCreate) MarshalJSON() ([]byte, error) {
	type wire TaskCreate
	var items *[]WritableChecklistItem
	var allDay *bool
	if t.AllDayPresent || t.IsAllDay {
		allDay = &t.IsAllDay
	}
	if t.ItemsPresent || len(t.Items) != 0 {
		copyOfItems := append([]WritableChecklistItem(nil), t.Items...)
		if copyOfItems == nil {
			copyOfItems = []WritableChecklistItem{}
		}
		items = &copyOfItems
	}
	return json.Marshal(struct {
		wire
		Items    *[]WritableChecklistItem `json:"items,omitempty"`
		IsAllDay *bool                    `json:"isAllDay,omitempty"`
	}{wire: wire(t), Items: items, IsAllDay: allDay})
}

type Project struct {
	ID         string          `json:"id,omitempty"`
	Name       string          `json:"name"`
	Color      string          `json:"color,omitempty"`
	SortOrder  int64           `json:"sortOrder,omitempty"`
	Closed     bool            `json:"closed,omitempty"`
	GroupID    string          `json:"groupId,omitempty"`
	ViewMode   string          `json:"viewMode,omitempty"`
	Permission string          `json:"permission,omitempty"`
	Kind       string          `json:"kind,omitempty"`
	Raw        json.RawMessage `json:"-"`
}

type Column struct {
	ID        string          `json:"id,omitempty"`
	ProjectID string          `json:"projectId,omitempty"`
	Name      string          `json:"name,omitempty"`
	SortOrder int64           `json:"sortOrder,omitempty"`
	Raw       json.RawMessage `json:"-"`
}

type ProjectData struct {
	Project Project  `json:"project"`
	Tasks   []Task   `json:"tasks"`
	Columns []Column `json:"columns"`
}

type RawTask struct {
	Task Task
	Raw  json.RawMessage
}

type ProjectDataRaw struct {
	Project Project
	Tasks   []RawTask
	Columns []Column
}

type projectDataRaw struct {
	Project Project            `json:"project"`
	Tasks   *[]json.RawMessage `json:"tasks"`
	Columns []Column           `json:"columns"`
}
