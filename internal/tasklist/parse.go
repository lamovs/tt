// Package tasklist parses a markdown-like batch document into tasks,
// checklist items and child tasks. Parse is a pure function: it has no
// dependency on store, api or model, so it can run on raw editor text
// before anything is persisted.
package tasklist

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Error implements the error interface for ParseError.
func (e *ParseError) Error() string {
	return fmt.Sprintf("line %d: %s", e.Line, e.Msg)
}

// The refusals a document is read back with, for the three things a line can
// be that looks like something it is not.
const (
	headingSyntax = `a list heading needs a space after its "#": write "# List name", ` +
		`with one to six "#" in front of the space`
	nothingNamed = "this line names nothing: after the list marker and the checkbox " +
		"nothing is left of it but punctuation"
	mixedIndent = "this line is indented with other whitespace than the line that opened " +
		"the block: indent every line of one block the same way, with tabs or with spaces"
)

// Parse turns src into a Document. Any returned error is always a
// *ParseError pointing at the offending line.
func Parse(src string) (Document, error) {
	lines := strings.Split(src, "\n")

	var (
		groups []Group

		hasGroup bool
		curList  string
		curLine  int
		curTasks []Task

		curTask        *Task
		curBlockIndent string // leading whitespace of the first indented line of the current task's block; empty until there is one
		curItemCount   int
		taskTotal      int
	)

	flushGroup := func() {
		if hasGroup {
			groups = append(groups, Group{List: curList, Tasks: curTasks, Line: curLine})
		}
		hasGroup = false
		curList = ""
		curLine = 0
		curTasks = nil
	}

	for i, raw := range lines {
		lineNum := i + 1
		line := strings.TrimSuffix(raw, "\r")

		if strings.TrimSpace(line) == "" {
			continue
		}

		indent, rest := leadingIndent(line)

		if strings.HasPrefix(rest, "//") {
			continue
		}

		// A line without indentation opens something of its own: a heading
		// switches lists, anything else is a task. An indented "#" line is
		// part of the block above it, so it reads as a child task or an item
		// by the ordinary rules and never switches lists silently.
		if indent == "" {
			if name, ok := headingName(rest); ok {
				if name == "" {
					return Document{}, &ParseError{Line: lineNum, Msg: "empty list name"}
				}
				flushGroup()
				hasGroup = true
				curList = name
				curLine = lineNum
				curTasks = nil
				curTask = nil
				curBlockIndent = ""
				continue
			}
			// A line that opens with a "#" and is not a heading is a heading
			// written wrong, never a task called after it: taken as a task it
			// would write the whole block below it into the list the line
			// above it named, or into the default one, without ever looking
			// the name up.
			if strings.HasPrefix(rest, "#") {
				return Document{}, &ParseError{Line: lineNum, Msg: headingSyntax}
			}
			title, done := stripTitle(rest)
			if title == "" {
				return Document{}, &ParseError{Line: lineNum, Msg: "empty title"}
			}
			if !named(title) {
				return Document{}, &ParseError{Line: lineNum, Msg: nothingNamed}
			}
			if !hasGroup {
				hasGroup = true
				curList = ""
				curLine = lineNum
				curTasks = nil
			}
			if taskTotal+1 > MaxTasks {
				return Document{}, &ParseError{Line: lineNum, Msg: fmt.Sprintf("too many tasks (max %d)", MaxTasks)}
			}
			curTasks = append(curTasks, Task{Title: title, Done: done, Line: lineNum})
			taskTotal++
			curTask = &curTasks[len(curTasks)-1]
			curBlockIndent = ""
			curItemCount = 0
			continue
		}

		// Indented line: belongs to the current top-level task's block. Its
		// indentation is read against the line that opened the block rather
		// than measured, so that a block indented with tabs and one indented
		// with spaces are told apart whichever of the two comes first.
		if curTask == nil {
			return Document{}, &ParseError{Line: lineNum, Msg: "indented line has no preceding task"}
		}
		switch {
		case curBlockIndent == "":
			curBlockIndent = indent
		case indent == curBlockIndent || strings.HasPrefix(curBlockIndent, indent):
			// The same indentation, or a shallower one that is still the
			// same whitespace: either way the line stays in this block.
		case strings.HasPrefix(indent, curBlockIndent):
			return Document{}, &ParseError{Line: lineNum, Msg: "only one level of nesting is supported"}
		default:
			return Document{}, &ParseError{Line: lineNum, Msg: mixedIndent}
		}

		content := stripListMarker(rest)
		body, done, isItem := stripCheckbox(content)
		title := strings.TrimSpace(body)
		if title == "" {
			return Document{}, &ParseError{Line: lineNum, Msg: "empty title"}
		}
		if !named(title) {
			return Document{}, &ParseError{Line: lineNum, Msg: nothingNamed}
		}

		if isItem {
			if curItemCount+1 > MaxItemsPerTask {
				return Document{}, &ParseError{Line: lineNum, Msg: fmt.Sprintf("too many items in task (max %d)", MaxItemsPerTask)}
			}
			curTask.Items = append(curTask.Items, Item{Title: title, Done: done, Line: lineNum})
			curItemCount++
		} else {
			if taskTotal+1 > MaxTasks {
				return Document{}, &ParseError{Line: lineNum, Msg: fmt.Sprintf("too many tasks (max %d)", MaxTasks)}
			}
			curTask.Children = append(curTask.Children, Task{Title: title, Line: lineNum})
			taskTotal++
		}
	}

	flushGroup()
	return Document{Groups: groups}, nil
}

