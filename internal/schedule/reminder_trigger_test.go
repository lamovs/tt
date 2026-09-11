package schedule

import (
	"slices"
	"testing"
	"time"

	"github.com/movsar/tt/internal/model"
)

func TestParseTriggerPreservesComponentsAndRaw(t *testing.T) {
	raw := "TRIGGER;RELATED=START:-P1Y02M3W4DT5H6M7S"
	got, err := ParseTrigger(raw)
	if err != nil || got.Raw != raw || got.Relation != TriggerRelatedStart || !got.Negative || got.Years != 1 || got.Months != 2 || got.Weeks != 3 || got.Days != 4 || got.Hours != 5 || got.Minutes != 6 || got.Seconds != 7 {
		t.Fatalf("parsed = %+v, %v", got, err)
	}
	for _, raw := range []string{"TRIGGER:PT0S", "TRIGGER:-PT60M", "TRIGGER:P0Y", "TRIGGER:P1W", "TRIGGER;RELATED=END:PT90S", "TRIGGER;RELATED=START:P1M", "TRIGGER:PT9223372036S"} {
		parsed, err := ParseTrigger(raw)
		if err != nil || parsed.Raw != raw {
			t.Errorf("ParseTrigger(%q) = %+v, %v", raw, parsed, err)
		}
	}
}

func TestParseTriggerRejectsMalformedAndOverflow(t *testing.T) {
	for _, raw := range []string{
		"", "TRIGGER:", "TRIGGER:P", "TRIGGER:PT", "TRIGGER:P1DT", "TRIGGER:P1D2D", "TRIGGER:P1D1Y",
		"TRIGGER:PT1M1H", "TRIGGER:PT1H2H", "TRIGGER:P-1D", "TRIGGER:+P1D", "TRIGGER:--P1D",
		"TRIGGER:PT0.5S", "TRIGGER:PT1,5S", "TRIGGER:P1h", "TRIGGER:P1S", "TRIGGER:PT1D", "TRIGGER:P1Ygarbage",
		"TRIGGER;RELATED=DUE:PT1M", "TRIGGER;RELATED=START;RELATED=END:PT1M", "TRIGGER;related=START:PT1M",
		"TRIGGER;RELATED=START:PT1M ", "TRIGGER:PT1M\n", "TRIGGER:PT1\x00M", "TRIGGER:vendor-extension",
		"TRIGGER:P9223372036854775808Y", "TRIGGER:PT9223372036854775807H", "TRIGGER:PT9223372037S",
		"TRIGGER:PT2562047H47M17S",
	} {
		if parsed, err := ParseTrigger(raw); err == nil {
			t.Errorf("accepted %q: %+v", raw, parsed)
		}
	}
}

func TestRelatedReminderValidationPreservesRawCompatibility(t *testing.T) {
	values := []string{"TRIGGER;RELATED=START:-PT30S", "TRIGGER;RELATED=END:-P1Y", "TRIGGER:vendor-extension ", "TRIGGER;RELATED=END:vendor-extension "}
	got, err := ParseReminders(values)
	if err != nil || !slices.Equal(got, values) {
		t.Fatalf("raw reminders = %q, %v", got, err)
	}
	for _, raw := range values {
		if input := ReminderInput(raw); input != raw {
			t.Errorf("raw display changed %q to %q", raw, input)
		}
	}
	for _, raw := range []string{"TRIGGER;RELATED=START:", "TRIGGER;RELATED=DUE:PT1M", "TRIGGER;RELATED=START:PT1M\r", "TRIGGER;RELATED=END:PT1M\x1b"} {
		if err := ValidateReminder(raw); err == nil {
			t.Errorf("accepted invalid reminder %q", raw)
		}
	}
	if _, err := ParseReminders([]string{values[0], values[0]}); err == nil {
		t.Fatal("accepted duplicate related reminder")
	}
	original := model.Task{Reminders: []string{"unsupported raw value"}}
	change := Change{Reminders: &original.Reminders}
	if edit, err := change.Edit(original); err != nil || !edit.IsEmpty() {
		t.Fatalf("untouched unsupported reminder changed: %+v, %v", edit, err)
	}
}

