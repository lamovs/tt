package ai

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/movsar/tt/internal/config"
)

// childEnvAllowlist is the fixed environment every agent subprocess gets,
// regardless of profile or engine; engineEnvAllowlist adds what one engine's
// own CLI needs. It deliberately excludes tt's own settings
// (XDG_CONFIG_HOME, XDG_DATA_HOME, TT_TOKEN), Herdr's session variables, and
// everything else in tt's own environment: the child gets just enough to
// find itself, and the network path to reach its provider - nothing that
// could leak tt or TickTick material.
//
// XDG_CACHE_HOME is deliberately left out even though it is a legitimate
// claude/codex-owned variable (not one of tt's): every built-in call already
// runs without a persisted session (--no-session-persistence, --ephemeral),
// so a warm cache is not required for a call to succeed, only to gain a
// little speed; without it, the child falls back to its own default under
// HOME, which is already forwarded. Forwarding it would only reproduce the
// same behavior while a customized value could hint at unrelated local
// layout.
var childEnvAllowlist = []string{
	// Identity, shell and home: some macOS keychain and Node.js paths
	// consult these even when HOME alone would do.
	"HOME", "USER", "LOGNAME", "SHELL", "PATH", "TMPDIR", "LANG", "LC_ALL", "TERM",

	// Network path to the provider, for proxied or firewalled networks.
	// Both cases are forwarded because HTTP client libraries are not
	// consistent about which one they read.
	"HTTPS_PROXY", "HTTP_PROXY", "NO_PROXY",
	"https_proxy", "http_proxy", "no_proxy",
	"SSL_CERT_FILE", "SSL_CERT_DIR", "NODE_EXTRA_CA_CERTS",
}

// engineEnvAllowlist is each built-in engine's own auth and config
// location, forwarded to that engine only: one vendor's API key never
// reaches the other vendor's CLI, and engine = "command" gets neither, nor
// anything else beyond childEnvAllowlist, unless its profile lists it in
// env.
var engineEnvAllowlist = map[config.AIEngine][]string{
	config.AIEngineClaude: {"ANTHROPIC_API_KEY", "CLAUDE_CONFIG_DIR"},
	config.AIEngineCodex:  {"OPENAI_API_KEY", "CODEX_HOME"},
}

// engineEnvFixed is set in an engine's subprocess whatever tt's own
// environment or the profile says. For claude:
// CLAUDE_CODE_DISABLE_AUTO_MEMORY stops it from keeping a memory directory
// for the call's temporary working directory under its config dir, one
// more per call, left behind for good; CLAUDE_CODE_DISABLE_CLAUDE_MDS keeps
// the user's CLAUDE.md files out of the request, which --setting-sources
// already does, should that flag's reach ever change;
// ENABLE_CLAUDEAI_MCP_SERVERS=false skips even fetching the list of the
// user's claude.ai connectors, which --strict-mcp-config would load none
// of anyway; CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC stops the second
// request claude otherwise makes to name the session, which carries the
// request text once more.
var engineEnvFixed = map[config.AIEngine][]string{
	config.AIEngineClaude: {
		"CLAUDE_CODE_DISABLE_AUTO_MEMORY=1",
		"CLAUDE_CODE_DISABLE_CLAUDE_MDS=1",
		"ENABLE_CLAUDEAI_MCP_SERVERS=false",
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1",
	},
}

// buildEnv returns the environment for one subprocess of engine: the fixed
// allowlists above, plus any profile-specific names in extra that are set in
// tt's own environment, plus the engine's fixed settings, which neither tt's
// environment nor extra can override. extra should already have passed
// config's validation (config.ForbiddenAIEnvVar), but the check is repeated
// here so a Runner built by hand, outside config parsing, gets the same
// guarantee.
func buildEnv(engine config.AIEngine, extra []string) []string {
	fixed := engineEnvFixed[engine]
	names := len(childEnvAllowlist) + len(engineEnvAllowlist[engine]) + len(extra)
	env := make([]string, 0, names+len(fixed))
	seen := make(map[string]bool, names+len(fixed))
	for _, kv := range fixed {
		name, _, _ := strings.Cut(kv, "=")
		seen[name] = true
	}
	add := func(name string) {
		if seen[name] {
			return
		}
		seen[name] = true
		if v, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+v)
		}
	}
	for _, name := range childEnvAllowlist {
		add(name)
	}
	for _, name := range engineEnvAllowlist[engine] {
		add(name)
	}
	for _, name := range extra {
		if _, forbidden := config.ForbiddenAIEnvVar(name); forbidden {
			continue
		}
		add(name)
	}
	return append(env, fixed...)
}

// runExec is the real execFunc: it runs name/args with stdin on the child's
// standard input, dir as its working directory and temp-file home, and env
// (from buildEnv) in place of tt's own environment. The child leads a
// process group of its own. Cancelling ctx kills that whole group (see
// setProcessGroup), and so does the child's own exit, before it is reaped
// (see killGroupAfterExit): an agent CLI is often a wrapper that starts the
// real binary as its own child, and either may leave processes behind that
// would otherwise outlive tt, some holding the output pipes open until
// WaitDelay gives up on them.
func runExec(ctx context.Context, name string, args []string, stdin []byte, dir string, env []string) (stdout, stderr []byte, err error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = env
	cmd.Stdin = bytes.NewReader(stdin)

	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	cmd.WaitDelay = 5 * time.Second
	setProcessGroup(cmd)

	if err := cmd.Start(); err != nil {
		return nil, nil, err
	}
	killGroupAfterExit(cmd.Process.Pid)
	err = cmd.Wait()
	return outBuf.Bytes(), errBuf.Bytes(), err
}
