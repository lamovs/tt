package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/movsar/tt/internal/api"
	"github.com/movsar/tt/internal/app"
	"github.com/movsar/tt/internal/cli"
	"github.com/movsar/tt/internal/config"
	"github.com/movsar/tt/internal/focus"
	"github.com/movsar/tt/internal/store"
	"github.com/movsar/tt/internal/tui"
)

type uiSystem struct {
	store         *store.Store
	output        io.Writer
	color         config.ColorMode
	colorOverride bool
}

func (s *uiSystem) close() {
	if s.store != nil {
		_ = s.store.Close()
	}
}

func (s *uiSystem) runtime(cfg config.Config) tui.SettingsResult {
	client := func() (*api.Client, error) {
		token, err := syncToken()
		if err != nil {
			return nil, err
		}
		return managedAPIClient(token), nil
	}
	mode := cfg.Color
	if s.colorOverride {
		mode = s.color
	}
	return tui.SettingsResult{
		Resources:   app.NewResources(s.store, client),
		ServerTasks: app.NewServerTaskQueries(s.store, client),
		Queries:     app.NewBrowser(s.store), Actions: app.NewActions(s.store, cfg, client).WithTopicClient(func() (focus.TopicClient, error) {
			client, _, err := webClient(context.Background())
			return client, err
		}),
		Timers: app.NewTimers(s.store, cfg, client, startTimerWatcher, func(ctx context.Context, session store.TimerSession) error {
			return notifyTimerSession(ctx, s.store, session, cfg.Timer.OnEnd, RunNotify)
		}).WithTopicClient(func() (focus.TopicClient, error) {
			client, _, err := webClient(context.Background())
			return client, err
		}).WithCompletionWake(startBackgroundWorker),
		DefaultProject: cfg.DefaultProject, Color: cli.PaletteFor(mode, s.output).Enabled(),
	}
}

func uiPrivateText(text string) string {
	values := []string{strings.TrimSpace(os.Getenv(tokenEnvVar))}
	if token, err := api.LoadToken(); err == nil {
		values = append(values, token.Value)
	}
	if c, _, err := loadCredentials(); err == nil {
		values = append(values, c.ClientSecret, c.RefreshToken)
	}
	if c, _, err := loadWebCredentials(); err == nil {
		values = append(values, c.Session)
	}
	for _, value := range values {
		if value == "" {
			continue
		}
		quoted := strconv.Quote(value)
		for _, form := range []string{value, quoted[1 : len(quoted)-1]} {
			text = strings.ReplaceAll(text, form, "[private]")
		}
	}
	return text
}

func (s *uiSystem) Settings(ctx context.Context) (tui.SettingsResult, error) {
	result := tui.SettingsResult{Lines: []string{"tt version " + version}}
	account, _ := checkToken()
	result.Lines = append(result.Lines, "Account (local credential status; not remote verification):", uiPrivateText(account.status.label()+": "+account.summary))
	path, err := config.Path()
	if err != nil {
		return result, errors.New("Cannot resolve the config path; run tt config for diagnostics")
	}
	result.Lines = append(result.Lines, "Config: "+uiPrivateText(path))
	cfg, err := config.Load()
	if err != nil {
		return result, errors.New("Configuration is invalid or unreadable; run tt config for diagnostics")
	}
	if _, err := os.Lstat(path); errors.Is(err, fs.ErrNotExist) {
		result.Lines = append(result.Lines, "No config file; using defaults. i previews explicit initialization.")
	}
	encoded, err := cfg.Encode()
	if err != nil {
		return result, err
	}
	result.Lines = append(result.Lines, strings.Split(uiPrivateText(string(encoded)), "\n")...)
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if s.store == nil {
		s.store, err = store.Open(ctx, "")
		if err != nil {
			return result, errors.New("Cache could not be opened; use d for doctor")
		}
	}
	runtime := s.runtime(cfg)
	runtime.Lines = append(result.Lines, "Validated settings applied. CLI color override still takes precedence.",
		"S: settings | r reload | g default focus | i init | d doctor | f fix | n test | l browser login | L token login", "?: help | Esc: back")
	return runtime, nil
}