func TestEvaluateTriggerExplicitClockAndPolicyGates(t *testing.T) {
	start := time.Date(2026, 9, 11, 9, 0, 30, 123456, time.UTC)
	end := start.Add(2 * time.Hour)
	reference := TriggerDates{Start: start, End: end, TimeZone: "UTC"}
	for _, test := range []struct {
		raw    string
		want   time.Time
		reason string
	}{
		{"TRIGGER;RELATED=START:-PT30S", start.Add(-30 * time.Second), ""},
		{"TRIGGER;RELATED=END:PT1H2M3S", end.Add(time.Hour + 2*time.Minute + 3*time.Second), ""},
		{"TRIGGER;RELATED=END:PT0S", end, ""},
		{"TRIGGER;RELATED=END:-PT90M", end.Add(-90 * time.Minute), ""},
		{"TRIGGER:PT0S", time.Time{}, "default_reference_unverified"},
		{"TRIGGER;RELATED=START:P1D", time.Time{}, "calendar_days_unverified"},
		{"TRIGGER;RELATED=END:P1W", time.Time{}, "calendar_days_unverified"},
		{"TRIGGER;RELATED=END:P0D", time.Time{}, "calendar_days_unverified"},
		{"TRIGGER;RELATED=END:P1M", time.Time{}, "calendar_months_unverified"},
		{"TRIGGER;RELATED=END:P0Y", time.Time{}, "calendar_months_unverified"},
		{"TRIGGER:vendor-extension", time.Time{}, "unsupported_trigger"},
	} {
		t.Run(test.raw, func(t *testing.T) {
			assertTriggerResult(t, EvaluateTrigger(test.raw, reference, TriggerPolicy{}), test.want, test.reason)
		})
	}
	assertTriggerResult(t, EvaluateTrigger("TRIGGER:-PT30S", reference, TriggerPolicy{DefaultRelation: TriggerRelatedStart}), start.Add(-30*time.Second), "")
	assertTriggerResult(t, EvaluateTrigger("TRIGGER:PT0S", reference, TriggerPolicy{DefaultRelation: "DUE"}), time.Time{}, "invalid_reference_policy")
	assertTriggerResult(t, EvaluateTrigger("TRIGGER;RELATED=END:PT0S", TriggerDates{}, TriggerPolicy{}), time.Time{}, "missing_end")
	assertTriggerResult(t, EvaluateTrigger("TRIGGER;RELATED=START:PT0S", TriggerDates{End: end}, TriggerPolicy{}), time.Time{}, "missing_start")
	assertTriggerResult(t, EvaluateTrigger("TRIGGER;RELATED=END:PT0S", TriggerDates{End: end, TimeZone: "bad-zone"}, TriggerPolicy{}), time.Time{}, "invalid_timezone")
	assertTriggerResult(t, EvaluateTrigger("TRIGGER;RELATED=END:PT0S", TriggerDates{End: end, AllDay: true}, TriggerPolicy{}), time.Time{}, "all_day_unverified")
}

func TestEvaluateTriggerExplicitCalendarPolicies(t *testing.T) {
	for _, test := range []struct {
		name, raw, anchor, want, reason string
		policy                          TriggerPolicy
	}{
		{"clamp", "P1M", "2026-01-31T12:00:00Z", "2026-02-28T12:00:00Z", "", TriggerPolicy{Months: TriggerMonthsClamp}},
		{"normalize", "P1M", "2026-01-31T12:00:00Z", "2026-03-03T12:00:00Z", "", TriggerPolicy{Months: TriggerMonthsNormalize}},
		{"exact", "P1M", "2026-01-31T12:00:00Z", "", "calendar_date_missing", TriggerPolicy{Months: TriggerMonthsExact}},
		{"leap-year", "P1Y", "2024-02-29T12:00:00Z", "2025-02-28T12:00:00Z", "", TriggerPolicy{Months: TriggerMonthsClamp}},
		{"negative-mixed", "-P1M1DT30S", "2026-03-31T12:00:00Z", "2026-02-27T11:59:30Z", "", TriggerPolicy{Months: TriggerMonthsClamp, Days: TriggerDaysWallClock}},
		{"weeks-days", "P1W2D", "2026-09-11T12:00:00Z", "2026-09-20T12:00:00Z", "", TriggerPolicy{Days: TriggerDaysWallClock}},
		{"month-overflow", "P9223372036854775807Y", "2026-01-31T12:00:00Z", "", "date_out_of_range", TriggerPolicy{Months: TriggerMonthsClamp}},
		{"day-overflow", "P9223372036854775807W", "2026-01-31T12:00:00Z", "", "date_out_of_range", TriggerPolicy{Days: TriggerDaysWallClock}},
		{"elapsed-overflow", "P200000D", "2026-01-31T12:00:00Z", "", "duration_out_of_range", TriggerPolicy{Days: TriggerDaysElapsed}},
		{"year-boundary", "P1Y", "9999-01-01T12:00:00Z", "", "date_out_of_range", TriggerPolicy{Months: TriggerMonthsClamp}},
		{"clock-boundary", "PT2H", "9999-12-31T23:00:00Z", "", "date_out_of_range", TriggerPolicy{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			anchor, _ := time.Parse(time.RFC3339, test.anchor)
			want, _ := time.Parse(time.RFC3339, test.want)
			assertTriggerResult(t, EvaluateTrigger("TRIGGER;RELATED=END:"+test.raw, TriggerDates{End: anchor, TimeZone: "UTC"}, test.policy), want, test.reason)
		})
	}
}

