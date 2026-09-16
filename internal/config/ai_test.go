package config

import (
	"errors"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/movsar/tt/internal/model"
)

func TestAIDefaults(t *testing.T) {
	ai := Default().AI
	if ai.Default != "fast" {
		t.Errorf("AI.Default = %q, want %q", ai.Default, "fast")
	}
	if ai.Timeout.Duration() != 90*time.Second {
		t.Errorf("AI.Timeout = %v, want 90s", ai.Timeout)
	}
	if ai.Context != AIContextMinimal {
		t.Errorf("AI.Context = %q, want %q", ai.Context, AIContextMinimal)
	}
	if len(ai.Fallback) != 0 {
		t.Errorf("AI.Fallback = %v, want none", ai.Fallback)
	}
	fast, ok := ai.Profiles["fast"]
	if !ok {
		t.Fatal(`AI.Profiles["fast"] is missing`)
	}
	if fast.Engine != AIEngineClaude || fast.Model != "sonnet" || fast.Effort != "high" {
		t.Errorf("AI.Profiles[fast] = %+v, want claude/sonnet/high", fast)
	}
	if len(ai.Tasks) != 0 {
		t.Errorf("AI.Tasks = %v, want none", ai.Tasks)
	}
}

func TestParseAIContext(t *testing.T) {
	for _, ok := range []string{"minimal", "today", "all", "TODAY", " all "} {
		if _, err := ParseAIContext(ok); err != nil {
			t.Errorf("ParseAIContext(%q) = %v, want no error", ok, err)
		}
	}
	if _, err := ParseAIContext("sometimes"); err == nil {
		t.Error("ParseAIContext(\"sometimes\") accepted an unknown value")
	}
}

func TestParseAIEngine(t *testing.T) {
	for _, ok := range []string{"claude", "codex", "command", "CLAUDE"} {
		if _, err := ParseAIEngine(ok); err != nil {
			t.Errorf("ParseAIEngine(%q) = %v, want no error", ok, err)
		}
	}
	if _, err := ParseAIEngine("gpt"); err == nil {
		t.Error("ParseAIEngine(\"gpt\") accepted an unknown value")
	}
}

func TestLoadFileParsesTheApprovedAIShape(t *testing.T) {
	src := `
[ai]
default = "fast"
timeout = "45s"
context = "today"
fallback = ["local"]

[ai.profiles.fast]
engine = "claude"
model = "sonnet"
effort = "high"

[ai.profiles.deep]
engine = "codex"
model = "gpt-5.6-sol"
effort = "high"

[ai.profiles.local]
engine = "command"
command = ["ollama", "run", "llava"]

[ai.tasks.ai]
profile = "fast"

[ai.tasks.find]
profile = "fast"
`
	got, err := LoadFile(write(t, src))
	if err != nil {
		t.Fatalf("LoadFile(): %v", err)
	}
	want := AI{
		Default:  "fast",
		Timeout:  model.Duration(45 * time.Second),
		Context:  AIContextToday,
		Fallback: []string{"local"},
		Profiles: map[string]AIProfile{
			"fast":  {Engine: AIEngineClaude, Model: "sonnet", Effort: "high"},
			"deep":  {Engine: AIEngineCodex, Model: "gpt-5.6-sol", Effort: "high"},
			"local": {Engine: AIEngineCommand, Command: []string{"ollama", "run", "llava"}},
		},
		Tasks: map[string]AITask{
			"ai":   {Profile: "fast"},
			"find": {Profile: "fast"},
		},
	}
	if !reflect.DeepEqual(got.AI, want) {
		t.Errorf("got %+v\nwant %+v", got.AI, want)
	}
}

func TestAITaskPromptExpandsHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory")
	}
	got, err := LoadFile(write(t, "[ai.tasks.ai]\nprofile = \"fast\"\nprompt = \"~/prompts/ai.md\"\n"))
	if err != nil {
		t.Fatalf("LoadFile(): %v", err)
	}
	want := home + "/prompts/ai.md"
	if got.AI.Tasks[AITaskAI].Prompt != want {
		t.Errorf("prompt = %q, want %q", got.AI.Tasks[AITaskAI].Prompt, want)
	}
}

func TestAIEngineEnumIsValidated(t *testing.T) {
	_, err := LoadFile(write(t, "[ai.profiles.x]\nengine = \"ollama\"\n"))
	if err == nil || !strings.Contains(err.Error(), `ai.profiles.x.engine: unknown value "ollama"`) {
		t.Errorf("got %v", err)
	}
}

