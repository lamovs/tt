package model

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestParsePriority(t *testing.T) {
	cases := map[string]Priority{
		"none":   PriorityNone,
		"low":    PriorityLow,
		"med":    PriorityMedium,
		"medium": PriorityMedium,
		"HIGH":   PriorityHigh,
		" high ": PriorityHigh,
	}
	for in, want := range cases {
		got, err := ParsePriority(in)
		if err != nil {
			t.Fatalf("ParsePriority(%q): %v", in, err)
		}
		if got != want {
			t.Errorf("ParsePriority(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestParsePriorityError(t *testing.T) {
	_, err := ParsePriority("hgh")
	if err == nil {
		t.Fatal("want error")
	}
	const want = `unknown priority "hgh" (allowed: none, low, med, high)`
	if err.Error() != want {
		t.Errorf("got %q, want %q", err, want)
	}
}

func TestPriorityCanonicalString(t *testing.T) {
	if got := PriorityMedium.String(); got != "med" {
		t.Errorf("medium prints as %q, want \"med\"", got)
	}
	for _, name := range PriorityNames() {
		p, err := ParsePriority(name)
		if err != nil {
			t.Fatalf("name %q from the table does not parse: %v", name, err)
		}
		if p.String() != name {
			t.Errorf("round trip %q -> %v -> %q", name, int(p), p.String())
		}
	}
}

func TestPriorityWire(t *testing.T) {
	wire := map[int]Priority{0: PriorityNone, 1: PriorityLow, 3: PriorityMedium, 5: PriorityHigh}
	for n, want := range wire {
		got, err := PriorityFromWire(n)
		if err != nil {
			t.Fatalf("PriorityFromWire(%d): %v", n, err)
		}
		if got != want || got.Wire() != n {
			t.Errorf("PriorityFromWire(%d) = %v (wire %d)", n, got, got.Wire())
		}
	}
	if _, err := PriorityFromWire(2); err == nil {
		t.Error("priority 2 must be rejected")
	} else if !strings.Contains(err.Error(), "allowed: 0, 1, 3, 5") {
		t.Errorf("error must list wire values, got %q", err)
	}
}

func TestStatusWireValuesDiffer(t *testing.T) {
	if TaskDone.Wire() != 2 {
		t.Errorf("task done wire = %d, want 2", TaskDone.Wire())
	}
	if ItemDone.Wire() != 1 {
		t.Errorf("item done wire = %d, want 1", ItemDone.Wire())
	}
	if _, err := TaskStatusFromWire(1); err == nil {
		t.Error("task status 1 must be rejected")
	}
	if _, err := ItemStatusFromWire(2); err == nil {
		t.Error("item status 2 must be rejected")
	}
}

func TestEnumTablesRoundTrip(t *testing.T) {
	tables := []struct {
		names []string
		parse func(string) (string, error)
	}{
		{TaskStatusNames(), func(s string) (string, error) {
			v, err := ParseTaskStatus(s)
			return v.String(), err
		}},
		{ItemStatusNames(), func(s string) (string, error) {
			v, err := ParseItemStatus(s)
			return v.String(), err
		}},
		{EditorHintsNames(), func(s string) (string, error) {
			v, err := ParseEditorHints(s)
			return v.String(), err
		}},
		{SessionKindNames(), func(s string) (string, error) {
			v, err := ParseSessionKind(s)
			return v.String(), err
		}},
		{SessionOutcomeNames(), func(s string) (string, error) {
			v, err := ParseSessionOutcome(s)
			return v.String(), err
		}},
	}
	for _, tab := range tables {
		if len(tab.names) == 0 {
			t.Fatal("empty enum table")
		}
		for _, name := range tab.names {
			got, err := tab.parse(name)
			if err != nil {
				t.Fatalf("parse(%q): %v", name, err)
			}
			if got != name {
				t.Errorf("round trip %q -> %q", name, got)
			}
		}
	}
}

func TestEnumNamesAreExpected(t *testing.T) {
	cases := []struct {
		got  []string
		want string
	}{
		{PriorityNames(), "none, low, med, high"},
		{TaskStatusNames(), "open, done"},
		{ItemStatusNames(), "open, done"},
		{EditorHintsNames(), "full, short, none"},
		{SessionKindNames(), "focus, short_break, long_break"},
		{SessionOutcomeNames(), "done, aborted"},
	}
	for _, c := range cases {
		if got := strings.Join(c.got, ", "); got != c.want {
			t.Errorf("got %q, want %q", got, c.want)
		}
	}
}

func TestNamesAreCopies(t *testing.T) {
	names := PriorityNames()
	names[0] = "mutated"
	if PriorityNames()[0] != "none" {
		t.Error("the enum table leaks its name slice")
	}
}

func TestSessionOutcomeUnset(t *testing.T) {
	var o SessionOutcome
	if o.IsSet() || o.String() != "" {
		t.Errorf("zero outcome is %q, want unset", o.String())
	}

	if _, err := ParseSessionOutcome(""); err == nil {
		t.Error("empty outcome must not parse")
	}

	b, err := OutcomeUnset.MarshalText()
	if err != nil || len(b) != 0 {
		t.Fatalf("MarshalText of the unset outcome = %q, %v", b, err)
	}
	if b, err = OutcomeAborted.MarshalText(); err != nil || string(b) != "aborted" {
		t.Fatalf("MarshalText = %q, %v", b, err)
	}
	o = OutcomeDone
	if err := o.UnmarshalText(nil); err != nil || o != OutcomeUnset {
		t.Fatalf("UnmarshalText of empty text = %v, %v", o, err)
	}

	raw, err := json.Marshal(FocusSession{Id: "s1", Kind: SessionFocus})
	if err != nil {
		t.Fatalf("marshal a running session: %v", err)
	}
	var back FocusSession
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal %s: %v", raw, err)
	}
	if back.Id != "s1" || back.Outcome != OutcomeUnset {
		t.Fatalf("round trip gave %+v", back)
	}
}

func TestEnumTextMarshal(t *testing.T) {
	b, err := PriorityMedium.MarshalText()
	if err != nil || string(b) != "med" {
		t.Fatalf("MarshalText = %q, %v", b, err)
	}
	var h EditorHints
	if err := h.UnmarshalText([]byte("short")); err != nil || h != HintsShort {
		t.Fatalf("UnmarshalText = %v, %v", h, err)
	}
	if err := h.UnmarshalText([]byte("shrt")); err == nil {
		t.Error("want error for an unknown value")
	} else if h != HintsShort {
		t.Error("a failed unmarshal must not change the value")
	}
	if _, err := Priority(7).MarshalText(); err == nil {
		t.Error("want error for a value outside the table")
	}
}

func TestUnknownValueString(t *testing.T) {
	if got := Priority(7).String(); got != "priority(7)" {
		t.Errorf("got %q", got)
	}
}
