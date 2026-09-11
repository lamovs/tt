package cli

import (
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/movsar/tt/internal/dates"
	"github.com/movsar/tt/internal/model"
)

var listNow = time.Date(2026, 9, 6, 12, 0, 0, 0, time.Local)

func day(y int, m time.Month, d int) model.Time {
	return model.NewTime(time.Date(y, m, d, 0, 0, 0, 0, time.Local))
}

func TestListLinesLaysOutTheColumns(t *testing.T) {
	rows := []Row{
		{Num: 1, Project: "Office Hub", Task: model.Task{
			Title: "Take the documents from the bank", Priority: model.PriorityHigh,
			DueDate: day(2026, 9, 7), IsAllDay: true}},
		{Num: 2, Project: "Personal", Task: model.Task{
			Title: "Collect the parcel", Priority: model.PriorityMedium}},
		{Num: 5, Project: "Office Hub", Task: model.Task{
			Title: "Renew the domain", Status: model.TaskDone,
			DueDate: day(2026, 9, 4), IsAllDay: true}},
	}
	want := []string{
		"1  [ ] !!  Take the documents from the bank  tmr         Office Hub",
		"2  [ ] !   Collect the parcel                --          Personal",
		"5  [x]     Renew the domain                  overdue 2d  Office Hub",
	}
	got := ListLines(rows, PlainPalette(), listNow)
	if len(got) != len(want) {
		t.Fatalf("got %d lines, want %d:\n%s", len(got), len(want), strings.Join(got, "\n"))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d:\ngot  %q\nwant %q", i+1, got[i], want[i])
		}
	}
}

func TestListLinesFitTheWidth(t *testing.T) {
	long := strings.Repeat("word ", 40)
	rows := []Row{
		{Num: 1, Project: "Office Hub", Task: model.Task{Title: long}},
		{Num: 2, Project: "Office Hub", Task: model.Task{Title: "short"}},
	}
	got := ListLines(rows, PlainPalette(), listNow)
	for i, line := range got {
		if n := runeLen(line); n > Width {
			t.Errorf("line %d is %d columns wide, want at most %d:\n%q", i+1, n, Width, line)
		}
	}
	if !strings.Contains(got[0], ellipsis) {
		t.Errorf("a cut title is not marked as cut: %q", got[0])
	}

	if !strings.HasSuffix(got[0], "Office Hub") {
		t.Errorf("the line lost its project column: %q", got[0])
	}
}

func TestListLinesCapTheProjectColumn(t *testing.T) {
	rows := []Row{
		{Num: 1, Project: "Personal projects and other odds and ends", Task: model.Task{
			Title: "Take the documents from the bank"}},
		{Num: 2, Project: "Work", Task: model.Task{
			Title: "Collect the parcel"}},
	}
	got := ListLines(rows, PlainPalette(), listNow)
	for i, line := range got {
		if n := runeLen(line); n > Width {
			t.Errorf("line %d is %d columns wide:\n%q", i+1, n, line)
		}
	}

	for i, want := range []string{"Take the documents from the bank", "Collect the parcel"} {
		if !strings.Contains(got[i], want) {
			t.Errorf("line %d lost its title to the project column:\n%q", i+1, got[i])
		}
	}
	if !strings.Contains(got[0], ellipsis) {
		t.Errorf("the long list name was not capped: %q", got[0])
	}
}

func TestListLinesCountRunesNotBytes(t *testing.T) {
	rows := []Row{
		{Num: 1, Project: "Личное", Task: model.Task{Title: "Забрать посылку"}},
		{Num: 2, Project: "Личное", Task: model.Task{Title: "Купить билеты"}},
	}
	got := ListLines(rows, PlainPalette(), listNow)
	first := strings.Index(got[0], "--")
	second := strings.Index(got[1], "--")
	if first == second {
		t.Fatalf("the two due columns start at the same byte, which cannot happen with titles of different lengths:\n%q\n%q", got[0], got[1])
	}
	if a, b := runeIndex(got[0], "--"), runeIndex(got[1], "--"); a != b {
		t.Errorf("the due column starts at rune %d on one line and %d on the other:\n%q\n%q", a, b, got[0], got[1])
	}
}

func runeIndex(s, sub string) int {
	i := strings.Index(s, sub)
	if i < 0 {
		return -1
	}
	return runeLen(s[:i])
}

func TestListLinesAlignWideAndCombiningTitlesByCells(t *testing.T) {
	rows := []Row{
		{Num: 1, Project: "界", Task: model.Task{Title: "界界"}},
		{Num: 2, Project: "e\u0301", Task: model.Task{Title: "abcd"}},
	}
	got := ListLines(rows, PlainPalette(), listNow)
	if a, b := runeIndex(got[0], "--"), runeIndex(got[1], "--"); a != b {
		t.Errorf("due columns start at cells %d and %d:\n%s", a, b, strings.Join(got, "\n"))
	}
	for _, line := range got {
		if width := DisplayWidth(line); width > Width {
			t.Errorf("list line is %d cells, want at most %d: %q", width, Width, line)
		}
	}
}

