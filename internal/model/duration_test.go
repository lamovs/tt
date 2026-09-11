package model

import (
	"testing"
	"time"
)

func TestParseDuration(t *testing.T) {
	cases := []struct {
		in    string
		want  time.Duration
		print string
	}{
		{"0", 0, "0"},
		{"25m", 25 * time.Minute, "25m"},
		{"1h30m", 90 * time.Minute, "1h30m"},
		{"90s", 90 * time.Second, "1m30s"},
		{"5m", 5 * time.Minute, "5m"},
		{"60s", time.Minute, "1m"},
		{"1h", time.Hour, "1h"},
		{"2h15m30s", 2*time.Hour + 15*time.Minute + 30*time.Second, "2h15m30s"},
		{" 25m ", 25 * time.Minute, "25m"},
		{"0m", 0, "0"},
	}
	for _, c := range cases {
		got, err := ParseDuration(c.in)
		if err != nil {
			t.Fatalf("ParseDuration(%q): %v", c.in, err)
		}
		if got.Duration() != c.want {
			t.Errorf("ParseDuration(%q) = %v, want %v", c.in, got.Duration(), c.want)
		}
		if got.String() != c.print {
			t.Errorf("ParseDuration(%q).String() = %q, want %q", c.in, got.String(), c.print)
		}
	}
}

func TestParseDurationErrors(t *testing.T) {
	bad := []string{"", "25", "m", "25min", "-5m", "1.5h", "25 m", "300ms", "30m1h", "1h1h", "5s5s", "abc", "0s0",

		"3000000h", "2147483647h", "2147483647m"}
	for _, in := range bad {
		if got, err := ParseDuration(in); err == nil {
			t.Errorf("ParseDuration(%q) = %v, want error", in, got)
		}
	}
}

func TestParseDurationRange(t *testing.T) {
	const max = "2562047h47m16s"
	got, err := ParseDuration(max)
	if err != nil {
		t.Fatalf("ParseDuration(%q): %v", max, err)
	}
	if got.Duration() != 9223372036*time.Second {
		t.Errorf("ParseDuration(%q) = %v", max, got.Duration())
	}
	if got.String() != max {
		t.Errorf("ParseDuration(%q).String() = %q", max, got.String())
	}
	for _, in := range []string{
		"2562047h47m17s",
		"2562048h",
		"1h9223372036s",
		"99999999999999999999s",
	} {
		if v, err := ParseDuration(in); err == nil {
			t.Errorf("ParseDuration(%q) = %v (%s), want error", in, v.Duration(), v)
		}
	}
}

func TestDurationCanonicalPrinting(t *testing.T) {
	if got := Duration(25 * time.Minute).String(); got != "25m" {
		t.Errorf("got %q, want \"25m\"", got)
	}

	if got := Duration(1500 * time.Millisecond).String(); got == "" {
		t.Error("sub-second duration prints empty")
	}
}

func TestDurationHelpers(t *testing.T) {
	d, err := ParseDuration("90s")
	if err != nil {
		t.Fatal(err)
	}
	if d.Seconds() != 90 {
		t.Errorf("Seconds() = %d", d.Seconds())
	}
	if d.IsOff() {
		t.Error("90s must not read as off")
	}
	off, _ := ParseDuration("0")
	if !off.IsOff() {
		t.Error("0 must read as off")
	}
}

func TestDurationText(t *testing.T) {
	var d Duration
	if err := d.UnmarshalText([]byte("1h30m")); err != nil {
		t.Fatal(err)
	}
	b, err := d.MarshalText()
	if err != nil || string(b) != "1h30m" {
		t.Fatalf("MarshalText = %q, %v", b, err)
	}
	if err := d.UnmarshalText([]byte("nope")); err == nil {
		t.Error("want error")
	}
}
