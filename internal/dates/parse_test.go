package dates

import (
	"strings"
	"testing"
	"time"
)

var fixedNow = time.Date(2026, 9, 4, 15, 30, 0, 0, time.Local)

func mustParse(t *testing.T, in string, now time.Time) Result {
	t.Helper()
	got, err := Parse(in, now)
	if err != nil {
		t.Fatalf("Parse(%q): %v", in, err)
	}
	return got
}

func wantDate(t *testing.T, r Result, y int, m time.Month, d int) {
	t.Helper()
	if !r.AllDay {
		t.Errorf("AllDay = false, want true")
	}
	if r.Clear {
		t.Errorf("Clear = true, want false")
	}
	got := r.Time.In(time.Local)
	if gy, gm, gd := got.Date(); gy != y || gm != m || gd != d {
		t.Errorf("Time = %v, want the local day %04d-%02d-%02d", got, y, int(m), d)
		return
	}
	if earlier := got.Add(-time.Nanosecond); civilOf(earlier) == civilOf(got) {
		t.Errorf("Time = %v is not the first moment of its local day: %v is earlier and on the same day", got, earlier)
	}
}

func wantDateTime(t *testing.T, r Result, y int, m time.Month, d, hh, mm int) {
	t.Helper()
	if r.AllDay {
		t.Errorf("AllDay = true, want false")
	}
	got := r.Time.In(time.Local)
	gy, gm, gd := got.Date()
	if gy != y || gm != m || gd != d || got.Hour() != hh || got.Minute() != mm {
		t.Errorf("Time = %v, want %04d-%02d-%02d %02d:%02d local", got, y, int(m), d, hh, mm)
	}
}

func TestParseRelativeWords(t *testing.T) {
	wantDate(t, mustParse(t, "today", fixedNow), 2026, 9, 4)
	wantDate(t, mustParse(t, "TODAY", fixedNow), 2026, 9, 4)
	wantDate(t, mustParse(t, "tmr", fixedNow), 2026, 9, 5)
	wantDate(t, mustParse(t, "yst", fixedNow), 2026, 9, 3)
}

func TestParseWeekdayIsStrictlyForward(t *testing.T) {

	wantDate(t, mustParse(t, "fri", fixedNow), 2026, 9, 11)
	wantDate(t, mustParse(t, "sat", fixedNow), 2026, 9, 5)
	wantDate(t, mustParse(t, "sun", fixedNow), 2026, 9, 6)
	wantDate(t, mustParse(t, "mon", fixedNow), 2026, 9, 7)
	wantDate(t, mustParse(t, "thu", fixedNow), 2026, 9, 10)
}

func TestParseNextWeekday(t *testing.T) {

	wantDate(t, mustParse(t, "next mon", fixedNow), 2026, 9, 14)
	wantDate(t, mustParse(t, "NEXT Fri", fixedNow), 2026, 9, 18)
}

func TestParseEow(t *testing.T) {

	wantDate(t, mustParse(t, "eow", fixedNow), 2026, 9, 6)
	sunday := time.Date(2026, 9, 6, 10, 0, 0, 0, time.Local)
	wantDate(t, mustParse(t, "eow", sunday), 2026, 9, 6)
}

func TestParseEom(t *testing.T) {
	wantDate(t, mustParse(t, "eom", fixedNow), 2026, 9, 30)
	dec := time.Date(2026, 12, 15, 12, 0, 0, 0, time.Local)
	wantDate(t, mustParse(t, "eom", dec), 2026, 12, 31)
	feb := time.Date(2027, 2, 1, 12, 0, 0, 0, time.Local)
	wantDate(t, mustParse(t, "eom", feb), 2027, 2, 28)
	febLeap := time.Date(2028, 2, 1, 12, 0, 0, 0, time.Local)
	wantDate(t, mustParse(t, "eom", febLeap), 2028, 2, 29)
}

func TestParseExplicitDate(t *testing.T) {
	wantDate(t, mustParse(t, "25sep", fixedNow), 2026, 9, 25)
	wantDate(t, mustParse(t, "sep25", fixedNow), 2026, 9, 25)
	wantDate(t, mustParse(t, "25.09", fixedNow), 2026, 9, 25)
	wantDate(t, mustParse(t, "25.09.2026", fixedNow), 2026, 9, 25)
	wantDate(t, mustParse(t, "4.9.2027", fixedNow), 2027, 9, 4)
	wantDate(t, mustParse(t, "2026-09-25", fixedNow), 2026, 9, 25)
	wantDate(t, mustParse(t, "25sep27", fixedNow), 2027, 9, 25)
}

