package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/movsar/tt/internal/ai"
	"github.com/movsar/tt/internal/config"
)

func doctorAIStub(t *testing.T, findings []ai.Finding) {
	t.Helper()
	previous := aiProfileCheck
	aiProfileCheck = func(context.Context, config.AI) []ai.Finding { return findings }
	t.Cleanup(func() { aiProfileCheck = previous })
}

func doctorAILines(t *testing.T, r checkResult) string {
	t.Helper()
	var out bytes.Buffer
	printCheck(&out, r)
	for _, line := range strings.Split(strings.TrimRight(out.String(), "\n"), "\n") {
		if n := len([]rune(line)); n > 80 {
			t.Errorf("a doctor line is %d columns wide:\n%q", n, line)
		}
		if strings.ContainsAny(line, "\x1b\u202e") {
			t.Errorf("a doctor line carries a raw control or direction character:\n%q", line)
		}
	}
	return out.String()
}

func TestDoctorAICheckBoundsProfileNames(t *testing.T) {
	isolate(t)
	name := `evil\u202e` + strings.Repeat("z", 90)
	writeConfig(t, "[ai]\ndefault = \""+name+"\"\n\n[ai.profiles.\""+name+"\"]\nengine = \"command\"\ncommand = [\""+name+"\"]\n")
	ctx := context.Background()

	r := checkAI(ctx)
	if r.status != statusWarn || len(r.notes) == 0 {
		t.Fatalf("a profile whose binary is missing = %+v, want a warning with notes", r)
	}
	out := doctorAILines(t, r)
	if !strings.Contains(out, `"evil\u202e`) || !strings.Contains(out, "which is not on PATH") {
		t.Errorf("the warning does not name the profile escaped:\n%s", out)
	}

	hostile := "evil\x1b[31m" + strings.Repeat("z", 90)
	doctorAIStub(t, []ai.Finding{{Profile: hostile, Message: "profile not configured"}})
	if out := doctorAILines(t, checkAI(ctx)); !strings.Contains(out, `"evil\x1b[31m`) {
		t.Errorf("the warning does not escape a hostile finding:\n%s", out)
	}

	doctorAIStub(t, nil)
	r = checkAI(ctx)
	if r.status != statusOK {
		t.Fatalf("a profile that can run = %+v", r)
	}
	if out := doctorAILines(t, r); !strings.Contains(out, `default profile "evil\u202e`) || !strings.Contains(out, "can run") {
		t.Errorf("the summary does not name the profile escaped:\n%s", out)
	}
}

func TestDoctorWarnsThatCodexSendsTheGlobalAgentsFile(t *testing.T) {
	isolate(t)
	doctorAIStub(t, nil)
	home, codexHomeDir := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", "")
	const private = "private-instruction-4821"
	write := func(dir, name, content string) {
		t.Helper()
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	says := func(r checkResult, file string) {
		t.Helper()
		out := doctorAILines(t, r)
		for _, want := range []string{"ai: codex sends your global instructions file with every tt ai call",
			"codex adds " + file + " to every request", `profile(s) that run codex: "deep\u202e"`} {
			if !strings.Contains(strings.Join(strings.Fields(out), " "), want) {
				t.Errorf("the warning does not say %q:\n%s", want, out)
			}
		}
		if strings.Contains(out, private) || strings.Contains(out, home) || strings.Contains(out, codexHomeDir) {
			t.Errorf("the warning repeats what the file says, or where it is:\n%s", out)
		}
	}
	codexProfile := "[ai]\ndefault = \"fast\"\n\n[ai.profiles.fast]\nengine = \"claude\"\n\n[ai.profiles.\"deep\\u202e\"]\nengine = \"codex\"\n"
	dotCodex := filepath.Join(home, ".codex")

	write(dotCodex, "AGENTS.md", private)
	writeConfig(t, "[ai]\ncontext = \"today\"\n")
	if r, ok := checkCodexInstructions(); ok {
		t.Errorf("no profile runs codex, yet = %+v", r)
	}

	writeConfig(t, codexProfile)
	r, ok := checkCodexInstructions()
	if !ok || r.status != statusWarn {
		t.Fatalf("a codex profile with ~/.codex/AGENTS.md = %+v, %v; want a warning", r, ok)
	}
	says(r, "~/.codex/AGENTS.md")

	// codex reads the override first; a file with nothing but whitespace in it
	// is not one it sends.
	for _, blank := range []string{"", "\n   \n", " \n"} {
		write(dotCodex, "AGENTS.override.md", blank)
		if r, ok := checkCodexInstructions(); !ok {
			t.Errorf("an override of %q in front of AGENTS.md = no warning", blank)
		} else {
			says(r, "~/.codex/AGENTS.md")
		}
	}
	write(dotCodex, "AGENTS.override.md", private)
	if r, ok := checkCodexInstructions(); !ok {
		t.Errorf("an override = no warning")
	} else {
		says(r, "~/.codex/AGENTS.override.md")
	}

	t.Setenv("CODEX_HOME", codexHomeDir)
	if r, ok := checkCodexInstructions(); ok {
		t.Errorf("CODEX_HOME holds no instructions file, yet = %+v", r)
	}
	write(codexHomeDir, "AGENTS.md", "")
	write(codexHomeDir, "AGENTS.override.md", " \n")
	if r, ok := checkCodexInstructions(); ok {
		t.Errorf("CODEX_HOME holds only an empty AGENTS.md and a blank override, yet = %+v", r)
	}
	if err := os.Remove(filepath.Join(codexHomeDir, "AGENTS.override.md")); err != nil {
		t.Fatal(err)
	}
	write(codexHomeDir, "AGENTS.md", private)
	var report strings.Builder
	runDoctorChecks(context.Background(), &report)
	assertDoctorFits(t, "tt doctor", report.String())
	if flat := strings.Join(strings.Fields(report.String()), " "); !strings.Contains(flat, "warn ai: codex sends your global instructions file") ||
		!strings.Contains(flat, "codex adds $CODEX_HOME/AGENTS.md") || strings.Contains(report.String(), private) {
		t.Errorf("tt doctor does not warn about $CODEX_HOME/AGENTS.md, or repeats it:\n%s", report.String())
	}
}

func TestDoctorAICheckSkipsAnUnconfiguredAI(t *testing.T) {
	isolate(t)
	ctx := context.Background()
	doctorAIStub(t, []ai.Finding{{Profile: "fast", Message: `"claude" not found on PATH`}})
	if r := checkAI(ctx); r.status != statusSkipped {
		t.Errorf("an unconfigured AI with no claude = %+v, want it skipped", r)
	}
	writeConfig(t, "[ai]\ncontext = \"today\"\n")
	if r := checkAI(ctx); r.status != statusWarn {
		t.Errorf("a configured AI with no binary = %+v, want a warning", r)
	}
	doctorAIStub(t, nil)
	if r := checkAI(ctx); r.status != statusOK || !strings.Contains(r.summary, "ai.context today") {
		t.Errorf("a ready AI = %+v", r)
	}
}
