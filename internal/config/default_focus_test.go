package config

import (
	"bytes"
	"strings"
	"testing"
)

func TestDefaultFocusReferencesAndRoundTrip(t *testing.T) {
	for _, value := range []string{"none", "task:abc123", "task:local-123_A"} {
		cfg, err := ParseBytes("test", []byte("default_focus = "+QuoteTOMLString(value)))
		if err != nil || cfg.DefaultFocus.String() != value {
			t.Fatalf("%q: %+v %v", value, cfg, err)
		}
		encoded, err := cfg.Encode()
		if err != nil {
			t.Fatal(err)
		}
		again, err := ParseBytes("test", encoded)
		if err != nil || again != cfg {
			t.Fatalf("round trip: %+v %v", again, err)
		}
	}
	for _, value := range []string{"", "id:x", "task:", "task:a/b", "timer:x", "None", " task:x", "task:x\n"} {
		if _, err := ParseBytes("test", []byte("default_focus = "+QuoteTOMLString(value))); err == nil {
			t.Fatalf("accepted %q", value)
		}
	}
	cfg, err := ParseBytes("test", nil)
	if err != nil || cfg.DefaultFocus.String() != "none" {
		t.Fatalf("default %+v %v", cfg, err)
	}
}

func TestDefaultFocusEditPreservesOwnerBytes(t *testing.T) {
	for _, old := range []string{"'none'", "\"\"\"none\"\"\"", "'''none'''"} {
		src := []byte("# owner\r\n\"default_focus\" = " + old + " # keep\r\ndefault_project = 'Work'\r\n[timer]\r\nfocus = '40m'\r\n")
		got, err := SetDefaultFocus(src, FocusReference{TaskID: "t1"})
		if err != nil || string(got) != strings.Replace(string(src), old, "'task:t1'", 1) {
			t.Fatalf("%q %v", got, err)
		}
	}
	src := []byte("# owner\n[timer]\nfocus = '40m'\n")
	got, err := SetDefaultFocus(src, FocusReference{TaskID: "t1"})
	if err != nil || !bytes.Equal(got, append([]byte("default_focus = 'task:t1'\n"), src...)) {
		t.Fatalf("%q %v", got, err)
	}
	for _, bad := range []string{"default_focus =", "default_focus='none'\ndefault_focus='none'", "unknown=1"} {
		if _, err := SetDefaultFocus([]byte(bad), FocusReference{}); err == nil {
			t.Fatalf("changed invalid config %q", bad)
		}
	}
}
