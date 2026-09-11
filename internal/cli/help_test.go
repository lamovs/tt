package cli

import (
	"strings"
	"testing"
)

var dueHelp = Help{
	Verb:    "due",
	Summary: "move a task's due date",
	Examples: []Example{
		{Cmd: "tt due 1 fri", What: "the task numbered 1 in the last listing, this Friday"},
		{Cmd: "tt due 1 fri 18:00", What: "same day, at six in the evening"},
		{Cmd: "tt due 1-3 mon", What: "a range of numbers from the last listing"},
		{Cmd: "tt due 1 none", What: "take the date off"},
	},
	Sections: []HelpSection{{
		Title: "Dates",
		Items: []string{"English words or ISO: today, tomorrow, mon..sun, 3d, 2w, 2026-09-14."},
	}},
	SeeAlso: []string{"pri", "show"},
}

func TestHelpRender(t *testing.T) {
	want := []string{
		"tt due - move a task's due date",
		"",
		"Examples",
		"  tt due 1 fri        the task numbered 1 in the last listing, this Friday",
		"  tt due 1 fri 18:00  same day, at six in the evening",
		"  tt due 1-3 mon      a range of numbers from the last listing",
		"  tt due 1 none       take the date off",
		"",
		"Dates",
		"  - English words or ISO: today, tomorrow, mon..sun, 3d, 2w, 2026-09-14.",
		"",
		"See also",
		"  - tt pri, tt show",
	}
	got := dueHelp.Render(PlainPalette())
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("got:\n%s\n\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestHelpFitsTheWidth(t *testing.T) {
	h := Help{
		Verb:    "sync",
		Summary: "send queued changes and refresh the cache",
		Examples: []Example{
			{Cmd: "tt sync --allow-project-drop", What: "let the cache follow a server list that lost projects, which is a thing that happens"},
			{Cmd: "tt sync", What: "one pass"},
		},
	}
	got := h.Render(PlainPalette())
	for _, line := range got {
		if n := runeLen(line); n > Width {
			t.Errorf("a help line is %d columns wide, want at most %d:\n%q", n, Width, line)
		}
	}

	head := got[3]
	continuation := got[4]
	if want := strings.Index(head, "let the cache"); runeLen(continuation)-runeLen(strings.TrimLeft(continuation, " ")) != want {
		t.Errorf("the wrapped description does not line up with the one above it:\n%q\n%q", head, continuation)
	}
}

func TestHelpStacksAnExampleTooWideToShareALine(t *testing.T) {
	h := Help{
		Verb:    "add",
		Summary: "add a task",
		Examples: []Example{
			{Cmd: `tt add "call the bank about the statement" --list Office --due fri`,
				What: "a task with a list and a date, all on one line"},
			{Cmd: "tt add milk", What: "the short way"},
		},
		Sections: []HelpSection{{Title: "Defaults", Items: []string{"The default list is the one in the config."}}},
		SeeAlso:  []string{"ls"},
	}
	got := h.Render(PlainPalette())
	for _, line := range got {
		if n := runeLen(line); n > Width {
			t.Errorf("a help line is %d columns wide, want at most %d:\n%q", n, Width, line)
		}
	}
	body := strings.Join(got, "\n")
	for _, want := range []string{"tt add milk", "the short way", "a task with a list and a date"} {
		if !strings.Contains(body, want) {
			t.Errorf("the stacked layout lost %q:\n%s", want, body)
		}
	}

	if !strings.Contains(body, "See also\n  - tt ls") || !strings.Contains(body, "The default list") {
		t.Errorf("the stacked layout lost the notes or the siblings:\n%s", body)
	}
}

func TestHelpAlignsWideCommandsByCells(t *testing.T) {
	h := Help{
		Verb:    "show",
		Summary: "show one task",
		Examples: []Example{
			{Cmd: "tt 界", What: "wide command"},
			{Cmd: "tt abc", What: "ASCII command"},
		},
	}
	lines := h.Render(PlainPalette())
	first := strings.Index(lines[3], "wide command")
	second := strings.Index(lines[4], "ASCII command")
	if first < 0 || second < 0 {
		t.Fatalf("help descriptions are missing: %q", lines)
	}
	if a, b := DisplayWidth(lines[3][:first]), DisplayWidth(lines[4][:second]); a != b {
		t.Errorf("help descriptions start at cells %d and %d:\n%s", a, b, strings.Join(lines, "\n"))
	}
}

func TestHelpNeedsNoExamples(t *testing.T) {
	got := Help{Verb: "version", Summary: "print the version"}.Render(PlainPalette())
	if len(got) != 1 || got[0] != "tt version - print the version" {
		t.Errorf("got %q, want one line", got)
	}
}

func TestHelpRendersStructuredSectionsWithHangingIndent(t *testing.T) {
	h := Help{
		Verb:    "edit",
		Summary: "edit a task",
		Sections: []HelpSection{{
			Title: "Safety",
			Items: []string{
				"Preview the exact change before accepting it.",
				strings.Repeat("long ", 30),
			},
		}},
		SeeAlso: []string{"show"},
	}
	lines := h.Render(PlainPalette())
	body := strings.Join(lines, "\n")
	for _, want := range []string{"Safety\n  - Preview", "\n    long", "See also\n  - tt show"} {
		if !strings.Contains(body, want) {
			t.Errorf("structured help misses %q:\n%s", want, body)
		}
	}
	for _, line := range lines {
		if DisplayWidth(line) > Width {
			t.Errorf("structured help line exceeds %d columns: %q", Width, line)
		}
	}
}

func TestRenderIndexGroups(t *testing.T) {
	groups := []IndexGroup{
		{Title: "Everyday", Verbs: []Help{
			{Verb: "add", Summary: "add a task"},
			{Verb: "ls", Summary: "list tasks"},
		}},
		{Title: "Nothing here yet"},
		{Title: "Setup and maintenance", Verbs: []Help{
			{Verb: "doctor", Summary: "check the token, one API call, cache and configuration"},
		}},
	}
	want := []string{
		"tt: TickTick in the terminal",
		"",
		"Everyday",
		"  add     add a task",
		"  ls      list tasks",
		"",
		"Setup and maintenance",
		"  doctor  check the token, one API call, cache and configuration",
		"",
		"Run \"tt help <command>\" for examples.",
	}
	got := RenderIndex("tt: TickTick in the terminal", groups, `Run "tt help <command>" for examples.`, PlainPalette())
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("got:\n%s\n\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestRenderIndexMarksTheVerb(t *testing.T) {
	groups := []IndexGroup{{Title: "Everyday", Verbs: []Help{{Verb: "add", Summary: "add a task"}}}}
	got := RenderIndex("", groups, "", NewPalette(true))
	heading, entry := got[1], got[2]
	if !strings.Contains(heading, sgrDim) {
		t.Errorf("the group heading is not dim: %q", heading)
	}
	if !strings.Contains(entry, sgrBold+"add") {
		t.Errorf("the verb is not bold: %q", entry)
	}
	if strings.Contains(entry, sgrBold+"add a task") || strings.Count(entry, sgrBold) != 1 {
		t.Errorf("something other than the verb was marked: %q", entry)
	}
}
