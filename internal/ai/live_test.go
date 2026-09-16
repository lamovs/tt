package ai

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/movsar/tt/internal/config"
	"github.com/movsar/tt/internal/model"
)

// TestLiveAgents makes real calls to the claude and codex CLIs through the
// real Runner - real runExec, real buildEnv, no shell scripting and no
// fakes. It needs both binaries installed and authenticated and spends real
// API usage, so it runs only with TT_AI_LIVE=1 set; it is skipped by
// default and in any normal `go test` run.
//
// The prompt is neutral dummy text, never real task data, per this
// package's privacy rules. Each case's Response is written as evidence to
// TT_AI_LIVE_DIR (a scratchpad directory) if set, or to a t.TempDir()
// otherwise - never into the repository.
func TestLiveAgents(t *testing.T) {
	if os.Getenv("TT_AI_LIVE") != "1" {
		t.Skip("set TT_AI_LIVE=1 to run live calls against claude and codex")
	}

	for _, name := range []string{"HERDR_ENV", "HERDR_PANE_ID", "HERDR_SOCKET"} {
		if old, had := os.LookupEnv(name); had {
			os.Unsetenv(name)
			t.Cleanup(func() { os.Setenv(name, old) })
		}
	}

	evidenceDir := os.Getenv("TT_AI_LIVE_DIR")
	if evidenceDir == "" {
		evidenceDir = t.TempDir()
	}

	// additionalProperties: false is required by codex/OpenAI's structured
	// output enforcement (a schema without it is rejected with HTTP 400
	// invalid_json_schema); claude accepts either form.
	schema := []byte(`{"type":"object","properties":{"ok":{"type":"boolean"}},"required":["ok"],"additionalProperties":false}`)
	const prompt = "Reply with JSON only. Set ok to true."

	cases := []struct {
		name    string
		profile config.AIProfile
	}{
		{"claude", config.AIProfile{Engine: config.AIEngineClaude, Model: "sonnet", Effort: "low"}},
		{"codex", config.AIProfile{Engine: config.AIEngineCodex, Model: "gpt-5.6-sol", Effort: "low"}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := config.AI{
				Default: c.name,
				Timeout: model.Duration(60 * time.Second),
				Profiles: map[string]config.AIProfile{
					c.name: c.profile,
				},
			}
			r := New(cfg)

			resp, err := r.Run(context.Background(), Request{Prompt: prompt, Schema: schema})

			evidence := map[string]any{"engine": c.name}
			if err != nil {
				evidence["error"] = err.Error()
			} else {
				var parsed any
				_ = json.Unmarshal(resp.JSON, &parsed)
				evidence["profile"] = resp.Profile
				evidence["response_engine"] = resp.Engine
				evidence["duration"] = resp.Duration.String()
				evidence["json"] = parsed
			}
			raw, _ := json.MarshalIndent(evidence, "", "  ")
			path := filepath.Join(evidenceDir, "live-"+c.name+".json")
			if writeErr := os.WriteFile(path, raw, 0o600); writeErr != nil {
				t.Logf("could not write evidence to %s: %v", path, writeErr)
			} else {
				t.Logf("evidence written to %s", path)
			}

			if err != nil {
				t.Fatalf("Run(%s): %v", c.name, err)
			}
			var out struct {
				OK bool `json:"ok"`
			}
			if err := json.Unmarshal(resp.JSON, &out); err != nil || !out.OK {
				t.Fatalf("Run(%s) returned %s, want {\"ok\":true}", c.name, resp.JSON)
			}
		})
	}
}
