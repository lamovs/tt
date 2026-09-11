package api

import (
	"encoding/json"
	"fmt"
	"strings"
)

type ProjectGroup struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	SortOrder int64           `json:"sortOrder,omitempty"`
	ShowAll   bool            `json:"showAll,omitempty"`
	ViewMode  string          `json:"viewMode,omitempty"`
	Raw       json.RawMessage `json:"-"`
}

type ProjectGroupCreate struct {
	Name string `json:"name"`
}

type ProjectGroupUpdate struct {
	Name *string `json:"name,omitempty"`
}

type ColumnCreate struct {
	Name string `json:"name"`
}

type ColumnUpdate struct {
	Name *string `json:"name,omitempty"`
}

type Tag struct {
	Name      string          `json:"name"`
	Label     string          `json:"label"`
	SortOrder int64           `json:"sortOrder,omitempty"`
	Color     string          `json:"color,omitempty"`
	Parent    string          `json:"parent,omitempty"`
	Type      int             `json:"type,omitempty"`
	Raw       json.RawMessage `json:"-"`
}

type TagCreate struct {
	Name  string `json:"name"`
	Label string `json:"label"`
}

type Habit struct {
	ID              string          `json:"id"`
	Name            string          `json:"name"`
	IconRes         string          `json:"iconRes,omitempty"`
	Color           string          `json:"color,omitempty"`
	SortOrder       int64           `json:"sortOrder,omitempty"`
	Status          int             `json:"status,omitempty"`
	Encouragement   string          `json:"encouragement,omitempty"`
	TotalCheckIns   int             `json:"totalCheckIns,omitempty"`
	CreatedTime     string          `json:"createdTime,omitempty"`
	ModifiedTime    string          `json:"modifiedTime,omitempty"`
	ArchivedTime    string          `json:"archivedTime,omitempty"`
	Type            string          `json:"type,omitempty"`
	Goal            float64         `json:"goal,omitempty"`
	Step            float64         `json:"step,omitempty"`
	Unit            string          `json:"unit,omitempty"`
	Etag            string          `json:"etag,omitempty"`
	RepeatRule      string          `json:"repeatRule,omitempty"`
	Reminders       []string        `json:"reminders,omitempty"`
	RecordEnable    bool            `json:"recordEnable,omitempty"`
	SectionID       string          `json:"sectionId,omitempty"`
	TargetDays      int             `json:"targetDays,omitempty"`
	TargetStartDate int             `json:"targetStartDate,omitempty"`
	CompletedCycles int             `json:"completedCycles,omitempty"`
	ExDates         []string        `json:"exDates,omitempty"`
	Style           int             `json:"style,omitempty"`
	Raw             json.RawMessage `json:"-"`
}

type HabitUpdate struct {
	Name            *string   `json:"name,omitempty"`
	IconRes         *string   `json:"iconRes,omitempty"`
	Color           *string   `json:"color,omitempty"`
	SortOrder       *int64    `json:"sortOrder,omitempty"`
	Status          *int      `json:"status,omitempty"`
	Encouragement   *string   `json:"encouragement,omitempty"`
	Type            *string   `json:"type,omitempty"`
	Goal            *float64  `json:"goal,omitempty"`
	Step            *float64  `json:"step,omitempty"`
	Unit            *string   `json:"unit,omitempty"`
	RepeatRule      *string   `json:"repeatRule,omitempty"`
	Reminders       *[]string `json:"reminders,omitempty"`
	RecordEnable    *bool     `json:"recordEnable,omitempty"`
	SectionID       *string   `json:"sectionId,omitempty"`
	TargetDays      *int      `json:"targetDays,omitempty"`
	TargetStartDate *int      `json:"targetStartDate,omitempty"`
	CompletedCycles *int      `json:"completedCycles,omitempty"`
	ExDates         *[]string `json:"exDates,omitempty"`
	Style           *int      `json:"style,omitempty"`
}

type HabitCreate HabitUpdate

type HabitSection struct {
	ID           string          `json:"id"`
	Name         string          `json:"name"`
	SortOrder    int64           `json:"sortOrder,omitempty"`
	CreatedTime  string          `json:"createdTime,omitempty"`
	ModifiedTime string          `json:"modifiedTime,omitempty"`
	Etag         string          `json:"etag,omitempty"`
	Raw          json.RawMessage `json:"-"`
}

type HabitCheckinInput struct {
	Stamp  int      `json:"stamp"`
	Time   *string  `json:"time,omitempty"`
	OpTime *string  `json:"opTime,omitempty"`
	Value  *float64 `json:"value,omitempty"`
	Goal   *float64 `json:"goal,omitempty"`
	Status *int     `json:"status,omitempty"`
}

type HabitCheckinQuery struct {
	HabitIDs []string
	From     int
	To       int
}

type HabitCheckinItem struct {
	ID     string          `json:"id,omitempty"`
	Stamp  int             `json:"stamp"`
	Time   string          `json:"time,omitempty"`
	OpTime string          `json:"opTime,omitempty"`
	Value  *float64        `json:"value,omitempty"`
	Goal   *float64        `json:"goal,omitempty"`
	Status *int            `json:"status,omitempty"`
	Raw    json.RawMessage `json:"-"`
}

func (value *HabitCheckinItem) UnmarshalJSON(data []byte) error {
	type wire HabitCheckinItem
	var out wire
	if err := json.Unmarshal(data, &out); err != nil {
		return err
	}
	if err := validateDateStamp(out.Stamp); err != nil {
		return fmt.Errorf("invalid check-in response date: %w", ErrIncompleteAnswer)
	}
	out.Raw = append(json.RawMessage(nil), data...)
	*value = HabitCheckinItem(out)
	return nil
}

