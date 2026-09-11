package schedule

import (
	"errors"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type TriggerRelation string

const (
	TriggerRelationUnspecified TriggerRelation = ""
	TriggerRelatedStart        TriggerRelation = "START"
	TriggerRelatedEnd          TriggerRelation = "END"
)

type Trigger struct {
	Raw                        string
	Relation                   TriggerRelation
	Negative                   bool
	Years, Months, Weeks, Days int64
	Hours, Minutes, Seconds    int64
	hasMonths, hasDays         bool
}

var triggerPattern = regexp.MustCompile(`^TRIGGER(?:;RELATED=(START|END))?:(-)?P(?:(\d+)Y)?(?:(\d+)M)?(?:(\d+)W)?(?:(\d+)D)?(?:T(?:(\d+)H)?(?:(\d+)M)?(?:(\d+)S)?)?$`)

// ParseTrigger recognizes supported duration syntax without choosing provider
// semantics for an omitted reference, calendar arithmetic, or all-day tasks.
func ParseTrigger(raw string) (Trigger, error) {
	parts := triggerPattern.FindStringSubmatch(raw)
	if parts == nil {
		return Trigger{}, errors.New("unsupported reminder trigger syntax")
	}
	out := Trigger{Raw: raw, Relation: TriggerRelation(parts[1]), Negative: parts[2] != ""}
	values := []*int64{&out.Years, &out.Months, &out.Weeks, &out.Days, &out.Hours, &out.Minutes, &out.Seconds}
	count := 0
	for i, value := range parts[3:] {
		if value == "" {
			continue
		}
		parsed, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return Trigger{}, errors.New("reminder duration component overflows")
		}
		*values[i] = parsed
		count++
	}
	if count == 0 || strings.Contains(raw[strings.IndexByte(raw, ':')+1:], "T") && parts[7] == "" && parts[8] == "" && parts[9] == "" {
		return Trigger{}, errors.New("reminder duration needs a component after P or T")
	}
	out.hasMonths = parts[3] != "" || parts[4] != ""
	out.hasDays = parts[5] != "" || parts[6] != ""
	if _, ok := out.clockDuration(); !ok {
		return Trigger{}, errors.New("reminder clock duration overflows")
	}
	return out, nil
}

func (trigger Trigger) clockDuration() (time.Duration, bool) {
	const limit = int64((1<<63 - 1) / int64(time.Second))
	seconds := int64(0)
	for _, component := range []struct{ value, scale int64 }{{trigger.Hours, 3600}, {trigger.Minutes, 60}, {trigger.Seconds, 1}} {
		if component.value < 0 || component.value > (limit-seconds)/component.scale {
			return 0, false
		}
		seconds += component.value * component.scale
	}
	duration := time.Duration(seconds) * time.Second
	if trigger.Negative {
		duration = -duration
	}
	return duration, true
}
