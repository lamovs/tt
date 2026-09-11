package main

import (
	"strings"
	"testing"

	"github.com/movsar/tt/internal/cli"
	"github.com/movsar/tt/internal/store"
)

func TestQuoteWordMeasuresPrefixInDisplayCells(t *testing.T) {
	before := "界 "
	line := quoteWord(before, strings.Repeat("z", 300))
	if width := cli.DisplayWidth(line); width != cli.Width {
		t.Errorf("quoteWord wrote %d cells, want %d: %q", width, cli.Width, line)
	}
	if !strings.HasPrefix(line, before) {
		t.Errorf("quoteWord lost its prefix: %q", line)
	}
}

func TestDroppedRecordMeasuresRenderedOperationInDisplayCells(t *testing.T) {
	m := store.DroppedMutation{
		Seq:   7,
		Op:    strings.Repeat("界", 100),
		Title: strings.Repeat("a", 100),
	}
	var out strings.Builder
	printDroppedMutations(&out, []store.DroppedMutation{m}, func(string) string { return "" })
	for _, line := range strings.Split(out.String(), "\n") {
		if !strings.HasPrefix(line, "  entry 7: ") {
			continue
		}
		if width := cli.DisplayWidth(line); width > cli.Width {
			t.Errorf("drop entry is %d cells, want at most %d: %q", width, cli.Width, line)
		}
		if !strings.Contains(line, "...") {
			t.Errorf("drop entry does not mark its bounded values: %q", line)
		}
		return
	}
	t.Fatalf("drop report did not print the entry: %q", out.String())
}
