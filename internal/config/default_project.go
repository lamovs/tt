package config

import (
	"bytes"
	"errors"
	"reflect"
	"strings"

	"github.com/movsar/tt/internal/model"
)

func ProjectReference(value string) (id, name string, err error) {
	value = strings.TrimSpace(value)
	if strings.HasPrefix(value, "id:") {
		id = strings.TrimPrefix(value, "id:")
		if !model.ValidProjectID(id) {
			return "", "", errors.New("id: requires a non-empty list ID using letters, digits, '-' or '_'")
		}
		return id, "", nil
	}
	name = strings.TrimSpace(strings.TrimPrefix(value, "name:"))
	if name == "" {
		return "", "", errors.New("must not be empty")
	}
	return "", name, nil
}

func SetDefaultProject(src []byte, value string) ([]byte, error) {
	if _, _, err := ProjectReference(value); err != nil {
		return nil, err
	}
	return setRootString(src, "default_project", value, func(c *Config) { c.DefaultProject = value })
}

func setRootString(src []byte, key, value string, update func(*Config)) ([]byte, error) {
	before, err := ParseBytes("config", src)
	if err != nil {
		return nil, err
	}
	encoded := []byte(QuoteTOMLString(value))
	line := indexLines(src)[identity([]string{key})]
	var candidate []byte
	if line == 0 {
		candidate = append([]byte(key+" = "+string(encoded)+"\n"), src...)
	} else {
		start := 0
		for n := 1; n < line; n++ {
			start += bytes.IndexByte(src[start:], '\n') + 1
		}
		end := bytes.IndexByte(src[start:], '\n')
		if end < 0 {
			end = len(src) - start
		}
		eq := unquotedEqual(string(src[start : start+end]))
		if eq < 0 {
			return nil, errors.New("cannot locate " + key + " value")
		}
		start += eq + 1
		for start < len(src) && (src[start] == ' ' || src[start] == '\t') {
			start++
		}
		end, ok := configStringEnd(src, start)
		if !ok {
			return nil, errors.New("cannot safely replace " + key + " value")
		}
		candidate = append(candidate, src[:start]...)
		candidate = append(candidate, encoded...)
		candidate = append(candidate, src[end:]...)
	}
	after, err := ParseBytes("config", candidate)
	if err != nil {
		return nil, err
	}
	update(&before)
	if !reflect.DeepEqual(before, after) {
		return nil, errors.New(key + " edit changed another setting")
	}
	return candidate, nil
}

func configStringEnd(src []byte, start int) (int, bool) {
	if start >= len(src) || src[start] != '\'' && src[start] != '"' {
		return 0, false
	}
	quote := src[start]
	width := 1
	if start+2 < len(src) && src[start+1] == quote && src[start+2] == quote {
		width = 3
	}
	for i := start + width; i < len(src); {
		if quote == '"' && src[i] == '\\' {
			i += 2
			continue
		}
		if src[i] == quote {
			n := 1
			for i+n < len(src) && src[i+n] == quote {
				n++
			}
			if n >= width {
				if width == 1 {
					return i + 1, true
				}
				return i + n, true
			}
			i += n
		} else {
			i++
		}
	}
	return 0, false
}