func TestParseYearInference(t *testing.T) {

	wantDate(t, mustParse(t, "25sep", fixedNow), 2026, 9, 25)

	wantDate(t, mustParse(t, "25aug", fixedNow), 2027, 8, 25)

	wantDate(t, mustParse(t, "4sep", fixedNow), 2026, 9, 4)
}

func TestParseFeb29Inference(t *testing.T) {

	wantDate(t, mustParse(t, "29feb", fixedNow), 2028, 2, 29)
}

func TestParseRelativeOffsets(t *testing.T) {
	wantDate(t, mustParse(t, "+3d", fixedNow), 2026, 9, 7)
	wantDate(t, mustParse(t, "+2w", fixedNow), 2026, 9, 18)
	wantDate(t, mustParse(t, "+1m", fixedNow), 2026, 10, 4)
}

func TestParseMonthClamping(t *testing.T) {
	jan31 := time.Date(2026, 1, 31, 12, 0, 0, 0, time.Local)
	wantDate(t, mustParse(t, "+1m", jan31), 2026, 2, 28)
	jan31leap := time.Date(2028, 1, 31, 12, 0, 0, 0, time.Local)
	wantDate(t, mustParse(t, "+1m", jan31leap), 2028, 2, 29)
	oct31 := time.Date(2026, 10, 31, 12, 0, 0, 0, time.Local)
	wantDate(t, mustParse(t, "+1m", oct31), 2026, 11, 30)
}

func TestParseNone(t *testing.T) {
	r, err := Parse("none", fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Clear {
		t.Errorf("Clear = false, want true")
	}
	if !r.Time.IsZero() {
		t.Errorf("Time = %v, want zero", r.Time.Time)
	}
	if _, err := Parse("none extra", fixedNow); err == nil {
		t.Error("want error for trailing words after none")
	}
}

func TestParseWithTime(t *testing.T) {
	wantDateTime(t, mustParse(t, "fri 18:00", fixedNow), 2026, 9, 11, 18, 0)
	wantDateTime(t, mustParse(t, "today 9:00", fixedNow), 2026, 9, 4, 9, 0)
	wantDateTime(t, mustParse(t, "today 6pm", fixedNow), 2026, 9, 4, 18, 0)
	wantDateTime(t, mustParse(t, "today 6:30pm", fixedNow), 2026, 9, 4, 18, 30)
	wantDateTime(t, mustParse(t, "today 9am", fixedNow), 2026, 9, 4, 9, 0)
	wantDateTime(t, mustParse(t, "today 12am", fixedNow), 2026, 9, 4, 0, 0)
	wantDateTime(t, mustParse(t, "today 12pm", fixedNow), 2026, 9, 4, 12, 0)
	wantDateTime(t, mustParse(t, "next mon 8:15", fixedNow), 2026, 9, 14, 8, 15)
}

func TestParseCollapsesWhitespace(t *testing.T) {
	wantDateTime(t, mustParse(t, "  fri   18:00  ", fixedNow), 2026, 9, 11, 18, 0)
}

func TestParseErrors(t *testing.T) {
	bad := []string{
		"",
		"   ",
		"18",
		"18:00",
		"6pm",
		"nextfoo",
		"next tuesday",
		"32sep",
		"25xyz",
		"2026-13-01",
		"2026-02-30",
		"31.02.2026",
		"01.01.1999",
		"01.01.2100",
		"31apr",
		"fri 25",
		"fri sat",
		"25sep 18",
		"today 24:00",
		"today 13pm",
		"whatever",
	}
	for _, in := range bad {
		if got, err := Parse(in, fixedNow); err == nil {
			t.Errorf("Parse(%q) = %+v, want error", in, got)
		}
	}
}

func TestParseCitesTheOffendingWordOnlyOnce(t *testing.T) {
	_, err := Parse("zzz", fixedNow)
	if err == nil {
		t.Fatal("Parse(\"zzz\"): want error")
	}
	if n := strings.Count(err.Error(), "zzz"); n != 1 {
		t.Errorf("Parse(\"zzz\") error = %q, mentions \"zzz\" %d times, want 1", err.Error(), n)
	}
}

func TestParseUnrecognizedDateSuggestsExplicitFormats(t *testing.T) {
	_, err := Parse("zzz", fixedNow)
	if err == nil {
		t.Fatal("Parse(zzz): want error")
	}
	for _, want := range []string{"21.10.2026", "2026-10-21"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Parse(zzz) error = %q, want suggestion %q", err.Error(), want)
		}
	}
}

