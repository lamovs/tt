package dates

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/movsar/tt/internal/model"
)

type Interval struct {
	Start model.Time `json:"start"`
	End   model.Time `json:"end"`
	Zone  string     `json:"zone"`
}

func IntervalZone(name string) (*time.Location, error) {
	if name == "" || name == "Local" || (name != "UTC" && !strings.Contains(name, "/")) || strings.Contains(name, "..") || strings.HasPrefix(name, "/") {
		return nil, errors.New("choose an IANA timezone, such as Europe/Moscow or UTC")
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, fmt.Errorf("unknown interval timezone %q", name)
	}
	return loc, nil
}

func DefaultIntervalZone(stored string) string {
	if _, err := IntervalZone(stored); err == nil {
		return stored
	}
	for _, name := range []string{os.Getenv("TZ"), time.Local.String()} {
		if _, err := IntervalZone(name); err == nil {
			return name
		}
	}
	if path, err := filepath.EvalSymlinks("/etc/localtime"); err == nil {
		if _, name, ok := strings.Cut(path, "/zoneinfo/"); ok {
			if _, err := IntervalZone(name); err == nil {
				return name
			}
		}
	}
	return ""
}

func (i Interval) Validate() error {
	if _, err := IntervalZone(i.Zone); err != nil {
		return err
	}
	if i.Start.IsZero() || i.End.IsZero() || !i.End.After(i.Start.Time) {
		return errors.New("interval end must be after its start")
	}
	for _, t := range []model.Time{i.Start, i.End} {
		loc, _ := IntervalZone(i.Zone)
		if year := t.In(loc).Year(); year < minYear || year > maxYear {
			return errors.New("interval endpoints must be in years 2000-2099")
		}
		if t.Nanosecond()%int(time.Millisecond) != 0 {
			return errors.New("interval timestamps allow at most millisecond precision")
		}
	}
	return nil
}

func ParseInterval(expression, zone string, now time.Time) (Interval, error) {
	parts := strings.Split(strings.Join(strings.Fields(expression), " "), " + ")
	if len(parts) != 2 {
		return Interval{}, errors.New("schedule needs START + DURATION, such as 14:00 + 1h30min")
	}
	return ParseIntervalFields(parts[0], parts[1], zone, now)
}

func ParseIntervalFields(start, duration, zone string, now time.Time) (Interval, error) {
	loc, err := IntervalZone(zone)
	if err != nil {
		return Interval{}, err
	}
	d, err := ParseElapsed(duration)
	if err != nil {
		return Interval{}, err
	}
	t, err := intervalStart(strings.TrimSpace(start), loc, now)
	if err != nil {
		return Interval{}, err
	}
	out := Interval{Start: model.NewTime(t), End: model.NewTime(t.Add(d)), Zone: zone}
	return out, out.Validate()
}

func intervalStart(value string, loc *time.Location, now time.Time) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339Nano, value); err == nil {
		_, given := t.Zone()
		_, actual := t.In(loc).Zone()
		if given != actual {
			return time.Time{}, errors.New("timestamp offset does not match the selected timezone")
		}
		return t.In(loc), nil
	}
	fields := strings.Fields(strings.ToLower(value))
	if len(fields) == 0 {
		return time.Time{}, errors.New("interval start needs a date and clock, or a clock for today")
	}
	var elapsed string
	if fields[0] == "in" {
		elapsed = strings.Join(fields[1:], " ")
	} else if len(fields) == 1 && strings.HasPrefix(fields[0], "+") && (strings.Contains(fields[0], "h") || strings.HasSuffix(fields[0], "min")) {
		elapsed = fields[0][1:]
	}
	if fields[0] == "in" || elapsed != "" {
		d, err := ParseElapsed(elapsed)
		return now.In(loc).Add(d).Truncate(time.Millisecond), err
	}
	h, m, err := parseTimeOfDay(fields[len(fields)-1])
	if err != nil {
		return time.Time{}, errors.New("interval start needs a clock, such as tmr 14:00")
	}
	day := civilOf(now.In(loc))
	if len(fields) > 1 {
		day, err = parseDatePart(fields[:len(fields)-1], day)
		if err != nil {
			return time.Time{}, err
		}
	}
	guess := time.Date(day.y, day.m, day.d, h, m, 0, 0, loc)
	var matches []time.Time

	for t, end := guess.Add(-26*time.Hour), guess.Add(26*time.Hour); !t.After(end); t = t.Add(time.Minute) {
		if civilOf(t) == day && t.Hour() == h && t.Minute() == m {
			matches = append(matches, t)
		}
	}
	if len(matches) == 0 {
		return time.Time{}, errors.New("that local clock does not exist; choose an existing time")
	}
	if len(matches) != 1 {
		return time.Time{}, errors.New("that local clock occurs twice; supply an RFC3339 timestamp with an explicit UTC offset")
	}
	return matches[0], nil
}

func ElapsedLabel(d time.Duration) string {
	if d%time.Minute != 0 {
		return d.String()
	}
	m := int64(d / time.Minute)
	if m < 60 {
		return fmt.Sprintf("%dmin", m)
	}
	if m%60 == 0 {
		return fmt.Sprintf("%dh", m/60)
	}
	return fmt.Sprintf("%dh%dmin", m/60, m%60)
}