// CountTasks returns the number of tasks in the document, including
// children at any depth.
func (d Document) CountTasks() int {
	var count func(tasks []Task) int
	count = func(tasks []Task) int {
		n := len(tasks)
		for _, t := range tasks {
			n += count(t.Children)
		}
		return n
	}
	n := 0
	for _, g := range d.Groups {
		n += count(g.Tasks)
	}
	return n
}

// Template returns a short hint template for the editor, explaining the
// batch document syntax. It always parses to an empty Document.
func Template() string {
	return strings.Join([]string{
		"// One line is one task.",
		"// An indented line belongs to the task its block starts with:",
		"// the nearest line above it that carries no indentation.",
		"// Indent it with \"[ ]\" to make it a checklist item of that task; \"[x]\" marks the item done.",
		"// Indent it without a checkbox to make it a subtask of that task instead.",
		"// \"# List name\", written without indentation, switches which list the tasks below it go into.",
		"// Blank lines and lines starting with \"//\", like this one, are ignored.",
		"// Leave the whole file empty to cancel.",
		"",
	}, "\n")
}

// leadingIndent splits s into its leading run of whitespace runes and the
// remainder of the string. The whitespace goes back as it was written: a tab
// is not worth a number of spaces here, because the one thing a block does
// with it is compare it with the indentation of the line that opened the
// block.
func leadingIndent(s string) (indent, rest string) {
	idx := 0
	for idx < len(s) {
		r, size := utf8.DecodeRuneInString(s[idx:])
		if !unicode.IsSpace(r) {
			break
		}
		idx += size
	}
	return s[:idx], s[idx:]
}

// named reports whether s holds anything a task can be called by. One letter
// or one digit is enough; a line with neither is a list marker somebody left
// alone, or a rule drawn across the document, and tt would create a task
// called "-" or "---" out of it.
func named(s string) bool {
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return true
		}
	}
	return false
}

// headingName reports whether s (already stripped of leading indent)
// opens a group heading: one to six '#' characters followed by a
// whitespace character. ok is true whenever the marker matches, even if
// the resulting name is empty (the caller turns that into an error). Only
// a line the caller read at zero indent is asked about: inside a block a
// '#' is text of the line, not a heading. A line that opens with a '#' and
// is refused here is refused outright by the caller, because a heading
// written wrong is not a task.
func headingName(s string) (name string, ok bool) {
	hashes := 0
	for hashes < len(s) && s[hashes] == '#' {
		hashes++
	}
	if hashes < 1 || hashes > 6 || hashes >= len(s) {
		return "", false
	}
	r, _ := utf8.DecodeRuneInString(s[hashes:])
	if !unicode.IsSpace(r) {
		return "", false
	}
	return strings.TrimSpace(s[hashes:]), true
}

// stripTitle strips one optional list marker and one optional checkbox
// from s (already stripped of leading indent) and returns the trimmed
// title together with the checkbox's done state (false if there was no
// checkbox).
func stripTitle(s string) (title string, done bool) {
	content := stripListMarker(s)
	body, isDone, _ := stripCheckbox(content)
	return strings.TrimSpace(body), isDone
}

// stripListMarker removes one leading list marker ("- ", "* ", "+ ",
// "<digits>. " or "<digits>) ") from s, if present.
func stripListMarker(s string) string {
	for _, p := range []string{"- ", "* ", "+ "} {
		if strings.HasPrefix(s, p) {
			return s[len(p):]
		}
	}
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	if i > 0 && i < len(s) && (s[i] == '.' || s[i] == ')') {
		after := s[i+1:]
		if strings.HasPrefix(after, " ") {
			return after[1:]
		}
	}
	return s
}

// stripCheckbox removes one leading checkbox ("[ ]", "[x]" or "[X]") from
// s, if present. ok reports whether a checkbox was found. The space
// editors put after the brackets is not required: "[x]Book tickets" is the
// same checkbox as "[x] Book tickets", and the callers trim what is left.
func stripCheckbox(s string) (rest string, done bool, ok bool) {
	if len(s) < 3 || s[0] != '[' || s[2] != ']' {
		return s, false, false
	}
	switch s[1] {
	case ' ':
		return s[3:], false, true
	case 'x', 'X':
		return s[3:], true, true
	default:
		return s, false, false
	}
}
