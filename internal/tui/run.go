package tui

import (
	"context"
	"errors"
	"fmt"
	"os"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/term"
)

var ErrInterrupted = errors.New("interrupted")

type Options struct {
	ServerTasks    ServerTaskActions
	Resources      ResourceActions
	System         SystemActions
	Timers         TimerActions
	Actions        Actions
	DefaultProject string
	gate           *commandGate
	drafts         *draftJournal
	Notice         string
	Err            error
	Color          bool
}

func Run(ctx context.Context, input, output *os.File, queries Queries, opts Options) (err error) {
	if input == nil || output == nil || !term.IsTerminal(input.Fd()) || !term.IsTerminal(output.Fd()) {
		return errors.New("tt ui needs an interactive terminal on stdin and stdout; use tt ls for piped output")
	}
	inState, err := term.GetState(input.Fd())
	if err != nil {
		return fmt.Errorf("save terminal input: %w", err)
	}
	outState, err := term.GetState(output.Fd())
	if err != nil {
		return fmt.Errorf("save terminal output: %w", err)
	}
	opts.drafts = &draftJournal{}

	defer func() {
		err = errors.Join(err, term.Restore(input.Fd(), inState), term.Restore(output.Fd(), outState))
		opts.drafts.write(output)
	}()
	ctx, cancel := context.WithCancel(ctx)
	opts.gate = &commandGate{}
	defer opts.gate.close(cancel)
	programOpts := []tea.ProgramOption{
		tea.WithInput(input), tea.WithOutput(output), tea.WithoutSignalHandler(),
	}
	m := newModel(ctx, queries, opts)
	for {
		m, err = runTerminalSession(ctx, m, programOpts)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			return err
		}
		if m.login != nil {
			handoff := m.login

			m.gate.running.Wait()
			result := "Login refused: account files changed since the preview. Reopen it."
			if _, checkErr := handoff.check(ctx); checkErr == nil {
				result = m.system.Login(ctx, handoff.token, input, output)
			}
			m.login = nil
			m.systemPanel.preview = nil
			m.systemPanel.lines = []string{result}
			m.systemPanel.offset = 0
			m.timerEnsured = ""
			if ctx.Err() != nil {
				return ctx.Err()
			}
			m, _ = m.load()
			continue
		}
		if m.editor == nil {
			break
		}

		command := m.editor
		command.input, command.output, command.diagnostic = input, output, os.Stderr
		_ = command.Run()
		m, _ = m.finishEditor(command.finished)
		if ctx.Err() != nil {
			return ctx.Err()
		}

	}
	if m.interrupted {
		return ErrInterrupted
	}
	return m.err
}

func runTerminalSession(ctx context.Context, m browserModel, options []tea.ProgramOption) (browserModel, error) {
	options = append(append([]tea.ProgramOption(nil), options...), tea.WithContext(context.WithoutCancel(ctx)))
	p := tea.NewProgram(m, options...)
	defer p.Kill()
	finished := make(chan struct{})
	defer close(finished)
	go func() {
		select {
		case <-ctx.Done():
			p.Quit()
		case <-finished:
		}
	}()
	final, err := p.Run()
	if err != nil {
		return m, err
	}
	return final.(browserModel), nil
}
