package cli

import (
	"slices"
	"strings"
	"testing"
	"unicode"

	"github.com/movsar/tt/internal/model"
)

func TestCardLines(t *testing.T) {
	task := model.Task{
		Title:    "Take the documents from the bank",
		Priority: model.PriorityHigh,
		DueDate:  day(2026, 9, 7),
		IsAllDay: true,
		Tags:     []string{"bank", "docs"},
		Items: []model.Item{
			{Title: "find the passport", Status: model.ItemDone},
			{Title: "print the application"},
		},
		Content: "Ask for the statement covering the whole year.",
	}
	want := []string{
		"[ ] Take the documents from the bank",
		"    list      Office Hub",
		"    due       tmr",
		"    priority  high",
		"    tags      bank, docs",
		"",
		"    1. [x] find the passport",
		"    2. [ ] print the application",
		"",
		"    Ask for the statement covering the whole year.",
	}
	got := CardLines(task, "Office Hub", PlainPalette(), listNow)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("got:\n%s\n\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestCardLinesShowCachedScheduleMetadata(t *testing.T) {
	task := model.Task{
		Title:      "send the report",
		Tags:       []string{"work"},
		RepeatFlag: "RRULE:FREQ=WEEKLY;INTERVAL=1",
		Reminders: []string{
			"TRIGGER:-PT10M",
			"TRIGGER:-PT1H",
			"TRIGGER:-PT10M",
		},
		Items:   []model.Item{{Title: "attach the figures"}},
		Content: "Use the signed copy.",
	}
	want := []string{
		"[ ] send the report",
		"    list      Work",
		"    due       --",
		"    priority  none",
		"    tags      work",
		"    repeat    RRULE:FREQ=WEEKLY;INTERVAL=1",
		"    reminder  TRIGGER:-PT10M",
		"    reminder  TRIGGER:-PT1H",
		"    reminder  TRIGGER:-PT10M",
		"",
		"    1. [ ] attach the figures",
		"",
		"    Use the signed copy.",
	}
	got := CardLines(task, "Work", PlainPalette(), listNow)
	if !slices.Equal(got, want) {
		t.Errorf("got:\n%s\n\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestCardLinesKeepBlankAndUnknownScheduleValues(t *testing.T) {
	task := model.Task{
		Title:      "opaque schedule",
		RepeatFlag: "   ",
		Reminders:  []string{"", "  ", "X-UNKNOWN:whenever"},
	}
	want := []string{
		"[ ] opaque schedule",
		"    list      Work",
		"    due       --",
		"    priority  none",
		"    repeat    --",
		"    reminder  --",
		"    reminder  --",
		"    reminder  X-UNKNOWN:whenever",
	}
	got := CardLines(task, "Work", PlainPalette(), listNow)
	if !slices.Equal(got, want) {
		t.Errorf("got:\n%s\n\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestCardLinesEscapeHostileScheduleMetadata(t *testing.T) {
	task := model.Task{
		Title:      "hostile schedule",
		RepeatFlag: "R\tN\nE\x1b\u202e\xff" + `\n`,
		Reminders:  []string{"T\rR\bI\x00\u2066\xfe" + `\x1b`},
	}
	want := []string{
		"[ ] hostile schedule",
		"    list      Work",
		"    due       --",
		"    priority  none",
		`    repeat    R\tN\nE\x1b\u202e\xff\n`,
		`    reminder  T\rR\bI\x00\u2066\xfe\x1b`,
	}
	got := CardLines(task, "Work", PlainPalette(), listNow)
	if !slices.Equal(got, want) {
		t.Errorf("got:\n%s\n\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	for i, line := range got {
		for _, bidi := range []rune{'\u202e', '\u2066'} {
			if strings.ContainsRune(line, bidi) {
				t.Errorf("card line %d carries raw bidi formatting %U: %q", i+1, bidi, line)
			}
		}
		for _, r := range line {
			if unicode.IsControl(r) {
				t.Errorf("card line %d carries raw control %U: %q", i+1, r, line)
			}
		}
	}
}

func TestCardLinesWrapScheduleMetadataLikeEveryOtherField(t *testing.T) {
	task := model.Task{
		Title:      "wide schedule",
		RepeatFlag: "012345678901234567890123456789012345678901234567890123456789 abc d e",
		Reminders:  []string{"XXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXX"},
	}
	want := []string{
		"[ ] wide schedule",
		"    list      Work",
		"    due       --",
		"    priority  none",
		"    repeat    012345678901234567890123456789012345678901234567890123456789 abc d",
		"              e",
		"    reminder  XXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXX",
	}
	got := CardLines(task, "Work", PlainPalette(), listNow)
	if !slices.Equal(got, want) {
		t.Errorf("got:\n%s\n\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	oversizedRule := model.Task{
		Title:      "oversized rule",
		RepeatFlag: "RRRRRRRRRRRRRRRRRRRRRRRRRRRRRRRRRRRRRRRRRRRRRRRRRRRRRRRRRRRRRRRRRRR",
	}
	wantOversized := []string{
		"[ ] oversized rule",
		"    list      Work",
		"    due       --",
		"    priority  none",
		"    repeat    RRRRRRRRRRRRRRRRRRRRRRRRRRRRRRRRRRRRRRRRRRRRRRRRRRRRRRRRRRRRRRRRRRR",
	}
	gotOversized := CardLines(oversizedRule, "Work", PlainPalette(), listNow)
	if !slices.Equal(gotOversized, wantOversized) {
		t.Errorf("oversized rule got:\n%s\n\nwant:\n%s", strings.Join(gotOversized, "\n"), strings.Join(wantOversized, "\n"))
	}
}

func TestCardLinesOfAnEmptyTask(t *testing.T) {
	got := CardLines(model.Task{Title: "bare", Status: model.TaskDone}, "", PlainPalette(), listNow)
	want := []string{
		"[x] bare",
		"    list      --",
		"    due       --",
		"    priority  none",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("got:\n%s\n\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestCardLinesAnswerAFieldOfBlankSpace(t *testing.T) {
	got := CardLines(model.Task{Title: "bare"}, "   ", PlainPalette(), listNow)
	want := []string{
		"[ ] bare",
		"    list      --",
		"    due       --",
		"    priority  none",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("got:\n%s\n\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	if got, want := field(PlainPalette(), "list", "\t", nil), []string{`    list      \t`}; !slices.Equal(got, want) {
		t.Errorf("a list name of one tab printed as %q, want %q", got, want)
	}
}

func TestCardTitleWrapsInsideTheIndent(t *testing.T) {

	head := strings.Repeat("t", Width-len(cardIndent))
	lines := CardLines(model.Task{Title: head + " end"}, "Work", PlainPalette(), listNow)
	for i, line := range lines {
		if n := runeLen(line); n > Width && !holdsAnUnbreakableWord(line) {
			t.Errorf("line %d of the card is %d columns wide with nothing on it too long to wrap, want at most %d:\n%q", i+1, n, Width, line)
		}
	}
	if len(lines) < 2 || strings.TrimSpace(lines[1]) != "end" {
		t.Fatalf("the title did not wrap where the card wraps it:\n%s", strings.Join(lines, "\n"))
	}
}

func TestCardTitleWrapsWideWordsAtCellBoundary(t *testing.T) {
	head := strings.Repeat("界", (Width-len(cardIndent))/2)
	lines := CardLines(model.Task{Title: head + " end"}, "", PlainPalette(), listNow)
	if len(lines) < 2 || lines[0] != "[ ] "+head || lines[1] != cardIndent+"end" {
		t.Fatalf("wide card title wrapped as %q", lines[:min(len(lines), 2)])
	}
	if width := DisplayWidth(lines[0]); width != Width {
		t.Errorf("first card line is %d cells, want %d: %q", width, Width, lines[0])
	}
}

func TestCardLinesFitTheWidth(t *testing.T) {
	task := model.Task{
		Title:    "Забрать документы из банка и отвезти их в налоговую на Мясникова, пока она открыта",
		Tags:     []string{"bank", "documents", "paperwork", "quarterly", "deadline", "errands", "city-centre"},
		Content:  strings.Repeat("word ", 60),
		Items:    []model.Item{{Title: strings.Repeat("step ", 30)}},
		Priority: model.PriorityHigh,
	}
	lines := CardLines(task, "Personal projects and other odds and ends", PlainPalette(), listNow)
	for _, line := range lines {
		if n := runeLen(line); n > Width && !holdsAnUnbreakableWord(line) {
			t.Errorf("a card line is %d columns wide with nothing on it too long to wrap, want at most %d:\n%q", n, Width, line)
		}
	}

	trimmed := make([]string, len(lines))
	for i, line := range lines {
		trimmed[i] = strings.TrimSpace(line)
	}
	body := strings.Join(trimmed, " ")
	for _, want := range []string{"пока она открыта", "city-centre", "other odds and ends"} {
		if !strings.Contains(body, want) {
			t.Errorf("the card lost %q:\n%s", want, strings.Join(lines, "\n"))
		}
	}
	if strings.Contains(body, ellipsis) {
		t.Errorf("something on the card was cut short:\n%s", strings.Join(lines, "\n"))
	}

	label := slices.IndexFunc(lines, func(line string) bool {
		return strings.HasPrefix(line, cardIndent+"tags")
	})
	if label < 0 {
		t.Fatalf("the card does not show the tags at all:\n%s", strings.Join(lines, "\n"))
	}
	continuations := 0
	for _, line := range lines[label+1:] {
		if line == "" {
			break
		}
		continuations++
		if want := len(cardIndent) + cardLabel; !strings.HasPrefix(line, strings.Repeat(" ", want)) {
			t.Errorf("a wrapped tag line does not line up with the tags above it: %q", line)
		}
	}
	if continuations == 0 {
		t.Errorf("the tags fitted on one line, so nothing here was checked:\n%s", strings.Join(lines, "\n"))
	}
}

func TestCardLinesKeepAControlCharacterOffTheCard(t *testing.T) {
	task := model.Task{
		Title: "pay\nthe rent",
		Items: []model.Item{{Title: "call\tthe bank"}},
	}
	got := CardLines(task, "Office Hub", PlainPalette(), listNow)
	if got[0] != `[ ] pay\nthe rent` {
		t.Errorf("the title line is %q, want the title on one line beside its box", got[0])
	}
	if want := cardIndent + `1. [ ] call\tthe bank`; !slices.Contains(got, want) {
		t.Errorf("the checklist item is not on one line beside its box:\n%s", strings.Join(got, "\n"))
	}
	for i, line := range got {
		for _, c := range line {
			if unicode.IsControl(c) {
				t.Errorf("card line %d carries %U, which the terminal would act on: %q", i+1, c, line)
			}
		}
	}
}

func TestCardLinesEscapeTheBlocksAndKeepTheirStructure(t *testing.T) {
	task := model.Task{
		Title:   "x",
		Tags:    []string{"bank\tdocs"},
		Content: "one\n\ntwo\rthree\x1b[31m",
	}
	got := CardLines(task, "Ama\nnat", PlainPalette(), listNow)
	want := []string{
		"[ ] x",
		`    list      Ama\nnat`,
		"    due       --",
		"    priority  none",
		`    tags      bank\tdocs`,
		"",
		"    one",
		"",
		`    two\rthree\x1b[31m`,
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("got:\n%s\n\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	for i, line := range got {
		for _, c := range line {
			if unicode.IsControl(c) {
				t.Errorf("card line %d carries %U, which the terminal would act on: %q", i+1, c, line)
			}
		}
	}

	if i := slices.Index(got, `    list      Ama\nnat`); i < 0 || got[i+1] != "    due       --" {
		t.Errorf("the list name did not stay on the line its label is on:\n%s", strings.Join(got, "\n"))
	}

	one, two := slices.Index(got, "    one"), slices.Index(got, `    two\rthree\x1b[31m`)
	if one < 0 || two != one+2 || got[one+1] != "" {
		t.Errorf("the description was run together into one paragraph:\n%s", strings.Join(got, "\n"))
	}
}

func holdsAnUnbreakableWord(line string) bool {
	for _, word := range strings.Fields(line) {
		if runeLen(word) > Width-len(cardIndent) {
			return true
		}
	}
	return false
}

func TestCardLinesTrimTheDescriptionBeforeTheyEscapeIt(t *testing.T) {
	task := model.Task{Title: "one", Content: "the whole description\r"}
	lines := CardLines(task, "", PlainPalette(), listNow)
	last := lines[len(lines)-1]
	if want := cardIndent + "the whole description"; last != want {
		t.Errorf("the card ends %q, want %q - the trim has to come before the escaping", last, want)
	}
}
