package store

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestIndicatorMigrationPreservesActiveSession(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.db")
	db := openAtVersion(t, path, 9)
	now := timerTestNow()
	_, err := db.Exec(`INSERT INTO timer_state
		(id,kind,started_at,planned_sec,session_id,focus_type,note,time_precision,planned_ms,pause_ms,last_event_at)
		VALUES (1,'focus',?,60,'existing',0,'keep',3,60000,0,?)`, now.UnixMilli(), now.UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	st, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	snapshot, err := st.ReadTimer(context.Background(), now)
	if err != nil || snapshot.State == nil || snapshot.State.SessionID != "existing" ||
		snapshot.State.Note != "keep" || snapshot.State.Indicator != IndicatorDefault {
		t.Fatalf("migration changed session: %+v %v", snapshot, err)
	}
	if version, err := st.SchemaVersion(context.Background()); err != nil || version != latestVersion(t) {
		t.Fatalf("migration version %d: %v", version, err)
	}
}

func TestIndicatorReadOnlyLifecycleAndGuard(t *testing.T) {
	for _, mode := range []IndicatorMode{IndicatorDefault, IndicatorOn, IndicatorOff} {
		t.Run(mode.String(), func(t *testing.T) {
			ctx, st, now := context.Background(), testStore(t), timerTestNow()
			start, err := st.StartTimer(ctx, TimerStartOptions{Planned: time.Minute, Indicator: mode}, now)
			if err != nil {
				t.Fatal(err)
			}
			before := timerTables(t, st)
			inspection, err := Inspect(ctx, st.Path())
			if err != nil {
				t.Fatal(err)
			}
			defer inspection.Close()
			for range 8 {
				snapshot, err := inspection.Store.ReadTimer(ctx, now.Add(time.Hour))
				if err != nil || snapshot.State == nil || snapshot.State.SessionID != start.State.SessionID || snapshot.State.Indicator != mode {
					t.Fatalf("read %+v %v", snapshot, err)
				}
			}
			if !reflect.DeepEqual(before, timerTables(t, st)) {
				t.Fatal("indicator reads completed or changed the session")
			}
			for _, action := range []string{"pause", "resume"} {
				snapshot, _ := st.ReadTimer(ctx, now)
				result, err := st.ControlTimer(ctx, action, TimerStartOptions{}, &snapshot.Guard, now)
				if err != nil || result.State.Indicator != mode {
					t.Fatalf("%s lost visibility: %+v %v", action, result, err)
				}
			}
			snapshot, _ := st.ReadTimer(ctx, now)
			if _, err := st.DB().Exec("UPDATE timer_state SET indicator_mode=?", (mode+1)%3); err != nil {
				t.Fatal(err)
			}
			if _, err := st.ControlTimer(ctx, "stop", TimerStartOptions{}, &snapshot.Guard, now); !errors.Is(err, ErrTimerChanged) {
				t.Fatalf("changed indicator escaped preview fence: %v", err)
			}
		})
	}
}

func TestIndicatorInspectionBoundsExclusiveLockWait(t *testing.T) {
	ctx, st, now := context.Background(), testStore(t), timerTestNow()
	if _, err := st.StartTimer(ctx, TimerStartOptions{FocusType: 1}, now); err != nil {
		t.Fatal(err)
	}
	before := timerTables(t, st)
	conn, err := st.DB().Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "PRAGMA journal_mode=DELETE"); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, "BEGIN EXCLUSIVE"); err != nil {
		t.Fatal(err)
	}
	defer conn.ExecContext(ctx, "ROLLBACK")
	deadline, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	inspection, err := Inspect(deadline, st.Path())
	if err == nil {
		_, _, _, err = inspection.Version(deadline)
		inspection.Close()
	}
	if err == nil {
		t.Fatal("exclusive lock unexpectedly readable")
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("short observer inherited a long busy wait: %s", elapsed)
	}
	if _, err := conn.ExecContext(ctx, "ROLLBACK"); err != nil {
		t.Fatal(err)
	}
	conn.Close()
	if !reflect.DeepEqual(before, timerTables(t, st)) {
		t.Fatal("failed observation changed timer rows")
	}
}
