package ai

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/movsar/tt/internal/config"
	"github.com/movsar/tt/internal/model"
)

// claudeSuccessFixture is a hand-sanitized, minimal copy of the shape of a
// real `claude -p --output-format json --json-schema ...` reply (session
// id, cost and token accounting fields are dropped; the ones kept are
// replaced with placeholder values). It anchors parseClaudeOutput to the
// real envelope rather than a guessed one.
const claudeSuccessFixture = `{"session_id":"00000000-0000-0000-0000-000000000000","total_cost_usd":0,"is_error":false,"result":"{\"ok\":true}","structured_output":{"ok":true},"type":"result"}`

const claudeErrorFixture = `{"session_id":"00000000-0000-0000-0000-000000000000","is_error":true,"result":"There's an issue with the selected model (bogus). It may not exist or you may not have access to it.","type":"result"}`

func baseAIConfig() config.AI {
	return config.AI{
		Default: "fast",
		Timeout: model.Duration(time.Second),
		Context: config.AIContextMinimal,
		Profiles: map[string]config.AIProfile{
			"fast": {Engine: config.AIEngineClaude, Model: "sonnet", Effort: "high"},
		},
		Tasks: map[string]config.AITask{},
	}
}

type fakeCall struct {
	name string
	args []string
	dir  string
	env  []string
}

// fakeExec returns an execFunc that plays back one canned result per call,
// in order, and records every invocation it received.
func fakeExec(t *testing.T, results ...func(call fakeCall) (stdout, stderr []byte, err error)) (execFunc, *[]fakeCall) {
	t.Helper()
	calls := &[]fakeCall{}
	i := 0
	f := func(_ context.Context, name string, args []string, stdin []byte, dir string, env []string) ([]byte, []byte, error) {
		call := fakeCall{name: name, args: append([]string(nil), args...), dir: dir, env: append([]string(nil), env...)}
		*calls = append(*calls, call)
		if i >= len(results) {
			t.Fatalf("unexpected extra call #%d: %s %v", i+1, name, args)
		}
		r := results[i]
		i++
		return r(call)
	}
	return f, calls
}

func ok(stdout string) func(fakeCall) ([]byte, []byte, error) {
	return func(fakeCall) ([]byte, []byte, error) { return []byte(stdout), nil, nil }
}

func failWith(stderr string) func(fakeCall) ([]byte, []byte, error) {
	return func(fakeCall) ([]byte, []byte, error) { return nil, []byte(stderr), errors.New("exit status 1") }
}

