package tui

import (
	"fmt"
	"strings"
	"testing"

	"github.com/movsar/tt/internal/app"
	"github.com/movsar/tt/internal/cli"
)

func TestSyncCatalogFailureAndRecoveryWrapAtNarrowWidths(t *testing.T) {
	for _, width := range []int{32, 48, 80, 120} {
		t.Run(fmt.Sprint(width), func(t *testing.T) {
			m, _ := writableModel(t)
			m.width = width
			m.sync = &syncPanel{result: app.SyncOutcome{
				Refreshes: []app.SyncRefresh{
					{Name: "Timer topics", State: "failed", Message: "Browser authorization rejected. Run tt login web --same-account. Queued work is preserved."},
					{Name: "project", State: "refreshed", Count: 3},
				},
				FocusSkipped: "automatic focus upload is disabled; use tt timer sync for an explicit upload",
			}}
			lines := m.syncLines()
			for _, line := range lines {
				if cli.DisplayWidth(line) > width-4 || strings.ContainsRune(line, '\x1b') {
					t.Fatalf("unsafe or overflowing line: %q", line)
				}
			}
			view := strings.Join(lines, "")
			for _, want := range []string{"Sync failed or partially completed", "Timer topics: failed", "tt login web --same-account", "project: refreshed 3", "use tt timer sync"} {
				if !strings.Contains(strings.ReplaceAll(view, " ", ""), strings.ReplaceAll(want, " ", "")) {
					t.Fatalf("missing %q in %q", want, view)
				}
			}
		})
	}
}