func (s *uiSystem) Doctor(ctx context.Context) ([]string, error) {
	results := collectDoctorChecks(ctx, observeConfigWrite)
	if err := ctx.Err(); err != nil {
		return []string{"Doctor canceled; no diagnosis claimed."}, err
	}
	lines := []string{"Doctor results (diagnostics; no repair or notifier test)", ""}
	counts := make(map[checkStatus]int)
	for _, result := range results {
		counts[result.status]++
		lines = append(lines, uiPrivateText(result.status.label()+": "+result.summary))
		for _, note := range result.notes {
			lines = append(lines, uiPrivateText(note))
		}
		lines = append(lines, "")
	}
	lines = append(lines, fmt.Sprintf("%d ok, %d warn, %d fail, %d skip", counts[statusOK], counts[statusWarn], counts[statusFail], counts[statusSkipped]))
	return lines, nil
}

func (s *uiSystem) FocusChoices(ctx context.Context) (app.FocusChoices, error) {
	cfg, err := config.Load()
	if err != nil {
		return app.FocusChoices{}, errors.New("Configuration is invalid or unreadable; run tt config")
	}
	if s.store == nil {
		return app.FocusChoices{}, errors.New("Cache is unavailable; reload settings first")
	}
	choices, err := app.DefaultFocusChoices(ctx, s.store, cfg.DefaultProject)
	for i := range choices.Projects {
		choices.Projects[i].Name = uiPrivateText(choices.Projects[i].Name)
	}
	for i := range choices.Tasks {
		choices.Tasks[i].Title = uiPrivateText(choices.Tasks[i].Title)
	}
	return choices, err
}

func (s *uiSystem) PrepareDefaultFocus(ctx context.Context, ref config.FocusReference) (tui.SystemPreview, error) {
	prepared, err := readDefaultProjectConfig()
	if err != nil {
		return tui.SystemPreview{}, errors.New(uiPrivateText(err.Error()))
	}
	if s.store == nil && ref.TaskID != "" {
		return tui.SystemPreview{}, errors.New("Cache is unavailable; reload settings first")
	}
	preview, err := prepareDefaultFocus(ctx, prepared, s.store, ref)
	if err != nil {
		return preview, errors.New(uiPrivateText(err.Error()))
	}
	for i := range preview.Lines {
		preview.Lines[i] = uiPrivateText(preview.Lines[i])
	}
	apply := preview.Apply
	preview.Apply = func(ctx context.Context) (string, error) {
		text, err := apply(ctx)
		if err != nil {
			err = errors.New(uiPrivateText(err.Error()))
		}
		return uiPrivateText(text), err
	}
	return preview, nil
}

