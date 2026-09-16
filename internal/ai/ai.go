// Package ai calls a configured agent CLI (claude, codex, or a literal
// command) headlessly, as a subprocess, and returns its structured JSON
// reply. tt never talks to a model API directly; it shells out to a CLI the
// user already has installed and authenticated.
package ai

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/movsar/tt/internal/config"
)

// Request is one call to an agent.
type Request struct {
	// Task names an entry in config.AI.Tasks. It selects that task's
	// profile and prompt file unless Profile overrides the profile.
	Task string

	// System is the built-in instructions for this call (schema shape,
	// output contract). claude gets it as its system prompt, in place of its
	// own default one; codex and engine = "command" get it ahead of any task
	// prompt file and the caller's Prompt. It is treated as public text, tt's
	// own: it goes into claude's argv, and an error is redacted against the
	// task prompt file and Prompt only, so System must never carry the
	// user's words or tasks.
	System string

	// Prompt is the caller-supplied text, appended last. It holds whatever
	// private data the call sends: the user's request and any task context.
	Prompt string

	// Schema is a JSON Schema the reply must match.
	Schema []byte

	// Images are local file paths attached to the request, engine
	// permitting. A relative path is taken from tt's own working directory.
	// The agent gets each one under a neutral name in its temporary
	// directory, never the path itself (see stageImages).
	Images []string

	// Profile overrides the profile resolved from Task or config.AI.Default.
	Profile string

	// Model and Effort override the resolved profile's own values.
	Model  string
	Effort string
}

// Response is a successful call's result.
type Response struct {
	// JSON is the reply, extracted and validated as a JSON value.
	JSON []byte

	// Profile and Engine name which profile actually answered, which may
	// be a fallback rather than the one first resolved.
	Profile string
	Engine  string

	Duration time.Duration
}

// Finding is one problem Check found with a configured profile.
type Finding struct {
	Profile string
	Message string
}

// execFunc runs one subprocess to completion and collects its stdout and
// stderr. Real use passes stdin on the child's standard input, dir as its
// working directory, and env (built by buildEnv from the fixed allowlist
// plus the profile's own passthrough list) in place of tt's own
// environment.
type execFunc func(ctx context.Context, name string, args []string, stdin []byte, dir string, env []string) (stdout, stderr []byte, err error)

// Runner resolves a Request against configured profiles and runs it,
// falling back through config.AI.Fallback on failure.
type Runner struct {
	Config config.AI

	// Exec runs the resolved subprocess. Tests replace it with a fake.
	Exec execFunc

	// LookPath resolves a binary name to a path, or reports it is missing.
	LookPath func(string) (string, error)

	// Now provides the current time, for measuring Duration in tests.
	Now func() time.Time
}

// New returns a Runner that shells out for real.
func New(cfg config.AI) *Runner {
	return &Runner{
		Config:   cfg,
		Exec:     runExec,
		LookPath: exec.LookPath,
		Now:      time.Now,
	}
}

const defaultTimeout = 90 * time.Second

const stderrTailLimit = 2000

// Run resolves a profile for req, runs it, and on failure tries each
// configured fallback profile in order. The returned error aggregates every
// attempt.
func (r *Runner) Run(ctx context.Context, req Request) (Response, error) {
	order, err := r.resolveOrder(req)
	if err != nil {
		return Response{}, err
	}

	combined, private, err := r.prompts(req)
	if err != nil {
		return Response{}, err
	}

	var attempts []string
	for _, name := range order {
		profile, ok := r.Config.Profiles[name]
		if !ok {
			attempts = append(attempts, fmt.Sprintf("%s: profile not configured", name))
			continue
		}
		resp, err := r.attempt(ctx, name, profile, req, combined, private)
		if err == nil {
			return resp, nil
		}
		attempts = append(attempts, fmt.Sprintf("%s: %v", name, err))
	}
	return Response{}, fmt.Errorf("ai: every profile failed:\n%s", strings.Join(attempts, "\n"))
}