func TestListLinesAccentOverdueAndHighPriority(t *testing.T) {
	rows := []Row{
		{Num: 1, Project: "Work", Task: model.Task{
			Title: "late", DueDate: day(2026, 9, 1), IsAllDay: true}},
		{Num: 2, Project: "Work", Task: model.Task{
			Title: "urgent", Priority: model.PriorityHigh}},

		{Num: 3, Project: "Work", Task: model.Task{
			Title: "ordinary", Priority: model.PriorityMedium,
			DueDate: model.NewTime(time.Date(2026, 12, 25, 9, 0, 0, 0, time.Local))}},
	}
	colored := ListLines(rows, NewPalette(true), listNow)
	plain := ListLines(rows, PlainPalette(), listNow)

	if !strings.Contains(colored[0], sgrAccent+"overdue") {
		t.Errorf("an overdue date is not accented: %q", colored[0])
	}
	if !strings.Contains(colored[1], sgrAccent+"!!") {
		t.Errorf("a high priority is not accented: %q", colored[1])
	}
	if colored[2] != plain[2] {
		t.Errorf("an ordinary row was coloured:\ngot  %q\nwant %q", colored[2], plain[2])
	}

	plainAt, coloredAt := runeIndex(plain[1], "urgent"), strings.Index(colored[1], "urgent")
	if plainAt < 0 || coloredAt < 0 {
		t.Fatalf("the title went missing: %q / %q", plain[1], colored[1])
	}
	before := colored[1][:coloredAt]
	before = strings.ReplaceAll(before, sgrAccent, "")
	before = strings.ReplaceAll(before, sgrDefaultText, "")
	if got := runeLen(before); got != plainAt {
		t.Errorf("the accented cell was padded over its escape sequences: the title starts at column %d without colour and %d with it", plainAt, got)
	}

	for i := range plain {
		if got := unstyle(colored[i]); got != plain[i] {
			t.Errorf("colour changed line %d under it:\ngot  %q\nwant %q", i+1, got, plain[i])
		}
	}
}

func unstyle(s string) string {
	for _, seq := range []string{sgrAccent, sgrDefaultText, sgrBold, sgrDim, sgrWeight} {
		s = strings.ReplaceAll(s, seq, "")
	}
	return s
}

func TestOverdueIsSpelledTheWayTheListingReads(t *testing.T) {
	past := dates.Format(day(2026, 9, 1), true, time.Local, listNow)
	if !isOverdue(past) {
		t.Errorf("dates.Format writes %q for a date that has passed; the listing looks for a %q prefix", past, "overdue")
	}
	if isOverdue(dates.Format(day(2026, 9, 7), true, time.Local, listNow)) {
		t.Error("a date in the future was read as overdue")
	}
}

func TestListLinesOfNothing(t *testing.T) {
	if got := ListLines(nil, PlainPalette(), listNow); got != nil {
		t.Errorf("ListLines(nil) = %q, want nothing at all", got)
	}
}

func TestRowsNumberFromOneAndMatchTheIDs(t *testing.T) {
	tasks := []model.Task{
		{Id: "a", Title: "one", ProjectId: "p1"},
		{Id: "b", Title: "two", ProjectId: "p2"},
	}
	rows := Rows(tasks, map[string]string{"p1": "Work"})
	if len(rows) != 2 || rows[0].Num != 1 || rows[1].Num != 2 {
		t.Fatalf("rows = %+v, want them numbered from 1", rows)
	}
	if rows[0].Project != "Work" {
		t.Errorf("row 1 project = %q, want the name behind its id", rows[0].Project)
	}
	if rows[1].Project != "" {
		t.Errorf("row 2 project = %q, want nothing for a project the cache has not seen", rows[1].Project)
	}
	ids := TaskIDs(tasks)
	for i, row := range rows {
		if ids[row.Num-1] != row.Task.Id {
			t.Errorf("row %d shows %s and the numbering records %s", row.Num, row.Task.Id, ids[i])
		}
	}
}

func TestWrap(t *testing.T) {
	got := Wrap("one two three four", 9)
	want := []string{"one two", "three", "four"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("Wrap = %q, want %q", got, want)
	}

	if got := Wrap("https://example.invalid/a/very/long/path", 10); len(got) != 1 {
		t.Errorf("Wrap broke an unbreakable word: %q", got)
	}
	if got := Wrap("a\n\nb", 10); len(got) != 3 || got[1] != "" {
		t.Errorf("Wrap = %q, want the paragraph break kept", got)
	}
}

