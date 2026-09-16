package ai

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/movsar/tt/internal/config"
)

// buildClaudeArgv builds a headless, non-interactive claude call. Flags
// whose value is empty are omitted so the CLI's own default applies, except
// --system-prompt, which always goes in.
//
// --system-prompt puts system, tt's instructions for this call, in place of
// claude's own default system prompt, some 13K characters written for a
// coding agent, which then is not sent at all, even when system is empty;
// system does not go out on stdin as well. Only tt's own public text may go
// into argv, which any local user can read: the request itself travels on
// stdin.
//
// --tools with an empty value removes every built-in tool, so the model can
// neither run commands nor read a file; --strict-mcp-config loads no MCP
// server either, since none is given with --mcp-config, so neither the
// user's own servers nor their claude.ai connectors add tools back.
// --setting-sources with an empty value skips the user's, the project's and
// the local settings.json, so none of the user's hooks run; --no-session-persistence
// keeps the call from writing a resumable session. None of these disturbs
// subscription (OAuth) authentication, unlike --bare, which restricts auth
// to an API key.
func buildClaudeArgv(model, effort, system string, schema []byte) []string {
	argv := []string{"claude", "-p", "--system-prompt", system}
	if model != "" {
		argv = append(argv, "--model", model)
	}
	if effort != "" {
		argv = append(argv, "--effort", effort)
	}
	if len(schema) > 0 {
		argv = append(argv, "--json-schema", compactJSON(schema))
	}
	argv = append(argv,
		"--output-format", "json",
		"--tools", "",
		"--strict-mcp-config",
		"--no-session-persistence",
		"--setting-sources", "",
	)
	return argv
}

// codexToolSwitches turn off every codex tool that could read local files
// or reach past the prompt, and some a one-shot JSON call has no use for.
// -s read-only only limits writes: under it the model's shell still reads
// anything the user can, the tt token and cache included. shell_tool is
// that shell (exec_command and write_stdin); view_image loads a local image
// by path; apps brings the ChatGPT connectors and their MCP resource tools;
// web_search = "disabled" removes the web tool; image_generation the image
// tool; multi_agent the sub-agent tools of a model whose catalog entry has
// no multi-agent version of its own; the request_user_input switch removes
// the blocking question tool, which exec mode cannot answer.
//
// Some tools stay, since no switch removes them, and none reads a local file.
// The models tt was tried with keep wait and exec. exec runs JavaScript with
// no file system or network of its own and can call only the nested tools
// left on - here apply_patch, whose writes the read-only sandbox refuses, and
// a clock tool on some models; wait waits on such a run. A model with a
// multi-agent version (gpt-5.6-sol, say) keeps its collaboration tools, and
// the default model also keeps request_user_input_async and its clock tools.
//
// codex rejects a feature name it does not know. It would silently ignore
// an unknown -c key, though, so buildCodexArgv also passes --strict-config:
// after a rename in codex, the call fails instead of running with the tool
// or context the old key used to switch off.
var codexToolSwitches = []string{
	"--disable", "shell_tool",
	"--disable", "view_image",
	"--disable", "apps",
	"--disable", "image_generation",
	"--disable", "multi_agent",
	"-c", `web_search="disabled"`,
	"-c", "tools.experimental_request_user_input.enabled=false",
}

// codexContextSwitches keep the user's own codex setup and machine out of
// the request: skills.include_instructions = false drops the list of the
// user's skills, include_environment_context = false the block naming the
// working directory, shell, date and time zone (tt's own context carries the
// time the request needs). --ignore-user-config already skips config.toml.
// The user's global instructions are still sent - $CODEX_HOME/AGENTS.override.md
// when it holds more than whitespace, $CODEX_HOME/AGENTS.md otherwise: codex
// has no switch for them that keeps CODEX_HOME, where its login lives.
var codexContextSwitches = []string{
	"-c", "skills.include_instructions=false",
	"-c", "include_environment_context=false",
}

// buildCodexArgv builds a headless codex exec call. schemaPath and outPath
// are files under the request's own temp dir; schemaPath is omitted when
// there is no schema to enforce.
//
// --ignore-user-config skips $CODEX_HOME/config.toml, so none of the user's
// own codex hooks run; auth still reads from CODEX_HOME, and -m/-c still
// override the model and reasoning effort, since neither depends on that
// file.
//
// images are the staged paths from stageImages. Each goes in as
// --image=PATH: -i takes any number of values, so a bare -i PATH right
// before the closing "-" would swallow that "-" as one more image path.
func buildCodexArgv(model, effort, schemaPath, outPath, dir string, images []string) []string {
	argv := []string{"codex", "exec"}
	if model != "" {
		argv = append(argv, "-m", model)
	}
	if effort != "" {
		argv = append(argv, "-c", "model_reasoning_effort="+effort)
	}
	argv = append(argv, "--ephemeral", "-s", "read-only", "--skip-git-repo-check", "--ignore-user-config", "--strict-config")
	argv = append(argv, codexToolSwitches...)
	argv = append(argv, codexContextSwitches...)
	if schemaPath != "" {
		argv = append(argv, "--output-schema", schemaPath)
	}
	argv = append(argv, "-o", outPath, "-C", dir)
	for _, img := range images {
		argv = append(argv, "--image="+img)
	}
	argv = append(argv, "-")
	return argv
}