func TestAICommandEngineRequiresCommand(t *testing.T) {
	_, err := LoadFile(write(t, "[ai.profiles.x]\nengine = \"command\"\n"))
	if err == nil || !strings.Contains(err.Error(), `command is required when engine = "command"`) {
		t.Errorf("got %v", err)
	}
}

func TestAICommandPlaceholdersAreValidated(t *testing.T) {
	_, err := LoadFile(write(t, "[ai.profiles.x]\nengine = \"command\"\ncommand = [\"echo\", \"{bogus}\"]\n"))
	if err == nil || !strings.Contains(err.Error(), `unknown placeholder "{bogus}"`) {
		t.Errorf("got %v", err)
	}
	_, err = LoadFile(write(t, "[ai.profiles.x]\nengine = \"command\"\ncommand = [\"echo\", \"{model}\", \"{effort}\", \"{schema}\", \"{schema_file}\", \"{output_file}\", \"{image}\", \"{prompt}\"]\n"))
	if err != nil {
		t.Errorf("all accepted placeholders were rejected: %v", err)
	}
}

func TestAIProfileEnvPassthroughIsAccepted(t *testing.T) {
	got, err := LoadFile(write(t, "[ai.profiles.x]\nengine = \"command\"\ncommand = [\"ollama\"]\nenv = [\"OLLAMA_HOST\", \"HTTPS_PROXY\"]\n"))
	if err != nil {
		t.Fatalf("LoadFile(): %v", err)
	}
	want := []string{"OLLAMA_HOST", "HTTPS_PROXY"}
	if !reflect.DeepEqual(got.AI.Profiles["x"].Env, want) {
		t.Errorf("Env = %v, want %v", got.AI.Profiles["x"].Env, want)
	}
}

func TestAIProfileEnvRejectsTTAndHerdrAndXDG(t *testing.T) {
	for _, bad := range []string{"TT_TOKEN", "HERDR_ENV", "XDG_CONFIG_HOME", "XDG_DATA_HOME"} {
		src := "[ai.profiles.x]\nengine = \"command\"\ncommand = [\"ollama\"]\nenv = [\"" + bad + "\"]\n"
		_, err := LoadFile(write(t, src))
		if err == nil || !strings.Contains(err.Error(), `ai.profiles.x.env: "`+bad+`" is not allowed`) {
			t.Errorf("env = [%q]: got %v, want a rejection naming it", bad, err)
		}
	}
}

func TestForbiddenAIEnvVar(t *testing.T) {
	for _, bad := range []string{"TT_TOKEN", "TT_ANYTHING", "HERDR_ENV", "HERDR_PANE_ID", "XDG_CONFIG_HOME", "XDG_DATA_HOME"} {
		if _, forbidden := ForbiddenAIEnvVar(bad); !forbidden {
			t.Errorf("ForbiddenAIEnvVar(%q) = false, want true", bad)
		}
	}
	for _, ok := range []string{"OLLAMA_HOST", "HTTPS_PROXY", "HOME", "PATH"} {
		if _, forbidden := ForbiddenAIEnvVar(ok); forbidden {
			t.Errorf("ForbiddenAIEnvVar(%q) = true, want false", ok)
		}
	}
}

func TestAIReferencesMustNameAKnownProfile(t *testing.T) {
	cases := map[string]string{
		"[ai]\ndefault = \"nope\"\n[ai.profiles.fast]\nengine = \"claude\"\n":          `ai.default: references unknown profile "nope"`,
		"[ai]\nfallback = [\"nope\"]\n[ai.profiles.fast]\nengine = \"claude\"\n":       `ai.fallback: references unknown profile "nope"`,
		"[ai.tasks.ai]\nprofile = \"nope\"\n[ai.profiles.fast]\nengine = \"claude\"\n": `ai.tasks.ai.profile: references unknown profile "nope"`,
	}
	for src, want := range cases {
		_, err := LoadFile(write(t, src))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("src %q: got %v, want to contain %q", src, err, want)
		}
	}
}

func TestAITimeoutMustBePositive(t *testing.T) {
	_, err := LoadFile(write(t, "[ai]\ntimeout = \"0\"\n"))
	if err == nil || !strings.Contains(err.Error(), "ai.timeout: must be greater than 0") {
		t.Errorf("got %v", err)
	}
}

func TestAIUnknownTopLevelKeySuggestsTheRealOne(t *testing.T) {
	_, err := LoadFile(write(t, "[ai]\ndefualt = \"fast\"\n"))
	if err == nil || !strings.Contains(err.Error(), `unknown key "ai.defualt" (did you mean "ai.default"?)`) {
		t.Errorf("got %v", err)
	}
}