func (s *uiSystem) Prepare(ctx context.Context, action string) (tui.SystemPreview, error) {
	p := tui.SystemPreview{Action: action}
	if err := ctx.Err(); err != nil {
		return p, err
	}
	if action == "login" || action == "login token" {
		p.Lines = []string{"Run tt " + action + " in the restored terminal.", "This can replace the saved credential for one account.", "No existing token or client secret is displayed. Ctrl+C cancels and exits tt ui."}
		if action == "login" {
			p.Lines = append(p.Lines, fmt.Sprintf("The existing OAuth flow opens a browser and listens on localhost:%d.", defaultLoginPort))
		}
		return prepareUILogin(p)
	}
	path, err := config.Path()
	if err != nil {
		return p, errors.New("Cannot resolve config path")
	}
	if uiPrivateText(path) != path {
		return p, errors.New("Config path contains private credential data; preview refused")
	}
	if action == "config init" {
		if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
			return p, errors.New("Config already exists or cannot be inspected; initialization refused")
		}
		p.Lines = []string{"Create 1 new config file: " + path, "Expected version: absent (including symlinks).", "Every template key stays commented; defaults are not overwritten."}
		p.Apply = func(ctx context.Context) (string, error) {
			if err := ctx.Err(); err != nil {
				return "No config written.", err
			}
			if current, err := config.Path(); err != nil || current != path {
				return "", errors.New("Config target changed; reopen preview")
			}
			if err := createConfigFile(path); err != nil {
				return "No config replaced.", errors.New(uiPrivateText(err.Error()))
			}
			return "Created config: " + path, nil
		}
		return p, nil
	}
	if action == "notification fix" {
		if !shellAvailable() {
			return p, errors.New(noShellSentence)
		}
		prepared, unchanged, err := prepareOnEndCandidate(path, selectDoctorOnEnd(thisMachine()))
		if err != nil {
			return p, errors.New(uiPrivateText(err.Error()))
		}
		if unchanged {
			return p, errors.New("timer.on_end needs no supported repair; nothing written")
		}
		if uiPrivateText(prepared.target+prepared.backup+prepared.before.Timer.OnEnd+prepared.hint) != prepared.target+prepared.backup+prepared.before.Timer.OnEnd+prepared.hint {
			return p, errors.New("Notification preview contains private credential data; action refused")
		}
		if probeWord(notifyFirstWord(prepared.hint)) == wordDoesNotResolve {
			return p, errors.New("Selected notifier program does not resolve; nothing written")
		}
		if observeConfigWriteAt(prepared.target).refused() {
			return p, errors.New("Config write probe was refused; nothing written")
		}
		p.Lines = []string{"Change 1 setting: timer.on_end", "Target: " + prepared.target,
			fmt.Sprintf("Source SHA256: %x", sha256.Sum256(prepared.source)),
			"Old: " + prepared.before.Timer.OnEnd, "New: " + prepared.hint}
		if prepared.backup != "" {
			p.Lines = append(p.Lines, "Backup: "+prepared.backup)
		} else {
			p.Lines = append(p.Lines, "Source absent; create config without backup.")
		}
		p.Lines = append(p.Lines, "No cache/WAL, task queue or other repairs. This does not run the notifier.")
		p.Apply = func(ctx context.Context) (string, error) {
			if err := ctx.Err(); err != nil {
				return "No fix started.", err
			}
			written, backup, err := applyPreparedOnEndHint(prepared)
			text := "Written: " + written
			if backup != "" {
				text += "\nBackup retained: " + backup
			}
			if err != nil {
				return text, errors.New(uiPrivateText(err.Error()))
			}
			return text, nil
		}
		return p, nil
	}
	if action != "notifier test" {
		return p, errors.New("Unsupported settings action")
	}
	target, err := resolveConfigPath(path)
	if err != nil {
		return p, errors.New("Cannot resolve config target")
	}
	source, exists, mode, err := readOnEndSource(target)
	if err != nil {
		return p, errors.New("Cannot read config")
	}
	if !exists {
		return p, errors.New("timer.on_end is not configured")
	}
	cfg, err := config.ParseBytes(target, source)
	if err != nil {
		return p, errors.New("Config validation failed; no notifier test")
	}
	if cfg.Timer.OnEnd == "" {
		return p, errors.New("timer.on_end is not configured")
	}
	if uiPrivateText(cfg.Timer.OnEnd) != cfg.Timer.OnEnd {
		return p, errors.New("Notifier command contains private credential data; test preview refused")
	}
	guard := onEndFix{path: path, target: target, source: source, exists: exists, mode: mode}
	p.Lines = []string{"Run 1 notifier test using timer.on_end.", "Target: " + target, fmt.Sprintf("Source SHA256: %x", sha256.Sum256(source)),
		"Command: " + cfg.Timer.OnEnd, "Synthetic values: focus, Test task, Inbox, 25m, cycle 1.", "Command stdout/stderr stay private. Exit status cannot prove visual receipt."}
	p.Apply = func(ctx context.Context) (string, error) {
		if err := ctx.Err(); err != nil {
			return "No notifier started.", err
		}
		if err := verifyOnEndSource(guard); err != nil {
			return "", errors.New("Config changed; reopen notifier preview")
		}
		res, err := RunNotify(ctx, cfg.Timer.OnEnd, NotifyVars{Kind: "focus", Task: "Test task", Project: "Inbox", Duration: "25m", Cycle: "1"})
		if ctx.Err() != nil {
			return "Notifier canceled; visual receipt unknown.", ctx.Err()
		}
		if err != nil {
			return "", errors.New("Notifier could not run; output withheld")
		}
		if res.TimedOut {
			return "Notifier timed out and was killed; visual receipt unknown.", nil
		}
		if res.Killed != "" {
			return "Notifier was killed; visual receipt unknown.", nil
		}
		return fmt.Sprintf("Notifier exited with status %d; visual receipt unknown. Output withheld.", res.ExitCode), nil
	}
	return p, nil
}