type HabitCheckin struct {
	ID           string             `json:"id,omitempty"`
	HabitID      string             `json:"habitId"`
	CreatedTime  string             `json:"createdTime,omitempty"`
	ModifiedTime string             `json:"modifiedTime,omitempty"`
	Etag         string             `json:"etag,omitempty"`
	Year         int                `json:"year,omitempty"`
	Checkins     []HabitCheckinItem `json:"checkins"`
	Raw          json.RawMessage    `json:"-"`
}

type Comment struct {
	ID             string          `json:"id"`
	UserID         int64           `json:"userId,omitempty"`
	Title          string          `json:"title"`
	CreatedTime    string          `json:"createdTime,omitempty"`
	ModifiedTime   string          `json:"modifiedTime,omitempty"`
	ReplyCommentID string          `json:"replyCommentId,omitempty"`
	ReplyUserID    int64           `json:"replyUserId,omitempty"`
	Raw            json.RawMessage `json:"-"`
}

type CommentCreate struct {
	Title string `json:"title"`
}

type Countdown struct {
	ID                string          `json:"id"`
	Type              int             `json:"type,omitempty"`
	IconRes           string          `json:"iconRes,omitempty"`
	Color             string          `json:"color,omitempty"`
	Name              string          `json:"name,omitempty"`
	Date              int             `json:"date,omitempty"`
	IgnoreYear        bool            `json:"ignoreYear,omitempty"`
	ShowCalendarType  int             `json:"showCalendarType,omitempty"`
	Reminders         []string        `json:"reminders,omitempty"`
	AnnoyingAlert     int             `json:"annoyingAlert,omitempty"`
	RepeatFlag        string          `json:"repeatFlag,omitempty"`
	Remark            string          `json:"remark,omitempty"`
	Status            int             `json:"status,omitempty"`
	SortOrder         int64           `json:"sortOrder,omitempty"`
	Style             string          `json:"style,omitempty"`
	StyleColor        []string        `json:"styleColor,omitempty"`
	DateDisplayFormat string          `json:"dateDisplayFormat,omitempty"`
	TimerMode         int             `json:"timerMode,omitempty"`
	ShowAge           bool            `json:"showAge,omitempty"`
	DaysOption        int             `json:"daysOption,omitempty"`
	ShowRemark        bool            `json:"showRemark,omitempty"`
	CreatedTime       string          `json:"createdTime,omitempty"`
	ModifiedTime      string          `json:"modifiedTime,omitempty"`
	Raw               json.RawMessage `json:"-"`
}

func decodeResource(data []byte, out any, identity string) (json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return nil, err
	}
	var id string
	if err := json.Unmarshal(fields[identity], &id); err != nil || strings.TrimSpace(id) == "" {
		return nil, fmt.Errorf("resource response has no valid %s: %w", identity, ErrIncompleteAnswer)
	}
	if err := json.Unmarshal(data, out); err != nil {
		return nil, err
	}
	return append(json.RawMessage(nil), data...), nil
}

func (value *ProjectGroup) UnmarshalJSON(data []byte) error {
	type wire ProjectGroup
	var out wire
	raw, err := decodeResource(data, &out, "id")
	if err == nil {
		out.Raw = raw
		*value = ProjectGroup(out)
	}
	return err
}

func (value *Tag) UnmarshalJSON(data []byte) error {
	type wire Tag
	var out wire
	raw, err := decodeResource(data, &out, "name")
	if err == nil {
		out.Raw = raw
		*value = Tag(out)
	}
	return err
}

func (value *Habit) UnmarshalJSON(data []byte) error {
	type wire Habit
	var out wire
	raw, err := decodeResource(data, &out, "id")
	if err == nil {
		out.Raw = raw
		*value = Habit(out)
	}
	return err
}

func (value *HabitSection) UnmarshalJSON(data []byte) error {
	type wire HabitSection
	var out wire
	raw, err := decodeResource(data, &out, "id")
	if err == nil {
		out.Raw = raw
		*value = HabitSection(out)
	}
	return err
}

func (value *HabitCheckin) UnmarshalJSON(data []byte) error {
	type wire HabitCheckin
	var out wire
	raw, err := decodeResource(data, &out, "habitId")
	if err != nil {
		return err
	}
	if out.Checkins == nil {
		return fmt.Errorf("habit check-in response has no entries: %w", ErrIncompleteAnswer)
	}
	seen := make(map[int]bool, len(out.Checkins))
	for _, item := range out.Checkins {
		if err := validateDateStamp(item.Stamp); err != nil {
			return fmt.Errorf("invalid check-in response date: %w", ErrIncompleteAnswer)
		}
		if out.Year != 0 && item.Stamp/10000 != out.Year {
			return fmt.Errorf("check-in response date is outside its aggregate year: %w", ErrIncompleteAnswer)
		}
		if seen[item.Stamp] {
			return fmt.Errorf("duplicate check-in response date: %w", ErrIncompleteAnswer)
		}
		seen[item.Stamp] = true
	}
	out.Raw = raw
	*value = HabitCheckin(out)
	return nil
}

func (value *Comment) UnmarshalJSON(data []byte) error {
	type wire Comment
	var out wire
	raw, err := decodeResource(data, &out, "id")
	if err == nil {
		out.Raw = raw
		*value = Comment(out)
	}
	return err
}

func (value *Countdown) UnmarshalJSON(data []byte) error {
	type wire Countdown
	var out wire
	raw, err := decodeResource(data, &out, "id")
	if err == nil {
		out.Raw = raw
		*value = Countdown(out)
	}
	return err
}
