package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/movsar/tt/internal/cli"
	"github.com/movsar/tt/internal/config"
)

const backupSuffix = ".bak"

const configFilePerm = 0o600

var canAnswer = cli.IsTerminal

const fixMessageWidth = cli.Width - len("tt: ")

func fixParagraph(s string) string {
	var out []string
	for _, para := range strings.Split(s, "\n") {
		if strings.HasPrefix(para, " ") {
			out = append(out, para)
			continue
		}
		out = append(out, cli.Wrap(para, fixMessageWidth)...)
	}
	return strings.Join(out, "\n")
}

func fixValue(s string) string {
	return cli.Foreign(s, 2+quotedRuneMax*utf8.RuneCountInString(s))
}

func doctorFix(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer) int {
	return doctorFixWithWriteProbe(ctx, stdin, stdout, stderr, observeConfigWriteAt)
}

func doctorFixWithWriteProbe(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer,
	probe func(string) configWriteObservation,
) int {
	machine := thisMachine()

	if !shellAvailable() {
		fmt.Fprintf(stderr, "tt: %s\n", fixParagraph(noShellSentence))
		return exitError
	}

	path, err := config.Path()
	if err != nil {
		fmt.Fprintf(stderr, "tt: %s\n", fixParagraph(err.Error()))
		return exitError
	}
	prepared, unchanged, err := prepareOnEndCandidate(path, selectDoctorOnEnd(machine))
	if err != nil {
		if errors.Is(err, errNoReadyOnEndCandidate) {
			fmt.Fprintf(stderr, "tt: %s\n", fixParagraph(noReadyCommandNotice(machine)))
			cli.WriteLines(stderr, cli.Wrap("set timer.on_end manually; available variables: "+varList(), cli.Width))
			return exitError
		}
		if errors.Is(err, errManualOnEndRepair) {
			fmt.Fprint(stderr, "tt: timer.on_end is outside --fix's narrow repair; edit it by hand; nothing written and no question asked\n")
			return exitError
		}
		printOnEndPreparationError(stderr, err)
		return exitError
	}
	if unchanged {
		fmt.Fprintf(stdout, "timer.on_end is already %s; nothing written\n", fixValue(prepared.before.Timer.OnEnd))
		return exitOK
	}

	if probeWord(notifyFirstWord(prepared.hint)) == wordDoesNotResolve {
		if prepared.generated {
			fmt.Fprintf(stderr, "tt: %s\n", fixParagraph(readyProgramMissingWayOut(machine.system, prepared.hint)))
		} else {
			fmt.Fprintf(stderr, "tt: selected timer.on_end repair still runs %s, which does not resolve here; nothing written\n",
				fixValue(notifyFirstWord(prepared.hint)))
		}
		return exitError
	}
	write := probe(prepared.target)
	if write.refused() {
		fmt.Fprint(stderr, "tt: config write probe was refused; nothing written and no question asked\n")
		cli.WriteLines(stderr, configWriteRefusal(write.anchor, write.err, prepared.exists))
		return exitError
	}

	fmt.Fprintf(stderr, "will replace timer.on_end (detected: %s):\n", systemLabel(machine.system))
	fmt.Fprintf(stderr, "  old: %s\n", fixValue(prepared.before.Timer.OnEnd))
	fmt.Fprintf(stderr, "  new: %s\n", fixValue(prepared.hint))
	cli.WriteLines(stderr, cli.Wrap("this writes "+fullReportAtom(prepared.target), cli.Width))
	if prepared.target != path {
		cli.WriteLines(stderr, cli.Wrap(fmt.Sprintf("tt looks for the config at %s, and one of the names on the way there is a symbolic link; nothing on that path will be replaced", fullReportAtom(path)), cli.Width))
	}

	if prepared.backup != "" {
		cli.WriteLines(stderr, cli.Wrap("the config as it is now will be kept at "+fullReportAtom(prepared.backup), cli.Width))
	}

	if !canAnswer(stdin) {
		fmt.Fprint(stderr, "tt: refusing to prompt: stdin is not a terminal\n")
		return exitError
	}
	fmt.Fprint(stderr, "write it? [y/N] ")

	if !confirm(stdin) {

		fmt.Fprint(stderr, "aborted, nothing written\n")
		return exitOK
	}

	if interrupted(ctx) {
		fmt.Fprint(stderr, "interrupted, nothing written\n")
		return exitInterrupted
	}

	return finishFix(prepared, stdout, stderr)
}