func (r *Runner) resolveOrder(req Request) ([]string, error) {
	primary := req.Profile
	if primary == "" {
		if task, ok := r.Config.Tasks[req.Task]; ok {
			primary = task.Profile
		}
	}
	if primary == "" {
		primary = r.Config.Default
	}
	if primary == "" {
		return nil, errors.New("ai: no profile configured")
	}

	seen := map[string]bool{primary: true}
	order := []string{primary}
	for _, name := range r.Config.Fallback {
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		order = append(order, name)
	}
	return order, nil
}

// prompts joins the system text, the task's prompt file if any, and the
// caller's prompt into combined, the one blob codex and engine = "command"
// are sent. private is the same minus the system text: the prompt file and
// the caller's prompt, which carry the user's own words and tasks. private
// is what an error is redacted against, and all claude gets on stdin, with
// the system text as its system prompt instead. The system text is tt's own
// public instructions; matching against it too would cut real diagnostics
// that merely share a phrase with it.
func (r *Runner) prompts(req Request) (combined, private string, err error) {
	var parts, privateParts []string
	if strings.TrimSpace(req.System) != "" {
		parts = append(parts, req.System)
	}
	if task, ok := r.Config.Tasks[req.Task]; ok && task.Prompt != "" {
		data, err := os.ReadFile(task.Prompt)
		if err != nil {
			return "", "", fmt.Errorf("ai: read task prompt: %w", err)
		}
		parts = append(parts, string(data))
		privateParts = append(privateParts, string(data))
	}
	if strings.TrimSpace(req.Prompt) != "" {
		parts = append(parts, req.Prompt)
		privateParts = append(privateParts, req.Prompt)
	}
	return strings.Join(parts, "\n\n"), strings.Join(privateParts, "\n\n"), nil
}

func (r *Runner) attempt(ctx context.Context, name string, profile config.AIProfile, req Request, combined, private string) (Response, error) {
	model := firstNonEmpty(req.Model, profile.Model)
	effort := firstNonEmpty(req.Effort, profile.Effort)

	dir, err := os.MkdirTemp("", "tt-ai-")
	if err != nil {
		return Response{}, fmt.Errorf("ai: create work dir: %w", err)
	}
	defer os.RemoveAll(dir)

	timeout := r.Config.Timeout.Duration()
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	argv, stdin, outPath, err := r.build(profile, model, effort, req, dir, combined, private)
	if err != nil {
		return Response{}, err
	}
	if len(argv) == 0 {
		return Response{}, errors.New("ai: empty command")
	}
	// sent is the prompt text the engine was given, which it may echo back:
	// claude takes System through --system-prompt, so only private.
	sent := combined
	if profile.Engine == config.AIEngineClaude {
		sent = private
	}

	env := buildEnv(profile.Engine, profile.Env)

	now := r.now()
	stdout, stderr, err := r.Exec(runCtx, argv[0], argv[1:], stdin, dir, env)
	duration := r.now().Sub(now)
	if err != nil {
		if runCtx.Err() == context.DeadlineExceeded {
			return Response{}, fmt.Errorf("timed out after %s", timeout)
		}
		// claude exits non-zero on its own errors too, with the reason in
		// the envelope on stdout rather than on stderr.
		if profile.Engine == config.AIEngineClaude {
			if msg, ok := claudeErrorMessage(stdout, sent, private); ok {
				return Response{}, fmt.Errorf("%w: %s (%s)", err, msg, tail(stderr, sent, private))
			}
		}
		return Response{}, fmt.Errorf("%w (%s)", err, tail(stderr, sent, private))
	}

	payload := stdout
	if outPath != "" {
		payload, err = os.ReadFile(outPath)
		if err != nil {
			return Response{}, fmt.Errorf("ai: read output file: %w", err)
		}
	}

	result, err := parseOutput(profile.Engine, payload, sent, private)
	if err != nil {
		return Response{}, fmt.Errorf("invalid response: %w (%s)", err, tail(stderr, sent, private))
	}

	return Response{JSON: result, Profile: name, Engine: profile.Engine.String(), Duration: duration}, nil
}

