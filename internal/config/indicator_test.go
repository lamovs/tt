package config

import (
	"strings"
	"testing"
)

func TestIndicatorConfigDefaultValidationAndEncoding(t *testing.T) {
	for _, source := range []string{"", "[timer]\nindicator = true\n"} {
		cfg, err := parse("fixture.toml", []byte(source))
		if err != nil || !cfg.Timer.Indicator {
			t.Fatalf("default %q: %v %+v", source, err, cfg.Timer)
		}
	}
	cfg, err := parse("fixture.toml", []byte("[timer]\nindicator = false\n"))
	if err != nil || cfg.Timer.Indicator {
		t.Fatalf("false: %v %+v", err, cfg.Timer)
	}
	encoded, err := cfg.Encode()
	if err != nil || !strings.Contains(string(encoded), "indicator = false") {
		t.Fatalf("encoding %s: %v", encoded, err)
	}
	for _, value := range []string{"1", "'off'"} {
		if _, err := parse("fixture.toml", []byte("[timer]\nindicator = "+value+"\n")); err == nil {
			t.Fatalf("accepted invalid indicator %s", value)
		}
	}
}
