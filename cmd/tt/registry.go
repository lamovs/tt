package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/movsar/tt/internal/cli"
	"github.com/movsar/tt/internal/config"
	"github.com/movsar/tt/internal/model"
	"github.com/movsar/tt/internal/store"
)

type command struct {
	help cli.Help

	run func(inv *invocation) int
}

var commands = map[string]command{}

func register(c command) {
	if c.help.Verb == "" {
		panic("tt: a command was registered with no verb")
	}
	if _, dup := commands[c.help.Verb]; dup {
		panic("tt: two commands registered as " + c.help.Verb)
	}
	commands[c.help.Verb] = c
}

type invocation struct {
	ctx    context.Context
	verb   string
	stdin  io.Reader
	stdout io.Writer
	stderr io.Writer

	args        []string
	data        []string
	refinements []string

	color           config.ColorMode
	colorKnown      bool
	jsonOutput      bool
	rawOutput       bool
	resultWriter    io.Writer
	resultWritten   bool
	resultExit      int
	resultError     error
	resultData      any
	resultChanged   bool
	resultSource    string
	resultWarnings  []string
	mutationResults []taskCommandOutcome

	cfg       config.Config
	cfgErr    error
	cfgLoaded bool
}

func (inv *invocation) config() (config.Config, error) {
	if !inv.cfgLoaded {
		inv.cfgLoaded = true

		if cfg, err := config.Load(); err != nil {
			inv.cfg, inv.cfgErr = config.Default(), err
		} else {
			inv.cfg = cfg
		}
	}
	return inv.cfg, inv.cfgErr
}

func (inv *invocation) colorMode() config.ColorMode {
	if !inv.colorKnown {
		inv.color, inv.colorKnown = config.ColorAuto, true
		if cfg, err := inv.config(); err == nil {
			inv.color = cfg.Color
		}
	}
	return inv.color
}

func (inv *invocation) outPalette() cli.Palette { return cli.PaletteFor(inv.colorMode(), inv.stdout) }
func (inv *invocation) errPalette() cli.Palette { return cli.PaletteFor(inv.colorMode(), inv.stderr) }

func (inv *invocation) fail(err error) int {
	if inv.jsonOutput {
		return inv.jsonFailure("operation_failed", err.Error(), exitError)
	}
	if writeAuthorizationAdvice(inv.stderr, "tt: "+inv.verb+": ", err) {
		return exitError
	}
	fmt.Fprintf(inv.stderr, "tt: %s: %v\n", inv.verb, err)
	return exitError
}

func (inv *invocation) misuse(format string, a ...any) int {
	if inv.jsonOutput {
		return inv.jsonFailure("usage", fmt.Sprintf(format, a...), exitUsage)
	}
	fmt.Fprintf(inv.stderr, "tt: %s: %s\n", inv.verb, fmt.Sprintf(format, a...))
	fmt.Fprintf(inv.stderr, "run \"tt help %s\" for examples\n", inv.verb)
	return exitUsage
}

func (inv *invocation) misuseWord(before, word string) int {
	prefix := "tt: " + inv.verb + ": "
	return inv.misuse("%s", strings.TrimPrefix(quoteWord(prefix+before, word), prefix))
}

func quoteWord(before, word string) string {
	return before + cli.ReportTitle(word, cli.Width-cli.DisplayWidth(before))
}

func (inv *invocation) openStore() (*store.Store, error) {
	return store.Open(inv.ctx, "")
}

func (inv *invocation) interrupted() bool { return interrupted(inv.ctx) }

func (inv *invocation) listTasks(st *store.Store, tasks []model.Task) error {
	return inv.listTasksAt(st, tasks, time.Now())
}

func (inv *invocation) listTasksAt(st *store.Store, tasks []model.Task, now time.Time) error {
	if inv.jsonOutput {
		if tasks == nil {
			tasks = []model.Task{}
		}
		inv.resultData = tasks
		return nil
	}
	names, err := cli.ProjectNames(inv.ctx, st)
	if err != nil {
		return err
	}
	if err := st.SetListing(inv.ctx, cli.TaskIDs(tasks)); err != nil {
		return err
	}
	return cli.WriteLines(inv.stdout, cli.ListLines(cli.Rows(tasks, names), inv.outPalette(), now))
}

var canAsk = func(inv *invocation) bool {
	return !inv.jsonOutput && askable(cli.IsTerminal(inv.stdin), cli.IsTerminal(inv.stderr))
}

func askable(stdinIsTTY, stderrIsTTY bool) bool {
	return stdinIsTTY && stderrIsTTY
}

func (inv *invocation) resolver(st *store.Store) cli.Resolver {
	return cli.Resolver{
		Store:   st,
		Stdin:   inv.stdin,
		Stderr:  inv.stderr,
		Stdout:  inv.stdout,
		Palette: inv.errPalette(),
		Ask:     !inv.jsonOutput && canAsk(inv),
	}
}

const noTaskGiven = "name a task: a number from the last listing, a range like 1-3, or some of its title"

func noTaskGivenFor(verb string) string {
	return strings.Join(cli.Wrap(noTaskGiven, cli.Width-len("tt: "+verb+": ")), "\n")
}

const oneTaskOnly = "takes one task at a time, and that reference names %d"

