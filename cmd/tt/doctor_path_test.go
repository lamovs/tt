package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/movsar/tt/internal/api"
	"github.com/movsar/tt/internal/config"
	"github.com/movsar/tt/internal/store"
)

func TestFullReportAtomSurvivesDoctorWrapping(t *testing.T) {
	cases := map[string]string{
		"leading repeated and trailing spaces": "  reader  stopped  ",
		"newline carriage return and tab":      "one\ntwo\rthree\tfour",
		"unicode whitespace":                   "one\u00a0two\u2003three\u3000four",
		"terminal controls and bidi":           "one\x1b\x7f\u0085\u202etwo",
		"literal escape syntax":                `one\n\x20\u00a0two`,
		"invalid UTF-8":                        string([]byte{'a', 0xff, 'b'}),
		"width boundary":                       strings.Repeat("x", 77) + "  " + strings.Repeat("y", 79),
	}

	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			var out bytes.Buffer
			printCheck(&out, checkResult{status: statusWarn, summary: "atom", notes: doctorNote(fullReportAtom(raw))})
			lines := strings.Split(out.String(), "\n")
			if len(lines) != 3 || lines[2] != "" || !strings.HasPrefix(lines[1], doctorIndent) {
				t.Fatalf("atom did not remain one complete wrapped note: %q", out.String())
			}
			got, err := strconv.Unquote(lines[1][len(doctorIndent):])
			if err != nil {
				t.Fatalf("atom is not one quoted string: %v", err)
			}
			if got != raw {
				t.Fatalf("atom recovered %q, want exact input %q", got, raw)
			}
		})
	}
}

func TestConfigUnusableKeepsTheCompleteSelectedReadReason(t *testing.T) {
	path := "/config/settings.toml"
	other := "/config/linked/a  b/settings.toml"
	reasonRaw := "  reader\u00a0\u2003 stopped\n\x1b\u202e " + `literal\n\x20` + " " +
		strings.Repeat("w", 79) + string([]byte{0xff}) + "  "
	denied := errors.New(reasonRaw)
	nested := &fs.PathError{Op: "stat", Path: other, Err: denied}

	cases := map[string]struct {
		err        error
		wantReason string
	}{
		"matching path": {
			err:        &fs.PathError{Op: "open", Path: path, Err: denied},
			wantReason: denied.Error(),
		},
		"wrapped matching path": {
			err:        fmt.Errorf("load config: %w", &fs.PathError{Op: "open", Path: path, Err: denied}),
			wantReason: denied.Error(),
		},
		"unmatched path": {
			err:        &fs.PathError{Op: "open", Path: other, Err: denied},
			wantReason: (&fs.PathError{Op: "open", Path: other, Err: denied}).Error(),
		},
		"generic error": {
			err:        errors.New(reasonRaw),
			wantReason: reasonRaw,
		},
		"distinct nested path": {
			err:        &fs.PathError{Op: "open", Path: path, Err: nested},
			wantReason: nested.Error(),
		},
	}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			var out bytes.Buffer
			result := configUnusable(path, c.err)
			printCheck(&out, result)
			if result.status != statusFail {
				t.Fatal("config read failure changed its verdict")
			}
			lines := strings.Split(out.String(), "\n")
			if len(lines) != 3 || lines[2] != "" || !strings.HasPrefix(lines[1], doctorIndent) {
				t.Fatalf("selected read reason did not remain one complete note: %q", out.String())
			}
			got, err := strconv.Unquote(lines[1][len(doctorIndent):])
			if err != nil {
				t.Fatalf("selected read reason is not one quoted string: %v", err)
			}
			if got != c.wantReason {
				t.Fatalf("selected read reason recovered %q, want %q", got, c.wantReason)
			}
		})
	}
}

