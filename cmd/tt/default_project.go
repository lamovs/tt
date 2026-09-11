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
	"strconv"
	"strings"

	"github.com/movsar/tt/internal/cli"
	"github.com/movsar/tt/internal/config"
	"github.com/movsar/tt/internal/model"
	"github.com/movsar/tt/internal/store"
)

func cmdDefaultProject(inv *invocation) int {
	args := inv.args[1:]
	if len(args) > 1 {
		return inv.misuse("usage: tt config default-project [id:ID]")
	}
	id := ""
	if len(args) == 1 {
		var err error
		id, _, err = config.ProjectReference(args[0])
		if err != nil || id == "" {
			return inv.misuse("expected id:ID; omit it for the cached list picker")
		}
	} else if !canAsk(inv) {
		return inv.misuse("the list picker needs terminal stdin and stderr; use tt config default-project id:ID")
	}
	if inv.interrupted() {
		return exitInterrupted
	}
	prepared, err := readDefaultProjectConfig()
	if err != nil {
		printConfigError(inv.stderr, err)
		return exitError
	}
	inspection, err := store.Inspect(inv.ctx, "")
	if errors.Is(err, store.ErrNoCache) {
		return defaultProjectFailure(inv, errors.New("no cached lists; run tt sync first"))
	}
	if err != nil {
		return defaultProjectFailure(inv, err)
	}
	defer inspection.Close()
	version, hasMeta, hasRow, err := inspection.Version(inv.ctx)
	if err != nil {
		return defaultProjectFailure(inv, err)
	}
	if !hasMeta || !hasRow || version != inspection.SchemaLatest {
		return defaultProjectFailure(inv, errors.New("cache schema is incompatible; use a compatible tt and sync before choosing a list"))
	}
	projects, err := inspection.Store.Projects(inv.ctx)
	if err != nil {
		return defaultProjectFailure(inv, err)
	}
	if id == "" {
		var offered []model.Project
		for _, p := range projects {
			if p.CreateUnavailable() == "" {
				offered = append(offered, p)
			}
		}
		if len(offered) == 0 {
			return defaultProjectFailure(inv, errors.New("no usable cached lists; run tt sync and check tt doctor"))
		}
		if _, err := fmt.Fprintln(inv.stderr, "Choose the default list from the cache (no server check):"); err != nil {
			return defaultProjectFailure(inv, err)
		}
		for n, p := range offered {
			if _, err := fmt.Fprintf(inv.stderr, "%d. %s [id:%s]\n", n+1, cli.Foreign(p.Name, 48), p.Id); err != nil {
				return defaultProjectFailure(inv, err)
			}
		}
		if _, err := fmt.Fprint(inv.stderr, "List number (empty to cancel): "); err != nil {
			return defaultProjectFailure(inv, err)
		}
		answer, err := readDefaultProjectAnswer(inv.ctx, inv.stdin)
		if inv.interrupted() {
			return exitInterrupted
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return defaultProjectFailure(inv, err)
		}
		if strings.TrimSpace(answer) == "" || errors.Is(err, io.EOF) {
			fmt.Fprintln(inv.stdout, "Canceled; config unchanged.")
			return exitOK
		}
		n, err := strconv.Atoi(strings.TrimSpace(answer))
		if err != nil || n < 1 || n > len(offered) {
			return inv.misuse("choose one of the displayed list numbers; config unchanged")
		}
		id = offered[n-1].Id
	}

	project, err := inspection.Store.Project(inv.ctx, id)
	if err != nil {
		return defaultProjectFailure(inv, errors.New("selected list is no longer cached; sync and choose again"))
	}
	if reason := project.CreateUnavailable(); reason != "" {
		return defaultProjectFailure(inv, fmt.Errorf("selected list is unusable in the cache: %s", reason))
	}
	value := "id:" + id
	if err := prepared.verify(); err != nil {
		return defaultProjectFailure(inv, err)
	}
	if prepared.config.DefaultProject == value {
		fmt.Fprintln(inv.stdout, "Default list unchanged.")
		return exitOK
	}
	candidate, err := config.SetDefaultProject(prepared.source, value)
	if err != nil {
		return defaultProjectFailure(inv, err)
	}
	if inv.interrupted() {
		return exitInterrupted
	}
	if err := os.MkdirAll(filepath.Dir(prepared.target), 0o700); err != nil {
		return defaultProjectFailure(inv, err)
	}
	backup := ""
	if prepared.exists {
		backup, err = nextBackupPath(prepared.target)
		if err == nil {
			err = writeBackupFile(backup, prepared.source, prepared.mode)
		}
		if err != nil {
			return defaultProjectFailure(inv, err)
		}
	}
	check := func() error {
		if err := inv.ctx.Err(); err != nil {
			return err
		}
		return prepared.verify()
	}
	if prepared.exists {
		err = writeConfigFileChecked(prepared.target, candidate, prepared.mode, check)
	} else {
		err = writeNewDefaultConfig(prepared.target, candidate, check)
	}
	if err != nil {
		return defaultProjectFailure(inv, fmt.Errorf("%w%s", err, backupNote(backup)))
	}
	fmt.Fprintf(inv.stdout, "Default list: %s [%s]\n", cli.Foreign(project.Name, 48), value)
	if backup != "" {
		fmt.Fprintf(inv.stdout, "Backup: %s\n", fullReportAtom(backup))
	}
	return exitOK
}

