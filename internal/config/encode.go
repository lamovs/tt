package config

import (
	"bytes"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/BurntSushi/toml"
)

func (c Config) Encode() ([]byte, error) {
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(toRaw(c)); err != nil {
		return nil, err
	}
	return preferLiteralStrings(escapeUnsafeTOMLValues(buf.Bytes())), nil
}

func escapeUnsafeTOMLValues(src []byte) []byte {
	var out bytes.Buffer
	out.Grow(len(src))
	inString := false

	for i := 0; i < len(src); {
		b := src[i]
		if !inString {
			out.WriteByte(b)
			i++
			if b == '"' {
				inString = true
			}
			continue
		}

		switch b {
		case '"':
			out.WriteByte(b)
			i++
			inString = false
			continue
		case '\\':
			out.WriteByte(b)
			i++
			if i < len(src) {
				out.WriteByte(src[i])
				i++
			}
			continue
		}

		r, size := utf8.DecodeRune(src[i:])
		if terminalUnsafeTOMLRune(r) {
			if r <= 0xffff {
				fmt.Fprintf(&out, `\u%04X`, r)
			} else {
				fmt.Fprintf(&out, `\U%08X`, r)
			}
		} else {
			out.Write(src[i : i+size])
		}
		i += size
	}
	return out.Bytes()
}

func terminalUnsafeTOMLRune(r rune) bool {
	if r < 0x20 || (r >= 0x7f && r <= 0x9f) {
		return true
	}
	switch r {
	case 0x061c, 0x200e, 0x200f,
		0x202a, 0x202b, 0x202c, 0x202d, 0x202e,
		0x2066, 0x2067, 0x2068, 0x2069:
		return true
	default:
		return false
	}
}

func preferLiteralStrings(src []byte) []byte {
	lines := strings.Split(string(src), "\n")
	for i, line := range lines {
		if requoted, ok := requoteAssignment(line); ok {
			lines[i] = requoted
		}
	}
	return []byte(strings.Join(lines, "\n"))
}

func requoteAssignment(line string) (string, bool) {

	sep := strings.Index(line, " = ")
	if sep < 0 {
		return "", false
	}
	head, body := line[:sep+len(" = ")], line[sep+len(" = "):]
	if !strings.HasPrefix(body, `"`) {
		return "", false
	}

	var value strings.Builder
	for i := 1; i < len(body); i++ {
		switch body[i] {
		case '"':
			if i != len(body)-1 {
				return "", false
			}
			quoted := QuoteTOMLString(value.String())
			if !strings.HasPrefix(quoted, "'") {
				return "", false
			}
			return head + quoted, true
		case '\\':
			i++
			if i == len(body) || (body[i] != '"' && body[i] != '\\') {
				return "", false
			}
			value.WriteByte(body[i])
		default:
			value.WriteByte(body[i])
		}
	}
	return "", false
}

func QuoteTOMLString(s string) string {
	if !strings.ContainsFunc(s, needsTOMLEscape) {
		return "'" + s + "'"
	}

	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\t':
			b.WriteString(`\t`)
		case '\r':
			b.WriteString(`\r`)
		default:
			if isTOMLControl(r) {

				fmt.Fprintf(&b, `\u%04X`, r)
				continue
			}
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

func needsTOMLEscape(r rune) bool {
	return r == '\'' || isTOMLControl(r)
}

func isTOMLControl(r rune) bool {
	return r < 0x20 || r == 0x7f
}

const (
	NotifyMacOS = `tt-notify -- "$TT_KIND" "$TT_NOTE" "$TT_TASK" "$TT_DURATION"`

	NotifyLinux = `notify-send -- "tt: $TT_KIND done" "$TT_TASK ($TT_PROJECT)"`
)

const MoveByRecreateNotice = `This tt move workflow moves a task by creating a copy of the task in the new list and
deletes the original, in that order. It looks for the copy before it deletes.
So a failure usually leaves the task in its old list or in both, rather than in
neither - usually, and not always: nothing holds the copy in place
between that look and the delete. The copy is a new task with an id of
its own, and the server dates it from when it was made, so the creation
date is the copy's rather than the original's. Its title, description,
priority, tags, start and due dates, all-day flag, time zone and checklist
come across; this workflow does not copy attachments, comments or task history.
Nothing puts the move back afterwards,
undo included.`

func Template() string {
	return fmt.Sprintf(template,
		QuoteTOMLString(NotifyMacOS), QuoteTOMLString(NotifyLinux),
		commentBlock("#   ", MoveByRecreateNotice))
}

func commentBlock(prefix, s string) string {
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		lines[i] = strings.TrimRight(prefix+line, " \t")
	}
	return strings.Join(lines, "\n")
}

const template = `# tt configuration: $XDG_CONFIG_HOME/tt/config.toml, or ~/.config/tt/config.toml.
# Every key is optional and defaults to the value shown below. An unknown key is
# an error, so uncomment a line instead of inventing one. Tokens are not kept
# here.

# Use id:ID for stable selection; legacy names or name:NAME also work.
# default_project = 'Личное'
# default_focus = 'none'  # none | task:ID; cached open task, exact ID
# editor_hints = 'full'  # full | short | none
# color = 'auto'  # auto | always | never
#
#   'auto' colours only a terminal, and only when NO_COLOR is unset and TERM
#   is not 'dumb'. --color=always|never overrides this for one run; 'always'
#   is what a pipe into a pager wants, since a pipe is not a terminal.

# [timer]
# focus = '25m'
# short_break = '5m'
# long_break = '15m'
# long_every = 4  # long break after this many focus sessions, 1 to 12
# auto_break = true
# auto_focus = false
# warn_before = '0'  # '0' turns the warning off
# on_end = ''  # shell command to run when a focus session ends
# on_break_end = ''
#
#   The session's values reach the command through the environment:
#   $TT_KIND, $TT_NOTE, $TT_TASK, $TT_PROJECT, $TT_DURATION, $TT_CYCLE. The
#   shorthands {kind} {note} {task} {project} {duration} and {cycle} are
#   replaced by a reference to
#   the same variable, never by the value itself, so a task title cannot run
#   as shell code. Keep them outside quotes: "{task}" ends up as ""$TT_TASK""
#   and splits on spaces and globs, and '{task}' does not expand at all.
#
#   A bare {word} that is none of these reads as a typo: awk needs '{ print }'.
#
#   Single-quoted TOML takes no escapes, so the command's own quoting is all
#   there is to read. Both commands below hand the values to the notifier as
#   arguments: a value written into the middle of an osascript or powershell
#   script is compiled by it, task title and all.
#
#   macOS:  on_end = %s
#   Linux:  on_end = %s
# upload_aborted = false
# indicator = true  # external status; an explicit session choice overrides this

# [sync]
# interval = '1m'
# move_by_recreate = 'ask'  # ask | always | never
#
%s

# [focus_upload]
# enabled = false
`
