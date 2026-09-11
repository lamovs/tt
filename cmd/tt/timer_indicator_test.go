package main

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/movsar/tt/internal/cli"
	"github.com/movsar/tt/internal/config"
	"github.com/movsar/tt/internal/model"
	"github.com/movsar/tt/internal/store"
)

func TestIndicatorCommandDoesNotCreateCache(t *testing.T) {
	isolate(t)
	for _, args := range [][]string{{"indicator"}, {"indicator", "--format", "json"}} {
		inv, out, diagnostic := timerTestInvocation("timer", args, config.Default())
		if code := cmdTimer(inv); code != exitOK {
			t.Fatalf("code %d: %s", code, diagnostic)
		}
		if len(args) == 1 && out.Len() != 0 {
			t.Fatalf("idle output %q", out)
		}
	}
	path, _ := store.DefaultPath()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("indicator created cache: %v", err)
	}
	for _, args := range [][]string{
		{"indicator", "extra"}, {"indicator", "--format"}, {"indicator", "--format", "bad"},
		{"indicator", "--width", "31"}, {"indicator", "--width", "513"},
		{"indicator", "--width", "40", "--width", "50"}, {"indicator", "--indicator", "off"},
		{"start", "--indicator", "bad"}, {"start", "--indicator"},
		{"pause", "--indicator", "off"}, {"start", "--indicator", "off", "--indicator", "on"},
	} {
		inv, _, diagnostic := timerTestInvocation("timer", args, config.Default())
		if code := cmdTimer(inv); code != exitUsage {
			t.Fatalf("%v: code %d: %s", args, code, diagnostic)
		}
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("invalid command created cache: %v", err)
	}
}

func TestIndicatorConcurrentExpiredReadsHaveNoEffects(t *testing.T) {
	isolate(t)
	id := seedFeatureCommandTask(t, model.Task{Title: "Observed task"})
	st, err := store.Open(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Now().Add(-time.Hour)
	started, err := st.StartTimer(context.Background(), store.TimerStartOptions{TaskID: id, Planned: time.Second, Indicator: store.IndicatorOn}, now)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := st.ReadTimer(context.Background(), time.Now())
	var queueBefore int
	st.DB().QueryRow("SELECT count(*) FROM outbox").Scan(&queueBefore)
	cfg := config.Default()
	cfg.Timer.Indicator, cfg.FocusUpload.Enabled = false, true
	cfg.Timer.OnEnd = "exit 99"
	var wg sync.WaitGroup
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			inv, out, diagnostic := timerTestInvocation("pomodoro", []string{"indicator", "--format", "json"}, cfg)
			if code := cmdTimer(inv); code != exitOK {
				t.Errorf("read failed %d: %s", code, diagnostic)
				return
			}
			var result indicatorOutput
			if err := json.Unmarshal(out.Bytes(), &result); err != nil || !result.Visible || result.State != "expired" || result.SessionID != started.State.SessionID || result.RemainingMS == nil || *result.RemainingMS != 0 {
				t.Errorf("wrong snapshot %+v: %v", result, err)
			}
		}()
	}
	wg.Wait()
	after, err := st.ReadTimer(context.Background(), time.Now())
	if err != nil || before.Guard != after.Guard || after.State == nil || !before.State.LastEventAt.Equal(after.State.LastEventAt) {
		t.Fatalf("read changed active session: %+v %v", after, err)
	}
	var claimed, completed, queueAfter int
	if err := st.DB().QueryRow("SELECT notification_claimed, ended_at IS NOT NULL FROM focus_sessions WHERE id=?", started.State.SessionID).Scan(&claimed, &completed); err != nil {
		t.Fatal(err)
	}
	st.DB().QueryRow("SELECT count(*) FROM outbox").Scan(&queueAfter)
	if claimed != 0 || completed != 0 || queueBefore != queueAfter {
		t.Fatalf("read effects claim=%d completed=%d queue=%d/%d", claimed, completed, queueBefore, queueAfter)
	}
}

