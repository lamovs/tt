package config

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/movsar/tt/internal/model"
)

type AI struct {
	Default  string
	Timeout  model.Duration
	Context  AIContext
	Fallback []string
	Profiles map[string]AIProfile
	Tasks    map[string]AITask
}

type AIContext string

const (
	AIContextMinimal AIContext = "minimal"

	AIContextToday AIContext = "today"

	AIContextAll AIContext = "all"
)

var aiContextNames = []AIContext{AIContextMinimal, AIContextToday, AIContextAll}

func ParseAIContext(s string) (AIContext, error) {
	m := AIContext(strings.ToLower(strings.TrimSpace(s)))
	if slices.Contains(aiContextNames, m) {
		return m, nil
	}
	names := make([]string, 0, len(aiContextNames))
	for _, n := range aiContextNames {
		names = append(names, string(n))
	}
	return "", fmt.Errorf("unknown value %q (allowed: %s)", s, strings.Join(names, ", "))
}

func (c AIContext) String() string { return string(c) }

type AIEngine string

const (
	AIEngineClaude AIEngine = "claude"

	AIEngineCodex AIEngine = "codex"

	AIEngineCommand AIEngine = "command"
)

var aiEngineNames = []AIEngine{AIEngineClaude, AIEngineCodex, AIEngineCommand}

func ParseAIEngine(s string) (AIEngine, error) {
	m := AIEngine(strings.ToLower(strings.TrimSpace(s)))
	if slices.Contains(aiEngineNames, m) {
		return m, nil
	}
	names := make([]string, 0, len(aiEngineNames))
	for _, n := range aiEngineNames {
		names = append(names, string(n))
	}
	return "", fmt.Errorf("unknown value %q (allowed: %s)", s, strings.Join(names, ", "))
}

func (e AIEngine) String() string { return string(e) }

// AIProfile is one named way to call an agent. Fields are written in full:
// redefining a profile does not merge with a previous definition of the same
// name, field by field.
type AIProfile struct {
	Engine  AIEngine
	Model   string
	Effort  string
	Command []string

	// Env names extra environment variables, read from tt's own
	// environment, to forward to this profile's subprocess in addition to
	// the runner's fixed allowlist. tt's own settings and credentials
	// (TT_*, HERDR_*, XDG_CONFIG_HOME, XDG_DATA_HOME) can never be listed
	// here; buildAI rejects them.
	Env []string
}

type AITask struct {
	Profile string
	Prompt  string
}

// The names an [ai.tasks.<name>] table may take, one per command that calls
// an agent. Any other name is an unknown table.
const (
	// AITaskAI configures `tt ai`.
	AITaskAI = "ai"

	// AITaskFind configures `tt ai find`.
	AITaskFind = "find"
)

var aiTaskNames = []string{AITaskAI, AITaskFind}

func defaultAI() AI {
	return AI{
		Default: "fast",
		Timeout: model.Duration(90 * time.Second),
		Context: AIContextMinimal,
		Profiles: map[string]AIProfile{
			"fast": {Engine: AIEngineClaude, Model: "sonnet", Effort: "high"},
		},
		Tasks: map[string]AITask{},
	}
}

// aiPlaceholders are substituted into an engine = "command" argv override.
// This is a separate namespace from Placeholders() (used by timer.on_end and
// timer.on_break_end), which only allows single lowercase words.
var aiPlaceholders = []string{"model", "effort", "schema", "schema_file", "output_file", "image", "prompt"}

func AIPlaceholders() []string { return append([]string(nil), aiPlaceholders...) }

var aiPlaceholderRe = regexp.MustCompile(`\{[a-z_]+\}`)

func AIPlaceholderPattern() *regexp.Regexp { return aiPlaceholderRe }

type rawAI struct {
	Default  string                  `toml:"default"`
	Timeout  string                  `toml:"timeout"`
	Context  string                  `toml:"context"`
	Fallback []string                `toml:"fallback"`
	Profiles map[string]rawAIProfile `toml:"profiles"`
	Tasks    map[string]rawAITask    `toml:"tasks"`
}

type rawAIProfile struct {
	Engine  string   `toml:"engine"`
	Model   string   `toml:"model"`
	Effort  string   `toml:"effort"`
	Command []string `toml:"command"`
	Env     []string `toml:"env"`
}

type rawAITask struct {
	Profile string `toml:"profile"`
	Prompt  string `toml:"prompt"`
}

func toRawAI(a AI) rawAI {
	profiles := make(map[string]rawAIProfile, len(a.Profiles))
	for name, p := range a.Profiles {
		profiles[name] = rawAIProfile{
			Engine:  p.Engine.String(),
			Model:   p.Model,
			Effort:  p.Effort,
			Command: append([]string(nil), p.Command...),
			Env:     append([]string(nil), p.Env...),
		}
	}
	tasks := make(map[string]rawAITask, len(a.Tasks))
	for name, t := range a.Tasks {
		tasks[name] = rawAITask{Profile: t.Profile, Prompt: t.Prompt}
	}
	return rawAI{
		Default:  a.Default,
		Timeout:  a.Timeout.String(),
		Context:  a.Context.String(),
		Fallback: append([]string(nil), a.Fallback...),
		Profiles: profiles,
		Tasks:    tasks,
	}
}

