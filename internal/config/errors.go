package config

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
)

type Problem struct {
	Line int
	Msg  string
}

type Error struct {
	Path     string
	Problems []Problem
}

func (e *Error) Error() string {
	msgs := make([]string, 0, len(e.Problems))
	for _, p := range e.Problems {
		if p.Line > 0 {
			msgs = append(msgs, fmt.Sprintf("%s:%d: %s", e.Path, p.Line, p.Msg))
			continue
		}
		msgs = append(msgs, fmt.Sprintf("%s: %s", e.Path, p.Msg))
	}
	return strings.Join(msgs, "\n")
}

var tomlContext = regexp.MustCompile(`^(?:line (\d+) )?\(last key "([^"]*)"\): `)

func decodeProblem(err error) Problem {
	var pe toml.ParseError
	if errors.As(err, &pe) {
		msg := pe.Message
		if pe.LastKey != "" {
			msg = pe.LastKey + ": " + msg
		}
		return Problem{Line: pe.Position.Line, Msg: msg}
	}
	msg := strings.TrimPrefix(err.Error(), "toml: ")
	if m := tomlContext.FindStringSubmatch(msg); m != nil {
		line, _ := strconv.Atoi(m[1])
		return Problem{Line: line, Msg: m[2] + ": " + msg[len(m[0]):]}
	}
	return Problem{Msg: msg}
}

type collector struct {
	lines    lineIndex
	problems []collectedProblem
}

type collectedProblem struct {
	Problem
	key keyIdentity
}

type keyIdentity string

type lineIndex map[keyIdentity]int

func identity(parts []string) keyIdentity {
	var b strings.Builder
	for _, part := range parts {
		fmt.Fprintf(&b, "%d:", len(part))
		b.WriteString(part)
	}
	return keyIdentity(b.String())
}

func semanticParts(key string) []string { return strings.Split(key, ".") }

func (c *collector) add(key, format string, args ...any) {
	c.addAtParts(semanticParts(key), key+": "+fmt.Sprintf(format, args...))
}

func (c *collector) addAtParts(parts []string, msg string) {
	c.addAt(parts, c.lines[identity(parts)], msg)
}

func (c *collector) addAt(parts []string, line int, msg string) {
	c.problems = append(c.problems, collectedProblem{
		Problem: Problem{Line: line, Msg: msg},
		key:     identity(parts),
	})
}

func (c *collector) sorted() []Problem {
	p := append([]collectedProblem(nil), c.problems...)
	sort.Slice(p, func(i, j int) bool {
		left, right := p[i], p[j]
		if (left.Line == 0) != (right.Line == 0) {
			return left.Line != 0
		}
		if left.Line != right.Line {
			return left.Line < right.Line
		}
		if left.key != right.key {
			return left.key < right.key
		}
		return left.Msg < right.Msg
	})
	out := make([]Problem, len(p))
	for i := range p {
		out[i] = p[i].Problem
	}
	return out
}

func indexLines(src []byte) lineIndex {
	lines := make(lineIndex)
	put := func(parts []string, n int) {
		key := identity(parts)
		if _, seen := lines[key]; !seen {
			lines[key] = n
		}
	}

	var table []string
	state := scanState{}
	for i, raw := range strings.Split(string(src), "\n") {
		if !state.topLevel() {
			scanValue(raw, &state)
			continue
		}

		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") {
			end, ok := headerEnd(line)
			if !ok {
				continue
			}
			parts, ok := decodeDeclaration(line[:end])
			if !ok {
				continue
			}
			table = parts
			put(parts, i+1)
			scanValue(line[end:], &state)
			continue
		}

		eq := unquotedEqual(line)
		if eq <= 0 {
			continue
		}
		parts, ok := decodeDeclaration(strings.TrimSpace(line[:eq]) + " = 0")
		valueOK := scanValue(line[eq+1:], &state)
		if ok && valueOK {
			full := append(append([]string(nil), table...), parts...)
			put(full, i+1)
		}
	}
	if !state.topLevel() {
		return make(lineIndex)
	}
	return lines
}

func decodeDeclaration(fragment string) ([]string, bool) {
	text := fragment
	if strings.HasPrefix(strings.TrimSpace(fragment), "[") {
		text += "\n"
	}
	md, err := toml.Decode(text, &struct{}{})
	if err != nil {
		return nil, false
	}
	keys := md.Keys()
	if len(keys) == 0 {
		return nil, false
	}
	parts := append([]string(nil), keys[0]...)
	return parts, len(parts) > 0
}

type scanState struct {
	multiline byte
	square    int
	curly     int
}

func (s scanState) topLevel() bool {
	return s.multiline == 0 && s.square == 0 && s.curly == 0
}

func unquotedEqual(line string) int {
	for i := 0; i < len(line); {
		switch line[i] {
		case '#':
			return -1
		case '=':
			return i
		case '"', '\'':
			end, ok := scanSingleQuoted(line, i+1, line[i])
			if !ok {
				return -1
			}
			i = end
		default:
			i++
		}
	}
	return -1
}

func headerEnd(line string) (int, bool) {
	want := 1
	if strings.HasPrefix(line, "[[") {
		want = 2
	}
	for i := want; i < len(line); {
		switch line[i] {
		case '"', '\'':
			end, ok := scanSingleQuoted(line, i+1, line[i])
			if !ok {
				return 0, false
			}
			i = end
		case ']':
			if want == 1 {
				return i + 1, true
			}
			if i+1 < len(line) && line[i+1] == ']' {
				return i + 2, true
			}
			i++
		default:
			i++
		}
	}
	return 0, false
}

func scanValue(line string, state *scanState) bool {
	for i := 0; i < len(line); {
		if state.multiline != 0 {
			quote := state.multiline
			if quote == '"' && line[i] == '\\' {
				i += 2
				continue
			}
			if line[i] == quote {
				run := quoteRun(line, i)
				if run >= 3 {
					state.multiline = 0
					i += run
					continue
				}
				i += run
				continue
			}
			i++
			continue
		}

		switch line[i] {
		case '#':
			return true
		case '"', '\'':
			quote := line[i]
			if quoteRun(line, i) >= 3 {
				state.multiline = quote
				i += 3
				continue
			}
			end, ok := scanSingleQuoted(line, i+1, quote)
			if !ok {
				return false
			}
			i = end
		case '[':
			state.square++
			i++
		case ']':
			if state.square > 0 {
				state.square--
			}
			i++
		case '{':
			state.curly++
			i++
		case '}':
			if state.curly > 0 {
				state.curly--
			}
			i++
		default:
			i++
		}
	}
	return true
}

func scanSingleQuoted(line string, from int, quote byte) (end int, closed bool) {
	for i := from; i < len(line); i++ {
		if quote == '"' && line[i] == '\\' {
			i++
			continue
		}
		if line[i] == quote {
			return i + 1, true
		}
	}
	return len(line), false
}

func quoteRun(line string, i int) int {
	n := 0
	for ; i+n < len(line) && line[i+n] == line[i]; n++ {
	}
	return n
}