func TestListLinesKeepOneTaskOnOneLine(t *testing.T) {
	rows := []Row{
		{Num: 1, Project: "Office Hub", Task: model.Task{Title: "pay\nthe rent"}},
		{Num: 2, Project: "Office Hub", Task: model.Task{Title: "pay\tthe bill"}},
		{Num: 3, Project: "Личное", Task: model.Task{Title: "Забрать посылку"}},

		{Num: 4, Project: "Office Hub", Task: model.Task{Title: `say "hi" about C:\temp`}},

		{Num: 5, Project: "Ama\nnat", Task: model.Task{Title: "rent"}},
		{Num: 6, Project: "Ama\tnat", Task: model.Task{Title: "bill"}},
	}
	got := ListLines(rows, PlainPalette(), listNow)
	if len(got) != len(rows) {
		t.Fatalf("got %d lines for %d rows:\n%s", len(got), len(rows), strings.Join(got, "\n"))
	}
	for i, line := range got {
		for _, c := range line {
			if unicode.IsControl(c) {
				t.Errorf("line %d carries %U, which the terminal would act on: %q", i+1, c, line)
			}
		}
		if n := runeLen(line); n > Width {
			t.Errorf("line %d is %d columns wide: %q", i+1, n, line)
		}
	}

	if !strings.Contains(got[0], `pay\nthe rent`) || !strings.Contains(got[1], `pay\tthe bill`) {
		t.Errorf("a title that breaks the layout was not written out:\n%s", strings.Join(got, "\n"))
	}

	if want := `say "hi" about C:\temp`; !strings.Contains(got[3], want) {
		t.Errorf("the cell did not print %q as it stands:\n%s", want, strings.Join(got, "\n"))
	}
	if !strings.HasSuffix(got[4], `Ama\nnat`) || !strings.HasSuffix(got[5], `Ama\tnat`) {
		t.Errorf("a list name that breaks the layout was not written out:\n%s", strings.Join(got, "\n"))
	}

	for i := range got {
		if a, b := runeIndex(got[i], "--"), runeIndex(got[0], "--"); a != b {
			t.Errorf("the due column starts at rune %d on line %d and %d on line 1:\n%s", a, i+1, b, strings.Join(got, "\n"))
		}
	}
}

func TestListLinesMeasureTheTitleTheyPrint(t *testing.T) {
	rows := []Row{
		{Num: 1, Project: "Works", Task: model.Task{Title: strings.Repeat("\t", 40)}},
		{Num: 2, Project: "Works", Task: model.Task{Title: "short"}},
	}
	got := ListLines(rows, PlainPalette(), listNow)
	for i, line := range got {
		if n := runeLen(line); n > Width {
			t.Errorf("line %d is %d columns wide: %q", i+1, n, line)
		}
		if !strings.HasSuffix(line, "Works") {
			t.Errorf("line %d lost its project column, so the title took more room than it was measured for: %q", i+1, line)
		}
	}
	if strings.Contains(got[0], `\`+ellipsis) {
		t.Errorf("the cut fell inside an escape sequence: %q", got[0])
	}
	if a, b := runeIndex(got[0], "--"), runeIndex(got[1], "--"); a != b {
		t.Errorf("the due column starts at rune %d on one line and %d on the other:\n%q\n%q", a, b, got[0], got[1])
	}
}

func TestListLinesMeasureTheProjectTheyPrint(t *testing.T) {
	rows := []Row{
		{Num: 1, Project: "Wo" + strings.Repeat("\t", 8) + "rk", Task: model.Task{Title: strings.Repeat("T", 90)}},
		{Num: 2, Project: "Home", Task: model.Task{Title: "short"}},
	}
	got := ListLines(rows, PlainPalette(), listNow)
	for i, line := range got {
		if n := runeLen(line); n > Width {
			t.Errorf("line %d is %d columns wide: %q", i+1, n, line)
		}
	}

	if want := "Wo" + strings.Repeat(`\t`, 8) + "rk"; !strings.HasSuffix(got[0], want) {
		t.Errorf("line 1 ends %q, want it to end with the escaped list name %q", got[0], want)
	}
}

func TestListLinesCutTheProjectThroughTheEscaping(t *testing.T) {
	rows := []Row{
		{Num: 1, Project: strings.Repeat("\t", 20), Task: model.Task{Title: strings.Repeat("T", 90)}},
	}
	got := ListLines(rows, PlainPalette(), listNow)
	for i, line := range got {
		if n := runeLen(line); n > Width {
			t.Errorf("line %d is %d columns wide: %q", i+1, n, line)
		}
	}

	want := strings.Repeat(`\t`, (maxProject-len(ellipsis))/len(`\t`)) + ellipsis
	if !strings.HasSuffix(got[0], want) {
		t.Errorf("line 1 ends %q, want it to end %q - the cut has fallen inside an escape sequence", got[0], want)
	}
}
