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

	cfg.AI = c.buildAI(r.AI, cfg.AI.Timeout)

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
	named := namedTables()
	type unknownTable struct {
		parts []string
		name  string
	}
	unknownTables := make(map[keyIdentity]unknownTable)
	markUnknown := func(parts []string) {
		unknownTables[identity(parts)] = unknownTable{
			parts: append([]string(nil), parts...),
			name:  strings.Join(parts, "."),
		}
	}
	// tableKeys returns the keys a table accepts, and whether tt knows the
	// table at all: the root, a top-level table, or a named sub-table such
	// as [ai.profiles.fast] whose name its named table allows.
	tableKeys := func(parts []string) ([]string, bool) {
		switch len(parts) {
		case 0:
			return leaves[""], true
		case 1:
			keys, known := leaves[parts[0]]
			return keys, known
		case 3:
			sub, known := named[[2]string{parts[0], parts[1]}]
			if !known || !sub.allows(parts[2]) {
				return nil, false
			}
			return sub.leaves, true
		}
		return nil, false
	}
	for _, k := range md.Undecoded() {
		parts := []string(k)
		if md.Type(k...) == "Hash" {
			if _, known := tableKeys(parts); !known {
				markUnknown(parts)
			}
			continue
		}
		if len(parts) > 1 {
			if _, known := tableKeys(parts[:len(parts)-1]); !known {
				markUnknown(parts[:len(parts)-1])
			}
		}
	}
	// A sub-table under a name its named table does not allow still decodes
	// into the map, so it is never undecoded: find it among all the keys,
	// however it was spelled (header, dotted key or inline table).
	for _, k := range md.Keys() {
		parts := []string(k)
		if len(parts) < 3 {
			continue
		}
		if sub, isNamed := named[[2]string{parts[0], parts[1]}]; isNamed && !sub.allows(parts[2]) {
			markUnknown(parts[:3])
		}
	}
	for _, table := range unknownTables {
		// A table inside another unknown table is that table's mistake,
		// already reported once, whatever line either of them is on.
		if insideUnknown(unknownTables, table.parts) {
			continue
		}
		msg := fmt.Sprintf("unknown table %q%s", table.name, unknownTableHint(table.parts, tables, named))
		c.addAt(table.parts, c.lineOf(md, table.parts), msg)
	}

	for _, k := range md.Undecoded() {
		parts := []string(k)
		if md.Type(k...) == "Hash" {
			continue
		}
		tableParts, name := parts[:len(parts)-1], parts[len(parts)-1]
		if _, unknown := unknownTables[identity(tableParts)]; unknown {
			continue
		}
		table := strings.Join(tableParts, ".")
		qualify := func(s string) string {
			if table == "" {
				return s
			}
			return table + "." + s
		}
		keys, _ := tableKeys(tableParts)
		path := strings.Join(parts, ".")
		c.addAt(parts, c.lineOf(md, parts), fmt.Sprintf("unknown key %q%s", path, suggest(name, keys, qualify)))
	}
}

// insideUnknown reports whether a table around parts is among unknown.
func insideUnknown[T any](unknown map[keyIdentity]T, parts []string) bool {
	for n := 1; n < len(parts); n++ {
		if _, found := unknown[identity(parts[:n])]; found {
			return true
		}
	}
	return false
}

// lineOf is the line to report a problem about parts at: the line of its own
// key or header; for a table written only through what is under it (a
// dotted key, a deeper header), the line of the first key under it; for a
// key inside an inline table, the line of the closest table around it.
func (c *collector) lineOf(md toml.MetaData, parts []string) int {
	if line := c.lines[identity(parts)]; line > 0 {
		return line
	}
	for _, k := range md.Keys() {
		if len(k) > len(parts) && slices.Equal([]string(k[:len(parts)]), parts) {
			if line := c.lines[identity(k)]; line > 0 {
				return line
			}
		}
	}
	for n := len(parts) - 1; n > 0; n-- {
		if line := c.lines[identity(parts[:n])]; line > 0 {
			return line
		}
	}
	return 0
}

// unknownTableHint is the suggestion for an unknown table: for a sub-table
// whose name its named table does not allow, the closest allowed name, or
// the full list when none is close; for a misspelt named table, such as
// [ai.profil.fast], the same path with the closest named table in it;
// otherwise the closest top-level table.
func unknownTableHint(parts, tables []string, named map[[2]string]namedTable) string {
	if len(parts) == 3 {
		if sub, isNamed := named[[2]string{parts[0], parts[1]}]; isNamed && sub.names != nil {
			prefix := parts[0] + "." + parts[1] + "."
			if hint := suggest(parts[2], sub.names, func(s string) string { return prefix + s }); hint != "" {
				return hint
			}
			return fmt.Sprintf(" (allowed: %s)", strings.Join(sub.names, ", "))
		}
	}
	if len(parts) >= 2 {
		if _, isNamed := named[[2]string{parts[0], parts[1]}]; !isNamed {
			var fields []string
			for path := range named {
				if path[0] == parts[0] {
					fields = append(fields, path[1])
				}
			}
			slices.Sort(fields)
			rest := ""
			if len(parts) > 2 {
				rest = "." + strings.Join(parts[2:], ".")
			}
			if hint := suggest(parts[1], fields, func(s string) string { return parts[0] + "." + s + rest }); hint != "" {
				return hint
			}
		}
	}
	return suggest(strings.Join(parts, "."), tables, nil)
}

// namedTable is a table whose sub-tables the user names, such as
// [ai.profiles.<name>]: a map of structs inside a top-level table.
type namedTable struct {
	// leaves are the keys every sub-table accepts, from the struct's tags.
	leaves []string

	// names are the only names a sub-table may take; nil allows any.
	names []string
}

func (t namedTable) allows(name string) bool {
	return t.names == nil || slices.Contains(t.names, name)
}

// namedTableNames restricts the sub-table names of a named table, keyed by
// its path; a named table left out, such as ai.profiles, takes any name.
var namedTableNames = map[[2]string][]string{
	{"ai", "tasks"}: aiTaskNames,
}

var namedTables = sync.OnceValue(func() map[[2]string]namedTable {
	named := map[[2]string]namedTable{}
	t := reflect.TypeOf(rawConfig{})
	for i := range t.NumField() {
		table := t.Field(i)
		if table.Type.Kind() != reflect.Struct {
			continue
		}
		for j := range table.Type.NumField() {
			field := table.Type.Field(j)
			if field.Type.Kind() != reflect.Map || field.Type.Elem().Kind() != reflect.Struct {
				continue
			}
			path := [2]string{table.Tag.Get("toml"), field.Tag.Get("toml")}
			sub := namedTable{names: namedTableNames[path]}
			elem := field.Type.Elem()
			for k := range elem.NumField() {
				sub.leaves = append(sub.leaves, elem.Field(k).Tag.Get("toml"))
			}
			named[path] = sub
		}
	}
	return named
})

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