func TestIndicatorVisibilityFormattingAndHostileTitles(t *testing.T) {
	now := time.Now()
	state := &store.TimerState{SessionID: "session", TaskID: "task", FocusType: 1, ActiveDuration: 42 * time.Second}
	snapshot := store.TimerSnapshot{State: state, ObservedAt: now, TaskFound: true, TaskTitle: "Title"}
	for _, tc := range []struct {
		mode             store.IndicatorMode
		enabled, visible bool
	}{
		{store.IndicatorDefault, true, true}, {store.IndicatorDefault, false, false},
		{store.IndicatorOn, false, true}, {store.IndicatorOff, true, false},
	} {
		state.Indicator = tc.mode
		out := renderIndicator(snapshot, tc.enabled, "json", 80)
		if out.Visible != tc.visible || !tc.visible && (out.Text != "" || out.TaskID != "" || out.SessionID != "") {
			t.Fatalf("visibility %+v: %+v", tc, out)
		}
	}
	state.Indicator, state.PausedAt = store.IndicatorOn, &now
	snapshot.TaskTitle = "\x1b]52;c;bad\a\n#[bg=red]#{pane_title}#(touch /tmp/no) " + strings.Repeat("界e\u0301", 1000)
	for _, format := range []string{"plain", "json", "tmux"} {
		out := renderIndicator(snapshot, true, format, 40)
		if strings.ContainsAny(out.Text+out.TaskLabel, "\x1b\a\n\r") || cli.DisplayWidth(out.Text) > 40 || cli.DisplayWidth(out.TaskLabel) > 40 || out.State != "paused" {
			t.Fatalf("unsafe output %+v", out)
		}
		if format == "tmux" && strings.Contains(out.Text, "#") {
			t.Fatalf("tmux interpreted title: %q", out.Text)
		}
	}
	snapshot.TaskTitle = "#[bg=red]#{pane_title}#(false)"
	for _, width := range []int{32, 80, 512} {
		out := renderIndicator(snapshot, true, "tmux", width)
		if strings.Contains(out.Text+out.TaskLabel, "#") || !strings.Contains(out.TaskLabel, "x23") || cli.DisplayWidth(out.Text) > width {
			t.Fatalf("unescaped tmux format at width %d: %+v", width, out)
		}
	}
}

func TestIndicatorCLIStartOverrideAndSchemaRefusal(t *testing.T) {
	isolate(t)
	cfg := config.Default()
	for _, mode := range []string{"off", "on"} {
		inv, _, diagnostic := timerTestInvocation("timer", []string{"start", "--indicator", mode}, cfg)
		if code := cmdTimer(inv); code != exitOK {
			t.Fatalf("start %s: %d %s", mode, code, diagnostic)
		}
		cfg.Timer.Indicator = mode == "off"
		inv, out, diagnostic := timerTestInvocation("timer", []string{"indicator", "--format", "json"}, cfg)
		if code := cmdTimer(inv); code != exitOK {
			t.Fatalf("indicator %d %s", code, diagnostic)
		}
		var result indicatorOutput
		if err := json.Unmarshal(out.Bytes(), &result); err != nil || result.Visible != (mode == "on") {
			t.Fatalf("override %s: %+v %v", mode, result, err)
		}
		inv, _, diagnostic = timerTestInvocation("timer", []string{"cancel"}, cfg)
		if code := cmdTimer(inv); code != exitOK {
			t.Fatalf("cancel %d %s", code, diagnostic)
		}
	}
	st, err := store.Open(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for _, version := range []string{"9", "999"} {
		if _, err := st.DB().Exec("UPDATE meta SET value=? WHERE key='schema_version'", version); err != nil {
			t.Fatal(err)
		}
		inv, out, diagnostic := timerTestInvocation("timer", []string{"indicator"}, cfg)
		if code := cmdTimer(inv); code != exitError || out.Len() != 0 || !strings.Contains(diagnostic.String(), "schema") {
			t.Fatalf("schema %s: %d %s %s", version, code, out, diagnostic)
		}
		var after string
		if err := st.DB().QueryRow("SELECT value FROM meta WHERE key='schema_version'").Scan(&after); err != nil || after != version {
			t.Fatalf("reader migrated schema: %s %v", after, err)
		}
	}
}