func TestParseErrorMentionsOffendingPiece(t *testing.T) {
	cases := map[string]string{
		"18:00":     "18:00",
		"whatever":  "whatever",
		"31apr":     "31apr",
		"next tues": "tues",
	}
	for in, want := range cases {
		_, err := Parse(in, fixedNow)
		if err == nil {
			t.Fatalf("Parse(%q): want error", in)
		}
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Parse(%q) error = %q, want it to mention %q", in, err.Error(), want)
		}
	}
}

func TestParseSuggestsOnlyATimeItWouldTake(t *testing.T) {
	_, err := Parse("25:99", fixedNow)
	if err == nil {
		t.Fatal("Parse(\"25:99\"): want error")
	}
	if !strings.Contains(err.Error(), "out of range") {
		t.Errorf("Parse(%q) error = %q, want it to say the time is out of range", "25:99", err.Error())
	}
	if strings.Contains(err.Error(), "today") {
		t.Errorf("Parse(%q) error = %q, want no \"today\" hint for a time Parse would refuse anyway", "25:99", err.Error())
	}

	_, err = Parse("18:30", fixedNow)
	if err == nil {
		t.Fatal("Parse(\"18:30\"): want error")
	}
	if !strings.Contains(err.Error(), "today 18:30") {
		t.Errorf("Parse(%q) error = %q, want it to suggest %q", "18:30", err.Error(), "today 18:30")
	}
}

func TestParseAdvisesTheTimeAsItWouldWriteIt(t *testing.T) {
	cases := []struct{ in, want string }{
		{"18:30", `try "today 18:30"`},
		{"  18:30  ", `try "today 18:30"`},
		{"6PM", `try "today 6pm"`},
	}
	for _, c := range cases {
		_, err := Parse(c.in, fixedNow)
		if err == nil {
			t.Errorf("Parse(%q): want an error", c.in)
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("Parse(%q) error = %q, want it to advise %q", c.in, err.Error(), c.want)
		}

		if strings.Contains(err.Error(), "  ") {
			t.Errorf("Parse(%q) error = %q, want no doubled space anywhere in it", c.in, err.Error())
		}
	}
}

func TestParseHoldsTheYearRange(t *testing.T) {
	outOfRange := []string{
		"+30000d",
		"+4000w",
		"+900m",
		"2108-10-24",
		"2100-01-01",
		"1999-12-31",
	}
	for _, in := range outOfRange {
		got, err := Parse(in, fixedNow)
		if err == nil {
			t.Errorf("Parse(%q) = %v, want an error", in, got.Time.Time)
			continue
		}
		for _, want := range []string{in, "2000", "2099"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("Parse(%q) error = %q, want it to mention %q", in, err.Error(), want)
			}
		}
	}

	wantDate(t, mustParse(t, "2000-01-01", fixedNow), 2000, 1, 1)
	wantDate(t, mustParse(t, "2099-12-31", fixedNow), 2099, 12, 31)
}

func TestParseRejectsOffsetsThatWouldOverflow(t *testing.T) {
	huge := []string{
		"+9223372036854775807d",
		"+9223372036854775807w",
		"+9223372036854775807m",
		"+1000000000000000d",
		"+99999999999999999999d",

		"+7905747460161235015w",
	}
	for _, in := range huge {
		got, err := Parse(in, fixedNow)
		if err == nil {
			t.Errorf("Parse(%q) = %v (allDay=%v), want an error", in, got.Time.In(time.Local), got.AllDay)
		}
	}
}

func TestInferYearReachesTheNextLeapYear(t *testing.T) {
	from := civil{y: 2096, m: time.March, d: 1}
	got, err := inferYear(from, time.February, 29, "29feb")
	if err != nil {
		t.Fatalf("inferYear(29feb) from %v: %v", from, err)
	}
	if want := (civil{y: 2104, m: time.February, d: 29}); got != want {
		t.Errorf("inferYear(29feb) from %v = %v, want %v", from, got, want)
	}

	now := time.Date(2096, 3, 1, 12, 0, 0, 0, time.Local)
	if res, err := Parse("29feb", now); err == nil {
		t.Errorf("Parse(29feb) in 2096 = %v, want the year-range error", res.Time.Time)
	}
}
