package focus

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/movsar/tt/internal/api"
)

func TestStandaloneFocusRequiresExactAnonymousInterval(t *testing.T) {
	for _, mode := range []api.FocusType{api.FocusPomodoro, api.FocusTiming} {
		request := api.FocusCreate{Type: mode, StartTime: "2026-09-09T14:00:00.396+0000", EndTime: "2026-09-09T14:00:03.396+0000", Duration: 3}
		interval := `{"startTime":"2026-09-09T14:00:00.396+0000","endTime":"2026-09-09T14:00:03.396+0000"}`
		for _, tc := range []struct {
			name, tasks, relation, extra string
			pass                         bool
		}{
			{"anonymous interval", "[" + interval + "]", "[0]", "", true},
			{"empty array", "[]", "[0]", "", true},
			{"empty task id", "[" + strings.Replace(interval, "{", `{"taskId":"",`, 1) + "]", "[0]", "", false},
			{"null task id", "[" + strings.Replace(interval, "{", `{"taskId":null,`, 1) + "]", "[0]", "", false},
			{"project relation", "[" + strings.Replace(interval, "{", `{"projectId":"foreign",`, 1) + "]", "[0]", "", false},
			{"different interval", "[" + strings.Replace(interval, "03.396", "03.397", 1) + "]", "[0]", "", false},
			{"multiple intervals", "[" + interval + "," + interval + "]", "[0]", "", false},
			{"task relation type", "[" + interval + "]", "[2]", "", false},
			{"mixed relation type", "[" + interval + "]", "[0,2]", "", false},
			{"null relation", "[" + interval + "]", "null", "", false},
			{"top task", "[" + interval + "]", "[0]", `,"taskId":"foreign"`, false},
		} {
			raw := []byte(fmt.Sprintf(`{"id":"fresh","type":%d,"startTime":%q,"endTime":%q,"duration":3000,"pauseDuration":0,"tasks":%s,"relationType":%s%s}`, mode, request.StartTime, request.EndTime, tc.tasks, tc.relation, tc.extra))
			var remote api.Focus
			if err := json.Unmarshal(raw, &remote); err != nil {
				t.Fatal(err)
			}
			if err := confirm(request, nil, remote); (err == nil) != tc.pass {
				t.Fatalf("type %d %s: %v", mode, tc.name, err)
			}
		}
	}
}
