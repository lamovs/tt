package config

import (
	"bytes"
	"strings"
	"testing"
)

func TestDefaultProjectReferences(t *testing.T) {
	for _, tc := range []struct{ value, id, name string }{
		{"Work", "", "Work"}, {" Work ", "", "Work"},
		{"id:Ab-12_x", "Ab-12_x", ""}, {"name:id:work", "", "id:work"},
		{"name:name:Work", "", "name:Work"},
	} {
		id, name, err := ProjectReference(tc.value)
		if err != nil || id != tc.id || name != tc.name {
			t.Errorf("%q: %q %q %v", tc.value, id, name, err)
		}
	}
	for _, value := range []string{"", "name:", "id:", "id:a b", "id:a/b", "id:x\nY", "id:x:1"} {
		if _, err := ParseBytes("test", []byte("default_project = "+QuoteTOMLString(value))); err == nil {
			t.Errorf("accepted invalid reference %q", value)
		}
	}
}

func TestSetDefaultProjectPreservesOtherBytes(t *testing.T) {
	for _, old := range []string{"'Work'", `"Wo\"rk"`, "'''Work\n[timer]\n'''", `"""Work
default_project = 'inside'
"""`, `"""Work"""""`} {
		src := []byte("# owner header\r\n\"default_project\"\t = " + old + " # owner comment\r\ncolor = 'never'\r\ntimer.focus = '40m'\r\n")
		got, err := SetDefaultProject(src, "id:p2")
		if err != nil {
			t.Fatalf("%q: %v", old, err)
		}
		want := bytes.Replace(src, []byte(old), []byte("'id:p2'"), 1)
		if !bytes.Equal(got, want) {
			t.Errorf("got %q, want %q", got, want)
		}
	}
	src := []byte("# owner header\n[timer]\nfocus = '40m'\n")
	got, err := SetDefaultProject(src, "id:p1")
	if err != nil || string(got) != "default_project = 'id:p1'\n"+string(src) {
		t.Fatalf("insert: %q %v", got, err)
	}
	for _, bad := range []string{"default_project =", "default_project='x'\ndefault_project='y'", "unknown = 1"} {
		if _, err := SetDefaultProject([]byte(bad), "id:p1"); err == nil {
			t.Errorf("rewrote invalid config %q", bad)
		}
	}
	if strings.Contains(string(src), "id:p1") {
		t.Fatal("mutated input bytes")
	}
}