type loginFileVersion struct {
	path, resolved string
	hash           [32]byte
	mode           fs.FileMode
	exists         bool
}

func readLoginVersion(path string) (loginFileVersion, error) {
	v := loginFileVersion{path: path}
	if _, err := os.Lstat(path); errors.Is(err, fs.ErrNotExist) {
		return v, nil
	}
	f, err := api.ReadRegularFile(path)
	if err != nil {
		return v, errors.New("Cannot inspect account file safely")
	}
	v.resolved, v.hash, v.mode, v.exists = f.ResolvedPath, sha256.Sum256(f.Data), f.Mode, true
	return v, nil
}

func prepareUILogin(p tui.SystemPreview) (tui.SystemPreview, error) {
	token, err := api.TokenPath()
	if err != nil {
		return p, err
	}
	credentials, err := credentialsPath()
	if err != nil {
		return p, err
	}
	paths := []string{token}
	if p.Action == "login" {
		paths = append(paths, credentials)
	}
	var versions []loginFileVersion
	for _, path := range paths {
		if uiPrivateText(path) != path {
			return p, errors.New("Account path contains private data; preview refused")
		}
		v, err := readLoginVersion(path)
		if err != nil {
			return p, err
		}
		versions = append(versions, v)
		p.Lines = append(p.Lines, "Account file: "+path)
		if v.exists {
			p.Lines = append(p.Lines, fmt.Sprintf("Expected SHA256: %x", v.hash))
		} else {
			p.Lines = append(p.Lines, "Expected version: absent")
		}
	}
	p.Lines = append(p.Lines, fmt.Sprintf("Exact account file targets: %d. Rechecked before terminal handoff.", len(versions)))
	p.Apply = func(ctx context.Context) (string, error) {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		for _, before := range versions {
			after, err := readLoginVersion(before.path)
			if err != nil || after != before {
				return "", errors.New("Account files changed; reopen login preview")
			}
		}
		return "", nil
	}
	return p, nil
}

func (s *uiSystem) Login(ctx context.Context, token bool, input, output *os.File) string {
	path, err := os.Executable()
	if err != nil {
		return "Login could not start; executable unavailable."
	}
	args := []string{"login"}
	if token {
		args = append(args, "token")
	}
	command := exec.CommandContext(ctx, path, args...)
	command.Stdin, command.Stdout, command.Stderr = input, output, output
	err = command.Run()
	if ctx.Err() != nil {
		return "Login canceled."
	}
	if err != nil {
		return "Login ended without success. Existing login diagnostics appeared in the terminal."
	}
	return "Login completed. r reloads local account status; d explicitly checks the API."
}

func createConfigFile(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp")
	if err != nil {
		return fmt.Errorf("create a temporary file in %s: %w", filepath.Dir(path), err)
	}
	defer os.Remove(f.Name())
	if _, err = f.WriteString(config.Template()); err == nil {
		err = f.Sync()
	}
	err = errors.Join(err, f.Close())
	if err != nil {
		return err
	}
	return os.Link(f.Name(), path)
}
