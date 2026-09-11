package main

import "strings"

type quoting int

const (
	quoteNone quoting = iota
	quoteSingle
	quoteDouble
	quoteEscaped
)

type segment struct {
	from, to int
	q        quoting
}

type word struct {
	from, to int
	value    string
	segs     []segment
}

type simpleCommand struct {
	words []word
}

func splitShellWords(s string) ([]simpleCommand, bool) {
	n := len(s)
	i := 0
	var commands []simpleCommand
	var words []word

	finishCommand := func() {
		if len(words) == 0 {
			return
		}
		k := 0
		for k < len(words) && isAssignmentPrefix(words[k]) {
			k++
		}
		commands = append(commands, simpleCommand{words: words[k:]})
		words = nil
	}

	for i < n {
		c := s[i]
		switch {
		case c == ' ' || c == '\t':
			i++

		case c == '\n':
			finishCommand()
			i++

		case c == ';':
			finishCommand()
			i++

		case c == '(' || c == ')':

			finishCommand()
			i++

		case c == '&':
			if i+1 < n && s[i+1] == '>' {
				return nil, false
			}
			finishCommand()
			if i+1 < n && s[i+1] == '&' {
				i += 2
			} else {
				i++
			}

		case c == '|':
			if i+1 < n && s[i+1] == '&' {
				return nil, false
			}
			finishCommand()
			if i+1 < n && s[i+1] == '|' {
				i += 2
			} else {
				i++
			}

		case c == '\\' && i+1 < n && s[i+1] == '\n':

			i += 2

		case c == '<' || c == '>':
			next, ok := parseRedirect(s, i)
			if !ok {
				return nil, false
			}
			i = next

		default:
			w, next, ok := parseWord(s, i)
			if !ok {
				return nil, false
			}
			if next < n && (s[next] == '<' || s[next] == '>') && isDigitsOnlyUnquoted(w) {

				i = next
				continue
			}
			if isLoneBraceWord(w) {
				return nil, false
			}
			words = append(words, w)
			i = next
		}
	}
	finishCommand()

	for _, cmd := range commands {
		if len(cmd.words) > 0 && isReservedProgramWord(cmd.words[0]) {
			return nil, false
		}
	}
	return commands, true
}

func parseWord(s string, start int) (word, int, bool) {
	n := len(s)
	i := start

	var val strings.Builder
	var segs []segment
	haveSeg := false
	var segFrom, segNext int
	var segQ quoting

	addByte := func(b byte, pos int, q quoting) {
		if haveSeg && q == segQ && pos == segNext {
			segNext = pos + 1
		} else {
			if haveSeg {
				segs = append(segs, segment{segFrom, segNext, segQ})
			}
			segFrom, segQ, segNext = pos, q, pos+1
			haveSeg = true
		}
		val.WriteByte(b)
	}

	for i < n && !isWordTerminator(s[i]) {
		c := s[i]

		if c == '\'' {

			j := i + 1
			for j < n && s[j] != '\'' {
				j++
			}
			if j >= n {
				return word{}, 0, false
			}
			for k := i + 1; k < j; k++ {
				addByte(s[k], k, quoteSingle)
			}
			i = j + 1
			continue
		}

		if c == '"' {
			k := i + 1
			closed := false
			for k < n {
				dc := s[k]
				if dc == '"' {
					closed = true
					k++
					break
				}
				if dc == '\\' {
					if k+1 >= n {
						return word{}, 0, false
					}
					nc := s[k+1]
					switch nc {
					case '$', '`', '"', '\\':
						addByte(nc, k+1, quoteEscaped)
						k += 2
					case '\n':
						k += 2
					default:

						addByte('\\', k, quoteDouble)
						k++
					}
					continue
				}
				if dc == '$' {
					if k+1 < n && s[k+1] == '(' {
						return word{}, 0, false
					}
					if k+1 < n && s[k+1] == '{' {
						end, okBrace := paramBraceEnd(s, k+1)
						if !okBrace {
							return word{}, 0, false
						}
						for p := k; p < end; p++ {
							addByte(s[p], p, quoteDouble)
						}
						k = end
						continue
					}
					addByte('$', k, quoteDouble)
					k++
					continue
				}
				if dc == '`' {
					return word{}, 0, false
				}
				addByte(dc, k, quoteDouble)
				k++
			}
			if !closed {
				return word{}, 0, false
			}
			i = k
			continue
		}

		if c == '\\' {
			if i+1 >= n {
				return word{}, 0, false
			}
			nc := s[i+1]
			if nc == '\n' {
				i += 2
				continue
			}
			addByte(nc, i+1, quoteEscaped)
			i += 2
			continue
		}

		if c == '$' {
			if i+1 < n && s[i+1] == '(' {
				return word{}, 0, false
			}
			if i+1 < n && (s[i+1] == '\'' || s[i+1] == '"') {

				return word{}, 0, false
			}
			if i+1 < n && s[i+1] == '{' {
				end, okBrace := paramBraceEnd(s, i+1)
				if !okBrace {
					return word{}, 0, false
				}
				for p := i; p < end; p++ {
					addByte(s[p], p, quoteNone)
				}
				i = end
				continue
			}
			addByte('$', i, quoteNone)
			i++
			continue
		}

		if c == '`' {
			return word{}, 0, false
		}

		addByte(c, i, quoteNone)
		i++
	}

	if haveSeg {
		segs = append(segs, segment{segFrom, segNext, segQ})
	}
	return word{from: start, to: i, value: val.String(), segs: segs}, i, true
}