func TestRunBuildsClaudeArgv(t *testing.T) {
	cfg := baseAIConfig()
	exec, calls := fakeExec(t, ok(claudeSuccessFixture))
	r := &Runner{Config: cfg, Exec: exec, Now: time.Now}

	resp, err := r.Run(context.Background(), Request{
		System: "You are a scheduler.",
		Prompt: "add milk",
		Schema: []byte(`{"type":"object"}`),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if string(resp.JSON) != `{"ok":true}` {
		t.Errorf("JSON = %s", resp.JSON)
	}
	if resp.Profile != "fast" || resp.Engine != "claude" {
		t.Errorf("Profile/Engine = %q/%q", resp.Profile, resp.Engine)
	}

	call := (*calls)[0]
	if call.name != "claude" {
		t.Fatalf("argv[0] = %q, want claude", call.name)
	}
	want := []string{
		"-p", "--system-prompt", "You are a scheduler.", "--model", "sonnet", "--effort", "high",
		"--json-schema", `{"type":"object"}`,
		"--output-format", "json", "--tools", "", "--strict-mcp-config",
		"--no-session-persistence", "--setting-sources", "",
	}
	if !reflect.DeepEqual(call.args, want) {
		t.Errorf("argv = %v\nwant %v", call.args, want)
	}
}

func TestRunBuildsCodexArgvAndReadsOutputFile(t *testing.T) {
	cfg := baseAIConfig()
	cfg.Default = "deep"
	cfg.Profiles["deep"] = config.AIProfile{Engine: config.AIEngineCodex, Model: "gpt-5.6-sol", Effort: "low"}

	var gotDir string
	exec, calls := fakeExec(t, func(call fakeCall) ([]byte, []byte, error) {
		gotDir = call.dir
		if err := os.WriteFile(filepath.Join(call.dir, "output.json"), []byte(`{"ok":true}`), 0o600); err != nil {
			t.Fatal(err)
		}
		return nil, nil, nil
	})
	r := &Runner{Config: cfg, Exec: exec, Now: time.Now}

	resp, err := r.Run(context.Background(), Request{Prompt: "find x", Schema: []byte(`{"type":"object"}`)})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if string(resp.JSON) != `{"ok":true}` || resp.Engine != "codex" {
		t.Errorf("resp = %+v", resp)
	}

	call := (*calls)[0]
	if call.name != "codex" {
		t.Fatalf("argv[0] = %q, want codex", call.name)
	}
	joined := strings.Join(call.args, " ")
	for _, want := range []string{
		"exec", "-m gpt-5.6-sol", "-c model_reasoning_effort=low",
		"--ephemeral", "-s read-only", "--skip-git-repo-check", "--ignore-user-config",
		"--disable shell_tool", "--disable view_image", "--disable apps", `-c web_search="disabled"`,
		"--output-schema " + filepath.Join(gotDir, "schema.json"),
		"-o " + filepath.Join(gotDir, "output.json"),
		"-C " + gotDir,
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("argv %v missing %q", call.args, want)
		}
	}
	if call.args[len(call.args)-1] != "-" {
		t.Errorf("argv must end with '-' to read the prompt from stdin, got %v", call.args)
	}

	if _, err := os.Stat(gotDir); !os.IsNotExist(err) {
		t.Errorf("temp dir %s was not removed after the call", gotDir)
	}
}

func TestRunCommandEngineExpandsPlaceholders(t *testing.T) {
	cfg := baseAIConfig()
	cfg.Default = "local"
	cfg.Profiles["local"] = config.AIProfile{
		Engine:  config.AIEngineCommand,
		Model:   "llava",
		Command: []string{"ollama", "run", "{model}", "--effort", "{effort}", "{image}", "{schema}"},
	}

	images := fakeImages(t, "a.png", "b.png")
	var gotStdin []byte
	exec, calls := fakeExec(t, func(call fakeCall) ([]byte, []byte, error) {
		return []byte(`{"ok":true}`), nil, nil
	})
	r := &Runner{Config: cfg, Exec: exec, Now: time.Now}
	origExec := r.Exec
	r.Exec = func(ctx context.Context, name string, args []string, stdin []byte, dir string, env []string) ([]byte, []byte, error) {
		gotStdin = stdin
		return origExec(ctx, name, args, stdin, dir, env)
	}

	resp, err := r.Run(context.Background(), Request{
		Prompt: "hello",
		Schema: []byte(`{"type":"object"}`),
		Images: images,
		Effort: "max",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if string(resp.JSON) != `{"ok":true}` {
		t.Errorf("JSON = %s", resp.JSON)
	}

	if (*calls)[0].name != "ollama" {
		t.Fatalf("argv[0] = %q, want ollama", (*calls)[0].name)
	}
	dir := (*calls)[0].dir
	want := []string{"run", "llava", "--effort", "max", filepath.Join(dir, "img-1.png"), filepath.Join(dir, "img-2.png"), `{"type":"object"}`}
	if !reflect.DeepEqual((*calls)[0].args, want) {
		t.Errorf("argv = %v\nwant %v", (*calls)[0].args, want)
	}
	if string(gotStdin) != "hello" {
		t.Errorf("stdin = %q, want the prompt (no {prompt} placeholder was used)", gotStdin)
	}
}

func TestRunCommandEngineDropsImagePlaceholderWhenThereAreNone(t *testing.T) {
	cfg := baseAIConfig()
	cfg.Default = "local"
	cfg.Profiles["local"] = config.AIProfile{
		Engine:  config.AIEngineCommand,
		Command: []string{"tool", "{image}", "--go"},
	}
	exec, calls := fakeExec(t, ok(`{"ok":true}`))
	r := &Runner{Config: cfg, Exec: exec, Now: time.Now}

	if _, err := r.Run(context.Background(), Request{Prompt: "x"}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if (*calls)[0].name != "tool" {
		t.Fatalf("argv[0] = %q, want tool", (*calls)[0].name)
	}
	want := []string{"--go"}
	if !reflect.DeepEqual((*calls)[0].args, want) {
		t.Errorf("argv = %v, want the {image} slot dropped: %v", (*calls)[0].args, want)
	}
}

func TestRunCommandEnginePromptPlaceholderReplacesStdin(t *testing.T) {
	cfg := baseAIConfig()
	cfg.Default = "local"
	cfg.Profiles["local"] = config.AIProfile{
		Engine:  config.AIEngineCommand,
		Command: []string{"tool", "{prompt}"},
	}
	var gotStdin []byte
	exec, calls := fakeExec(t, func(call fakeCall) ([]byte, []byte, error) { return []byte(`{"ok":true}`), nil, nil })
	r := &Runner{Config: cfg, Exec: exec, Now: time.Now}
	origExec := r.Exec
	r.Exec = func(ctx context.Context, name string, args []string, stdin []byte, dir string, env []string) ([]byte, []byte, error) {
		gotStdin = stdin
		return origExec(ctx, name, args, stdin, dir, env)
	}

	if _, err := r.Run(context.Background(), Request{Prompt: "hello there"}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if (*calls)[0].name != "tool" {
		t.Fatalf("argv[0] = %q, want tool", (*calls)[0].name)
	}
	if want := []string{"hello there"}; !reflect.DeepEqual((*calls)[0].args, want) {
		t.Errorf("argv = %v, want %v", (*calls)[0].args, want)
	}
	if gotStdin != nil {
		t.Errorf("stdin = %q, want nothing sent on stdin once {prompt} is used", gotStdin)
	}
}

func TestRunSendsSystemTaskPromptAndPromptOnStdin(t *testing.T) {
	dir := t.TempDir()
	promptPath := filepath.Join(dir, "ai.md")
	if err := os.WriteFile(promptPath, []byte("Follow the house style."), 0o600); err != nil {
		t.Fatal(err)
	}

	cases := map[config.AIEngine]struct {
		stdin, system string
	}{
		// claude takes the system text as its system prompt instead.
		config.AIEngineClaude: {"Follow the house style.\n\nadd milk", "System text."},
		config.AIEngineCodex:  {"System text.\n\nFollow the house style.\n\nadd milk", ""},
	}
	for engine, c := range cases {
		t.Run(engine.String(), func(t *testing.T) {
			cfg := baseAIConfig()
			cfg.Profiles["fast"] = config.AIProfile{Engine: engine}
			cfg.Tasks[config.AITaskAI] = config.AITask{Profile: "fast", Prompt: promptPath}

			var gotStdin []byte
			var gotArgs []string
			r := &Runner{Config: cfg, Now: time.Now, Exec: func(_ context.Context, _ string, args []string, stdin []byte, d string, _ []string) ([]byte, []byte, error) {
				gotStdin, gotArgs = stdin, args
				if err := os.WriteFile(filepath.Join(d, "output.json"), []byte(`{"ok":true}`), 0o600); err != nil {
					t.Fatal(err)
				}
				return []byte(claudeSuccessFixture), nil, nil
			}}

			if _, err := r.Run(context.Background(), Request{Task: config.AITaskAI, System: "System text.", Prompt: "add milk"}); err != nil {
				t.Fatalf("Run: %v", err)
			}
			if string(gotStdin) != c.stdin {
				t.Errorf("stdin = %q\nwant   %q", gotStdin, c.stdin)
			}
			if c.system != "" && !hasPair(gotArgs, "--system-prompt", c.system) {
				t.Errorf("argv %v lacks --system-prompt %q", gotArgs, c.system)
			}
			if c.system == "" && slices.Contains(gotArgs, "--system-prompt") {
				t.Errorf("argv %v passes a system prompt to an engine that takes it on stdin", gotArgs)
			}
		})
	}
}

func TestRunFallsBackOnFailure(t *testing.T) {
	cfg := baseAIConfig()
	cfg.Fallback = []string{"local"}
	cfg.Profiles["local"] = config.AIProfile{Engine: config.AIEngineCommand, Command: []string{"backup"}}

	exec, calls := fakeExec(t, failWith("network error"), ok(`{"ok":true}`))
	r := &Runner{Config: cfg, Exec: exec, Now: time.Now}

	resp, err := r.Run(context.Background(), Request{Prompt: "x"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if resp.Profile != "local" || resp.Engine != "command" {
		t.Errorf("resp = %+v, want the fallback profile to have answered", resp)
	}
	if len(*calls) != 2 {
		t.Fatalf("got %d calls, want 2 (primary then fallback)", len(*calls))
	}
}

func TestRunFallbackDeduplicatesThePrimary(t *testing.T) {
	cfg := baseAIConfig()
	cfg.Fallback = []string{"fast", "fast"}
	exec, calls := fakeExec(t, ok(claudeSuccessFixture))
	r := &Runner{Config: cfg, Exec: exec, Now: time.Now}

	if _, err := r.Run(context.Background(), Request{Prompt: "x"}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(*calls) != 1 {
		t.Errorf("got %d calls, want exactly 1 (the fallback duplicates the primary)", len(*calls))
	}
}

func TestRunAggregatesEveryFailedAttempt(t *testing.T) {
	cfg := baseAIConfig()
	cfg.Fallback = []string{"local"}
	cfg.Profiles["local"] = config.AIProfile{Engine: config.AIEngineCommand, Command: []string{"backup"}}

	exec, _ := fakeExec(t, failWith("first failure"), failWith("second failure"))
	r := &Runner{Config: cfg, Exec: exec, Now: time.Now}

	_, err := r.Run(context.Background(), Request{Prompt: "x"})
	if err == nil {
		t.Fatal("want an error when every profile fails")
	}
	if !strings.Contains(err.Error(), "fast") || !strings.Contains(err.Error(), "local") {
		t.Errorf("error does not name both attempts: %v", err)
	}
	if !strings.Contains(err.Error(), "first failure") || !strings.Contains(err.Error(), "second failure") {
		t.Errorf("error does not carry both stderr tails: %v", err)
	}
}

func TestRunErrorNeverEchoesThePrompt(t *testing.T) {
	cfg := baseAIConfig()
	exec, _ := fakeExec(t, failWith("boom"))
	r := &Runner{Config: cfg, Exec: exec, Now: time.Now}

	secret := "a very secret task description"
	_, err := r.Run(context.Background(), Request{Prompt: secret})
	if err == nil {
		t.Fatal("want an error")
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("error echoed the prompt: %v", err)
	}
}

// TestRunErrorNeverEchoesACodexStyleEcho mimics the layout codex exec writes
// to stderr on failure: a banner, then a bare "user" line, then the prompt
// verbatim, then a blank line, then the real diagnostic. The values here are
// synthetic (no real paths, ids, or logs); only the shape is copied.
func TestRunErrorNeverEchoesACodexStyleEcho(t *testing.T) {
	cfg := baseAIConfig()
	secret := "MARKER-FAKE-0001 do the fake thing now"
	stderr := "Fake Codex v0.0.0\n" +
		"--------\n" +
		"workdir: /fake/dir\n" +
		"model: fake-model\n" +
		"--------\n" +
		"user\n" +
		secret + "\n" +
		"\n" +
		"warning: fake warning line\n" +
		"ERROR: fake failure\n"
	exec, _ := fakeExec(t, failWith(stderr))
	r := &Runner{Config: cfg, Exec: exec, Now: time.Now}

	_, err := r.Run(context.Background(), Request{Prompt: secret})
	if err == nil {
		t.Fatal("want an error")
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("error echoed the codex-style prompt block: %v", err)
	}
	if !strings.Contains(err.Error(), "ERROR: fake failure") {
		t.Errorf("error dropped real diagnostic content: %v", err)
	}
}

// TestRedactPromptEcho exercises redactPromptEcho directly against the
// stderr shapes it must handle: a multi-line prompt inside a recognizable
// engine block, a bare prompt line among unrelated log lines, stderr that
// does not echo the prompt at all, and a prompt fragment embedded inside a
// longer, otherwise unrelated line. All text is synthetic.
func TestRedactPromptEcho(t *testing.T) {
	t.Run("multi-line prompt echoed in a user block", func(t *testing.T) {
		prompt := "fake line one of the prompt\nfake line two of the prompt\nfake line three of the prompt"
		stderr := "Fake Engine v1.0\n" +
			"--------\n" +
			"model: fake-model\n" +
			"--------\n" +
			"user\n" +
			prompt + "\n" +
			"\n" +
			"ERROR: fake failure after prompt\n"

		got := string(redactPromptEcho([]byte(stderr), prompt, prompt))

		if strings.Contains(got, "fake line one") || strings.Contains(got, "fake line two") || strings.Contains(got, "fake line three") {
			t.Errorf("prompt leaked into redacted stderr: %s", got)
		}
		if !strings.Contains(got, promptEchoOmitted) {
			t.Errorf("want the omitted marker, got: %s", got)
		}
		if !strings.Contains(got, "ERROR: fake failure after prompt") {
			t.Errorf("real diagnostic dropped: %s", got)
		}
	})

	t.Run("bare prompt line surrounded by log lines", func(t *testing.T) {
		prompt := "fake standalone prompt line for the test"
		stderr := "[fake] starting run\n" +
			prompt + "\n" +
			"[fake] some other log line\n"

		got := string(redactPromptEcho([]byte(stderr), prompt, prompt))

		if strings.Contains(got, prompt) {
			t.Errorf("prompt leaked into redacted stderr: %s", got)
		}
		if !strings.Contains(got, "[fake] starting run") || !strings.Contains(got, "[fake] some other log line") {
			t.Errorf("surrounding log lines dropped: %s", got)
		}
	})

	t.Run("claude-style error without an echo is unchanged", func(t *testing.T) {
		prompt := "fake prompt that never appears in the error"
		stderr := claudeErrorFixture

		got := string(redactPromptEcho([]byte(stderr), prompt, prompt))

		if got != stderr {
			t.Errorf("unrelated stderr was altered:\ngot:  %s\nwant: %s", got, stderr)
		}
	})

	t.Run("prompt fragment inside a longer stderr line", func(t *testing.T) {
		prompt := "the quick brown fox jumps over the lazy dog in the fake prompt"
		stderr := "[fake] trace: boundary near 'the quick brown fox' truncated\n" +
			"[fake] unrelated line kept\n"

		got := string(redactPromptEcho([]byte(stderr), prompt, prompt))

		if strings.Contains(got, "the quick brown fox") {
			t.Errorf("prompt fragment leaked into redacted stderr: %s", got)
		}
		if !strings.Contains(got, "[fake] unrelated line kept") {
			t.Errorf("unrelated line dropped: %s", got)
		}
	})
}

func TestRunTimesOut(t *testing.T) {
	cfg := baseAIConfig()
	cfg.Timeout = model.Duration(10 * time.Millisecond)
	exec := func(ctx context.Context, name string, args []string, stdin []byte, dir string, env []string) ([]byte, []byte, error) {
		<-ctx.Done()
		return nil, nil, ctx.Err()
	}
	r := &Runner{Config: cfg, Exec: exec, Now: time.Now}

	_, err := r.Run(context.Background(), Request{Prompt: "x"})
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Errorf("got %v, want a timeout error", err)
	}
}

func TestRunRejectsInvalidJSON(t *testing.T) {
	cfg := baseAIConfig()
	exec, _ := fakeExec(t, ok("not json at all"))
	r := &Runner{Config: cfg, Exec: exec, Now: time.Now}

	_, err := r.Run(context.Background(), Request{Prompt: "x"})
	if err == nil || !strings.Contains(err.Error(), "invalid response") {
		t.Errorf("got %v, want an invalid-response error", err)
	}
}

func TestRunSurfacesClaudeIsError(t *testing.T) {
	cfg := baseAIConfig()
	exec, _ := fakeExec(t, ok(claudeErrorFixture))
	r := &Runner{Config: cfg, Exec: exec, Now: time.Now}

	_, err := r.Run(context.Background(), Request{Prompt: "x"})
	if err == nil || !strings.Contains(err.Error(), "issue with the selected model") {
		t.Errorf("got %v, want the claude error message surfaced", err)
	}
}

func TestRunWithNoProfileConfigured(t *testing.T) {
	r := &Runner{Config: config.AI{}, Exec: func(context.Context, string, []string, []byte, string, []string) ([]byte, []byte, error) {
		t.Fatal("must not exec with no profile resolved")
		return nil, nil, nil
	}}
	if _, err := r.Run(context.Background(), Request{}); err == nil {
		t.Error("want an error when no profile is configured")
	}
}

func TestCheckReportsAMissingBinary(t *testing.T) {
	cfg := baseAIConfig()
	cfg.Fallback = []string{"local"}
	cfg.Profiles["local"] = config.AIProfile{Engine: config.AIEngineCommand, Command: []string{"ghost-tool"}}
	cfg.Tasks[config.AITaskFind] = config.AITask{Profile: "fast"}

	r := &Runner{
		Config: cfg,
		LookPath: func(name string) (string, error) {
			if name == "claude" {
				return "/usr/local/bin/claude", nil
			}
			return "", errors.New("not found")
		},
	}
	findings := r.Check(context.Background())
	if len(findings) != 1 || findings[0].Profile != "local" {
		t.Fatalf("findings = %+v, want exactly one for the missing ghost-tool", findings)
	}
}

func TestCheckMakesNoExecCalls(t *testing.T) {
	cfg := baseAIConfig()
	r := &Runner{
		Config: cfg,
		Exec: func(context.Context, string, []string, []byte, string, []string) ([]byte, []byte, error) {
			t.Fatal("Check must not run any subprocess")
			return nil, nil, nil
		},
		LookPath: func(string) (string, error) { return "/bin/true", nil },
	}
	if findings := r.Check(context.Background()); len(findings) != 0 {
		t.Errorf("findings = %+v, want none", findings)
	}
}

func TestExtractJSONHandlesFencesAndSurroundingText(t *testing.T) {
	cases := map[string]string{
		`{"ok":true}`:                           `{"ok":true}`,
		"```json\n{\"ok\":true}\n```":           `{"ok":true}`,
		"```\n{\"ok\":true}\n```":               `{"ok":true}`,
		"here you go: {\"ok\":true} thanks":     `{"ok":true}`,
		"[1, 2, {\"a\":\"}\"}, 3]":              `[1, 2, {"a":"}"}, 3]`,
		"prefix\n```json\n[1,2,3]\n```\nsuffix": `[1,2,3]`,
	}
	for in, want := range cases {
		got, err := extractJSON([]byte(in))
		if err != nil {
			t.Errorf("extractJSON(%q): %v", in, err)
			continue
		}
		if string(got) != want {
			t.Errorf("extractJSON(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestExtractJSONRejectsGarbage(t *testing.T) {
	for _, in := range []string{"", "not json", "```\nnope\n```"} {
		if _, err := extractJSON([]byte(in)); err == nil {
			t.Errorf("extractJSON(%q) accepted garbage", in)
		}
	}
}

func TestBuildEnvOnlyForwardsTheAllowlist(t *testing.T) {
	t.Setenv("HOME", "/home/tester")
	t.Setenv("PATH", "/usr/bin")
	t.Setenv("TT_TOKEN", "super-secret")
	t.Setenv("XDG_CONFIG_HOME", "/wherever")
	t.Setenv("XDG_DATA_HOME", "/wherever/data")
	t.Setenv("HERDR_ENV", "1")

	env := buildEnv(config.AIEngineClaude, nil)
	joined := strings.Join(env, "\n")
	if strings.Contains(joined, "TT_TOKEN") || strings.Contains(joined, "super-secret") {
		t.Errorf("buildEnv leaked TT_TOKEN: %v", env)
	}
	if strings.Contains(joined, "XDG_CONFIG_HOME") || strings.Contains(joined, "XDG_DATA_HOME") {
		t.Errorf("buildEnv leaked tt's XDG paths: %v", env)
	}
	if strings.Contains(joined, "HERDR_ENV") {
		t.Errorf("buildEnv leaked HERDR_ENV: %v", env)
	}
	if !strings.Contains(joined, "HOME=/home/tester") || !strings.Contains(joined, "PATH=/usr/bin") {
		t.Errorf("buildEnv dropped HOME or PATH: %v", env)
	}
}

func TestBuildEnvForwardsProfileExtras(t *testing.T) {
	t.Setenv("OLLAMA_HOST", "http://remote:11434")
	t.Setenv("UNLISTED_VAR", "should not appear")

	env := buildEnv(config.AIEngineCommand, []string{"OLLAMA_HOST"})
	joined := strings.Join(env, "\n")
	if !strings.Contains(joined, "OLLAMA_HOST=http://remote:11434") {
		t.Errorf("buildEnv dropped the requested extra: %v", env)
	}
	if strings.Contains(joined, "UNLISTED_VAR") {
		t.Errorf("buildEnv forwarded an unlisted variable: %v", env)
	}
}

func TestBuildEnvRefusesForbiddenExtrasEvenIfAsked(t *testing.T) {
	t.Setenv("TT_TOKEN", "super-secret")
	t.Setenv("HERDR_ENV", "1")
	t.Setenv("XDG_CONFIG_HOME", "/wherever")

	env := buildEnv(config.AIEngineCommand, []string{"TT_TOKEN", "HERDR_ENV", "XDG_CONFIG_HOME"})
	joined := strings.Join(env, "\n")
	if strings.Contains(joined, "TT_TOKEN") || strings.Contains(joined, "super-secret") ||
		strings.Contains(joined, "HERDR_ENV") || strings.Contains(joined, "XDG_CONFIG_HOME") {
		t.Errorf("buildEnv forwarded a forbidden name passed as an extra: %v", env)
	}
}

func TestRunForwardsTheProfilesEnvToExec(t *testing.T) {
	t.Setenv("OLLAMA_HOST", "http://remote:11434")
	cfg := baseAIConfig()
	cfg.Default = "local"
	cfg.Profiles["local"] = config.AIProfile{
		Engine:  config.AIEngineCommand,
		Command: []string{"ollama", "run", "llava"},
		Env:     []string{"OLLAMA_HOST"},
	}
	exec, calls := fakeExec(t, ok(`{"ok":true}`))
	r := &Runner{Config: cfg, Exec: exec, Now: time.Now}

	if _, err := r.Run(context.Background(), Request{Prompt: "x"}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	joined := strings.Join((*calls)[0].env, "\n")
	if !strings.Contains(joined, "OLLAMA_HOST=http://remote:11434") {
		t.Errorf("exec did not receive the profile's env passthrough: %v", (*calls)[0].env)
	}
}

// hasPair reports whether args holds flag immediately followed by value.
func hasPair(args []string, flag, value string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag && args[i+1] == value {
			return true
		}
	}
	return false
}

func TestBuildCodexArgvTurnsOffEveryToolThatReadsPastThePrompt(t *testing.T) {
	args := buildCodexArgv("m", "low", "/work/schema.json", "/work/output.json", "/work", nil)
	for _, pair := range [][2]string{
		{"-s", "read-only"},
		{"--disable", "shell_tool"},
		{"--disable", "view_image"},
		{"--disable", "apps"},
		{"--disable", "image_generation"},
		{"--disable", "multi_agent"},
		{"-c", `web_search="disabled"`},
		{"-c", "tools.experimental_request_user_input.enabled=false"},
		{"-c", "skills.include_instructions=false"},
		{"-c", "include_environment_context=false"},
	} {
		if !hasPair(args, pair[0], pair[1]) {
			t.Errorf("argv %v lacks %s %s", args, pair[0], pair[1])
		}
	}
	if !slices.Contains(args, "--strict-config") {
		t.Errorf("argv %v lacks --strict-config: codex would silently ignore a -c switch it no longer knows", args)
	}
	if args[len(args)-1] != "-" {
		t.Errorf("argv must end with '-' to read the prompt from stdin, got %v", args)
	}
}

func TestBuildClaudeArgvLoadsNoToolsAndNoMCPServers(t *testing.T) {
	args := buildClaudeArgv("sonnet", "high", "", nil)
	if !hasPair(args, "--tools", "") {
		t.Errorf("argv %v lacks --tools \"\"", args)
	}
	if !slices.Contains(args, "--strict-mcp-config") {
		t.Errorf("argv %v lacks --strict-mcp-config", args)
	}
	if slices.Contains(args, "--mcp-config") {
		t.Errorf("argv %v names an MCP config, which --strict-mcp-config would then load", args)
	}
	if !hasPair(args, "--system-prompt", "") {
		t.Errorf("argv %v lacks --system-prompt \"\": with no system text claude would send its own default prompt", args)
	}
}

// fakeImages writes small synthetic files standing in for images, in a
// directory whose path holds a comma, and returns their paths.
func fakeImages(t *testing.T, names ...string) []string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "fake,photos")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, name := range names {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("fake image "+name), 0o600); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, path)
	}
	return paths
}

func TestRunStagesCodexImagesUnderNeutralNames(t *testing.T) {
	cfg := baseAIConfig()
	cfg.Default = "deep"
	cfg.Profiles["deep"] = config.AIProfile{Engine: config.AIEngineCodex}
	images := fakeImages(t, "Holiday Photo.PNG", "scan.jpeg", "notes.tar,gz")

	var staged []string
	exec, calls := fakeExec(t, func(call fakeCall) ([]byte, []byte, error) {
		for _, arg := range call.args {
			path, ok := strings.CutPrefix(arg, "--image=")
			if !ok {
				continue
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Errorf("staged image %s cannot be read: %v", path, err)
			}
			staged = append(staged, string(data))
		}
		if err := os.WriteFile(filepath.Join(call.dir, "output.json"), []byte(`{"ok":true}`), 0o600); err != nil {
			t.Fatal(err)
		}
		return nil, nil, nil
	})
	r := &Runner{Config: cfg, Exec: exec, Now: time.Now}

	if _, err := r.Run(context.Background(), Request{Prompt: "x", Images: images}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	call := (*calls)[0]
	want := []string{
		"--image=" + filepath.Join(call.dir, "img-1.PNG"),
		"--image=" + filepath.Join(call.dir, "img-2.jpeg"),
		"--image=" + filepath.Join(call.dir, "img-3"),
		"-",
	}
	if got := call.args[len(call.args)-len(want):]; !reflect.DeepEqual(got, want) {
		t.Errorf("argv ends %v, want %v", got, want)
	}
	if joined := strings.Join(call.args, " "); strings.Contains(joined, "fake,photos") || strings.Contains(joined, "Holiday") {
		t.Errorf("argv %v names the user's own image path", call.args)
	}
	if slices.Contains(call.args, "-i") || slices.Contains(call.args, "--image") {
		t.Errorf("argv = %v: a separate image value would swallow the closing '-'", call.args)
	}
	wantData := []string{"fake image Holiday Photo.PNG", "fake image scan.jpeg", "fake image notes.tar,gz"}
	if !reflect.DeepEqual(staged, wantData) {
		t.Errorf("staged images read back %q, want %q", staged, wantData)
	}
	for _, img := range images {
		if _, err := os.Stat(img); err != nil {
			t.Errorf("the original image %s is gone after the call: %v", img, err)
		}
	}
}

func TestRunRefusesAMissingImage(t *testing.T) {
	cfg := baseAIConfig()
	cfg.Default = "deep"
	cfg.Profiles["deep"] = config.AIProfile{Engine: config.AIEngineCodex}
	r := &Runner{Config: cfg, Now: time.Now, Exec: func(context.Context, string, []string, []byte, string, []string) ([]byte, []byte, error) {
		t.Fatal("codex must not run without the image it was asked to attach")
		return nil, nil, nil
	}}

	missing := filepath.Join(t.TempDir(), "no-such-image.png")
	if _, err := r.Run(context.Background(), Request{Prompt: "x", Images: []string{missing}}); err == nil {
		t.Error("want an error for an image that does not exist")
	}
}

func TestRunCommandEngineStagesImagesOnlyForTheImagePlaceholder(t *testing.T) {
	cfg := baseAIConfig()
	cfg.Default = "local"
	cfg.Profiles["local"] = config.AIProfile{Engine: config.AIEngineCommand, Command: []string{"tool", "--go"}}
	exec, calls := fakeExec(t, ok(`{"ok":true}`))
	r := &Runner{Config: cfg, Exec: exec, Now: time.Now}

	missing := filepath.Join(t.TempDir(), "never-read.png")
	if _, err := r.Run(context.Background(), Request{Prompt: "x", Images: []string{missing}}); err != nil {
		t.Fatalf("Run: %v (a template without {image} must not touch the images)", err)
	}
	if want := []string{"--go"}; !reflect.DeepEqual((*calls)[0].args, want) {
		t.Errorf("argv = %v, want %v", (*calls)[0].args, want)
	}
}

func TestImageExt(t *testing.T) {
	cases := map[string]string{
		"/a/b.png":         ".png",
		"/a/b.JPEG":        ".JPEG",
		"/a/b":             "",
		"/a/b.":            "",
		"/a/b.tar,gz":      "",
		"/a/b.p n":         "",
		"/a/b.webp2":       ".webp2",
		"/a/b.abcdefghij":  ".abcdefghij",
		"/a/b.abcdefghijk": "",
	}
	for path, want := range cases {
		if got := imageExt(path); got != want {
			t.Errorf("imageExt(%q) = %q, want %q", path, got, want)
		}
	}
}

func envValues(env []string, name string) []string {
	var out []string
	for _, kv := range env {
		if strings.HasPrefix(kv, name+"=") {
			out = append(out, kv)
		}
	}
	return out
}

func TestBuildEnvGivesEachEngineOnlyItsOwnKeyAndConfig(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "fake-anthropic-key")
	t.Setenv("CLAUDE_CONFIG_DIR", "/fake/claude")
	t.Setenv("OPENAI_API_KEY", "fake-openai-key")
	t.Setenv("CODEX_HOME", "/fake/codex")
	t.Setenv("OLLAMA_HOST", "http://fake:11434")

	cases := map[config.AIEngine]struct{ want, never []string }{
		config.AIEngineClaude: {
			want:  []string{"ANTHROPIC_API_KEY=fake-anthropic-key", "CLAUDE_CONFIG_DIR=/fake/claude"},
			never: []string{"OPENAI_API_KEY", "CODEX_HOME", "OLLAMA_HOST"},
		},
		config.AIEngineCodex: {
			want:  []string{"OPENAI_API_KEY=fake-openai-key", "CODEX_HOME=/fake/codex"},
			never: []string{"ANTHROPIC_API_KEY", "CLAUDE_CONFIG_DIR", "OLLAMA_HOST"},
		},
		config.AIEngineCommand: {
			never: []string{"ANTHROPIC_API_KEY", "CLAUDE_CONFIG_DIR", "OPENAI_API_KEY", "CODEX_HOME", "OLLAMA_HOST"},
		},
	}
	for engine, c := range cases {
		env := buildEnv(engine, nil)
		for _, want := range c.want {
			if !slices.Contains(env, want) {
				t.Errorf("%s: env %v lacks %s", engine, env, want)
			}
		}
		for _, name := range c.never {
			if got := envValues(env, name); len(got) > 0 {
				t.Errorf("%s: env forwarded %v", engine, got)
			}
		}
	}
}

func TestBuildEnvForwardsAKeyToACommandProfileThatListsIt(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "fake-openai-key")
	env := buildEnv(config.AIEngineCommand, []string{"OPENAI_API_KEY"})
	if !slices.Contains(env, "OPENAI_API_KEY=fake-openai-key") {
		t.Errorf("env %v lacks the key its profile listed", env)
	}
}

func TestBuildEnvTurnsClaudeAutoMemoryOff(t *testing.T) {
	t.Setenv("CLAUDE_CODE_DISABLE_AUTO_MEMORY", "0")

	env := buildEnv(config.AIEngineClaude, []string{"CLAUDE_CODE_DISABLE_AUTO_MEMORY"})
	if got := envValues(env, "CLAUDE_CODE_DISABLE_AUTO_MEMORY"); !slices.Equal(got, []string{"CLAUDE_CODE_DISABLE_AUTO_MEMORY=1"}) {
		t.Errorf("claude env has %v, want exactly CLAUDE_CODE_DISABLE_AUTO_MEMORY=1 whatever tt's env or the profile says", got)
	}
	for _, engine := range []config.AIEngine{config.AIEngineCodex, config.AIEngineCommand} {
		if got := envValues(buildEnv(engine, nil), "CLAUDE_CODE_DISABLE_AUTO_MEMORY"); len(got) > 0 {
			t.Errorf("%s env has %v, want claude's setting left to claude", engine, got)
		}
	}
}

func TestRunGivesClaudeItsFixedEnv(t *testing.T) {
	cfg := baseAIConfig()
	exec, calls := fakeExec(t, ok(claudeSuccessFixture))
	r := &Runner{Config: cfg, Exec: exec, Now: time.Now}

	if _, err := r.Run(context.Background(), Request{Prompt: "x"}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !slices.Contains((*calls)[0].env, "CLAUDE_CODE_DISABLE_AUTO_MEMORY=1") {
		t.Errorf("exec env = %v, want CLAUDE_CODE_DISABLE_AUTO_MEMORY=1", (*calls)[0].env)
	}
}

// fakeSystem is public request text of the kind tt's built-in instructions
// hold. It shares " the current", " request can" and a run of spaces with
// realDiagnostics below, which is what used to get those lines dropped.
const fakeSystem = "You turn a fake request into operations. Use the current time from the context.\n" +
	"When a request cannot be expressed, say why in notes.\n" +
	"  mon tue            the next such weekday\n" +
	"------------------------------------------\n"

// fakePrivate is a synthetic request with context, shaped like the prompt
// tt ai builds: one line of context JSON, a note and the request.
const fakePrivate = "Context (JSON):\n" +
	`{"now":"2026-01-01 10:00","weekday":"thu","time_zone":"Etc/UTC","lists":["Inbox","Errands"],"tags":["home"],"tasks":[{"ref":"t1","title":"Pay the fake bill","list":"Errands"}]}` + "\n" +
	"Existing tasks are listed; ref names one of them.\n" +
	"notes:              none\n" +
	"------------------------------------------\n" +
	"\n" +
	"Request:\n" +
	"buy fake milk tomorrow at 10"

// realDiagnostics are the kind of lines claude and codex print on failure.
// None of them echoes a request, so every one must reach the error intact.
var realDiagnostics = []string{
	`Error: Invalid API key - Please run /login`,
	`API Error: 400 {"type":"error","error":{"type":"invalid_request_error","message":"prompt is too long: 215000 tokens > 200000 maximum"}}`,
	`ERROR: stream disconnected before completion: error sending request for url (https://chatgpt.com/backend-api/codex/responses)`,
	`ERROR: unexpected status 400 Bad Request: {"detail":"The 'gpt-5.6-sol' model is not supported when using Codex with a ChatGPT account."}`,
	`error: unexpected argument '--ignore-user-config' found`,
	`You've hit your usage limit. Upgrade to Pro or try again at 3:05 PM.`,
	`ERROR: Invalid schema for response_format 'codex_output_schema': In context=(), 'required' is required to be supplied and to be an array including every key in properties. Missing 'notes'.`,
	`Error: the model returned output that does not match the schema`,
	`Error: failed to read the current time zone from the system`,
	`warning: the tool call was denied; the request cannot be completed without approval`,
	`Error: Claude Code cannot run with a reply that is not valid JSON; see the logs for details`,
	`thread 'main' panicked at codex-rs/exec/src/lib.rs:412:10: called Option::unwrap() on a None value`,
	`Error: The operation was aborted because it took longer than the configured time limit`,
	`Error: 529 Overloaded - the service is temporarily unavailable, try again later`,
	`name          value                 note`,
	`------------------------------------------`,
}

func TestRunKeepsRealDiagnostics(t *testing.T) {
	for _, line := range realDiagnostics {
		t.Run(line, func(t *testing.T) {
			cfg := baseAIConfig()
			exec, _ := fakeExec(t, failWith("[fake] starting\n"+line+"\n"))
			r := &Runner{Config: cfg, Exec: exec, Now: time.Now}

			_, err := r.Run(context.Background(), Request{System: fakeSystem, Prompt: fakePrivate})
			if err == nil {
				t.Fatal("want an error")
			}
			if !strings.Contains(err.Error(), line) {
				t.Errorf("the diagnostic was redacted:\n%v", err)
			}
		})
	}
}

func TestRunDropsEchoesOfThePrivateRequest(t *testing.T) {
	dir := t.TempDir()
	promptPath := filepath.Join(dir, "ai.md")
	if err := os.WriteFile(promptPath, []byte("House style: fake rule number seven applies.\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cases := map[string]struct {
		line, secret string
	}{
		"a fragment of the request": {
			"[fake] retrying after: buy fake milk tomorrow at 10",
			"buy fake milk",
		},
		"a fragment of the context": {
			`warning: ignoring {"ref":"t1","title":"Pay the fake bill"}`,
			"Pay the fake bill",
		},
		"a whole short request line": {
			"Request:",
			"Request:",
		},
		"a fragment of the task prompt file": {
			"note: fake rule number seven was not followed",
			"fake rule number seven",
		},
		"a fragment in another script": {
			"[fake] ignoring: kupit' молоко завтра",
			"молоко завтра",
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := baseAIConfig()
			cfg.Tasks[config.AITaskAI] = config.AITask{Profile: "fast", Prompt: promptPath}
			exec, _ := fakeExec(t, failWith("[fake] starting\n"+c.line+"\nERROR: fake failure\n"))
			r := &Runner{Config: cfg, Exec: exec, Now: time.Now}

			private := fakePrivate + "\nкупить молоко завтра"
			_, err := r.Run(context.Background(), Request{Task: config.AITaskAI, System: fakeSystem, Prompt: private})
			if err == nil {
				t.Fatal("want an error")
			}
			if strings.Contains(err.Error(), c.secret) {
				t.Errorf("the echo leaked into the error:\n%v", err)
			}
			if !strings.Contains(err.Error(), promptEchoOmitted) || !strings.Contains(err.Error(), "ERROR: fake failure") {
				t.Errorf("want the omitted marker and the real diagnostic:\n%v", err)
			}
		})
	}
}

func TestRedactPromptEchoIgnoresRunsWithoutWords(t *testing.T) {
	private := "fake title one\n" +
		"            \n" +
		"a    ............    b\n" +
		"}\n"
	text := "|            |\n" +
		"............\n" +
		"}\n"

	if got := string(redactPromptEcho([]byte(text), private, private)); got != text {
		t.Errorf("lines without a letter or digit were redacted:\ngot:  %q\nwant: %q", got, text)
	}
}

func TestRedactPromptEchoDropsAnEchoedSystemTextToo(t *testing.T) {
	text := "user\nfake system text echoed back\n\nERROR: fake failure\n"
	got := string(redactPromptEcho([]byte(text), "fake system text echoed back", ""))
	if strings.Contains(got, "fake system text") || !strings.Contains(got, "ERROR: fake failure") {
		t.Errorf("got %q, want the echo block dropped and the diagnostic kept", got)
	}
}

func TestRunRedactsClaudesOwnErrorMessage(t *testing.T) {
	cfg := baseAIConfig()
	secret := "fake private request about the dentist on friday"
	envelope, err := json.Marshal(map[string]any{
		"type":     "result",
		"is_error": true,
		"result":   "Could not finish: " + secret + "\nPlease try again later.",
	})
	if err != nil {
		t.Fatal(err)
	}
	exec, _ := fakeExec(t, ok(string(envelope)))
	r := &Runner{Config: cfg, Exec: exec, Now: time.Now}

	_, err = r.Run(context.Background(), Request{System: fakeSystem, Prompt: secret})
	if err == nil {
		t.Fatal("want an error")
	}
	if strings.Contains(err.Error(), "dentist") {
		t.Errorf("claude's error message leaked the request: %v", err)
	}
	if !strings.Contains(err.Error(), promptEchoOmitted) || !strings.Contains(err.Error(), "Please try again later.") {
		t.Errorf("want the omitted marker and the rest of claude's message: %v", err)
	}
}

func TestCheckReportsInADeterministicOrder(t *testing.T) {
	cfg := baseAIConfig()
	cfg.Tasks[config.AITaskAI] = config.AITask{Profile: "ghost-a"}
	cfg.Tasks[config.AITaskFind] = config.AITask{Profile: "ghost-b"}
	r := &Runner{Config: cfg, LookPath: func(string) (string, error) { return "/bin/true", nil }}

	want := []Finding{
		{Profile: "ghost-a", Message: "profile not configured"},
		{Profile: "ghost-b", Message: "profile not configured"},
	}
	for i := 0; i < 50; i++ {
		if got := r.Check(context.Background()); !reflect.DeepEqual(got, want) {
			t.Fatalf("run %d: findings = %+v, want %+v", i, got, want)
		}
	}
}

// TestRunKeepsCodexErrorsRightAfterTheEchoedPrompt copies the layout codex
// 0.154 really prints: the prompt repeated under a bare "user" line, blank
// lines and all, and the next output on the very next line, with no blank
// line in between. The values are synthetic.
func TestRunKeepsCodexErrorsRightAfterTheEchoedPrompt(t *testing.T) {
	system := "You turn a fake request into operations.\n\nWhen a fake request cannot be expressed, say why."
	cases := map[string]struct {
		req    Request
		sent   string
		leaked []string
	}{
		"a one-line request and no system text": {
			Request{Prompt: "buy fake milk tomorrow at 10"},
			"buy fake milk tomorrow at 10",
			[]string{"buy fake milk"},
		},
		"system text with a blank line in it, then the request": {
			Request{System: system, Prompt: fakePrivate},
			system + "\n\n" + fakePrivate,
			[]string{"You turn a fake request", "When a fake request cannot", "buy fake milk", "Pay the fake bill"},
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := baseAIConfig()
			cfg.Default = "deep"
			cfg.Profiles["deep"] = config.AIProfile{Engine: config.AIEngineCodex}
			stderr := "Fake Codex v0.0.0\n--------\nworkdir: /fake/dir\n--------\nuser\n" + c.sent +
				"\nERROR: fake failure one\nERROR: fake failure two\n"
			exec, _ := fakeExec(t, failWith(stderr))
			r := &Runner{Config: cfg, Exec: exec, Now: time.Now}

			_, err := r.Run(context.Background(), c.req)
			if err == nil {
				t.Fatal("want an error")
			}
			for _, leaked := range c.leaked {
				if strings.Contains(err.Error(), leaked) {
					t.Errorf("the echoed prompt leaked %q into the error:\n%v", leaked, err)
				}
			}
			for _, kept := range []string{"ERROR: fake failure one", "ERROR: fake failure two", "workdir: /fake/dir"} {
				if !strings.Contains(err.Error(), kept) {
					t.Errorf("the error lost %q:\n%v", kept, err)
				}
			}
		})
	}
}

func TestRunDropsTheCodexReplyBlock(t *testing.T) {
	cfg := baseAIConfig()
	cfg.Default = "deep"
	cfg.Profiles["deep"] = config.AIProfile{Engine: config.AIEngineCodex}
	stderr := "--------\nuser\nx\ncodex\n{\"note\":\"a fake paraphrase of what was asked\"}\nanother fake reply line\ntokens used\n1,234\n" +
		"codex\nfake reply cut short\nERROR: fake failure\n"
	exec, _ := fakeExec(t, failWith(stderr))
	r := &Runner{Config: cfg, Exec: exec, Now: time.Now}

	_, err := r.Run(context.Background(), Request{Prompt: "x"})
	if err == nil {
		t.Fatal("want an error")
	}
	for _, leaked := range []string{"paraphrase", "another fake reply line", "cut short"} {
		if strings.Contains(err.Error(), leaked) {
			t.Errorf("the reply block leaked %q into the error:\n%v", leaked, err)
		}
	}
	for _, kept := range []string{"tokens used", "1,234", "ERROR: fake failure"} {
		if !strings.Contains(err.Error(), kept) {
			t.Errorf("the error lost %q:\n%v", kept, err)
		}
	}
}

func TestRunSurfacesClaudesMessageOnANonZeroExit(t *testing.T) {
	cfg := baseAIConfig()
	secret := "fake private request about the dentist on friday"
	envelope, err := json.Marshal(map[string]any{
		"type":     "result",
		"is_error": true,
		"result":   "There's an issue with the selected model (fake-model).\nWhile handling: " + secret,
	})
	if err != nil {
		t.Fatal(err)
	}
	exec, _ := fakeExec(t, func(fakeCall) ([]byte, []byte, error) {
		return envelope, []byte("[fake] model check failed\n"), errors.New("exit status 1")
	})
	r := &Runner{Config: cfg, Exec: exec, Now: time.Now}

	_, err = r.Run(context.Background(), Request{Prompt: secret})
	if err == nil {
		t.Fatal("want an error")
	}
	for _, kept := range []string{"exit status 1", "issue with the selected model (fake-model)", "[fake] model check failed", promptEchoOmitted} {
		if !strings.Contains(err.Error(), kept) {
			t.Errorf("the error lacks %q: %v", kept, err)
		}
	}
	if strings.Contains(err.Error(), "dentist") {
		t.Errorf("claude's message leaked the request: %v", err)
	}
}

func TestRunIgnoresANonEnvelopeStdoutOnANonZeroExit(t *testing.T) {
	cfg := baseAIConfig()
	exec, _ := fakeExec(t, func(fakeCall) ([]byte, []byte, error) {
		return []byte("not an envelope"), []byte("[fake] crashed\n"), errors.New("exit status 2")
	})
	r := &Runner{Config: cfg, Exec: exec, Now: time.Now}

	_, err := r.Run(context.Background(), Request{Prompt: "x"})
	if err == nil || !strings.Contains(err.Error(), "exit status 2 ([fake] crashed)") {
		t.Errorf("got %v, want the exit status with the stderr tail", err)
	}
}

func TestBuildEnvKeepsTheUsersClaudeSetupOut(t *testing.T) {
	t.Setenv("CLAUDE_CODE_DISABLE_CLAUDE_MDS", "0")
	t.Setenv("ENABLE_CLAUDEAI_MCP_SERVERS", "true")
	t.Setenv("CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC", "0")

	env := buildEnv(config.AIEngineClaude, []string{"CLAUDE_CODE_DISABLE_CLAUDE_MDS", "ENABLE_CLAUDEAI_MCP_SERVERS", "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC"})
	for name, want := range map[string]string{
		"CLAUDE_CODE_DISABLE_CLAUDE_MDS":           "CLAUDE_CODE_DISABLE_CLAUDE_MDS=1",
		"ENABLE_CLAUDEAI_MCP_SERVERS":              "ENABLE_CLAUDEAI_MCP_SERVERS=false",
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1",
	} {
		if got := envValues(env, name); !slices.Equal(got, []string{want}) {
			t.Errorf("claude env has %v, want exactly %s", got, want)
		}
	}
}
