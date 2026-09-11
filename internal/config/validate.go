package config

import (
	"fmt"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/BurntSushi/toml"

	"github.com/movsar/tt/internal/model"
)

var placeholders = []string{"kind", "note", "task", "project", "duration", "cycle"}

func Placeholders() []string { return append([]string(nil), placeholders...) }

var placeholderRe = regexp.MustCompile(`\$?\{[a-z]+\}`)

func PlaceholderPattern() *regexp.Regexp { return placeholderRe }

func PlaceholderName(match string) (name string, ok bool) {
	if strings.HasPrefix(match, "$") {
		return "", false
	}
	return strings.Trim(match, "{}"), true
}

func DottedTimerKeys(src []byte) []string {
	md, err := toml.Decode(string(src), &struct{}{})
	if err != nil {
		return nil
	}
	keys, _ := dottedKeysAt(md, TimerTable)
	return keys
}

func dottedKeysAt(md toml.MetaData, table string) (keys []string, declaredAt int) {
	declaredAt = len(md.Keys())
	for i, k := range md.Keys() {
		if len(k) == 1 && k[0] == table {
			declaredAt = i
			break
		}
	}
	for i, k := range md.Keys() {
		if i > declaredAt {
			break
		}
		if len(k) != 2 || k[0] != table {
			continue
		}
		if t := md.Type(k...); t == "Hash" || t == "ArrayHash" {
			continue
		}
		keys = append(keys, k.String())
	}
	return keys, declaredAt
}

func TableNames() []string {
	_, tables := knownKeys()
	return append([]string(nil), tables...)
}

type DoubleOpen struct {
	Table  string
	Dotted []string
}

