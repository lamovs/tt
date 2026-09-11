package model

import "testing"

func TestParseTaskEstimates(t *testing.T) {
	for _, raw := range []string{"", `null`, `[]`, `[{}]`, `[{"pomoCount":4}]`} {
		if _, err := ParseTaskEstimates([]byte(raw)); err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
	}
	value, err := ParseTaskEstimates([]byte(`[{"estimatedDuration":0,"estimatedPomo":0,"future":true}]`))
	if err != nil || value.EstimatedDuration == nil || *value.EstimatedDuration != 0 || value.EstimatedPomo == nil || *value.EstimatedPomo != 0 {
		t.Fatalf("explicit zeros lost: %+v %v", value, err)
	}
	for _, raw := range []string{`{}`, `[null]`, `[{},{}]`, `[{"estimatedDuration":null}]`, `[{"estimatedDuration":-1}]`, `[{"estimatedPomo":61}]`} {
		if _, err := ParseTaskEstimates([]byte(raw)); err == nil {
			t.Fatalf("ambiguous estimates accepted: %s", raw)
		}
	}
}
