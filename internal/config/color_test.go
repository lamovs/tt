package config

import (
	"strings"
	"testing"
)

func TestParseColorMode(t *testing.T) {
	for in, want := range map[string]ColorMode{
		"auto":     ColorAuto,
		"always":   ColorAlways,
		"never":    ColorNever,
		"  Never ": ColorNever,
	} {
		got, err := ParseColorMode(in)
		if err != nil || got != want {
			t.Errorf("ParseColorMode(%q) = %q, %v; want %q", in, got, err, want)
		}
	}

	for _, in := range []string{"", "yes", "on", "truecolor"} {
		if _, err := ParseColorMode(in); err == nil {
			t.Errorf("ParseColorMode(%q) was accepted", in)
		} else if !strings.Contains(err.Error(), "auto, always, never") {
			t.Errorf("ParseColorMode(%q) said %q, want it to list what is allowed", in, err)
		}
	}
}

func TestColorKeyIsValidated(t *testing.T) {
	if _, err := LoadFile(write(t, "color = 'sometimes'\n")); err == nil {
		t.Fatal("an unknown colour mode was accepted")
	} else if !strings.Contains(err.Error(), ":1: color: unknown value \"sometimes\"") {
		t.Errorf("got %q, want the problem reported at the key and its line", err)
	}

	cfg, err := LoadFile(write(t, "color = 'never'\n"))
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if cfg.Color != ColorNever {
		t.Errorf("color = %q, want %q", cfg.Color, ColorNever)
	}
}