func writeNewDefaultConfig(path string, content []byte, check func() error) error {
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(content); err == nil {
		err = f.Sync()
	}
	err = errors.Join(err, f.Close())
	if err != nil {
		return err
	}
	if err := check(); err != nil {
		return err
	}
	return os.Link(f.Name(), path)
}

type defaultProjectConfig struct {
	path, target string
	source       []byte
	exists       bool
	mode         fs.FileMode
	config       config.Config
}

func readDefaultProjectConfig() (p defaultProjectConfig, err error) {
	p.path, err = config.Path()
	if err != nil {
		return p, err
	}
	p.target, err = resolveConfigPath(p.path)
	if err != nil {
		return p, err
	}
	p.source, p.exists, p.mode, err = readOnEndSource(p.target)
	if err == nil {
		p.config, err = config.ParseBytes(p.target, p.source)
	}
	return p, err
}

func (p defaultProjectConfig) verify() error {
	path, err := config.Path()
	if err != nil || path != p.path {
		return errors.New("config path changed; repeat the selection")
	}
	target, err := resolveConfigPath(path)
	if err != nil || target != p.target {
		return errors.New("config target changed; repeat the selection")
	}
	source, exists, mode, err := readOnEndSource(target)
	if err != nil || exists != p.exists || mode != p.mode || !bytes.Equal(source, p.source) {
		return errors.New("config changed during selection; repeat the selection")
	}
	return nil
}

func defaultProjectFailure(inv *invocation, err error) int {
	fmt.Fprintf(inv.stderr, "tt: config: %s\n", fullReportAtom(err.Error()))
	return exitError
}

func readDefaultProjectAnswer(ctx context.Context, input io.Reader) (string, error) {
	type result struct {
		line string
		err  error
	}
	done := make(chan result, 1)
	go func() {
		reader, ok := input.(*bufio.Reader)
		if !ok {
			reader = bufio.NewReader(io.LimitReader(input, 64))
		}
		var line strings.Builder
		for range 64 {
			b, err := reader.ReadByte()
			if err != nil {
				done <- result{line.String(), err}
				return
			}
			line.WriteByte(b)
			if b == '\n' {
				done <- result{line.String(), nil}
				return
			}
		}
		done <- result{line.String(), errors.New("selection is too long")}
	}()

	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case answer := <-done:
		return answer.line, answer.err
	}
}