// stageImages links each image into dir under a neutral name - img-1.png,
// img-2.jpg and so on, keeping a plain extension - and returns those paths
// in order. The agent never sees where the user keeps a file (codex writes
// each image's path into the model's input), and no path reaches an argv
// with a character a CLI would misread (codex splits --image values at
// commas). A symlink is enough: the CLI opens the image through it, and
// removing dir afterwards removes only the link. Where a symlink cannot be
// made, the image is copied instead.
func stageImages(dir string, images []string) ([]string, error) {
	staged := make([]string, 0, len(images))
	for i, img := range images {
		src, err := filepath.Abs(img)
		if err != nil {
			return nil, fmt.Errorf("ai: image %q: %w", img, err)
		}
		info, err := os.Stat(src)
		if err != nil {
			return nil, fmt.Errorf("ai: image: %w", err)
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("ai: image %q is not a regular file", img)
		}
		dst := filepath.Join(dir, fmt.Sprintf("img-%d%s", i+1, imageExt(src)))
		if err := os.Symlink(src, dst); err != nil {
			if err := copyFile(src, dst); err != nil {
				return nil, fmt.Errorf("ai: stage image %q: %w", img, err)
			}
		}
		staged = append(staged, dst)
	}
	return staged, nil
}

// imageExt returns path's extension when it is a plain one, a dot and one
// to ten ASCII letters or digits, and "" otherwise, so that no other
// character carries over into the staged name.
func imageExt(path string) string {
	ext := filepath.Ext(path)
	if len(ext) < 2 || len(ext) > 11 {
		return ""
	}
	for _, r := range ext[1:] {
		if !('a' <= r && r <= 'z' || 'A' <= r && r <= 'Z' || '0' <= r && r <= '9') {
			return ""
		}
	}
	return ext
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// placeholderValues are substituted into an engine = "command" argv
// override. See config.AIPlaceholders for the allowed names.
type placeholderValues struct {
	model      string
	effort     string
	schema     string
	schemaFile string
	outputFile string
	prompt     string
	images     []string
}

// buildCommandArgv expands a literal argv override. When the template uses
// {prompt}, the built prompt goes into that argv slot instead of stdin, per
// config.AI's documented placeholder contract.
func (r *Runner) buildCommandArgv(profile config.AIProfile, model, effort string, req Request, dir, combined string) (argv []string, stdin []byte, outPath string, err error) {
	template := profile.Command
	usesPrompt := containsPlaceholder(template, "{prompt}")
	usesOutputFile := containsPlaceholder(template, "{output_file}")
	usesSchemaFile := containsPlaceholder(template, "{schema_file}")

	var images []string
	if containsPlaceholder(template, "{image}") {
		images, err = stageImages(dir, req.Images)
		if err != nil {
			return nil, nil, "", err
		}
	}

	schemaFilePath := ""
	if usesSchemaFile && len(req.Schema) > 0 {
		schemaFilePath, err = writeSchemaFile(dir, req.Schema)
		if err != nil {
			return nil, nil, "", err
		}
	}

	if usesOutputFile {
		outPath = filepath.Join(dir, "output.json")
	}

	values := placeholderValues{
		model:      model,
		effort:     effort,
		schema:     compactJSON(req.Schema),
		schemaFile: schemaFilePath,
		outputFile: outPath,
		prompt:     combined,
		images:     images,
	}
	argv = expandCommandArgs(template, values)

	if !usesPrompt {
		stdin = []byte(combined)
	}
	return argv, stdin, outPath, nil
}

func expandCommandArgs(template []string, v placeholderValues) []string {
	argv := make([]string, 0, len(template))
	for _, arg := range template {
		if arg == "{image}" {
			argv = append(argv, v.images...)
			continue
		}
		arg = strings.ReplaceAll(arg, "{schema_file}", v.schemaFile)
		arg = strings.ReplaceAll(arg, "{schema}", v.schema)
		arg = strings.ReplaceAll(arg, "{output_file}", v.outputFile)
		arg = strings.ReplaceAll(arg, "{model}", v.model)
		arg = strings.ReplaceAll(arg, "{effort}", v.effort)
		arg = strings.ReplaceAll(arg, "{prompt}", v.prompt)
		argv = append(argv, arg)
	}
	return argv
}

func containsPlaceholder(template []string, token string) bool {
	for _, arg := range template {
		if strings.Contains(arg, token) {
			return true
		}
	}
	return false
}

func writeSchemaFile(dir string, schema []byte) (string, error) {
	path := filepath.Join(dir, "schema.json")
	if err := os.WriteFile(path, schema, 0o600); err != nil {
		return "", fmt.Errorf("ai: write schema file: %w", err)
	}
	return path, nil
}
