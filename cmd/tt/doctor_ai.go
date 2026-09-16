package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"unicode"

	"github.com/movsar/tt/internal/ai"
	"github.com/movsar/tt/internal/cli"
	"github.com/movsar/tt/internal/config"
)

// aiProfileCheck resolves an agent binary for the AI check. Tests replace it.
var aiProfileCheck = func(ctx context.Context, cfg config.AI) []ai.Finding {
	return ai.New(cfg).Check(ctx)
}

const doctorAIProfileWidth = 24

func aiProfileBinary(p config.AIProfile) string {
	switch p.Engine {
	case config.AIEngineClaude, config.AIEngineCodex:
		return p.Engine.String()
	case config.AIEngineCommand:
		if len(p.Command) > 0 {
			return p.Command[0]
		}
	}
	return ""
}

// checkAI reports whether the agent CLI each referenced profile runs is on
// PATH. It starts nothing and sends nothing. A config that never mentions ai
// is only skipped when the built-in profile's binary is missing, since most
// users never run tt ai.
func checkAI(ctx context.Context) checkResult {
	cfg, err := config.Load()
	if err != nil {
		return checkResult{status: statusSkipped, summary: "ai: the config did not load, so its profiles were not checked"}
	}
	findings := aiProfileCheck(ctx, cfg.AI)
	scope := "ai.context " + cfg.AI.Context.String()
	if len(findings) == 0 {
		return checkResult{status: statusOK, summary: "ai: default profile " +
			cli.Foreign(cfg.AI.Default, doctorAIProfileWidth) + " can run (" + scope + "); no model was called"}
	}
	if reflect.DeepEqual(cfg.AI, config.Default().AI) {
		return checkResult{status: statusSkipped, summary: "ai: not configured, and the built-in profile's agent CLI is not on PATH; tt ai needs one (tt help ai)"}
	}
	var notes []string
	for _, f := range findings {
		profile, ok := cfg.AI.Profiles[f.Profile]
		why := "is referenced but not configured"
		if bin := aiProfileBinary(profile); ok && bin != "" {
			why = "runs " + cli.Foreign(bin, doctorNestedTextWidth-len("runs , which is not on PATH")) + ", which is not on PATH"
		}
		notes = append(notes, doctorNote("profile "+cli.Foreign(f.Profile, doctorTextWidth-len("profile "))+" "+why)...)
	}
	return checkResult{
		status:  statusWarn,
		summary: fmt.Sprintf("ai: %d configured profile(s) cannot run; tt ai falls back or fails", len(findings)),
		notes:   notes,
	}
}

// codexAgentsFile names the global instructions file codex sends: in
// $CODEX_HOME, or else in ~/.codex, AGENTS.override.md or, without one,
// AGENTS.md - the first that holds more than whitespace, since codex skips a
// file that does not. The name leaves the user's home out. ok is false when
// there is none.
func codexAgentsFile() (named string, ok bool) {
	dir, prefix := os.Getenv("CODEX_HOME"), "$CODEX_HOME/"
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", false
		}
		dir, prefix = filepath.Join(home, ".codex"), "~/.codex/"
	}
	for _, name := range []string{"AGENTS.override.md", "AGENTS.md"} {
		if codexFileHasText(filepath.Join(dir, name)) {
			return prefix + name, true
		}
	}
	return "", false
}

// codexFileHasText reports whether path is a regular file holding something
// besides whitespace. It reads only as far as the first character that is not
// whitespace, and keeps none of what it read.
func codexFileHasText(path string) bool {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	r := bufio.NewReader(f)
	for {
		c, _, err := r.ReadRune()
		if err != nil {
			return false
		}
		if !unicode.IsSpace(c) {
			return true
		}
	}
}

// checkCodexInstructions warns, when a configured profile runs codex, that
// codex adds its global instructions file to every call and has no switch
// that leaves it out. It looks only at whether the file is there and holds
// something, never inside it, and has nothing to say otherwise.
func checkCodexInstructions() (checkResult, bool) {
	cfg, err := config.Load()
	if err != nil {
		return checkResult{}, false
	}
	var names []string
	for name, profile := range cfg.AI.Profiles {
		if profile.Engine == config.AIEngineCodex {
			names = append(names, cli.Foreign(name, doctorAIProfileWidth))
		}
	}
	if len(names) == 0 {
		return checkResult{}, false
	}
	named, ok := codexAgentsFile()
	if !ok {
		return checkResult{}, false
	}
	slices.Sort(names)
	notes := doctorNote("codex adds " + named + " to every request it sends, and tt cannot leave it out; a claude profile sends no such file")
	notes = append(notes, doctorNote("profile(s) that run codex: "+strings.Join(names, ", "))...)
	return checkResult{
		status:  statusWarn,
		summary: "ai: codex sends your global instructions file with every tt ai call",
		notes:   notes,
	}, true
}
