package config

import (
	"errors"
	"strings"

	"github.com/movsar/tt/internal/model"
)

type FocusReference struct{ TaskID string }

func (r FocusReference) String() string {
	if r.TaskID == "" {
		return "none"
	}
	return "task:" + r.TaskID
}

func ParseFocusReference(value string) (FocusReference, error) {
	if value == "none" {
		return FocusReference{}, nil
	}
	if id, ok := strings.CutPrefix(value, "task:"); ok && model.ValidProjectID(id) {
		return FocusReference{TaskID: id}, nil
	}
	return FocusReference{}, errors.New("expected none or task:ID using letters, digits, '-' or '_'")
}

func SetDefaultFocus(src []byte, ref FocusReference) ([]byte, error) {
	if _, err := ParseFocusReference(ref.String()); err != nil {
		return nil, err
	}
	return setRootString(src, "default_focus", ref.String(), func(c *Config) { c.DefaultFocus = ref })
}
