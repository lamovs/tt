package model

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

const TimeLayout = "2006-01-02T15:04:05.000-0700"

const timeLayoutSeconds = "2006-01-02T15:04:05-0700"

const storeLayout = "2006-01-02T15:04:05.000Z"

type Time struct {
	time.Time
}

func NewTime(t time.Time) Time { return Time{t} }

func ParseTime(s string) (Time, error) {
	v := strings.TrimSpace(s)
	if v == "" {
		return Time{}, nil
	}
	for _, layout := range []string{TimeLayout, timeLayoutSeconds, time.RFC3339} {
		if t, err := time.Parse(layout, v); err == nil {
			return Time{t}, nil
		}
	}
	return Time{}, fmt.Errorf("invalid timestamp %q (want %q)", s, TimeLayout)
}

func (t Time) String() string {
	if t.IsZero() {
		return ""
	}
	return t.Format(TimeLayout)
}

func (t Time) StoreString() string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(storeLayout)
}

func ParseStoreTime(s string) (Time, error) { return ParseTime(s) }

func (t Time) MarshalText() ([]byte, error) { return []byte(t.String()), nil }

func (t *Time) UnmarshalText(b []byte) error {
	v, err := ParseTime(string(b))
	if err != nil {
		return err
	}
	*t = v
	return nil
}

func (t Time) MarshalJSON() ([]byte, error) {
	if t.IsZero() {
		return []byte("null"), nil
	}
	return json.Marshal(t.String())
}

func (t *Time) UnmarshalJSON(b []byte) error {
	if bytes.Equal(b, []byte("null")) {
		*t = Time{}
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	return t.UnmarshalText([]byte(s))
}
