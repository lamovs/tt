package editor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

const MaxSize = 4 << 20

type Draft struct {
	Path    string
	Data    []byte
	Changed bool
	dir     string
}

func (d *Draft) Discard() error { return os.RemoveAll(d.dir) }

func Open(ctx context.Context, initial []byte, stdin io.Reader, stdout, stderr io.Writer) (*Draft, error) {
	if len(initial) > MaxSize {
		return nil, errors.New("editor document exceeds the 4 MiB limit")
	}
	command := os.Getenv("VISUAL")
	if strings.TrimSpace(command) == "" {
		command = os.Getenv("EDITOR")
	}
	args, err := Command(command)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp("", "tt-edit-")
	if err != nil {
		return nil, fmt.Errorf("create editor directory: %w", err)
	}
	d := &Draft{Path: filepath.Join(dir, "task.md"), dir: dir}
	if err := os.WriteFile(d.Path, initial, 0600); err != nil {
		_ = d.Discard()
		return nil, fmt.Errorf("write editor document: %w", err)
	}
	cmd := exec.CommandContext(ctx, args[0], append(args[1:], d.Path)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
	if err := cmd.Run(); err != nil {
		return d, fmt.Errorf("editor failed: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return d, err
	}
	d.Data, err = readDraft(d.Path)
	if err != nil {
		return d, err
	}
	d.Changed = !bytes.Equal(initial, d.Data)
	return d, nil
}

func readDraft(path string) ([]byte, error) {

	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open saved editor document: %w", err)
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("inspect saved editor document: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("saved editor document must be a regular file")
	}
	if info.Size() > MaxSize {
		return nil, errors.New("saved editor document exceeds the 4 MiB limit")
	}
	if err := f.Chmod(0600); err != nil {
		return nil, fmt.Errorf("protect saved editor document: %w", err)
	}
	data, err := io.ReadAll(io.LimitReader(f, MaxSize+1))
	if err != nil {
		return nil, fmt.Errorf("read saved editor document: %w", err)
	}
	if len(data) > MaxSize {
		return nil, errors.New("saved editor document exceeds the 4 MiB limit")
	}
	if !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
		return nil, errors.New("saved editor document must be UTF-8 text without NUL bytes")
	}
	return data, nil
}

func Command(value string) ([]string, error) {
	if !utf8.ValidString(value) || strings.ContainsFunc(value, func(r rune) bool { return unicode.IsControl(r) && r != '\t' }) {
		return nil, errors.New("VISUAL or EDITOR contains invalid control characters")
	}
	var args []string
	var word strings.Builder
	var quote rune
	escaped, started := false, false
	for _, r := range value {
		if escaped {
			word.WriteRune(r)
			escaped = false
			started = true
			continue
		}
		if r == '\\' && quote != '\'' {
			escaped = true
			started = true
			continue
		}
		if quote != 0 {
			if r == quote {
				quote = 0
			} else {
				word.WriteRune(r)
			}
			continue
		}
		if r == '\'' || r == '"' {
			quote = r
			started = true
			continue
		}
		if unicode.IsSpace(r) {
			if started {
				args = append(args, word.String())
				word.Reset()
				started = false
			}
			continue
		}
		word.WriteRune(r)
		started = true
	}
	if escaped || quote != 0 {
		return nil, errors.New("VISUAL or EDITOR has an unfinished quote or escape")
	}
	if started {
		args = append(args, word.String())
	}
	if len(args) == 0 || args[0] == "" {
		return nil, errors.New("set VISUAL or EDITOR to an editor command (for example: vi or code --wait)")
	}
	return args, nil
}
