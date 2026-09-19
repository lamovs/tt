package tasklist

import (
	"reflect"
	"strings"
	"testing"
)

func mustParse(t *testing.T, src string) Document {
	t.Helper()
	doc, err := Parse(src)
	if err != nil {
		t.Fatalf("Parse(%q): unexpected error: %v", src, err)
	}
	return doc
}

func wantParseError(t *testing.T, src string, wantLine int) *ParseError {
	t.Helper()
	_, err := Parse(src)
	if err == nil {
		t.Fatalf("Parse(%q): expected error, got nil", src)
	}
	pe, ok := err.(*ParseError)
	if !ok {
		t.Fatalf("Parse(%q): error type = %T, want *ParseError", src, err)
	}
	if pe.Line != wantLine {
		t.Errorf("Parse(%q): error line = %d, want %d (msg: %s)", src, pe.Line, wantLine, pe.Msg)
	}
	return pe
}

// The example from the spec: tasks before any heading, a heading with two
// tasks (one carrying children and items, one done via a checkbox on a
// top-level line), and a second heading with a single task.
func TestParseWorkedExample(t *testing.T) {
	src := "Written before any heading\n" +
		"\n" +
		"# Work\n" +
		"Fix the build\n" +
		"  - update go.mod\n" +
		"  - bump CI image\n" +
		"  [ ] run the linter\n" +
		"  [x] read the changelog\n" +
		"- [x] Ship the release\n" +
		"\n" +
		"## Home\n" +
		"Buy milk\n"

	doc := mustParse(t, src)

	if len(doc.Groups) != 3 {
		t.Fatalf("len(Groups) = %d, want 3", len(doc.Groups))
	}

	g0 := doc.Groups[0]
	if g0.List != "" || len(g0.Tasks) != 1 || g0.Tasks[0].Title != "Written before any heading" {
		t.Errorf("Groups[0] = %+v, want the anonymous group with one task", g0)
	}

	g1 := doc.Groups[1]
	if g1.List != "Work" || g1.Line != 3 {
		t.Errorf("Groups[1].List/Line = %q/%d, want Work/3", g1.List, g1.Line)
	}
	if len(g1.Tasks) != 2 {
		t.Fatalf("len(Groups[1].Tasks) = %d, want 2", len(g1.Tasks))
	}

	fix := g1.Tasks[0]
	if fix.Title != "Fix the build" || fix.Done {
		t.Errorf("Tasks[0] = %+v, want Fix the build / not done", fix)
	}
	if len(fix.Children) != 2 || fix.Children[0].Title != "update go.mod" || fix.Children[1].Title != "bump CI image" {
		t.Errorf("Fix the build Children = %+v", fix.Children)
	}
	if len(fix.Items) != 2 {
		t.Fatalf("len(Fix the build Items) = %d, want 2", len(fix.Items))
	}
	if fix.Items[0].Title != "run the linter" || fix.Items[0].Done {
		t.Errorf("Items[0] = %+v, want run the linter / not done", fix.Items[0])
	}
	if fix.Items[1].Title != "read the changelog" || !fix.Items[1].Done {
		t.Errorf("Items[1] = %+v, want read the changelog / done", fix.Items[1])
	}

	ship := g1.Tasks[1]
	if ship.Title != "Ship the release" || !ship.Done {
		t.Errorf("Tasks[1] = %+v, want Ship the release / done", ship)
	}
	if len(ship.Items) != 0 || len(ship.Children) != 0 {
		t.Errorf("Ship the release should have no Items/Children: %+v", ship)
	}

	g2 := doc.Groups[2]
	if g2.List != "Home" || len(g2.Tasks) != 1 || g2.Tasks[0].Title != "Buy milk" {
		t.Errorf("Groups[2] = %+v, want Home with Buy milk", g2)
	}

	if got, want := doc.CountTasks(), 6; got != want {
		t.Errorf("CountTasks() = %d, want %d", got, want)
	}
}

func TestParseEmptyDocument(t *testing.T) {
	for _, src := range []string{"", "\n", "   \n\t\n", "// only a comment\n"} {
		doc := mustParse(t, src)
		if len(doc.Groups) != 0 {
			t.Errorf("Parse(%q).Groups = %+v, want empty", src, doc.Groups)
		}
	}
}