func TestEvaluateTriggerDSTAndAllDayPolicies(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	anchor := time.Date(2026, 3, 7, 12, 0, 0, 0, loc)
	reference := TriggerDates{End: anchor, TimeZone: loc.String()}
	assertTriggerResult(t, EvaluateTrigger("TRIGGER;RELATED=END:P1D", reference, TriggerPolicy{Days: TriggerDaysWallClock}), time.Date(2026, 3, 8, 12, 0, 0, 0, loc), "")
	assertTriggerResult(t, EvaluateTrigger("TRIGGER;RELATED=END:P1D", reference, TriggerPolicy{Days: TriggerDaysElapsed}), time.Date(2026, 3, 8, 13, 0, 0, 0, loc), "")
	reference.End = time.Date(2026, 3, 7, 2, 30, 0, 0, loc)
	assertTriggerResult(t, EvaluateTrigger("TRIGGER;RELATED=END:P1D", reference, TriggerPolicy{Days: TriggerDaysWallClock}), time.Time{}, "nonexistent_local_time")
	reference.End = time.Date(2026, 10, 31, 1, 30, 0, 0, loc)
	assertTriggerResult(t, EvaluateTrigger("TRIGGER;RELATED=END:P1D", reference, TriggerPolicy{Days: TriggerDaysWallClock}), time.Time{}, "ambiguous_local_time")
	assertTriggerResult(t, EvaluateTrigger("TRIGGER;RELATED=END:PT24H", reference, TriggerPolicy{}), reference.End.Add(24*time.Hour), "")
	reference = TriggerDates{End: anchor, AllDay: true, TimeZone: loc.String()}
	assertTriggerResult(t, EvaluateTrigger("TRIGGER;RELATED=END:-PT30S", reference, TriggerPolicy{AllDay: TriggerAllDayReferenceMidnight}), time.Date(2026, 3, 6, 23, 59, 30, 0, loc), "")
	reference.TimeZone = ""
	assertTriggerResult(t, EvaluateTrigger("TRIGGER;RELATED=END:PT0S", reference, TriggerPolicy{AllDay: TriggerAllDayReferenceMidnight}), time.Time{}, "invalid_timezone")
}

func TestExtendedTriggersDoNotEnableLegacyDelivery(t *testing.T) {
	due := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	for _, raw := range []string{"TRIGGER;RELATED=START:-PT10M", "TRIGGER;RELATED=END:-PT10M", "TRIGGER:-PT30S", "TRIGGER:PT1H", "TRIGGER:-P1D", "TRIGGER:-P1M", "TRIGGER:-P1Y", "TRIGGER:-PT60M", "TRIGGER:-PT0M"} {
		if _, ok := ReminderTime(raw, due); ok {
			t.Errorf("new trigger enabled for delivery: %q", raw)
		}
	}
	for _, raw := range []string{"TRIGGER:PT0S", "TRIGGER:-PT10M", "TRIGGER:-PT1H30M"} {
		if _, ok := ReminderTime(raw, due); !ok {
			t.Errorf("existing delivery trigger disabled: %q", raw)
		}
	}
}

func assertTriggerResult(t *testing.T, got TriggerResult, want time.Time, reason string) {
	t.Helper()
	if got.Supported != (reason == "") || got.Reason != reason || !got.At.Equal(want) {
		t.Fatalf("result = %+v; want at %v, reason %q", got, want, reason)
	}
}
