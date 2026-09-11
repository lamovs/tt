package model

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestParseTime(t *testing.T) {
	got, err := ParseTime("2027-04-10T21:00:00.000+0000")
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2027, 4, 10, 21, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("got %v, want %v", got.Time, want)
	}
	if got.String() != "2027-04-10T21:00:00.000+0000" {
		t.Errorf("String() = %q", got.String())
	}
}

func TestParseTimeVariants(t *testing.T) {

	want := time.Date(2026, 9, 10, 9, 0, 0, 0, time.UTC)
	for _, in := range []string{
		"2026-09-10T09:00:00.000+0000",
		"2026-09-10T09:00:00+0000",
		"2026-09-10T09:00:00Z",
		"2026-09-10T12:00:00+0300",
	} {
		got, err := ParseTime(in)
		if err != nil {
			t.Errorf("ParseTime(%q): %v", in, err)
			continue
		}
		if !got.Equal(want) {
			t.Errorf("ParseTime(%q) = %v, want %v", in, got.Time, want)
		}

		if !strings.Contains(got.String(), ".000") {
			t.Errorf("ParseTime(%q).String() = %q, want milliseconds", in, got.String())
		}
		back, err := ParseTime(got.String())
		if err != nil || !back.Equal(want) {
			t.Errorf("round trip of %q gave %v, %v", in, back.Time, err)
		}
	}
}

func TestParseTimeEmptyAndBad(t *testing.T) {
	got, err := ParseTime("")
	if err != nil || !got.IsZero() {
		t.Errorf("empty string = %v, %v", got, err)
	}
	if got.String() != "" {
		t.Errorf("zero time prints %q", got.String())
	}

	if got, err := ParseTime("   "); err != nil || !got.IsZero() {
		t.Errorf("blank string = %v, %v", got, err)
	}
	if _, err := ParseTime("10.09.2026"); err == nil {
		t.Error("want error")
	}
}

func TestTimeJSON(t *testing.T) {
	v, err := ParseTime("2026-09-10T09:00:00+0000")
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `"2026-09-10T09:00:00.000+0000"` {
		t.Fatalf("JSON = %s", b)
	}
	var back Time
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if !back.Equal(v.Time) {
		t.Errorf("round trip lost the value: %v", back.Time)
	}
	var zero Time
	if b, err := json.Marshal(zero); err != nil || string(b) != "null" {
		t.Fatalf("zero time JSON = %s, %v", b, err)
	}
	if err := json.Unmarshal([]byte("null"), &back); err != nil || !back.IsZero() {
		t.Fatalf("null = %v, %v", back, err)
	}
}

func TestStoreStringNormalisesTheOffset(t *testing.T) {
	same := []string{
		"2026-01-01T09:00:00.000+0300",
		"2026-01-01T06:00:00.000+0000",
		"2026-01-01T06:00:00Z",
	}
	const want = "2026-01-01T06:00:00.000Z"
	for _, in := range same {
		got, err := ParseTime(in)
		if err != nil {
			t.Fatalf("ParseTime(%q): %v", in, err)
		}
		if got.StoreString() != want {
			t.Errorf("ParseTime(%q).StoreString() = %q, want %q", in, got.StoreString(), want)
		}
	}
}

func TestStoreStringSortsChronologically(t *testing.T) {

	stamps := []string{
		"2026-01-01T09:00:00.000+0300",
		"2026-01-01T08:00:00.000+0000",
		"2026-01-01T09:00:00.000+0000",
		"2026-01-02T01:00:00.000+0900",
	}
	var prev Time
	var prevKey string
	for i, in := range stamps {
		got, err := ParseTime(in)
		if err != nil {
			t.Fatalf("ParseTime(%q): %v", in, err)
		}
		key := got.StoreString()
		if i > 0 {
			if !prev.Before(got.Time) {
				t.Fatalf("test data is not chronological at %q", in)
			}
			if !(prevKey < key) {
				t.Errorf("storage keys out of order: %q must sort below %q", prevKey, key)
			}
		}
		prev, prevKey = got, key
	}
}

func TestStoreRoundTrip(t *testing.T) {
	v, err := ParseTime("2027-04-10T21:00:00.000+0000")
	if err != nil {
		t.Fatal(err)
	}
	back, err := ParseStoreTime(v.StoreString())
	if err != nil {
		t.Fatalf("ParseStoreTime(%q): %v", v.StoreString(), err)
	}
	if !back.Equal(v.Time) {
		t.Errorf("round trip gave %v, want %v", back.Time, v.Time)
	}
	if back.StoreString() != v.StoreString() {
		t.Errorf("storage form is not stable: %q vs %q", back.StoreString(), v.StoreString())
	}

	var zero Time
	if zero.StoreString() != "" {
		t.Errorf("zero time stores as %q", zero.StoreString())
	}
	if got, err := ParseStoreTime(""); err != nil || !got.IsZero() {
		t.Errorf("empty storage value = %v, %v", got, err)
	}
}
