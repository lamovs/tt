package api

func Ptr[T any](v T) *T {
	return &v
}

type TaskUpdate struct {
	ColumnID       *string                 `json:"columnId,omitempty"`
	ParentID       *string                 `json:"parentId,omitempty"`
	FocusSummaries *[]WritableFocusSummary `json:"focusSummaries,omitempty"`
	ID             string                  `json:"id"`
	ProjectID      string                  `json:"projectId"`

	Title    *string `json:"title,omitempty"`
	Content  *string `json:"content,omitempty"`
	Desc     *string `json:"desc,omitempty"`
	IsAllDay *bool   `json:"isAllDay,omitempty"`

	StartDate     *string                  `json:"startDate,omitempty"`
	DueDate       *string                  `json:"dueDate,omitempty"`
	TimeZone      *string                  `json:"timeZone,omitempty"`
	Reminders     *[]string                `json:"reminders,omitempty"`
	Tags          *[]string                `json:"tags,omitempty"`
	RepeatFlag    *string                  `json:"repeatFlag,omitempty"`
	Priority      *int                     `json:"priority,omitempty"`
	SortOrder     *int64                   `json:"sortOrder,omitempty"`
	Items         *[]WritableChecklistItem `json:"items,omitempty"`
	Kind          *string                  `json:"kind,omitempty"`
	Status        *int                     `json:"status,omitempty"`
	CompletedTime *string                  `json:"completedTime,omitempty"`
}

type ProjectUpdate struct {
	ID string `json:"id"`

	Name      *string `json:"name,omitempty"`
	Color     *string `json:"color,omitempty"`
	SortOrder *int64  `json:"sortOrder,omitempty"`
	Closed    *bool   `json:"closed,omitempty"`
	GroupID   *string `json:"groupId,omitempty"`
	ViewMode  *string `json:"viewMode,omitempty"`
	Kind      *string `json:"kind,omitempty"`
}
