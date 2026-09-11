package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/movsar/tt/internal/store"
)

func TestIntervalReviewConsent(t *testing.T) {
	p := store.IntervalPreview{Overlaps: []store.IntervalOverlap{{}}}
	for _, tc := range []struct {
		name, answer   string
		ask, allow, ok bool
	}{
		{"nonTTY", "yes\n", false, false, false}, {"allow", "", false, true, true}, {"yes", "yes\n", true, false, true}, {"no", "n\n", true, false, false}, {"default", "\n", true, false, false}, {"EOF", "yes", true, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			err := ReviewInterval(context.Background(), p, strings.NewReader(tc.answer), &out, tc.ask, tc.allow)
			if (err == nil) != tc.ok {
				t.Fatalf("%v", err)
			}
		})
	}
}

func TestIntervalReviewCancellationDoesNotConsumeFutureInput(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		result <- ReviewInterval(ctx, store.IntervalPreview{Overlaps: []store.IntervalOverlap{{}}}, r, io.Discard, true, false)
	}()
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancellation stuck")
	}
	if _, err = w.Write([]byte("y\n")); err != nil {
		t.Fatal(err)
	}
	b := make([]byte, 2)
	if _, err = io.ReadFull(r, b); err != nil || string(b) != "y\n" {
		t.Fatalf("reader not released: %q %v", b, err)
	}
}
