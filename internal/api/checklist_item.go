package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/movsar/tt/internal/model"
)

func (item *ChecklistItem) UnmarshalJSON(raw []byte) error {
	type wire ChecklistItem
	var decoded struct {
		wire
		StartDate     json.RawMessage `json:"startDate"`
		CompletedTime json.RawMessage `json:"completedTime"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return err
	}
	start, err := decodeChecklistTime(decoded.StartDate)
	if err != nil {
		return fmt.Errorf("checklist startDate: %w", err)
	}
	completed, err := decodeChecklistTime(decoded.CompletedTime)
	if err != nil {
		return fmt.Errorf("checklist completedTime: %w", err)
	}
	decoded.wire.StartDate = start
	decoded.wire.CompletedTime = completed
	*item = ChecklistItem(decoded.wire)
	return nil
}

func decodeChecklistTime(raw json.RawMessage) (string, error) {
	stamp := bytes.TrimSpace(raw)
	if len(stamp) == 0 {
		return "", nil
	}
	if stamp[0] == '"' {
		var value string
		if err := json.Unmarshal(stamp, &value); err != nil {
			return "", err
		}
		return value, nil
	}
	var millis int64
	if bytes.Equal(stamp, []byte("null")) || json.Unmarshal(stamp, &millis) != nil {
		return "", errors.New("must be a string or integral epoch milliseconds")
	}
	value := time.UnixMilli(millis).UTC()
	if value.Year() < 0 || value.Year() > 9999 || value.IsZero() {
		return "", errors.New("milliseconds cannot be represented as a timestamp")
	}
	return value.Format(model.TimeLayout), nil
}