func (c *collector) buildAI(r rawAI, defTimeout model.Duration) AI {
	ai := AI{Default: r.Default}

	ai.Timeout = c.duration("ai.timeout", r.Timeout, true, defTimeout)

	if m, err := ParseAIContext(r.Context); err != nil {
		c.add("ai.context", "%s", err)
	} else {
		ai.Context = m
	}

	ai.Fallback = append([]string(nil), r.Fallback...)

	ai.Profiles = make(map[string]AIProfile, len(r.Profiles))
	for name, rp := range r.Profiles {
		prof := AIProfile{
			Model:   rp.Model,
			Effort:  rp.Effort,
			Command: append([]string(nil), rp.Command...),
			Env:     append([]string(nil), rp.Env...),
		}
		if eng, err := ParseAIEngine(rp.Engine); err != nil {
			c.add(fmt.Sprintf("ai.profiles.%s.engine", name), "%s", err)
		} else {
			prof.Engine = eng
		}
		if prof.Engine == AIEngineCommand && len(prof.Command) == 0 {
			c.add(fmt.Sprintf("ai.profiles.%s.command", name), "command is required when engine = \"command\"")
		}
		for _, arg := range prof.Command {
			c.aiCommandArg(fmt.Sprintf("ai.profiles.%s.command", name), arg)
		}
		for _, envVar := range prof.Env {
			if reason, forbidden := ForbiddenAIEnvVar(envVar); forbidden {
				c.add(fmt.Sprintf("ai.profiles.%s.env", name), "%q is not allowed (%s)", envVar, reason)
			}
		}
		ai.Profiles[name] = prof
	}

	ai.Tasks = make(map[string]AITask, len(r.Tasks))
	for name, rt := range r.Tasks {
		if !slices.Contains(aiTaskNames, name) {
			// checkUnknown reports the table itself; checking its fields
			// too would only repeat the same mistake.
			continue
		}
		task := AITask{Profile: rt.Profile}
		if rt.Prompt != "" {
			expanded, err := expandHome(rt.Prompt)
			if err != nil {
				c.add(fmt.Sprintf("ai.tasks.%s.prompt", name), "%s", err)
			} else {
				task.Prompt = expanded
			}
		}
		ai.Tasks[name] = task
	}

	if ai.Default != "" {
		if _, ok := ai.Profiles[ai.Default]; !ok {
			c.add("ai.default", "references unknown profile %q", ai.Default)
		}
	}
	for _, name := range ai.Fallback {
		if _, ok := ai.Profiles[name]; !ok {
			c.add("ai.fallback", "references unknown profile %q", name)
		}
	}
	for name, task := range ai.Tasks {
		if task.Profile != "" {
			if _, ok := ai.Profiles[task.Profile]; !ok {
				c.add(fmt.Sprintf("ai.tasks.%s.profile", name), "references unknown profile %q", task.Profile)
			}
		}
	}

	return ai
}

func (c *collector) aiCommandArg(key, val string) {
	for _, found := range aiPlaceholderRe.FindAllString(val, -1) {
		name := strings.Trim(found, "{}")
		if !slices.Contains(aiPlaceholders, name) {
			c.add(key, "unknown placeholder %q (allowed: %s)", found, allowedAIPlaceholders())
		}
	}
	// {image} expands to one argument per image, or to none, so it can only
	// stand for a whole argument; inside a longer one it would never expand.
	if strings.Contains(val, "{image}") && val != "{image}" {
		c.add(key, "%q must be a whole argument, got %q", "{image}", val)
	}
}

// ForbiddenAIEnvVar reports whether name may never be listed in a profile's
// env passthrough: tt's own settings and credentials, and Herdr's session
// variables, are never forwarded to an agent subprocess, no matter what a
// profile asks for.
func ForbiddenAIEnvVar(name string) (reason string, forbidden bool) {
	switch {
	case strings.HasPrefix(name, "TT_"):
		return "tt's own settings and credentials are never forwarded", true
	case strings.HasPrefix(name, "HERDR_"):
		return "Herdr's session variables are never forwarded", true
	case name == "XDG_CONFIG_HOME" || name == "XDG_DATA_HOME":
		return "tt's own config and data locations are never forwarded", true
	default:
		return "", false
	}
}

func allowedAIPlaceholders() string {
	names := make([]string, 0, len(aiPlaceholders))
	for _, p := range aiPlaceholders {
		names = append(names, "{"+p+"}")
	}
	return strings.Join(names, ", ")
}

// expandHome expands a leading "~" or "~/" to the user's home directory.
// Any other path, including one that is already absolute or relative, is
// returned unchanged.
func expandHome(path string) (string, error) {
	if path == "~" {
		return os.UserHomeDir()
	}
	if rest, ok := strings.CutPrefix(path, "~/"); ok {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, rest), nil
	}
	return path, nil
}
