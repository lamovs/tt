package focus

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/movsar/tt/internal/api"
)

func TestFocusConfirmationUsesExactServiceIntervalForBothTypes(t *testing.T) {
	for _, tc := range []struct {
		mode                  api.FocusType
		start, end            string
		pause, sent, received int64
	}{
		{api.FocusTiming, "2026-09-09T13:40:19.602+0000", "2026-09-09T13:45:55.452+0000", 290, 45, 45850},
		{api.FocusPomodoro, "2026-09-09T13:47:39.738+0000", "2026-09-09T13:47:47.268+0000", 2, 4, 5530},
	} {
		request := api.FocusCreate{Type: tc.mode, StartTime: tc.start, EndTime: tc.end, PauseDuration: tc.pause, Duration: tc.sent}
		for _, duration := range []int64{tc.received, tc.received - 1, tc.received + 1, tc.sent * 1000} {
			raw := []byte(fmt.Sprintf(`{"id":"fresh","type":%d,"startTime":%q,"endTime":%q,"pauseDuration":%d,"duration":%d}`, tc.mode, tc.start, tc.end, tc.pause, duration))
			var remote api.Focus
			if err := json.Unmarshal(raw, &remote); err != nil {
				t.Fatal(err)
			}
			if err := confirm(request, nil, remote); (err == nil) != (duration == tc.received) {
				t.Fatalf("type %d duration %d: %v", tc.mode, duration, err)
			}
		}
	}
}