func finishFix(prepared onEndFix, stdout, stderr io.Writer) int {
	written, backup, err := applyPreparedOnEndHint(prepared)
	if err != nil {
		fmt.Fprintf(stderr, "tt: %s\n", fullReportAtom(err.Error()))
		return exitError
	}

	fmt.Fprintf(stdout, "wrote %s\n", fullReportAtom(written))
	if written != prepared.path {
		cli.WriteLines(stdout, cli.Wrap(fmt.Sprintf("tt looks for the config at %s, and one of the names on the way there is a symbolic link; nothing on that path was replaced", fullReportAtom(prepared.path)), cli.Width))
	}

	if backup != "" {
		cli.WriteLines(stdout, cli.Wrap("the config as it was before is kept at "+fullReportAtom(backup), cli.Width))
	}
	return exitOK
}

func confirm(r io.Reader) bool {
	line, _ := bufio.NewReader(r).ReadString('\n')
	line = strings.ToLower(strings.TrimSpace(line))
	return line == "y" || line == "yes"
}

func resolveConfigPath(path string) (string, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err == nil {
		return resolved, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("resolve %s: %w", path, err)
	}

	info, lerr := os.Lstat(path)
	switch {
	case errors.Is(lerr, fs.ErrNotExist):

		dir, base := rawSplit(path)
		resolved, derr := resolveDirPrefix(dir)
		if derr != nil {
			return "", fmt.Errorf("resolve %s: %w", dir, derr)
		}
		return filepath.Join(resolved, base), nil
	case lerr != nil:
		return "", fmt.Errorf("resolve %s: %w", path, lerr)
	case info.Mode()&os.ModeSymlink == 0:

		return "", fmt.Errorf("resolve %s: %w", path, err)
	}

	const maxLinkHops = 32
	target := path
	for hops := 0; ; hops++ {
		if hops == maxLinkHops {
			return "", fmt.Errorf("resolve %s: more than %d symbolic links deep, which is a loop or as good as one", path, maxLinkHops)
		}
		next, rerr := os.Readlink(target)
		if rerr != nil {
			if hops == 0 {

				return "", fmt.Errorf("resolve %s: %w", path, rerr)
			}

			break
		}
		if !filepath.IsAbs(next) {

			dir, _ := rawSplit(target)
			resolved, derr := resolveDirPrefix(dir)
			if derr != nil {
				return "", fmt.Errorf("resolve %s: %w", dir, derr)
			}
			next = filepath.Join(resolved, next)
		}
		target = next
	}

	dir, base := rawSplit(target)
	resolved, derr := resolveDirPrefix(dir)
	if derr != nil {
		return "", fmt.Errorf("resolve %s: %w", dir, derr)
	}
	return filepath.Join(resolved, base), nil
}

func rawSplit(p string) (dir, base string) {
	dir, base = filepath.Split(p)
	switch {
	case dir == "":
		return ".", base
	case dir == string(filepath.Separator):
		return dir, base
	}
	trimmed := strings.TrimRight(dir, string(filepath.Separator))
	if trimmed == "" {
		return string(filepath.Separator), base
	}
	return trimmed, base
}