func TestParseCommentLines(t *testing.T) {
	src := "// leading comment\n" +
		"Task one\n" +
		"  // comment inside the indented block, must not break it\n" +
		"  - child of task one\n" +
		"//# not a heading, this whole line is a comment\n"
	doc := mustParse(t, src)
	if len(doc.Groups) != 1 || len(doc.Groups[0].Tasks) != 1 {
		t.Fatalf("Groups = %+v", doc.Groups)
	}
	task := doc.Groups[0].Tasks[0]
	if task.Title != "Task one" {
		t.Errorf("Title = %q, want %q", task.Title, "Task one")
	}
	if len(task.Children) != 1 || task.Children[0].Title != "child of task one" {
		t.Errorf("Children = %+v, want one child after skipping the comment", task.Children)
	}
}

func TestParseHeadingsOpenGroups(t *testing.T) {
	t.Run("duplicate names give two groups", func(t *testing.T) {
		doc := mustParse(t, "# Work\nA\n# Work\nB\n")
		if len(doc.Groups) != 2 {
			t.Fatalf("len(Groups) = %d, want 2", len(doc.Groups))
		}
		if doc.Groups[0].List != "Work" || doc.Groups[1].List != "Work" {
			t.Errorf("Groups = %+v", doc.Groups)
		}
		if doc.Groups[0].Tasks[0].Title != "A" || doc.Groups[1].Tasks[0].Title != "B" {
			t.Errorf("Groups tasks = %+v", doc.Groups)
		}
	})

	t.Run("heading alone creates no task, empty group is kept", func(t *testing.T) {
		doc := mustParse(t, "# Empty\n# Next\nX\n")
		if len(doc.Groups) != 2 {
			t.Fatalf("len(Groups) = %d, want 2", len(doc.Groups))
		}
		if doc.Groups[0].List != "Empty" || len(doc.Groups[0].Tasks) != 0 {
			t.Errorf("Groups[0] = %+v, want empty Empty group", doc.Groups[0])
		}
	})

	t.Run("six hashes is a heading, seven is refused", func(t *testing.T) {
		doc := mustParse(t, "###### Six\nA\n")
		if len(doc.Groups) != 1 || doc.Groups[0].List != "Six" {
			t.Fatalf("Groups = %+v, want a Six group", doc.Groups)
		}

		wantParseError(t, "####### Seven\n", 1)
	})

	// A heading written wrong used to read as a task, and every task under
	// it went to whatever list was open - the default one, silently, with no
	// name ever looked up.
	t.Run("a hash line that is not a heading is refused, never taken as a task", func(t *testing.T) {
		for _, src := range []string{"#Work\nBuy milk\n", "#\nBuy milk\n", "###\nBuy milk\n", "####### Seven\n"} {
			pe := wantParseError(t, src, 1)
			for _, want := range []string{"space", `"#"`} {
				if !strings.Contains(pe.Msg, want) {
					t.Errorf("Parse(%q): msg = %q, want it to say %q", src, pe.Msg, want)
				}
			}
		}
	})

	t.Run("a tab may separate the hashes from the name", func(t *testing.T) {
		doc := mustParse(t, "##\tTabbed\nA\n")
		if len(doc.Groups) != 1 || doc.Groups[0].List != "Tabbed" {
			t.Fatalf("Groups = %+v, want a Tabbed group", doc.Groups)
		}
	})

	t.Run("an indented hash is text of the block it is in, not a heading", func(t *testing.T) {
		// A note with a hash written under a task used to switch lists for
		// everything below it, which is the one thing a batch never does
		// without saying so.
		doc := mustParse(t, "Trip\n\t# Work\nBook tickets\n")
		if len(doc.Groups) != 1 || doc.Groups[0].List != "" {
			t.Fatalf("Groups = %+v, want the one anonymous group", doc.Groups)
		}
		tasks := doc.Groups[0].Tasks
		if len(tasks) != 2 || tasks[0].Title != "Trip" || tasks[1].Title != "Book tickets" {
			t.Fatalf("Tasks = %+v, want Trip and Book tickets in the same group", tasks)
		}
		if len(tasks[0].Children) != 1 || tasks[0].Children[0].Title != "# Work" {
			t.Errorf("Trip = %+v, want the indented line as a child task titled '# Work'", tasks[0])
		}
	})

	t.Run("leading whitespace before a hash makes it an indented line", func(t *testing.T) {
		wantParseError(t, "  ## Indented\nA\n", 1)
		doc := mustParse(t, "Parent\n  ## Indented\n")
		task := doc.Groups[0].Tasks[0]
		if len(task.Children) != 1 || task.Children[0].Title != "## Indented" {
			t.Fatalf("Parent = %+v, want the indented hash line as a child task", task)
		}
	})

	t.Run("tasks before the first heading form the anonymous group only if present", func(t *testing.T) {
		doc := mustParse(t, "# Work\nA\n")
		if len(doc.Groups) != 1 {
			t.Fatalf("Groups = %+v, want just the Work group, no anonymous group", doc.Groups)
		}
	})
}