func TestConfigUnusableKeepsAHostilePathAsOneRecoverableField(t *testing.T) {
	path := "/config/" + strings.Repeat("p", 80) + "  \"\\\n\x1b/settings.toml"
	reason := errors.New("permission denied")
	var out bytes.Buffer
	printCheck(&out, configUnusable(path, &fs.PathError{Op: "open", Path: path, Err: reason}))

	lines := strings.Split(out.String(), "\n")
	if len(lines) != 5 || lines[0] != "fail config:" || lines[2] != doctorIndent+"cannot be read" || lines[4] != "" {
		t.Fatalf("hostile path or reason opened another report line: %q", out.String())
	}
	if !strings.HasPrefix(lines[1], doctorIndent) || !strings.HasSuffix(lines[1], ":") {
		t.Fatalf("hostile path is not a distinct summary field: %q", lines[1])
	}
	pathToken := lines[1][len(doctorIndent) : len(lines[1])-1]
	gotPath, err := strconv.Unquote(pathToken)
	if err != nil {
		t.Fatalf("path is not one quoted field: %v", err)
	}
	if gotPath != path {
		t.Fatalf("path recovered %q, want exact input %q", gotPath, path)
	}
	gotReason, err := strconv.Unquote(lines[3][len(doctorIndent):])
	if err != nil || gotReason != reason.Error() {
		t.Fatalf("reason recovered %q, %v; want %q", gotReason, err, reason.Error())
	}
}

func quotedDoctorValues(text string) []string {
	var values []string
	for start := 0; start < len(text); {
		i := strings.IndexByte(text[start:], '"')
		if i < 0 {
			break
		}
		i += start
		found := false
		for j := i + 1; j < len(text); j++ {
			if text[j] != '"' {
				continue
			}
			value, err := strconv.Unquote(text[i : j+1])
			if err != nil {
				continue
			}
			values = append(values, value)
			start = j + 1
			found = true
			break
		}
		if !found {
			start = i + 1
		}
	}
	return values
}

func requireFinalDoctorAtom(t *testing.T, result checkResult, want ...string) {
	t.Helper()
	var out bytes.Buffer
	printCheck(&out, result)
	printed := out.String()
	if strings.Contains(printed, "\x1b") || strings.Contains(printed, "\u202e") ||
		strings.Contains(printed, "\u2003") || !utf8.ValidString(printed) {
		t.Fatalf("foreign atom reached the final report as terminal-active bytes: %q", printed)
	}
	decoded := quotedDoctorValues(printed)
	for _, expected := range want {
		if !slices.Contains(decoded, expected) {
			t.Fatalf("final report has quoted values %q, want exact value %q in %q", decoded, expected, printed)
		}
	}
}

func hostileDoctorPath(label string) string {
	return "/" + label + "/  edge\x1b\u202e\u2003" + string([]byte{0xff})
}

func TestDoctorCachePathConsumersKeepFinalAtomsRecoverable(t *testing.T) {
	path := hostileDoctorPath("cache")
	reason := "  denied\x1b\u202e\u2003" + string([]byte{0xff}) + "  "
	latest := 4

	cases := []struct {
		name string
		got  checkResult
		want []string
	}{
		{"missing", cacheOpenVerdict(path, fmt.Errorf("inspect: %w", store.ErrNoCache)), []string{path}},
		{"not a file", cacheOpenVerdict(path, fmt.Errorf("inspect: %w", store.ErrNotAFile)), []string{path}},
		{"cannot look", cacheOpenVerdict(path, fmt.Errorf("%w: %w", store.ErrCannotLook,
			&fs.PathError{Op: "stat", Path: path, Err: errors.New(reason)})), []string{path, reason}},
		{"bad migrations", cacheOpenVerdict(path, fmt.Errorf("%w: built-in failure", store.ErrBadMigrations)), []string{path}},
		{"run ended", cacheRunEnded(path), []string{path}},
		{"unreadable", cacheUnreadable(path, errors.New("driver refused")), []string{path}},
		{"version unreadable", cacheVersionUnreadable(path, errors.New("version refused")), []string{path}},
		{"later read failed", cacheReadFailure(path, "the task count", cacheDriverLead,
			errors.New("count refused")), []string{path}},
		{"reference comparison failed", cacheCompareFailure(path,
			fmt.Errorf("%w: reference refused", store.ErrBadReferenceSchema)), []string{path}},
		{"no meta table", func() checkResult {
			result, _ := cacheSchemaVerdict(path, 0, false, false, latest)
			return result
		}(), []string{path}},
		{"no version row", func() checkResult {
			result, _ := cacheSchemaVerdict(path, 0, true, false, latest)
			return result
		}(), []string{path}},
		{"invalid version", func() checkResult {
			result, _ := cacheSchemaVerdict(path, 0, true, true, latest)
			return result
		}(), []string{path}},
		{"future version", func() checkResult {
			result, _ := cacheSchemaVerdict(path, latest+1, true, true, latest)
			return result
		}(), []string{path}},
		{"blocking difference", func() checkResult {
			result, _ := cacheDifferenceVerdict(path, latest, store.SchemaDifference{
				Missing: []store.SchemaObject{{Kind: "table", Name: "tasks"}},
			})
			return result
		}(), []string{path}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			requireFinalDoctorAtom(t, c.got, c.want...)
		})
	}
}

