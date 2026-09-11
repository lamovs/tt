package model

import (
	"bytes"
	"encoding/json"
	"errors"
)

type TaskEstimateFields struct {
	EstimatedDuration *int64
	EstimatedPomo     *int
}

func ParseTaskEstimates(raw json.RawMessage) (TaskEstimateFields, error) {
	var out TaskEstimateFields
	if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return out, nil
	}
	var summaries []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &summaries); err != nil {
		return out, errors.New("focus summaries must be an array of objects")
	}
	if len(summaries) > 1 {
		return out, errors.New("multiple focus summaries have no verified estimate selection rule")
	}
	if len(summaries) == 0 {
		return out, nil
	}
	if summaries[0] == nil {
		return out, errors.New("focus summary must be an object")
	}
	if value, present := summaries[0]["estimatedDuration"]; present {
		var duration int64
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) || json.Unmarshal(value, &duration) != nil || duration < 0 {
			return out, errors.New("focus summary estimated duration is not a nonnegative integer")
		}
		out.EstimatedDuration = &duration
	}
	if value, present := summaries[0]["estimatedPomo"]; present {
		var pomo int
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) || json.Unmarshal(value, &pomo) != nil || pomo < 0 || pomo > 60 {
			return out, errors.New("focus summary estimated Pomodoros are outside 0..60")
		}
		out.EstimatedPomo = &pomo
	}
	return out, nil
}