func resolveDirPrefix(dir string) (string, error) {
	rest := ""
	for {
		resolved, err := filepath.EvalSymlinks(dir)
		if err == nil {
			if rest == "" {
				return resolved, nil
			}
			return filepath.Join(resolved, rest), nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		parent, last := rawSplit(dir)
		if last == "" {

			return "", err
		}
		if rest == "" {
			rest = last
		} else {
			rest = filepath.Join(last, rest)
		}
		dir = parent
	}
}

type onEndFix struct {
	path      string
	target    string
	source    []byte
	exists    bool
	mode      fs.FileMode
	before    config.Config
	candidate []byte
	backup    string
	hint      string
	generated bool
}

type onEndCandidate struct {
	command   string
	generated bool
}

type onEndCandidateSelector func(config.Config) (onEndCandidate, error)

var errProposedConfigInvalid = errors.New("proposed replacement rejected")

func printOnEndPreparationError(w io.Writer, err error) {
	var configErr *config.Error
	if errors.As(err, &configErr) {
		if errors.Is(err, errProposedConfigInvalid) {
			fmt.Fprint(w, "tt: proposed replacement rejected; original config unchanged\n")
		}
		printConfigError(w, configErr)
		return
	}
	fmt.Fprintf(w, "tt: %s\n", fullReportAtom(err.Error()))
}

func prepareOnEndHint(path, hint string) (onEndFix, bool, error) {
	return prepareOnEndCandidate(path, func(config.Config) (onEndCandidate, error) {
		return onEndCandidate{command: hint}, nil
	})
}

func prepareOnEndCandidate(path string, selectCandidate onEndCandidateSelector) (onEndFix, bool, error) {
	target, err := resolveConfigPath(path)
	if err != nil {
		return onEndFix{}, false, err
	}
	p := onEndFix{path: path, target: target, mode: configFilePerm}
	p.source, p.exists, p.mode, err = readOnEndSource(target)
	if err != nil {
		return onEndFix{}, false, err
	}

	base := []byte(config.Template())
	p.before = config.Default()
	if p.exists {
		base = p.source
		p.before, err = config.ParseBytes(target, p.source)
		if err != nil {
			return onEndFix{}, false, err
		}
	}
	selected, err := selectCandidate(p.before)
	if err != nil {
		return onEndFix{}, false, err
	}
	p.hint = selected.command
	p.generated = selected.generated
	hint := p.hint
	if p.before.Timer.OnEnd == hint {
		if err := verifyOnEndSource(p); err != nil {
			return onEndFix{}, false, err
		}
		return p, true, nil
	}
	if dotted := config.DottedTimerKeys(p.source); len(dotted) > 0 {
		return onEndFix{}, false, dottedTimerRefusal(target, dotted)
	}

	p.candidate = setTimerOnEnd(base, hint)
	after, err := config.ParseBytes(target, p.candidate)
	if err != nil {
		return onEndFix{}, false, fmt.Errorf("%w: %w", errProposedConfigInvalid, err)
	}
	if err := checkOnEndEdit(after, &p.before, hint); err != nil {
		return onEndFix{}, false, fmt.Errorf("the proposed edit was refused: %w", err)
	}
	if p.exists {
		p.backup, err = nextBackupPath(target)
		if err != nil {
			return onEndFix{}, false, err
		}
	}
	if err := verifyOnEndSource(p); err != nil {
		return onEndFix{}, false, err
	}
	return p, false, nil
}

var (
	errNoReadyOnEndCandidate = errors.New("no generated timer.on_end command is available")
	errManualOnEndRepair     = errors.New("timer.on_end needs a manual edit")
)

func manualOnEndRepair(string) error {
	return errManualOnEndRepair
}

func repairOnEndCommand(command string) (string, bool, error) {
	if command == "" || isReadyCommand(command) {
		return command, false, nil
	}
	substituted, spans := applyBraceTemplateSpans(command)
	hasRawReference := hasNotifyEnvironmentReference(command)
	if hasRawReference {
		return "", false, manualOnEndRepair("the command contains a raw notifier variable")
	}
	if len(spans) == 0 {
		if passesValueWithOptionsOpen(command) {
			return "", false, manualOnEndRepair("the command is outside the supported brace-placeholder repair")
		}
		return command, false, nil
	}

	originalWords, ok := oneCompleteSimpleCommand(command)
	if !ok || isShellScriptCommand(originalWords) {
		return "", false, manualOnEndRepair("the command is not one supported simple command")
	}
	substitutedCommands, ok := splitShellWords(substituted)
	if !ok || len(substitutedCommands) != 1 {
		return "", false, manualOnEndRepair("the substituted command cannot be proved to be one simple command")
	}

	type quoteRemoval struct{ from, to int }
	var removals []quoteRemoval
	beforeBare, beforeSingle := quotedPlaceholders(command)
	beforeOpen := passesValueWithOptionsOpen(command)
	for _, span := range spans {
		words, at, q, found := spanLanding(substitutedCommands, span)
		if !found || at == 0 {
			continue
		}
		original, found := wordContainingSource(originalWords, span.sourceFrom, span.sourceTo)
		exactQuoted := found && original.from+1 == span.sourceFrom && original.to-1 == span.sourceTo &&
			original.to-original.from == len(span.match)+2 &&
			(command[original.from] == '"' || command[original.from] == '\'') &&
			command[original.to-1] == command[original.from]

		problem := q == quoteNone || q == quoteSingle && !looksLikeOption(words[at-1])
		if !problem {
			if exactQuoted && !knownPositionalArgument(words, at) {
				return "", false, manualOnEndRepair("a quoted placeholder follows an option of unknown arity")
			}
			continue
		}
		if !exactQuoted {
			return "", false, manualOnEndRepair("a faulty placeholder is embedded or concatenated")
		}
		if !knownPositionalArgument(words, at) {
			return "", false, manualOnEndRepair("a quoted placeholder follows an option of unknown arity")
		}
		removals = append(removals, quoteRemoval{from: original.from, to: original.to})
	}
	if len(removals) == 0 {
		if hasRawReference || beforeOpen || len(beforeBare) > 0 || len(beforeSingle) > 0 {
			return "", false, manualOnEndRepair("the command has no supported whole-word quote repair")
		}
		return command, false, nil
	}

	var candidate strings.Builder
	last := 0
	for _, removal := range removals {
		candidate.WriteString(command[last:removal.from])
		candidate.WriteString(command[removal.from+1 : removal.to-1])
		last = removal.to
	}
	candidate.WriteString(command[last:])
	repaired := candidate.String()

	afterBare, afterSingle := quotedPlaceholders(repaired)
	if len(afterBare) != 0 || len(afterSingle) != 0 {
		return "", false, manualOnEndRepair("the selected quote repair leaves a quoted-placeholder finding")
	}
	if !beforeOpen && passesValueWithOptionsOpen(repaired) {
		return "", false, manualOnEndRepair("the selected quote repair adds an open option-list finding")
	}
	if _, ok := oneCompleteSimpleCommand(repaired); !ok {
		return "", false, manualOnEndRepair("the selected quote repair is not one supported simple command")
	}
	return repaired, true, nil
}

func oneCompleteSimpleCommand(command string) ([]word, bool) {
	if strings.Contains(command, "#") {
		return nil, false
	}
	commands, ok := splitShellWords(command)
	if !ok || len(commands) != 1 || len(commands[0].words) == 0 {
		return nil, false
	}
	words := commands[0].words
	covered := make([]bool, len(command))
	for _, word := range words {
		for i := word.from; i < word.to; i++ {
			covered[i] = true
		}
	}
	for i := range len(command) {
		if command[i] != ' ' && command[i] != '\t' && !covered[i] {
			return nil, false
		}
	}
	return words, true
}

func wordContainingSource(words []word, from, to int) (word, bool) {
	for _, word := range words {
		if word.from <= from && to <= word.to {
			return word, true
		}
	}
	return word{}, false
}

func knownPositionalArgument(words []word, at int) bool {
	if at <= 0 {
		return false
	}
	for _, word := range words[1:at] {
		if word.value == "--" {
			return true
		}
		if looksLikeOption(word) {
			return false
		}
	}
	return true
}

func isShellScriptCommand(words []word) bool {
	for i, candidate := range words {
		program := filepath.Base(candidate.value)
		switch program {
		case "sh", "bash", "dash", "ksh", "zsh":
		default:
			continue
		}
		for _, argument := range words[i+1:] {
			value := argument.value
			if value == "--" {
				break
			}
			if value == "--command" || strings.HasPrefix(value, "--command=") {
				return true
			}
			if len(value) > 1 && value[0] == '-' && value[1] != '-' && strings.Contains(value[1:], "c") {
				return true
			}
		}
	}
	return false
}

func hasNotifyEnvironmentReference(command string) bool {
	for _, name := range notifyVarNames {
		if strings.Contains(command, "$"+name) || strings.Contains(command, "${"+name+"}") {
			return true
		}
	}
	return false
}

func readOnEndSource(target string) ([]byte, bool, fs.FileMode, error) {
	src, err := os.ReadFile(target)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, configFilePerm, nil
	}
	if err != nil {
		return nil, false, 0, fmt.Errorf("read %s: %w", target, err)
	}
	info, err := os.Stat(target)
	if err != nil {
		return nil, false, 0, fmt.Errorf("stat %s: %w", target, err)
	}
	return src, true, info.Mode(), nil
}