func (inv *invocation) resolveTasks(st *store.Store, args []string, status store.StatusFilter) (ids []string, code int, ok bool) {
	return inv.taskOutcome(inv.resolver(st).Tasks(inv.ctx, args, status))
}

func (inv *invocation) taskOutcome(ids []string, err error) ([]string, int, bool) {
	switch {
	case err == nil:
		return ids, exitOK, true
	case errors.Is(err, cli.ErrCancelled):

		return nil, exitOK, false
	case errors.Is(err, cli.ErrNoReference):
		return nil, inv.misuse("%s", noTaskGivenFor(inv.verb)), false
	case inv.interrupted():

		return nil, exitInterrupted, false
	default:
		return nil, inv.fail(err), false
	}
}

func (inv *invocation) resolveTasksAndValue(st *store.Store, args []string, status store.StatusFilter,
	what string, parse cli.ValueParser) (ids []string, value string, code int, ok bool) {
	ref, value, err := cli.SplitValue(args, what, parse)
	if err != nil {

		return nil, "", inv.misuse("%v", err), false
	}
	ids, code, ok = inv.resolveTasks(st, ref, status)
	return ids, value, code, ok
}

const (
	indexIntro  = "tt: TickTick in the terminal"
	indexFooter = `Run "tt help <command>" for examples. Start with "tt ui", or try "tt add Call --in 2h" followed by "tt sync".`
)

var indexOrder = []struct {
	title string
	verbs []string
}{
	{"Everyday tasks", []string{"ui", "add", "batch", "ls", "s", "today", "show", "edit", "item", "repeat", "remind", "done", "due", "schedule", "pri", "mv", "rm", "undo", "ai"}},
	{"Focus and time", []string{"timer", "pomodoro"}},
	{"Projects and resources", []string{"project", "folder", "tag", "habit", "comment", "countdown"}},
	{"Setup and maintenance", []string{"setup", "login", "auth", "sync", "auto", "doctor", "config", "notify", "help", "version"}},
}

func indexGroups() []cli.IndexGroup {
	placed := map[string]bool{}
	groups := make([]cli.IndexGroup, 0, len(indexOrder)+1)
	for _, g := range indexOrder {
		group := cli.IndexGroup{Title: g.title}
		for _, verb := range g.verbs {
			c, ok := commands[verb]
			if !ok {
				continue
			}
			placed[verb] = true
			group.Verbs = append(group.Verbs, c.help)
		}
		groups = append(groups, group)
	}

	var rest []string
	for verb := range commands {
		if !placed[verb] {
			rest = append(rest, verb)
		}
	}
	if len(rest) > 0 {

		slices.Sort(rest)
		group := cli.IndexGroup{Title: "Other"}
		for _, verb := range rest {
			group.Verbs = append(group.Verbs, commands[verb].help)
		}
		groups = append(groups, group)
	}
	return groups
}

func writeIndex(w io.Writer, p cli.Palette) error {
	return cli.WriteLines(w, cli.RenderIndex(indexIntro, indexGroups(), indexFooter, p))
}

func init() {
	register(command{
		help: cli.Help{
			Verb:    "help",
			Summary: "show what a command does, with examples",
			Examples: []cli.Example{
				{Cmd: "tt help", What: "list every command"},
				{Cmd: "tt help due", What: "the examples for one command"},
				{Cmd: "tt due --help", What: "the same thing, from where you are"},
			},
		},
		run: cmdHelp,
	})
}

func cmdHelp(inv *invocation) int {
	if len(inv.refinements) > 0 {
		return inv.misuseWord("unknown option ", inv.refinements[0])
	}
	switch len(inv.data) {
	case 0:
		if err := writeIndex(inv.stdout, inv.outPalette()); err != nil {
			return inv.fail(err)
		}
		return exitOK
	case 1:
		c, ok := commands[inv.data[0]]
		if !ok {
			return inv.misuseWord("no command called ", inv.data[0])
		}
		if err := cli.WriteLines(inv.stdout, c.help.Render(inv.outPalette())); err != nil {
			return inv.fail(err)
		}
		return exitOK
	default:
		return inv.misuse("one command at a time")
	}
}

const colorOption = "--color"

func takeColor(args []string) (rest []string, mode config.ColorMode, given, beforeMarker bool, err error) {
	_, refinements := splitArgs(args)

	rest = append(rest, args[:len(args)-len(refinements)]...)
	for i := 0; i < len(refinements); i++ {
		if refinements[i] == endOfOptions {

			beforeMarker = given
			rest = append(rest, refinements[i:]...)
			break
		}
		name, value, hasValue := strings.Cut(refinements[i], "=")
		if name != colorOption {
			rest = append(rest, refinements[i])
			continue
		}
		if !hasValue {
			if i+1 >= len(refinements) {
				return nil, "", false, false, fmt.Errorf("%s needs a value (auto, always or never)", colorOption)
			}
			i++
			value = refinements[i]
		}
		m, perr := config.ParseColorMode(value)
		if perr != nil {
			return nil, "", false, false, fmt.Errorf("%s: %w", colorOption, perr)
		}
		mode, given = m, true
	}
	return rest, mode, given, beforeMarker, nil
}

var helpOptions = []string{"--help", "-h"}

func wantsHelp(refinements []string) bool {
	for _, r := range refinements {
		if slices.Contains(helpOptions, r) {
			return true
		}
	}
	return false
}