func TestParseTopLevelMarkersAndCheckboxes(t *testing.T) {
	cases := []struct {
		line  string
		title string
		done  bool
	}{
		{"Plain task", "Plain task", false},
		{"- Dash task", "Dash task", false},
		{"* Star task", "Star task", false},
		{"+ Plus task", "Plus task", false},
		{"1. Numbered dot task", "Numbered dot task", false},
		{"12) Numbered paren task", "Numbered paren task", false},
		{"[ ] Open checkbox task", "Open checkbox task", false},
		{"[x] Done lower task", "Done lower task", true},
		{"[X] Done upper task", "Done upper task", true},
		{"- [x] Dash plus done", "Dash plus done", true},
		{"[ ]Open without a space", "Open without a space", false},
		{"[x]Done lower without a space", "Done lower without a space", true},
		{"[X]Done upper without a space", "Done upper without a space", true},
	}
	for _, c := range cases {
		doc := mustParse(t, c.line+"\n")
		if len(doc.Groups) != 1 || len(doc.Groups[0].Tasks) != 1 {
			t.Fatalf("Parse(%q): Groups = %+v", c.line, doc.Groups)
		}
		got := doc.Groups[0].Tasks[0]
		if got.Title != c.title || got.Done != c.done {
			t.Errorf("Parse(%q) = {Title:%q Done:%v}, want {%q %v}", c.line, got.Title, got.Done, c.title, c.done)
		}
	}
}

func TestParseIndentedItemsAndChildren(t *testing.T) {
	cases := []struct {
		name      string
		indented  string
		wantItem  bool
		wantTitle string
		wantDone  bool
	}{
		{"dash child", "  - a child", false, "a child", false},
		{"star child", "  * a child", false, "a child", false},
		{"plus child", "  + a child", false, "a child", false},
		{"numbered dot child", "  1. a child", false, "a child", false},
		{"numbered paren child", "  2) a child", false, "a child", false},
		{"plain open item", "  [ ] an item", true, "an item", false},
		{"plain done-lower item", "  [x] an item", true, "an item", true},
		{"plain done-upper item", "  [X] an item", true, "an item", true},
		{"marker then item", "  - [ ] marked item", true, "marked item", false},
		{"marker then done item", "  * [x] marked done item", true, "marked done item", true},
		{"open item without a space", "  [ ]an item", true, "an item", false},
		{"done-lower item without a space", "  [x]an item", true, "an item", true},
		{"done-upper item without a space", "  [X]an item", true, "an item", true},
		{"tab after the checkbox", "  [ ]\tan item", true, "an item", false},
	}
	for _, c := range cases {
		src := "Parent\n" + c.indented + "\n"
		doc := mustParse(t, src)
		task := doc.Groups[0].Tasks[0]
		if c.wantItem {
			if len(task.Items) != 1 || len(task.Children) != 0 {
				t.Fatalf("%s: Items/Children = %+v/%+v", c.name, task.Items, task.Children)
			}
			if task.Items[0].Title != c.wantTitle || task.Items[0].Done != c.wantDone {
				t.Errorf("%s: Items[0] = %+v, want {%q %v}", c.name, task.Items[0], c.wantTitle, c.wantDone)
			}
		} else {
			if len(task.Children) != 1 || len(task.Items) != 0 {
				t.Fatalf("%s: Items/Children = %+v/%+v", c.name, task.Items, task.Children)
			}
			if task.Children[0].Title != c.wantTitle || task.Children[0].Done != false {
				t.Errorf("%s: Children[0] = %+v, want {%q false}", c.name, task.Children[0], c.wantTitle)
			}
			if len(task.Children[0].Items) != 0 || len(task.Children[0].Children) != 0 {
				t.Errorf("%s: a child task must have no Items/Children of its own: %+v", c.name, task.Children[0])
			}
		}
	}
}

func TestParseIndentationAttachesToNearestTopLevelTask(t *testing.T) {
	// The indented line belongs to the nearest preceding line WITHOUT
	// leading whitespace, not to the immediately preceding indented line.
	src := "Parent\n" +
		"  - first child\n" +
		"  - second child\n"
	doc := mustParse(t, src)
	task := doc.Groups[0].Tasks[0]
	if len(task.Children) != 2 {
		t.Fatalf("Children = %+v, want 2 (both attach to Parent, not to each other)", task.Children)
	}
}