func nextBackupPath(target string) (string, error) {
	for i := 0; ; i++ {
		candidate := target + backupSuffix
		if i > 0 {
			candidate = fmt.Sprintf("%s.%d", candidate, i)
		}
		_, err := os.Lstat(candidate)
		if errors.Is(err, fs.ErrNotExist) {
			return candidate, nil
		}
		if err != nil {
			return "", fmt.Errorf("inspect backup path %s: %w", candidate, err)
		}
	}
}

func verifyOnEndSource(p onEndFix) error {
	current, err := resolveConfigPath(p.path)
	if err != nil {
		return fmt.Errorf("config changed since it was checked: %w; %s again", err, runFixHint)
	}
	if current != p.target {
		return fmt.Errorf("config target changed since it was checked; %s again", runFixHint)
	}
	src, exists, mode, err := readOnEndSource(p.target)
	if err != nil {
		return fmt.Errorf("config changed since it was checked: %w; %s again", err, runFixHint)
	}
	if exists != p.exists || mode != p.mode || !bytes.Equal(src, p.source) {
		return fmt.Errorf("config contents, existence, or mode changed since it was checked; %s again", runFixHint)
	}
	return nil
}

func applyOnEndHint(path, hint string) (written, backup string, err error) {
	p, unchanged, err := prepareOnEndHint(path, hint)
	if err != nil {
		return "", "", err
	}
	if unchanged {
		return "", "", nil
	}
	return applyPreparedOnEndHint(p)
}

