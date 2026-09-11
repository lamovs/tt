package store

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestListingResolvesNumbersAndRanges(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)

	if _, err := s.ResolveRefs(ctx, []string{"1"}); !errors.Is(err, ErrNoListing) {
		t.Fatalf("resolving before any listing: %v", err)
	}
	if err := s.SetListing(ctx, []string{"a", "b", "c", "d", "e"}); err != nil {
		t.Fatal(err)
	}
	got, err := s.Listing(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "a,b,c,d,e" {
		t.Fatalf("listing %v", got)
	}

	for _, tc := range []struct {
		refs []string
		want string
	}{
		{[]string{"2"}, "b"},
		{[]string{"2", "4"}, "b,d"},
		{[]string{"2-4"}, "b,c,d"},
		{[]string{"1", "2-3", "3"}, "a,b,c"},
		{[]string{"3-3"}, "c"},

		{[]string{"6a0f1e2d3c4b5a6978807162", "1"}, "6a0f1e2d3c4b5a6978807162,a"},
		{[]string{"local-deadbeef"}, "local-deadbeef"},
	} {
		ids, err := s.ResolveRefs(ctx, tc.refs)
		if err != nil {
			t.Errorf("%v: %v", tc.refs, err)
			continue
		}
		if strings.Join(ids, ",") != tc.want {
			t.Errorf("%v resolved to %v, want %s", tc.refs, ids, tc.want)
		}
	}

	for _, ref := range []string{"0", "6", "2-9", "4-2"} {
		if _, err := s.ResolveRefs(ctx, []string{ref}); err == nil {
			t.Errorf("%q resolved to something", ref)
		}
	}

	if err := s.SetListing(ctx, []string{"z"}); err != nil {
		t.Fatal(err)
	}
	ids, err := s.ResolveRefs(ctx, []string{"1"})
	if err != nil || len(ids) != 1 || ids[0] != "z" {
		t.Fatalf("after a new listing: %v %v", ids, err)
	}
	if _, err := s.ResolveRefs(ctx, []string{"2"}); err == nil {
		t.Error("a number from the previous listing still resolves")
	}
}

func TestListingFollowsIDReplacement(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	local, err := NewLocalID()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetListing(ctx, []string{local}); err != nil {
		t.Fatal(err)
	}
	if err := s.ReplaceLocalID(ctx, local, "6a0f1e2d3c4b5a6978807162"); err != nil {
		t.Fatal(err)
	}
	ids, err := s.ResolveRefs(ctx, []string{"1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != "6a0f1e2d3c4b5a6978807162" {
		t.Fatalf("resolved to %v", ids)
	}
}