func paramBraceEnd(s string, open int) (end int, ok bool) {
	n := len(s)
	i := open + 1
	if i >= n || !isIdentStart(s[i]) {
		return 0, false
	}
	i++
	for i < n && isIdentByte(s[i]) {
		i++
	}
	if i >= n || s[i] != '}' {
		return 0, false
	}
	return i + 1, true
}

func parseRedirect(s string, i int) (next int, ok bool) {
	n := len(s)

	if s[i] == '<' {
		if i+1 < n && s[i+1] == '(' {
			return 0, false
		}
		switch {
		case i+2 < n && s[i+1] == '<' && s[i+2] == '<':
			i += 3
		case i+1 < n && s[i+1] == '<':
			return 0, false
		case i+1 < n && s[i+1] == '&':
			i += 2
		case i+1 < n && s[i+1] == '>':
			i += 2
		default:
			i++
		}
	} else {
		if i+1 < n && s[i+1] == '(' {
			return 0, false
		}
		switch {
		case i+1 < n && s[i+1] == '>':
			i += 2
		case i+1 < n && s[i+1] == '&':
			i += 2
		default:
			i++
		}
	}

	for i < n {
		if s[i] == ' ' || s[i] == '\t' {
			i++
			continue
		}
		if s[i] == '\\' && i+1 < n && s[i+1] == '\n' {
			i += 2
			continue
		}
		break
	}
	if i >= n || isWordTerminator(s[i]) {
		return 0, false
	}
	_, next, ok = parseWord(s, i)
	if !ok {
		return 0, false
	}
	return next, true
}

func isWordTerminator(b byte) bool {
	switch b {
	case ' ', '\t', '\n', ';', '&', '|', '(', ')', '<', '>':
		return true
	}
	return false
}

func isIdentStart(b byte) bool {
	return b == '_' || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

func isIdentByte(b byte) bool {
	return isIdentStart(b) || (b >= '0' && b <= '9')
}

func assignmentPrefixLen(value string) int {
	if len(value) == 0 || !isIdentStart(value[0]) {
		return 0
	}
	i := 1
	for i < len(value) && isIdentByte(value[i]) {
		i++
	}
	if i >= len(value) || value[i] != '=' {
		return 0
	}
	return i + 1
}

func prefixIsUnquoted(w word, n int) bool {
	pos := 0
	for _, sg := range w.segs {
		if pos >= n {
			break
		}
		segLen := sg.to - sg.from
		covered := segLen
		if pos+covered > n {
			covered = n - pos
		}
		if covered > 0 && sg.q != quoteNone {
			return false
		}
		pos += segLen
	}
	return pos >= n
}

func isAssignmentPrefix(w word) bool {
	n := assignmentPrefixLen(w.value)
	return n > 0 && prefixIsUnquoted(w, n)
}

func isDigitsOnlyUnquoted(w word) bool {
	if w.value == "" {
		return false
	}
	for i := 0; i < len(w.value); i++ {
		if w.value[i] < '0' || w.value[i] > '9' {
			return false
		}
	}
	return prefixIsUnquoted(w, len(w.value))
}

func isLoneBraceWord(w word) bool {
	if w.value != "{" && w.value != "}" {
		return false
	}
	return prefixIsUnquoted(w, len(w.value))
}

var shellReservedWords = map[string]bool{
	"if": true, "for": true, "while": true, "case": true,
	"do": true, "then": true, "!": true, "[[": true,
}

func isReservedProgramWord(w word) bool {
	if !shellReservedWords[w.value] {
		return false
	}
	return prefixIsUnquoted(w, len(w.value))
}
