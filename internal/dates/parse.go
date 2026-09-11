package dates

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/movsar/tt/internal/model"
)

type Result struct {
	Time   model.Time
	AllDay bool
	Clear  bool
}

var weekdayAbbrev = map[string]time.Weekday{
	"mon": time.Monday,
	"tue": time.Tuesday,
	"wed": time.Wednesday,
	"thu": time.Thursday,
	"fri": time.Friday,
	"sat": time.Saturday,
	"sun": time.Sunday,
}

var monthAbbrev = map[string]time.Month{
	"jan": time.January,
	"feb": time.February,
	"mar": time.March,
	"apr": time.April,
	"may": time.May,
	"jun": time.June,
	"jul": time.July,
	"aug": time.August,
	"sep": time.September,
	"oct": time.October,
	"nov": time.November,
	"dec": time.December,
}

var (
	reRelative     = regexp.MustCompile(`^\+(\d+)([dwm])$`)
	reISODate      = regexp.MustCompile(`^(\d{4})-(\d{2})-(\d{2})$`)
	reDotDateYear  = regexp.MustCompile(`^(\d{1,2})\.(\d{1,2})\.(\d{4})$`)
	reDayMonthYear = regexp.MustCompile(`^(\d{1,2})([a-z]{3})(\d{2})$`)
	reDayMonth     = regexp.MustCompile(`^(\d{1,2})([a-z]{3})$`)
	reMonthDay     = regexp.MustCompile(`^([a-z]{3})(\d{1,2})$`)
	reDotDate      = regexp.MustCompile(`^(\d{1,2})\.(\d{1,2})$`)

	reTimeHMAmPm = regexp.MustCompile(`^(\d{1,2}):(\d{2})(am|pm)$`)
	reTimeHAmPm  = regexp.MustCompile(`^(\d{1,2})(am|pm)$`)
	reTimeHM     = regexp.MustCompile(`^(\d{1,2}):(\d{2})$`)
)

func Parse(s string, now time.Time) (Result, error) {
	fields := strings.Fields(strings.ToLower(s))
	if len(fields) == 0 {
		return Result{}, fmt.Errorf("empty date expression")
	}
	if fields[0] == "none" {
		if len(fields) > 1 {
			return Result{}, fmt.Errorf("%q takes no extra words, got %q", "none", s)
		}
		return Result{Clear: true}, nil
	}

	if fields[0] == "in" {
		return parseElapsedDate(strings.Join(fields[1:], " "), now)
	}
	if len(fields) == 1 && strings.HasPrefix(fields[0], "+") && (strings.Contains(fields[0], "h") || strings.HasSuffix(fields[0], "min")) {
		return parseElapsedDate(fields[0][1:], now)
	}
	today := civilOf(now.In(time.Local))

	dateTokens := fields
	hasTime := false
	hour, minute := 0, 0
	last := fields[len(fields)-1]
	switch {
	case len(fields) > 1 && looksLikeTime(last):
		h, m, err := parseTimeOfDay(last)
		if err != nil {
			return Result{}, err
		}
		hour, minute, hasTime = h, m, true
		dateTokens = fields[:len(fields)-1]
	case len(fields) == 1 && looksLikeTime(last):

		if _, _, err := parseTimeOfDay(last); err != nil {
			return Result{}, err
		}

		return Result{}, fmt.Errorf("%q looks like a time of day with no date attached (try %q)", last, "today "+last)
	}

	day, err := parseDatePart(dateTokens, today)
	if err != nil {
		return Result{}, err
	}

	t := day.at(hour, minute, time.Local)
	return Result{Time: model.NewTime(t), AllDay: !hasTime}, nil
}

func ParseNow(s string) (Result, error) {
	return Parse(s, time.Now())
}

const (
	minYear = 2000
	maxYear = 2099
)

const maxOffset = 1000000

func parseDatePart(tokens []string, today civil) (civil, error) {
	day, err := parseDateForm(tokens, today)
	if err != nil {
		return civil{}, err
	}
	if day.y < minYear || day.y > maxYear {
		return civil{}, fmt.Errorf("%q lands in %d, outside the years %d-%d tt handles", strings.Join(tokens, " "), day.y, minYear, maxYear)
	}
	return day, nil
}

func parseDateForm(tokens []string, today civil) (civil, error) {
	switch len(tokens) {
	case 0:
		return civil{}, fmt.Errorf("missing date")
	case 1:
		return parseSingleDateToken(tokens[0], today)
	case 2:
		if tokens[0] != "next" {
			return civil{}, fmt.Errorf("unrecognized date %q", strings.Join(tokens, " "))
		}
		wd, ok := weekdayAbbrev[tokens[1]]
		if !ok {
			return civil{}, fmt.Errorf("unrecognized weekday %q in %q", tokens[1], strings.Join(tokens, " "))
		}
		return nextWeekday(today, wd).addDays(7), nil
	default:
		return civil{}, fmt.Errorf("too many words in date %q", strings.Join(tokens, " "))
	}
}