func DoubleOpenedTables(src []byte) []DoubleOpen {
	md, err := toml.Decode(string(src), &struct{}{})
	if err != nil {
		return nil
	}
	type found struct {
		open       DoubleOpen
		declaredAt int
	}
	var opened []found
	for _, table := range TableNames() {
		keys, declaredAt := dottedKeysAt(md, table)
		if len(keys) == 0 || md.Type(table) != "Hash" {
			continue
		}
		opened = append(opened, found{DoubleOpen{Table: table, Dotted: keys}, declaredAt})
	}
	slices.SortStableFunc(opened, func(a, b found) int { return a.declaredAt - b.declaredAt })
	out := make([]DoubleOpen, 0, len(opened))
	for _, f := range opened {
		out = append(out, f.open)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func SetsRootKey(src []byte, key string) bool {
	md, err := toml.Decode(string(src), &struct{}{})
	if err != nil {
		return false
	}
	return md.IsDefined(key)
}

func (c *collector) build(r rawConfig) Config {
	cfg := Default()

	if _, _, err := ProjectReference(r.DefaultProject); err != nil {
		c.add("default_project", "%s", err)
	} else {
		cfg.DefaultProject = r.DefaultProject
	}
	if ref, err := ParseFocusReference(r.DefaultFocus); err != nil {
		c.add("default_focus", "%s", err)
	} else {
		cfg.DefaultFocus = ref
	}
	if h, err := model.ParseEditorHints(r.EditorHints); err != nil {
		c.add("editor_hints", "%s", err)
	} else {
		cfg.EditorHints = h
	}
	if m, err := ParseColorMode(r.Color); err != nil {
		c.add("color", "%s", err)
	} else {
		cfg.Color = m
	}

	cfg.Timer.Focus = c.duration("timer.focus", r.Timer.Focus, true, cfg.Timer.Focus)
	cfg.Timer.ShortBreak = c.duration("timer.short_break", r.Timer.ShortBreak, true, cfg.Timer.ShortBreak)
	cfg.Timer.LongBreak = c.duration("timer.long_break", r.Timer.LongBreak, true, cfg.Timer.LongBreak)
	cfg.Timer.WarnBefore = c.duration("timer.warn_before", r.Timer.WarnBefore, false, cfg.Timer.WarnBefore)
	if r.Timer.LongEvery < 1 || r.Timer.LongEvery > 12 {
		c.add("timer.long_every", "must be between 1 and 12, got %d", r.Timer.LongEvery)
	} else {
		cfg.Timer.LongEvery = r.Timer.LongEvery
	}
	cfg.Timer.AutoBreak = r.Timer.AutoBreak
	cfg.Timer.AutoFocus = r.Timer.AutoFocus
	cfg.Timer.OnEnd = c.command("timer.on_end", r.Timer.OnEnd)
	cfg.Timer.OnBreakEnd = c.command("timer.on_break_end", r.Timer.OnBreakEnd)
	cfg.Timer.UploadAborted = r.Timer.UploadAborted
	cfg.Timer.Indicator = r.Timer.Indicator

	cfg.Sync.Interval = c.duration("sync.interval", r.Sync.Interval, true, cfg.Sync.Interval)
	if m, err := ParseMoveByRecreate(r.Sync.MoveByRecreate); err != nil {
		c.add("sync.move_by_recreate", "%s", err)
	} else {
		cfg.Sync.MoveByRecreate = m
	}
	cfg.FocusUpload.Enabled = r.FocusUpload.Enabled

	return cfg
}

func (c *collector) duration(key, val string, positive bool, def model.Duration) model.Duration {
	d, err := model.ParseDuration(val)
	if err != nil {
		c.add(key, "%s", err)
		return def
	}
	if positive && d.IsOff() {
		c.add(key, "must be greater than 0")
		return def
	}
	return d
}

func (c *collector) command(key, val string) string {
	for _, found := range placeholderRe.FindAllString(val, -1) {
		name, ok := PlaceholderName(found)
		if !ok {
			continue
		}
		if !slices.Contains(placeholders, name) {
			c.add(key, "unknown placeholder %q (allowed: %s)", found, allowedPlaceholders())
		}
	}
	return val
}

func allowedPlaceholders() string {
	names := make([]string, 0, len(placeholders))
	for _, p := range placeholders {
		names = append(names, "{"+p+"}")
	}
	return strings.Join(names, ", ")
}

func (c *collector) checkUnknown(md toml.MetaData) {
	leaves, tables := knownKeys()
	type unknownTable struct {
		parts []string
		name  string
	}
	unknownTables := make(map[keyIdentity]unknownTable)
	isKnownTable := func(parts []string) bool {
		if len(parts) != 1 {
			return false
		}
		_, known := leaves[parts[0]]
		return known
	}
	for _, k := range md.Undecoded() {
		parts := []string(k)
		if md.Type(k...) == "Hash" {
			if !isKnownTable(parts) {
				unknownTables[identity(parts)] = unknownTable{
					parts: append([]string(nil), parts...),
					name:  strings.Join(parts, "."),
				}
			}
			continue
		}
		if len(parts) > 1 {
			tableParts := parts[:len(parts)-1]
			if !isKnownTable(tableParts) {
				unknownTables[identity(tableParts)] = unknownTable{
					parts: append([]string(nil), tableParts...),
					name:  strings.Join(tableParts, "."),
				}
			}
		}
	}
	for _, table := range unknownTables {
		c.addAtParts(table.parts, fmt.Sprintf("unknown table %q%s", table.name, suggest(table.name, tables, nil)))
	}

	for _, k := range md.Undecoded() {
		parts := []string(k)
		if md.Type(k...) == "Hash" {
			continue
		}
		table, name := "", strings.Join(parts, ".")
		tableParts := []string(nil)
		if len(parts) > 1 {
			tableParts = parts[:len(parts)-1]
			table, name = strings.Join(tableParts, "."), parts[len(parts)-1]
		}
		if _, unknown := unknownTables[identity(tableParts)]; unknown {
			continue
		}
		qualify := func(s string) string {
			if table == "" {
				return s
			}
			return table + "." + s
		}
		path := strings.Join(parts, ".")
		c.addAtParts(parts, fmt.Sprintf("unknown key %q%s", path, suggest(name, leaves[table], qualify)))
	}
}

var knownKeys = sync.OnceValues(func() (map[string][]string, []string) {
	leaves := map[string][]string{"": nil}
	var tables []string
	t := reflect.TypeOf(rawConfig{})
	for i := range t.NumField() {
		f := t.Field(i)
		tag := f.Tag.Get("toml")
		if f.Type.Kind() != reflect.Struct {
			leaves[""] = append(leaves[""], tag)
			continue
		}
		tables = append(tables, tag)
		for j := range f.Type.NumField() {
			leaves[tag] = append(leaves[tag], f.Type.Field(j).Tag.Get("toml"))
		}
	}
	return leaves, tables
})

func suggest(word string, candidates []string, qualify func(string) string) string {
	limit := min(max(len(word)/3, 1), 3)
	best, bestDist := "", 0
	for _, cand := range candidates {
		d := levenshtein(word, cand)
		if d > limit || (best != "" && d >= bestDist) {
			continue
		}
		best, bestDist = cand, d
	}
	if best == "" {
		return ""
	}
	if qualify != nil {
		best = qualify(best)
	}
	return fmt.Sprintf(" (did you mean %q?)", best)
}

func levenshtein(a, b string) int {
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}
