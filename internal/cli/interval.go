package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/movsar/tt/internal/store"
	"github.com/muesli/cancelreader"
)

func ReviewInterval(ctx context.Context, p store.IntervalPreview, input io.Reader, output io.Writer, ask, allow bool) error {
	if err := WriteLines(output, IntervalLines(p)); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(p.Overlaps) == 0 || allow {
		return nil
	}
	if !ask {
		return errors.New("overlaps found; review --preview, then use --allow-overlap to save anyway")
	}
	if _, err := fmt.Fprint(output, "Save anyway? [y/N] "); err != nil {
		return err
	}
	reader, err := cancelreader.NewReader(input)
	if err != nil {
		return err
	}
	done := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { reader.Cancel(); close(done) })
	defer func() {
		if !stop() {
			<-done
		}
		reader.Close()
	}()
	line, err := bufio.NewReader(reader).ReadString('\n')
	if err := ctx.Err(); err != nil {
		return err
	}
	if err != nil {
		return err
	}
	if answer := strings.ToLower(strings.TrimSpace(line)); answer != "y" && answer != "yes" {
		return errors.New("interval canceled; nothing saved")
	}
	return nil
}

func IntervalLines(p store.IntervalPreview) []string {
	var lines []string
	for _, line := range p.Lines() {
		lines = append(lines, Wrap(line, Width)...)
	}
	return lines
}