func TestCheckCacheKeepsItsActualPathRecoverableAtFinalOutput(t *testing.T) {
	for _, name := range []string{"missing", "readable", "damaged"} {
		t.Run(name, func(t *testing.T) {
			isolate(t)
			dataHome := filepath.Join(t.TempDir(), "data  \x1b\u202e\u2003")
			t.Setenv("XDG_DATA_HOME", dataHome)
			path, err := store.DefaultPath()
			if err != nil {
				t.Fatal(err)
			}
			switch name {
			case "readable":
				if got := buildCache(t, nil); got != path {
					t.Fatalf("cache path = %q, want %q", got, path)
				}
			case "damaged":
				if got := damagedCache(t); got != path {
					t.Fatalf("cache path = %q, want %q", got, path)
				}
			}
			result, _ := checkCache(context.Background())
			requireFinalDoctorAtom(t, result, path)
		})
	}
}

func TestCheckTokenKeepsEverySelectedFileAnswerRecoverableAtFinalOutput(t *testing.T) {
	for _, name := range []string{"read failure", "empty", "unsendable", "readable", "insecure"} {
		t.Run(name, func(t *testing.T) {
			isolate(t)
			t.Setenv("XDG_DATA_HOME", filepath.Join(t.TempDir(), "data  \x1b\u202e\u2003"))
			path, err := api.TokenPath()
			if err != nil {
				t.Fatal(err)
			}
			var expected string
			switch name {
			case "read failure":
				if err := os.MkdirAll(path, 0o700); err != nil {
					t.Fatal(err)
				}
				_, loadErr := api.LoadToken()
				if loadErr == nil {
					t.Fatal("directory at token path was read as a token")
				}
				expected = loadErr.Error()
			case "empty":
				writeTokenFile(t, "")
				expected = path
			case "unsendable":
				writeTokenFile(t, "abc\x01def")
				expected = path
			case "readable":
				writeTokenFile(t, "a-token")
				expected = path
			case "insecure":
				writeTokenFile(t, "a-token")
				if err := os.Chmod(path, 0o644); err != nil {
					t.Fatal(err)
				}
				expected = path
			}
			result, _ := checkToken()
			requireFinalDoctorAtom(t, result, expected)
		})
	}
}

func TestCredentialsReadReasonRemainsRecoverableAtFinalOutput(t *testing.T) {
	isolate(t)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(t.TempDir(), "config  \x1b\u202e\u2003"))
	writeTokenFile(t, "a-token")
	path, err := credentialsPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, readErr := loadCredentials()
	if readErr == nil {
		t.Fatal("broken credentials unexpectedly loaded")
	}
	result, _ := checkToken()
	requireFinalDoctorAtom(t, result, readErr.Error())
}

func TestCheckConfigKeepsEverySourceStatePathRecoverableAtFinalOutput(t *testing.T) {
	for _, name := range []string{"missing", "readable", "unreadable", "invalid"} {
		t.Run(name, func(t *testing.T) {
			isolate(t)
			t.Setenv("XDG_CONFIG_HOME", filepath.Join(t.TempDir(), "config  \x1b\u202e\u2003"))
			path, err := config.Path()
			if err != nil {
				t.Fatal(err)
			}
			switch name {
			case "readable":
				writeConfig(t, "default_project = 'Inbox'\n")
			case "unreadable":
				if err := os.MkdirAll(path, 0o700); err != nil {
					t.Fatal(err)
				}
			case "invalid":
				writeConfig(t, "default_project = 'unterminated\n")
			}
			requireFinalDoctorAtom(t, checkConfig(doctorCache{}), path)
		})
	}
}
