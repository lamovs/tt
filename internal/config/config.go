package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/movsar/tt/internal/model"
)

const FileName = "config.toml"

const TimerTable = "timer"

type Config struct {
	DefaultProject string
	DefaultFocus   FocusReference
	EditorHints    model.EditorHints
	Color          ColorMode
	Timer          Timer
	Sync           Sync
	FocusUpload    FocusUpload
	AI             AI
}

type ColorMode string

const (
	ColorAuto ColorMode = "auto"

	ColorAlways ColorMode = "always"

	ColorNever ColorMode = "never"
)

var colorModeNames = []ColorMode{ColorAuto, ColorAlways, ColorNever}

func ParseColorMode(s string) (ColorMode, error) {
	m := ColorMode(strings.ToLower(strings.TrimSpace(s)))
	if slices.Contains(colorModeNames, m) {
		return m, nil
	}
	names := make([]string, 0, len(colorModeNames))
	for _, n := range colorModeNames {
		names = append(names, string(n))
	}
	return "", fmt.Errorf("unknown value %q (allowed: %s)", s, strings.Join(names, ", "))
}

func (m ColorMode) String() string { return string(m) }

type Timer struct {
	Focus         model.Duration
	ShortBreak    model.Duration
	LongBreak     model.Duration
	LongEvery     int
	AutoBreak     bool
	AutoFocus     bool
	WarnBefore    model.Duration
	OnEnd         string
	OnBreakEnd    string
	UploadAborted bool
	Indicator     bool
}

type Sync struct {
	Interval       model.Duration
	MoveByRecreate MoveByRecreate
}

type MoveByRecreate string

const (
	MoveAsk MoveByRecreate = "ask"

	MoveAlways MoveByRecreate = "always"

	MoveNever MoveByRecreate = "never"
)

var moveByRecreateNames = []MoveByRecreate{MoveAsk, MoveAlways, MoveNever}

func ParseMoveByRecreate(s string) (MoveByRecreate, error) {
	m := MoveByRecreate(strings.ToLower(strings.TrimSpace(s)))
	if slices.Contains(moveByRecreateNames, m) {
		return m, nil
	}
	names := make([]string, 0, len(moveByRecreateNames))
	for _, n := range moveByRecreateNames {
		names = append(names, string(n))
	}
	return "", fmt.Errorf("unknown value %q (allowed: %s)", s, strings.Join(names, ", "))
}

func (m MoveByRecreate) String() string { return string(m) }

func (m MoveByRecreate) Recreates() bool { return m == MoveAlways }

func (m MoveByRecreate) Refusal() string {
	if m != MoveNever {
		return ""
	}
	return "tt will not move a task to another list.\n" + MoveByRecreateNotice +
		"\nsync.move_by_recreate is set to 'never', so the task was left where it is."
}

type FocusUpload struct {
	Enabled bool
}

func Default() Config {
	return Config{
		DefaultProject: "Личное",
		EditorHints:    model.HintsFull,
		Color:          ColorAuto,
		Timer: Timer{
			Focus:      model.Duration(25 * time.Minute),
			ShortBreak: model.Duration(5 * time.Minute),
			LongBreak:  model.Duration(15 * time.Minute),
			LongEvery:  4,
			AutoBreak:  true,
			Indicator:  true,
		},
		Sync: Sync{
			Interval:       model.Duration(60 * time.Second),
			MoveByRecreate: MoveAsk,
		},
		AI: defaultAI(),
	}
}

func Path() (string, error) {
	if dir := os.Getenv("XDG_CONFIG_HOME"); filepath.IsAbs(dir) {
		return filepath.Join(dir, "tt", FileName), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "tt", FileName), nil
}

func Load() (Config, error) {
	path, err := Path()
	if err != nil {
		return Config{}, err
	}
	src, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Default(), nil
	}
	if err != nil {
		return Config{}, err
	}
	return parse(path, src)
}

func LoadFile(path string) (Config, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	return parse(path, src)
}

func ParseBytes(path string, src []byte) (Config, error) {
	return parse(path, src)
}

func OpensTimerTable(path string) bool {
	src, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return OpensTimerTableIn(src)
}

func OpensTimerTableIn(src []byte) bool {
	md, err := toml.Decode(string(src), &struct{}{})
	if err != nil {
		return false
	}
	return md.Type(TimerTable) == "Hash"
}

func IsTimerHeader(line string) bool {
	md, err := toml.Decode(line, &struct{}{})
	if err != nil {
		return false
	}
	keys := md.Keys()

	return len(keys) == 1 && len(keys[0]) == 1 && keys[0][0] == TimerTable &&
		md.Type(TimerTable) == "Hash"
}

func parse(path string, src []byte) (Config, error) {
	raw := toRaw(Default())
	md, err := toml.Decode(string(src), &raw)
	if err != nil {
		return Config{}, &Error{Path: path, Problems: []Problem{decodeProblem(err)}}
	}

	lines := indexLines(src)
	c := &collector{lines: lines}
	c.checkUnknown(md)
	cfg := c.build(raw)
	if len(c.problems) > 0 {
		return Config{}, &Error{Path: path, Problems: c.sorted()}
	}
	return cfg, nil
}

type rawConfig struct {
	DefaultProject string         `toml:"default_project"`
	DefaultFocus   string         `toml:"default_focus"`
	EditorHints    string         `toml:"editor_hints"`
	Color          string         `toml:"color"`
	Timer          rawTimer       `toml:"timer"`
	Sync           rawSync        `toml:"sync"`
	FocusUpload    rawFocusUpload `toml:"focus_upload"`
	AI             rawAI          `toml:"ai"`
}

type rawTimer struct {
	Focus         string `toml:"focus"`
	ShortBreak    string `toml:"short_break"`
	LongBreak     string `toml:"long_break"`
	LongEvery     int    `toml:"long_every"`
	AutoBreak     bool   `toml:"auto_break"`
	AutoFocus     bool   `toml:"auto_focus"`
	WarnBefore    string `toml:"warn_before"`
	OnEnd         string `toml:"on_end"`
	OnBreakEnd    string `toml:"on_break_end"`
	UploadAborted bool   `toml:"upload_aborted"`
	Indicator     bool   `toml:"indicator"`
}

type rawSync struct {
	Interval       string `toml:"interval"`
	MoveByRecreate string `toml:"move_by_recreate"`
}

type rawFocusUpload struct {
	Enabled bool `toml:"enabled"`
}

func toRaw(c Config) rawConfig {
	return rawConfig{
		DefaultProject: c.DefaultProject,
		DefaultFocus:   c.DefaultFocus.String(),
		EditorHints:    c.EditorHints.String(),
		Color:          c.Color.String(),
		Timer: rawTimer{
			Focus:         c.Timer.Focus.String(),
			ShortBreak:    c.Timer.ShortBreak.String(),
			LongBreak:     c.Timer.LongBreak.String(),
			LongEvery:     c.Timer.LongEvery,
			AutoBreak:     c.Timer.AutoBreak,
			AutoFocus:     c.Timer.AutoFocus,
			WarnBefore:    c.Timer.WarnBefore.String(),
			OnEnd:         c.Timer.OnEnd,
			OnBreakEnd:    c.Timer.OnBreakEnd,
			UploadAborted: c.Timer.UploadAborted,
			Indicator:     c.Timer.Indicator,
		},
		Sync: rawSync{
			Interval:       c.Sync.Interval.String(),
			MoveByRecreate: c.Sync.MoveByRecreate.String(),
		},
		FocusUpload: rawFocusUpload{Enabled: c.FocusUpload.Enabled},
		AI:          toRawAI(c.AI),
	}
}