func TestParseIndentSmallerButNonzeroStaysInBlock(t *testing.T) {
	src := "Parent\n" +
		"    - four spaces\n" +
		"  - two spaces\n"
	doc := mustParse(t, src)
	task := doc.Groups[0].Tasks[0]
	if len(task.Children) != 2 {
		t.Fatalf("Children = %+v, want 2: a shallower (but nonzero) indent stays in the same block", task.Children)
	}
}

func TestParseCRLF(t *testing.T) {
	src := "# Work\r\nTask one\r\n  - child\r\n"
	doc := mustParse(t, src)
	if len(doc.Groups) != 1 || doc.Groups[0].List != "Work" {
		t.Fatalf("Groups = %+v", doc.Groups)
	}
	task := doc.Groups[0].Tasks[0]
	if task.Title != "Task one" || len(task.Children) != 1 || task.Children[0].Title != "child" {
		t.Fatalf("task = %+v", task)
	}
}

func TestParseTabsAsIndent(t *testing.T) {
	src := "Parent\n\t- tab child\n\t[x] tab item\n"
	doc := mustParse(t, src)
	task := doc.Groups[0].Tasks[0]
	if len(task.Children) != 1 || task.Children[0].Title != "tab child" {
		t.Errorf("Children = %+v", task.Children)
	}
	if len(task.Items) != 1 || task.Items[0].Title != "tab item" || !task.Items[0].Done {
		t.Errorf("Items = %+v", task.Items)
	}
}

// Tabs and spaces in one block are refused whichever of the two opens it:
// counting a tab as one column made the same document pass one way round and
// fail the other, and the failure it gave was about nesting, which is not
// what the reader wrote.
func TestParseMixedTabsAndSpaces(t *testing.T) {
	for _, src := range []string{
		"Parent\n\t- tab child\n  - two space child\n",
		"Parent\n  - two space child\n\t- tab child\n",
		"Parent\n    - four space child\n\t- tab child\n",
	} {
		pe := wantParseError(t, src, 3)
		for _, want := range []string{"indented", "block"} {
			if !strings.Contains(pe.Msg, want) {
				t.Errorf("Parse(%q): msg = %q, want it to say %q", src, pe.Msg, want)
			}
		}
		if strings.Contains(pe.Msg, "nesting") {
			t.Errorf("Parse(%q): msg = %q, want it to say what happened, not to blame nesting", src, pe.Msg)
		}
	}
}

// A line that is nothing but a marker used to become a task called "-", and
// a rule drawn across the document a task called "---".
func TestParseRefusesLinesThatNameNothing(t *testing.T) {
	for _, c := range []struct {
		src  string
		line int
	}{
		{"-\n", 1},
		{"*\n", 1},
		{"+\n", 1},
		{"---\n", 1},
		{"Buy milk\n***\n", 2},
		{"- -\n", 1},
		{"[x] ...\n", 1},
		{"Parent\n  - ---\n", 2},
		{"Parent\n  [ ] -\n", 2},
	} {
		pe := wantParseError(t, c.src, c.line)
		if !strings.Contains(pe.Msg, "punctuation") {
			t.Errorf("Parse(%q): msg = %q, want it to say the line is only punctuation", c.src, pe.Msg)
		}
	}

	// One letter or one digit is a name, whatever else is on the line.
	for _, src := range []string{"1\n", "x\n", "- C++\n", "Parent\n  [ ] 3.5\n"} {
		mustParse(t, src)
	}
}

func TestParseLineNumbers(t *testing.T) {
	src := "\n" +
		"# Work\n" +
		"\n" +
		"Task A\n" +
		"  [ ] item A1\n" +
		"  - child A1\n"
	doc := mustParse(t, src)
	g := doc.Groups[0]
	if g.Line != 2 {
		t.Errorf("Group.Line = %d, want 2", g.Line)
	}
	task := g.Tasks[0]
	if task.Line != 4 {
		t.Errorf("Task.Line = %d, want 4", task.Line)
	}
	if task.Items[0].Line != 5 {
		t.Errorf("Item.Line = %d, want 5", task.Items[0].Line)
	}
	if task.Children[0].Line != 6 {
		t.Errorf("Child.Line = %d, want 6", task.Children[0].Line)
	}
}