func (r *Runner) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Runner) build(profile config.AIProfile, model, effort string, req Request, dir, combined, private string) (argv []string, stdin []byte, outPath string, err error) {
	switch profile.Engine {
	case config.AIEngineClaude:
		return buildClaudeArgv(model, effort, req.System, req.Schema), []byte(private), "", nil

	case config.AIEngineCodex:
		images, err := stageImages(dir, req.Images)
		if err != nil {
			return nil, nil, "", err
		}
		schemaPath := ""
		if len(req.Schema) > 0 {
			schemaPath, err = writeSchemaFile(dir, req.Schema)
			if err != nil {
				return nil, nil, "", err
			}
		}
		outPath = filepath.Join(dir, "output.json")
		return buildCodexArgv(model, effort, schemaPath, outPath, dir, images), []byte(combined), outPath, nil

	case config.AIEngineCommand:
		return r.buildCommandArgv(profile, model, effort, req, dir, combined)

	default:
		return nil, nil, "", fmt.Errorf("ai: unsupported engine %q", profile.Engine)
	}
}

// Check reports, for every profile actually referenced by the config
// (default, fallbacks, and task profiles), whether its binary is on PATH.
// It makes no network calls.
func (r *Runner) Check(ctx context.Context) []Finding {
	var findings []Finding
	seen := map[string]bool{}

	check := func(name string) {
		if name == "" || seen[name] {
			return
		}
		seen[name] = true
		profile, ok := r.Config.Profiles[name]
		if !ok {
			findings = append(findings, Finding{Profile: name, Message: "profile not configured"})
			return
		}
		bin := binaryFor(profile)
		if bin == "" {
			return
		}
		if _, err := r.LookPath(bin); err != nil {
			findings = append(findings, Finding{Profile: name, Message: fmt.Sprintf("%q not found on PATH", bin)})
		}
	}

	check(r.Config.Default)
	for _, name := range r.Config.Fallback {
		check(name)
	}
	for _, name := range slices.Sorted(maps.Keys(r.Config.Tasks)) {
		check(r.Config.Tasks[name].Profile)
	}
	return findings
}

