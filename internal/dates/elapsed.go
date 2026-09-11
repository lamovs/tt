package dates

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/movsar/tt/internal/model"
)

var elapsedPattern = regexp.MustCompile(`^(?:(\d+)h(?:(\d+)min)?|(\d+)min)$`)

func ParseElapsed(value string) (time.Duration, error) {
	m := elapsedPattern.FindStringSubmatch(strings.ToLower(strings.TrimSpace(value)))
	if m == nil {
		return 0, fmt.Errorf("duration %q needs hours/minutes, such as 2h, 30min or 1h30min", value)
	}
	var total int64
	for i, part := range m[1:] {
		if part == "" {
			continue
		}
		n, err := strconv.ParseInt(part, 10, 64)
		if err != nil || n > 100*366*24*60 {
			return 0, fmt.Errorf("duration %q is too large", value)
		}
		if i == 0 {
			n *= 60
		}
		total += n
	}
	if total <= 0 || total > 100*366*24*60 {
		return 0, fmt.Errorf("duration %q must be positive and at most 100 years", value)
	}
	return time.Duration(total) * time.Minute, nil
}

func parseElapsedDate(value string, now time.Time) (Result, error) {
	duration, err := ParseElapsed(value)
	if err != nil {
		return Result{}, err
	}

	t := now.In(time.Local).Add(duration).Truncate(time.Millisecond)
	if t.Year() < minYear || t.Year() > maxYear {
		return Result{}, fmt.Errorf("date lands in %d, outside the years %d-%d tt handles", t.Year(), minYear, maxYear)
	}
	return Result{Time: model.NewTime(t)}, nil
}
