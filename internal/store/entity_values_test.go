package store

import (
	"encoding/json"
	"testing"
)

func TestEntityNumericComparisonIsExactAndBounded(t *testing.T) {
	for _, tc := range []struct {
		a, b  string
		equal bool
	}{
		{`{"n":1}`, `{"n":1.0}`, true},
		{`[1e2,null]`, `[100,null]`, true},
		{`9007199254740993`, `9007199254740992`, false},
		{`1e999999999`, `2e999999999`, false},
		{`{"n":null}`, `{}`, false},
	} {
		if got := entityJSONEqual(json.RawMessage(tc.a), json.RawMessage(tc.b)); got != tc.equal {
			t.Errorf("%s vs %s = %v", tc.a, tc.b, got)
		}
	}
}
