package store

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestAuthBindingCountsResourceAndServerQueryData(t *testing.T) {
	for _, query := range []string{
		`INSERT INTO resource_entities(kind,entity_key) VALUES('tag','one')`,
		`INSERT INTO meta(key,value) VALUES('server_task_query_saved','{}')`,
	} {
		st := testStore(t)
		ctx := context.Background()
		present, err := st.HasCredentialData(ctx)
		if err != nil || present {
			t.Fatalf("fresh cache: %v %v", present, err)
		}
		if _, err := st.DB().ExecContext(ctx, query); err != nil {
			t.Fatal(err)
		}
		present, err = st.HasCredentialData(ctx)
		if err != nil || !present {
			t.Fatalf("resource cache ignored: %v %v", present, err)
		}
	}
}

func TestAuthBindingPinsCredentialAndRequiresExplicitResume(t *testing.T) {
	ctx, st := context.Background(), testStore(t)
	first, other := strings.Repeat("a", 64), strings.Repeat("b", 64)
	bound, err := st.BindCredential(ctx, first)
	if err != nil || bound.Generation != 1 || bound.SignedOut {
		t.Fatalf("bind: %+v %v", bound, err)
	}
	unchanged, err := st.BindCredential(ctx, first)
	if err != nil || unchanged != bound {
		t.Fatalf("check changed binding: %+v %v", unchanged, err)
	}
	if _, err := st.ResumeCredential(ctx, other); !errors.Is(err, ErrCredentialChanged) {
		t.Fatalf("different credential: %v", err)
	}
	loggedOut, err := st.SignOutCredential(ctx, other)
	if err != nil || !loggedOut.SignedOut || loggedOut.Fingerprint != first || loggedOut.Generation <= bound.Generation {
		t.Fatalf("logout: %+v %v", loggedOut, err)
	}
	if _, err := st.BindCredential(ctx, first); !errors.Is(err, ErrSignedOut) {
		t.Fatalf("automatic resume: %v", err)
	}
	resumed, err := st.ResumeCredential(ctx, first)
	if err != nil || resumed.SignedOut || resumed.Generation <= loggedOut.Generation {
		t.Fatalf("resume: %+v %v", resumed, err)
	}
}

func TestAuthBindingRejectsCorruptionAndAllowsInitialLogout(t *testing.T) {
	ctx, st := context.Background(), testStore(t)
	if _, err := st.SignOutCredential(ctx, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := st.BindCredential(ctx, strings.Repeat("a", 64)); !errors.Is(err, ErrSignedOut) {
		t.Fatalf("environment bypass: %v", err)
	}
	if _, err := st.ResumeCredential(ctx, strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []string{`{"generation":0}`, `{"generation":1,"fingerprint":"invalid"}`, `not json`} {
		if err := st.SetMeta(ctx, authBindingKey, invalid); err != nil {
			t.Fatal(err)
		}
		if _, _, err := st.AuthBinding(ctx); err == nil {
			t.Errorf("accepted %s", invalid)
		}
	}
}