func binaryFor(p config.AIProfile) string {
	switch p.Engine {
	case config.AIEngineClaude:
		return "claude"
	case config.AIEngineCodex:
		return "codex"
	case config.AIEngineCommand:
		if len(p.Command) > 0 {
			return p.Command[0]
		}
	}
	return ""
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// tail truncates stderr to a short trailer for error messages. It redacts
// any echo of the request (see redactPromptEcho) before truncating, so the
// cut can never land inside an echoed fragment and leak part of it.
func tail(stderr []byte, sent, private string) string {
	s := strings.TrimSpace(string(redactPromptEcho(stderr, sent, private)))
	if s == "" {
		return "no stderr"
	}
	if len(s) > stderrTailLimit {
		s = "..." + s[len(s)-stderrTailLimit:]
	}
	return s
}

// promptEchoOmitted replaces any run of stderr lines redactPromptEcho drops.
const promptEchoOmitted = "[prompt omitted]"

// echoBlockHeader, on a line of its own, is where codex starts repeating
// the prompt it was sent, verbatim and line for line, blank lines included.
// Nothing marks the end of the repeat: the next line may already be an
// error.
const echoBlockHeader = "user"

// replyBlockHeaders, on a line of their own, start a block of model output:
// codex prints its reply under a bare "codex" line. A reply is written from
// the request and can restate it in words no echo check would catch.
var replyBlockHeaders = map[string]bool{
	"codex":     true,
	"assistant": true,
	"system":    true,
}

// endsReplyBlock reports whether a trimmed line closes a reply block: a
// blank line, another block header, the "tokens used" line codex follows a
// reply with, or an error line.
func endsReplyBlock(trimmed string) bool {
	return trimmed == "" || trimmed == echoBlockHeader || replyBlockHeaders[trimmed] ||
		trimmed == "tokens used" || strings.HasPrefix(trimmed, "ERROR:")
}

// minEchoFragment is the shortest run of bytes shared with the private
// prompt text that is treated as an echo rather than coincidence.
const minEchoFragment = 12

// redactPromptEcho strips text (an engine's stderr, or an error message it
// reported) of whatever repeats the request. sent is the exact text the
// engine was given; private is the part of it that is the user's (see
// Runner.prompts). Three things go:
//
//   - an echo block: an echoBlockHeader line and the lines right after it
//     that repeat sent line for line, System text included;
//   - a reply block: a replyBlockHeaders line and the lines after it, up to
//     one that endsReplyBlock (which stays);
//   - any other line that equals a line of private, or holds a run of at
//     least minEchoFragment bytes found in private. tt's public system text
//     is never matched this way, so a real diagnostic that merely shares a
//     phrase with it stays. Whitespace runs are squeezed on both sides
//     first, and a run with no letter or digit in it never counts: padding
//     and punctuation carry nothing of the user's and turn up in any table
//     or banner.
//
// Removed runs of lines collapse to one promptEchoOmitted marker line.
func redactPromptEcho(text []byte, sent, private string) []byte {
	if len(text) == 0 {
		return text
	}

	echoes := newEchoIndex(private)
	var sentLines []string
	if sent != "" {
		sentLines = strings.Split(sent, "\n")
	}
	lines := strings.Split(string(text), "\n")
	drop := make([]bool, len(lines))

	for i := 0; i < len(lines); i++ {
		trimmed := strings.TrimSpace(lines[i])
		switch {
		case trimmed == echoBlockHeader:
			drop[i] = true
			j := i + 1
			for _, want := range sentLines {
				if j >= len(lines) || strings.TrimSuffix(lines[j], "\r") != strings.TrimSuffix(want, "\r") {
					break
				}
				drop[j] = true
				j++
			}
			i = j - 1
		case replyBlockHeaders[trimmed]:
			j := i + 1
			for j < len(lines) && !endsReplyBlock(strings.TrimSpace(lines[j])) {
				j++
			}
			for k := i; k < j; k++ {
				drop[k] = true
			}
			i = j - 1
		case echoes.echoedBy(trimmed):
			drop[i] = true
		}
	}

	var out []string
	collapsed := false
	for i, l := range lines {
		if drop[i] {
			if !collapsed {
				out = append(out, promptEchoOmitted)
				collapsed = true
			}
			continue
		}
		collapsed = false
		out = append(out, l)
	}
	return []byte(strings.Join(out, "\n"))
}

// echoIndex holds what a line is checked against: the lines of the private
// text, and every minEchoFragment-byte window within one of them, each with
// its runs of whitespace squeezed to one space (see squeezeSpaces). A
// window that crosses a newline in private could never match a single
// line, so none is kept.
type echoIndex struct {
	lines   map[string]bool
	windows map[string]bool
}

func newEchoIndex(private string) echoIndex {
	idx := echoIndex{lines: map[string]bool{}, windows: map[string]bool{}}
	for _, line := range strings.Split(private, "\n") {
		line = squeezeSpaces(line)
		if hasWordRune(line) {
			idx.lines[line] = true
		}
		for i := 0; i+minEchoFragment <= len(line); i++ {
			if window := line[i : i+minEchoFragment]; hasWordRune(window) {
				idx.windows[window] = true
			}
		}
	}
	return idx
}

// echoedBy reports whether line echoes the private text: either it is
// exactly one of its lines, or it shares a window with it, which also
// catches a short fragment embedded inside a longer, otherwise unrelated
// line.
func (idx echoIndex) echoedBy(line string) bool {
	line = squeezeSpaces(line)
	if !hasWordRune(line) {
		return false
	}
	if idx.lines[line] {
		return true
	}
	for i := 0; i+minEchoFragment <= len(line); i++ {
		if idx.windows[line[i:i+minEchoFragment]] {
			return true
		}
	}
	return false
}

// squeezeSpaces trims s and squeezes each run of whitespace inside it to one
// space. Padding is layout, not content: without this, a table's column gap
// ending in one letter would match any padded line of the request.
func squeezeSpaces(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// hasWordRune reports whether s holds a letter or a digit in any script. A
// multi-byte character cut in half at either end of s decodes as
// utf8.RuneError, which is neither.
func hasWordRune(s string) bool {
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return true
		}
	}
	return false
}
