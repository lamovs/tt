package tui

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/movsar/tt/internal/store"
)

func TestFocusIndicatorFormHelpAndFrozenStart(t *testing.T) {
	m, st := focusModel(t)
	m, cmd := press(m, 'a', 0)
	m = finishFocus(t, m, cmd)
	m.timer.form.field = 4
	m, _ = press(m, tea.KeyLeft, 0)
	if m.timer.form.indicator != store.IndicatorOff {
		t.Fatal("indicator selection")
	}
	m, _ = step(m, tea.WindowSizeMsg{Width: 32, Height: 10})
	if !strings.Contains(m.View().Content, "Indicator: off") {
		t.Fatal("selected indicator hidden at minimum size")
	}
	m, _ = step(m, tea.WindowSizeMsg{Width: 100, Height: 30})
	m, _ = press(m, tea.KeyF1, 0)
	m, _ = press(m, tea.KeyEscape, 0)
	if m.timer.form.indicator != store.IndicatorOff {
		t.Fatal("help changed indicator draft")
	}
	m, _ = press(m, 's', tea.ModCtrl)
	if m.timer.confirm == nil || m.timer.confirm.options.Indicator != store.IndicatorOff || !strings.Contains(m.View().Content, "off for this session") {
		t.Fatal("preview lost visibility")
	}
	m, cmd = press(m, tea.KeyEnter, 0)
	m = finishFocus(t, m, cmd)
	snapshot, err := st.ReadTimer(m.ctx, time.Now())
	if err != nil || snapshot.State == nil || snapshot.State.Indicator != store.IndicatorOff {
		t.Fatalf("visibility not persisted: %+v %v", snapshot, err)
	}
}
