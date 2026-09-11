package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestCredentialRecoveryIsFencedAndPreservesPendingFocus(t *testing.T) {
	st, ctx := testStore(t), context.Background()
	old, next := strings.Repeat("a", 64), strings.Repeat("b", 64)
	binding, err := st.BindCredential(ctx, old)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	started, err := st.StartTimer(ctx, TimerStartOptions{FocusType: 1}, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.StopTimer(ctx, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	before, err := st.ReadFocusTarget(ctx, started.State.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.RebindCredential(ctx, binding, next); err != nil {
		t.Fatal(err)
	}
	if _, err := st.BindCredential(ctx, next); !errors.Is(err, ErrSignedOut) {
		t.Fatalf("resumed before files were saved: %v", err)
	}
	if err := st.RebindCredential(ctx, binding, old); !errors.Is(err, ErrCredentialGeneration) {
		t.Fatalf("stale recovery: %v", err)
	}
	if _, err := st.ResumeCredential(ctx, next); err != nil {
		t.Fatal(err)
	}
	if _, err := st.BindCredential(ctx, old); !errors.Is(err, ErrCredentialChanged) {
		t.Fatalf("old token still accepted: %v", err)
	}
	after, err := st.ReadFocusTarget(ctx, started.State.SessionID)
	if err != nil || before.Version != after.Version {
		t.Fatalf("pending focus changed: %v", err)
	}
}

func TestBrowserRecoveryRetainsFrozenCredentialsAndRejectsForeignOnes(t *testing.T) {
	st, ctx := testStore(t), context.Background()
	old, next, last := strings.Repeat("a", 64), strings.Repeat("b", 64), strings.Repeat("c", 64)
	if allowed, err := st.BrowserCredentialAllows(ctx, old, next); err != nil || allowed {
		t.Fatalf("unconfirmed renewal: %v %v", allowed, err)
	}
	if err := st.RecoverBrowserCredential(ctx, old, next); err != nil {
		t.Fatal(err)
	}
	if allowed, err := st.BrowserCredentialAllows(ctx, old, next); err != nil || !allowed {
		t.Fatalf("confirmed renewal: %v %v", allowed, err)
	}
	if err := st.RecoverBrowserCredential(ctx, "", last); err != nil {
		t.Fatal(err)
	}
	if allowed, err := st.BrowserCredentialAllows(ctx, old, last); err != nil || !allowed {
		t.Fatalf("renewal after logout lost history: %v %v", allowed, err)
	}
	if allowed, err := st.BrowserCredentialAllows(ctx, strings.Repeat("d", 64), last); err != nil || allowed {
		t.Fatalf("foreign history accepted: %v %v", allowed, err)
	}
	if allowed, err := st.BrowserCredentialAllows(ctx, old, next); err != nil || allowed {
		t.Fatalf("stale credential accepted: %v %v", allowed, err)
	}
	if err := st.RecoverBrowserCredential(ctx, strings.Repeat("d", 64), next); !errors.Is(err, ErrCredentialGeneration) {
		t.Fatalf("unrecognized previous credential: %v", err)
	}
}