func parseSingleDateToken(tok string, today civil) (civil, error) {
	switch tok {
	case "today":
		return today, nil
	case "tmr":
		return today.addDays(1), nil
	case "yst":
		return today.addDays(-1), nil
	case "eow":
		return endOfWeek(today), nil
	case "eom":
		return endOfMonth(today), nil
	}
	if wd, ok := weekdayAbbrev[tok]; ok {
		return nextWeekday(today, wd), nil
	}
	if m := reRelative.FindStringSubmatch(tok); m != nil {
		n, err := strconv.Atoi(m[1])
		if err != nil || n > maxOffset {
			return civil{}, fmt.Errorf("%q: an offset of %s is too far out", tok, m[1])
		}
		switch m[2] {
		case "d":
			return today.addDays(n), nil
		case "w":
			return today.addDays(7 * n), nil
		default:
			return addMonthClamped(today, n), nil
		}
	}
	if m := reISODate.FindStringSubmatch(tok); m != nil {
		y, _ := strconv.Atoi(m[1])
		mo, _ := strconv.Atoi(m[2])
		d, _ := strconv.Atoi(m[3])
		return validDate(y, time.Month(mo), d, tok)
	}
	if m := reDotDateYear.FindStringSubmatch(tok); m != nil {
		d, _ := strconv.Atoi(m[1])
		mo, _ := strconv.Atoi(m[2])
		y, _ := strconv.Atoi(m[3])
		return validDate(y, time.Month(mo), d, tok)
	}
	if m := reDayMonthYear.FindStringSubmatch(tok); m != nil {
		d, _ := strconv.Atoi(m[1])
		mo, ok := monthAbbrev[m[2]]
		if !ok {
			return civil{}, fmt.Errorf("%q: unrecognized month %q", tok, m[2])
		}
		yy, _ := strconv.Atoi(m[3])
		return validDate(2000+yy, mo, d, tok)
	}
	if m := reDayMonth.FindStringSubmatch(tok); m != nil {
		d, _ := strconv.Atoi(m[1])
		mo, ok := monthAbbrev[m[2]]
		if !ok {
			return civil{}, fmt.Errorf("%q: unrecognized month %q", tok, m[2])
		}
		return inferYear(today, mo, d, tok)
	}
	if m := reMonthDay.FindStringSubmatch(tok); m != nil {
		mo, ok := monthAbbrev[m[1]]
		if !ok {
			return civil{}, fmt.Errorf("%q: unrecognized month %q", tok, m[1])
		}
		d, _ := strconv.Atoi(m[2])
		return inferYear(today, mo, d, tok)
	}
	if m := reDotDate.FindStringSubmatch(tok); m != nil {
		d, _ := strconv.Atoi(m[1])
		mo, _ := strconv.Atoi(m[2])
		return inferYear(today, time.Month(mo), d, tok)
	}
	return civil{}, fmt.Errorf("unrecognized date %q; try 21.10.2026 or 2026-10-21", tok)
}

func looksLikeTime(tok string) bool {
	return reTimeHM.MatchString(tok) || reTimeHMAmPm.MatchString(tok) || reTimeHAmPm.MatchString(tok)
}

func parseTimeOfDay(tok string) (hour, minute int, err error) {
	if m := reTimeHMAmPm.FindStringSubmatch(tok); m != nil {
		h, _ := strconv.Atoi(m[1])
		minute, _ = strconv.Atoi(m[2])
		if h < 1 || h > 12 || minute > 59 {
			return 0, 0, fmt.Errorf("time %q is out of range", tok)
		}
		return to24Hour(h, m[3]), minute, nil
	}
	if m := reTimeHAmPm.FindStringSubmatch(tok); m != nil {
		h, _ := strconv.Atoi(m[1])
		if h < 1 || h > 12 {
			return 0, 0, fmt.Errorf("time %q is out of range", tok)
		}
		return to24Hour(h, m[2]), 0, nil
	}
	if m := reTimeHM.FindStringSubmatch(tok); m != nil {
		hour, _ = strconv.Atoi(m[1])
		minute, _ = strconv.Atoi(m[2])
		if hour > 23 || minute > 59 {
			return 0, 0, fmt.Errorf("time %q is out of range", tok)
		}
		return hour, minute, nil
	}
	return 0, 0, fmt.Errorf("%q is not a recognized time of day", tok)
}

func to24Hour(h int, ampm string) int {
	if ampm == "am" {
		if h == 12 {
			return 0
		}
		return h
	}
	if h == 12 {
		return 12
	}
	return h + 12
}

func nextWeekday(today civil, wd time.Weekday) civil {
	diff := int(wd - today.weekday())
	if diff <= 0 {
		diff += 7
	}
	return today.addDays(diff)
}

func endOfWeek(today civil) civil {
	wd := int(today.weekday())
	if wd == 0 {
		wd = 7
	}
	return today.addDays(7 - wd)
}

func endOfMonth(today civil) civil {
	return civil{y: today.y, m: today.m, d: lastDayOfMonth(today.y, today.m)}
}

func addMonthClamped(c civil, months int) civil {
	total := int(c.m) - 1 + months
	y := c.y + total/12
	m := time.Month(total%12 + 1)
	d := c.d
	if last := lastDayOfMonth(y, m); d > last {
		d = last
	}
	return civil{y: y, m: m, d: d}
}

func validDate(y int, mo time.Month, d int, tok string) (civil, error) {
	c := civil{y: y, m: mo, d: d}
	if !c.valid() {
		return civil{}, fmt.Errorf("%q is not a valid date", tok)
	}
	return c, nil
}

func inferYear(today civil, mo time.Month, d int, tok string) (civil, error) {
	for y := today.y; y <= today.y+8; y++ {
		candidate := civil{y: y, m: mo, d: d}
		if !candidate.valid() {
			continue
		}
		if !candidate.before(today) {
			return candidate, nil
		}
	}
	return civil{}, fmt.Errorf("%q is not a valid date", tok)
}