func TestAITaskNames(t *testing.T) {
	if AITaskAI != "ai" || AITaskFind != "find" {
		t.Errorf("AITaskAI, AITaskFind = %q, %q, want ai, find", AITaskAI, AITaskFind)
	}
}

func TestAIUnknownTaskNameIsAnUnknownTable(t *testing.T) {
	cases := map[string]struct {
		src  string
		want []Problem
	}{
		"the old add task": {
			"[ai.tasks.add]\nprofile = \"fast\"\n",
			[]Problem{{Line: 1, Msg: `unknown table "ai.tasks.add" (allowed: ai, find)`}},
		},
		"a typo of find": {
			"[ai.tasks.fnd]\nprofile = \"fast\"\n",
			[]Problem{{Line: 1, Msg: `unknown table "ai.tasks.fnd" (did you mean "ai.tasks.find"?)`}},
		},
		"its keys are not checked on top": {
			"[ai.tasks.add]\nprofle = \"nope\"\n",
			[]Problem{{Line: 1, Msg: `unknown table "ai.tasks.add" (allowed: ai, find)`}},
		},
		"nor is its profile reference": {
			"[ai.tasks.add]\nprofile = \"nope\"\n",
			[]Problem{{Line: 1, Msg: `unknown table "ai.tasks.add" (allowed: ai, find)`}},
		},
		"written as an inline table": {
			"[ai.tasks]\nadd = { profile = \"fast\" }\n",
			[]Problem{{Line: 2, Msg: `unknown table "ai.tasks.add" (allowed: ai, find)`}},
		},
		"written as a dotted key": {
			"ai.tasks.add.profile = \"fast\"\n",
			[]Problem{{Line: 1, Msg: `unknown table "ai.tasks.add" (allowed: ai, find)`}},
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := LoadFile(write(t, c.src))
			var cerr *Error
			if !errors.As(err, &cerr) {
				t.Fatalf("LoadFile err = %v, want a *config.Error", err)
			}
			if !slices.Equal(cerr.Problems, c.want) {
				t.Errorf("problems = %#v\nwant %#v", cerr.Problems, c.want)
			}
		})
	}

	for _, task := range []string{AITaskAI, AITaskFind} {
		if _, err := LoadFile(write(t, "[ai.tasks."+task+"]\nprofile = \"fast\"\n")); err != nil {
			t.Errorf("[ai.tasks.%s] was rejected: %v", task, err)
		}
	}
}

func TestAIUnknownKeyInsideANamedTableNamesTheKey(t *testing.T) {
	cases := map[string]struct {
		src  string
		want []Problem
	}{
		"a typo inside a profile": {
			"[ai.profiles.fast]\nengine = \"claude\"\nengin = \"x\"\n",
			[]Problem{{Line: 3, Msg: `unknown key "ai.profiles.fast.engin" (did you mean "ai.profiles.fast.engine"?)`}},
		},
		"a typo inside a task": {
			"[ai.tasks.find]\nprofle = \"fast\"\n",
			[]Problem{{Line: 2, Msg: `unknown key "ai.tasks.find.profle" (did you mean "ai.tasks.find.profile"?)`}},
		},
		"a typo inside an inline profile": {
			"[ai.profiles]\nfast = { engine = \"claude\", modl = \"x\" }\n",
			[]Problem{{Line: 2, Msg: `unknown key "ai.profiles.fast.modl" (did you mean "ai.profiles.fast.model"?)`}},
		},
		"a key like no profile key": {
			"[ai.profiles.fast]\nengine = \"claude\"\nwhatever = 1\n",
			[]Problem{{Line: 3, Msg: `unknown key "ai.profiles.fast.whatever"`}},
		},
		"a misspelt profiles table": {
			"[ai.profil.fast]\nengine = \"claude\"\n",
			[]Problem{{Line: 1, Msg: `unknown table "ai.profil.fast" (did you mean "ai.profiles.fast"?)`}},
		},
		"a misspelt tasks table": {
			"[ai.taks.find]\nprofile = \"fast\"\n",
			[]Problem{{Line: 1, Msg: `unknown table "ai.taks.find" (did you mean "ai.tasks.find"?)`}},
		},
		"a misspelt profiles table written inline": {
			"[ai]\nprofil = { fast = { engine = \"claude\" } }\n",
			[]Problem{{Line: 2, Msg: `unknown table "ai.profil" (did you mean "ai.profiles"?)`}},
		},
		"a table inside an unknown task": {
			"[ai.tasks.add.extra]\nk = 1\n",
			[]Problem{{Line: 1, Msg: `unknown table "ai.tasks.add" (allowed: ai, find)`}},
		},
		"a table inside a profile": {
			"[ai.profiles.fast]\nengine = \"claude\"\n[ai.profiles.fast.extra]\nk = 1\n",
			[]Problem{{Line: 3, Msg: `unknown table "ai.profiles.fast.extra"`}},
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := LoadFile(write(t, c.src))
			var cerr *Error
			if !errors.As(err, &cerr) {
				t.Fatalf("LoadFile err = %v, want a *config.Error", err)
			}
			if !slices.Equal(cerr.Problems, c.want) {
				t.Errorf("problems = %#v\nwant %#v", cerr.Problems, c.want)
			}
		})
	}
}