func TestParseErrors(t *testing.T) {
	t.Run("empty list name", func(t *testing.T) {
		wantParseError(t, "#   \n", 1)
	})
	t.Run("empty top-level title after markers", func(t *testing.T) {
		wantParseError(t, "-   \n", 1)
	})
	t.Run("empty top-level title after checkbox", func(t *testing.T) {
		wantParseError(t, "[ ]   \n", 1)
	})
	t.Run("empty item title", func(t *testing.T) {
		wantParseError(t, "Parent\n  [ ]   \n", 2)
	})
	t.Run("empty child title", func(t *testing.T) {
		wantParseError(t, "Parent\n  -   \n", 2)
	})
	t.Run("indented line with no preceding task", func(t *testing.T) {
		wantParseError(t, "  - orphan child\n", 1)
	})
	t.Run("indented line right after a heading with no task yet", func(t *testing.T) {
		wantParseError(t, "# Work\n  - orphan child\n", 2)
	})
	t.Run("nesting deeper than one level", func(t *testing.T) {
		wantParseError(t, "Parent\n  - child\n    - grandchild\n", 3)
	})
}

func TestParseLimits(t *testing.T) {
	t.Run("MaxTasks across the whole document", func(t *testing.T) {
		var b strings.Builder
		for i := 0; i < MaxTasks+1; i++ {
			b.WriteString("Task\n")
		}
		wantParseError(t, b.String(), MaxTasks+1)
	})

	t.Run("MaxTasks counts children too", func(t *testing.T) {
		var b strings.Builder
		b.WriteString("Parent\n")
		for i := 0; i < MaxTasks; i++ {
			b.WriteString("  - child\n")
		}
		// Parent itself is task 1, so the (MaxTasks)-th child is task
		// MaxTasks+1 and must fail on that line (line MaxTasks+1, since
		// line 1 is Parent).
		wantParseError(t, b.String(), MaxTasks+1)
	})

	t.Run("MaxItemsPerTask", func(t *testing.T) {
		var b strings.Builder
		b.WriteString("Parent\n")
		for i := 0; i < MaxItemsPerTask+1; i++ {
			b.WriteString("  [ ] item\n")
		}
		wantParseError(t, b.String(), MaxItemsPerTask+2)
	})

	t.Run("exactly at the limits is fine", func(t *testing.T) {
		var b strings.Builder
		for i := 0; i < MaxTasks; i++ {
			b.WriteString("Task\n")
		}
		doc := mustParse(t, b.String())
		if doc.CountTasks() != MaxTasks {
			t.Errorf("CountTasks() = %d, want %d", doc.CountTasks(), MaxTasks)
		}
	})
}

func TestParseErrorFormat(t *testing.T) {
	err := &ParseError{Line: 7, Msg: "empty title"}
	if got, want := err.Error(), "line 7: empty title"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
	var _ error = err
}

func TestTemplateParsesToEmptyDocument(t *testing.T) {
	tpl := Template()
	if strings.TrimSpace(tpl) == "" {
		t.Fatal("Template() is empty, want an explanatory hint")
	}
	doc, err := Parse(tpl)
	if err != nil {
		t.Fatalf("Parse(Template()): unexpected error: %v", err)
	}
	if len(doc.Groups) != 0 {
		t.Errorf("Parse(Template()).Groups = %+v, want empty", doc.Groups)
	}

	// Template is deterministic (idempotent across calls).
	if got := Template(); got != tpl {
		t.Errorf("Template() is not deterministic across calls")
	}
}

func TestCountTasksIncludesChildren(t *testing.T) {
	doc := mustParse(t, "A\n  - a1\n  - a2\nB\n")
	if got, want := doc.CountTasks(), 4; got != want {
		t.Errorf("CountTasks() = %d, want %d", got, want)
	}
}

func TestParseDocumentDeepEqualSanity(t *testing.T) {
	// A small, fully specified document checked with reflect.DeepEqual to
	// catch any stray field (e.g. a non-nil empty slice where nil is
	// expected, or vice versa) that the field-by-field checks above might
	// not notice.
	doc := mustParse(t, "# List\nOnly task\n")
	want := Document{
		Groups: []Group{
			{
				List: "List",
				Line: 1,
				Tasks: []Task{
					{Title: "Only task", Line: 2},
				},
			},
		},
	}
	if !reflect.DeepEqual(doc, want) {
		t.Errorf("Parse() = %+v, want %+v", doc, want)
	}
}