func applyPreparedOnEndHint(p onEndFix) (written, backup string, err error) {
	return applyPreparedOnEndHintBeforePublish(p, nil)
}

func applyPreparedOnEndHintBeforePublish(p onEndFix, beforePublish func()) (written, backup string, err error) {
	if err := verifyOnEndSource(p); err != nil {
		return "", "", err
	}
	if !p.exists {
		if err := os.MkdirAll(filepath.Dir(p.target), 0o700); err != nil {
			return "", "", fmt.Errorf("create %s: %w", filepath.Dir(p.target), err)
		}
	}
	if p.backup != "" {
		if err := writeBackupFile(p.backup, p.source, p.mode); err != nil {
			return "", "", fmt.Errorf("create promised backup %s: %w", p.backup, err)
		}
		backup = p.backup
	}
	if err := writeConfigFileChecked(p.target, p.candidate, p.mode, func() error {
		if beforePublish != nil {
			beforePublish()
		}
		return verifyOnEndSource(p)
	}); err != nil {
		return "", backup, fmt.Errorf("%w; the config was left as it was%s", err, backupNote(backup))
	}
	return p.target, backup, nil
}

func writeBackupFile(path string, content []byte, mode fs.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	incomplete := func(cause error) error {
		f.Close()
		return fmt.Errorf("%w; the reserved backup at %s may be incomplete and was left in place", cause, path)
	}
	if err := f.Chmod(mode); err != nil {
		return incomplete(err)
	}
	if _, err := f.Write(content); err != nil {
		return incomplete(err)
	}
	if err := f.Sync(); err != nil {
		return incomplete(err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("%w; the reserved backup at %s may be incomplete and was left in place", err, path)
	}
	return nil
}

func checkOnEndEdit(after config.Config, before *config.Config, hint string) error {
	if after.Timer.OnEnd != hint {

		return fmt.Errorf("timer.on_end would read %s in the proposed config, not the command it was given",
			cli.ReportTitle(after.Timer.OnEnd, fixMessageWidth))
	}
	if before == nil {
		return nil
	}
	want := *before
	want.Timer.OnEnd = hint
	if after != want {
		return errors.New("the edit changed a setting other than timer.on_end")
	}
	return nil
}

func backupNote(backup string) string {
	if backup == "" {
		return ""
	}
	return fmt.Sprintf(" (a copy of it is at %s)", backup)
}

func writeConfigFileChecked(path string, content []byte, mode fs.FileMode, beforeRename func() error) error {
	dir := filepath.Dir(path)
	tmpPrefix := filepath.Base(path) + ".tmp"

	sweepWriteTemps(dir, tmpPrefix)

	f, err := os.CreateTemp(dir, tmpPrefix)
	if err != nil {
		return fmt.Errorf("create a temporary file in %s: %w", dir, err)
	}
	tmp := f.Name()
	renamed := false
	defer func() {
		if renamed {
			return
		}
		f.Close()
		os.Remove(tmp)
	}()

	if err := f.Chmod(mode); err != nil {
		return fmt.Errorf("chmod %s: %w", tmp, err)
	}
	if _, err := f.Write(content); err != nil {
		return fmt.Errorf("write %s: %w", tmp, err)
	}

	if err := f.Sync(); err != nil {
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if beforeRename != nil {
		if err := beforeRename(); err != nil {
			return err
		}
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	renamed = true
	return nil
}

const timerHeader = "[" + config.TimerTable + "]"

const runFixHint = `run "tt doctor --fix"`

const (
	dottedTimerByHand  = "the command can be set by hand where those keys already are, at the root:"
	dottedTimerSetting = config.TimerTable + ".on_end = ..."
	dottedTimerByFix   = "or those settings can move under a " + timerHeader + " header, which is the shape --fix writes into; then:"
)

func dottedTimerRefusal(path string, dotted []string) error {
	return fmt.Errorf("%s writes %s at the root rather than under a %s header, and --fix writes on_end under such a header and nowhere else;\n"+
		"adding one to this file would make it define the table both ways, which is invalid TOML 1.0 - tt's own parser happens to accept such a file and other readers do not.\n"+
		"Nothing was written; %s\n  %s\n%s\n  %s",
		path, strings.Join(dotted, ", "), timerHeader, dottedTimerByHand, dottedTimerSetting, dottedTimerByFix, runFixHint)
}

func setTimerOnEnd(src []byte, hint string) []byte {
	line := "on_end = " + config.QuoteTOMLString(hint)
	lines := strings.Split(string(src), "\n")

	headerAt := -1
	for i, l := range lines {
		if config.IsTimerHeader(normalizeTOMLLine(l)) {
			headerAt = i
			break
		}
	}
	if headerAt < 0 {
		if len(lines) > 0 && lines[len(lines)-1] == "" {
			lines = lines[:len(lines)-1]
		}
		lines = append(lines, "", timerHeader, line)
		return []byte(strings.Join(lines, "\n") + "\n")
	}
	if strings.HasPrefix(strings.TrimSpace(lines[headerAt]), "#") {
		lines[headerAt] = timerHeader
	}

	sectionEnd := len(lines)
	for i := headerAt + 1; i < len(lines); i++ {
		if strings.HasPrefix(normalizeTOMLLine(lines[i]), "[") {
			sectionEnd = i
			break
		}
	}
	for i := headerAt + 1; i < sectionEnd; i++ {
		if isKeyAssignment(normalizeTOMLLine(lines[i]), "on_end") {
			lines[i] = line
			return []byte(strings.Join(lines, "\n"))
		}
	}

	out := make([]string, 0, len(lines)+1)
	out = append(out, lines[:headerAt+1]...)
	out = append(out, line)
	out = append(out, lines[headerAt+1:]...)
	return []byte(strings.Join(out, "\n"))
}

func normalizeTOMLLine(l string) string {
	return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(l), "#"))
}

func isKeyAssignment(trimmed, name string) bool {
	rest := strings.TrimPrefix(trimmed, name)
	if rest == trimmed {
		return false
	}
	return strings.HasPrefix(strings.TrimLeft(rest, " \t"), "=")
}

func selectDoctorOnEnd(machine notifyMachine) onEndCandidateSelector {
	return func(cfg config.Config) (onEndCandidate, error) {
		if cfg.Timer.OnEnd == "" {
			hint, ok := notifyHints[machine.system]
			if !ok {
				return onEndCandidate{}, errNoReadyOnEndCandidate
			}
			return onEndCandidate{command: hint, generated: true}, nil
		}
		repaired, _, repairErr := repairOnEndCommand(cfg.Timer.OnEnd)
		if repairErr != nil {
			return onEndCandidate{}, repairErr
		}
		return onEndCandidate{command: repaired}, nil
	}
}