func TestNamedTables(t *testing.T) {
	named := namedTables()
	if len(named) != 2 {
		t.Errorf("namedTables() = %v, want ai.profiles and ai.tasks only", named)
	}
	profiles := named[[2]string{"ai", "profiles"}]
	if want := []string{"engine", "model", "effort", "command", "env"}; !slices.Equal(profiles.leaves, want) || profiles.names != nil {
		t.Errorf("ai.profiles = %+v, want leaves %v and any name", profiles, want)
	}
	tasks := named[[2]string{"ai", "tasks"}]
	if want := []string{"profile", "prompt"}; !slices.Equal(tasks.leaves, want) || !slices.Equal(tasks.names, []string{AITaskAI, AITaskFind}) {
		t.Errorf("ai.tasks = %+v, want leaves %v and names ai, find", tasks, want)
	}
}

func TestAIImagePlaceholderMustBeAWholeArgument(t *testing.T) {
	_, err := LoadFile(write(t, "[ai.profiles.x]\nengine = \"command\"\ncommand = [\"tool\", \"--img={image}\"]\n"))
	if err == nil || !strings.Contains(err.Error(), `ai.profiles.x.command: "{image}" must be a whole argument, got "--img={image}"`) {
		t.Errorf("got %v", err)
	}
	if _, err := LoadFile(write(t, "[ai.profiles.x]\nengine = \"command\"\ncommand = [\"tool\", \"--img\", \"{image}\"]\n")); err != nil {
		t.Errorf("a whole-argument {image} was rejected: %v", err)
	}
}

func TestAIEncodeRoundTrip(t *testing.T) {
	cfg := Default()
	cfg.AI = AI{
		Default:  "fast",
		Timeout:  model.Duration(2 * time.Minute),
		Context:  AIContextAll,
		Fallback: []string{"local"},
		Profiles: map[string]AIProfile{
			"fast":  {Engine: AIEngineClaude, Model: "sonnet", Effort: "high"},
			"local": {Engine: AIEngineCommand, Command: []string{"ollama", "run", "llava"}, Env: []string{"OLLAMA_HOST"}},
		},
		Tasks: map[string]AITask{
			AITaskAI: {Profile: "fast", Prompt: "/abs/prompts/ai.md"},
		},
	}
	src, err := cfg.Encode()
	if err != nil {
		t.Fatal(err)
	}
	got, err := parse("memory.toml", src)
	if err != nil {
		t.Fatalf("the encoded config must load back: %v\n%s", err, src)
	}
	if !reflect.DeepEqual(got, cfg) {
		t.Errorf("the ai section changed in the round trip:\ngot  %+v\nwant %+v", got.AI, cfg.AI)
	}
}

func TestAIPlaceholders(t *testing.T) {
	want := "model,effort,schema,schema_file,output_file,image,prompt"
	if got := strings.Join(AIPlaceholders(), ","); got != want {
		t.Errorf("AIPlaceholders() = %v", AIPlaceholders())
	}
	re := AIPlaceholderPattern()
	for _, name := range AIPlaceholders() {
		if got := re.FindString("x {" + name + "} y"); got != "{"+name+"}" {
			t.Errorf("AIPlaceholderPattern did not match {%s}, found %q", name, got)
		}
	}
}

func TestExpandHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory")
	}
	cases := map[string]string{
		"~":          home,
		"~/a/b":      home + "/a/b",
		"/abs/path":  "/abs/path",
		"relative/x": "relative/x",
		"":           "",
	}
	for in, want := range cases {
		got, err := expandHome(in)
		if err != nil || got != want {
			t.Errorf("expandHome(%q) = %q, %v, want %q", in, got, err, want)
		}
	}
}
