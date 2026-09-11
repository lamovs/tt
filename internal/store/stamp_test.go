package store

import (
	"database/sql"
	"testing"
	"time"

	"github.com/movsar/tt/internal/model"
)

func TestFormatStampMatchesModel(t *testing.T) {
	moscow := time.FixedZone("MSK", 3*60*60)
	for _, in := range []time.Time{
		{},
		time.Date(2026, 9, 3, 12, 30, 0, 500*int(time.Millisecond), moscow),
		time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2027, 12, 31, 23, 59, 59, 999*int(time.Millisecond), time.UTC),
	} {
		want := model.NewTime(in).StoreString()
		got := FormatStamp(in)
		if want == "" {
			if got != nil {
				t.Errorf("FormatStamp(%v) = %v, want NULL", in, got)
			}
			continue
		}
		if got != want {
			t.Errorf("FormatStamp(%v) = %v, model gives %q", in, got, want)
		}
		back, err := ParseStamp(sql.NullString{String: want, Valid: true})
		if err != nil {
			t.Fatalf("ParseStamp(%q): %v", want, err)
		}
		if !back.Equal(in) {
			t.Errorf("round trip of %v gave %v", in, back)
		}
	}
}
