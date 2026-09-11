package dates

import "time"

type civil struct {
	y int
	m time.Month
	d int
}

func civilOf(t time.Time) civil {
	y, m, d := t.Date()
	return civil{y: y, m: m, d: d}
}

func (c civil) epochDay() int {
	y := c.y
	if c.m <= time.February {
		y--
	}
	era := y
	if y < 0 {
		era = y - 399
	}
	era /= 400
	yoe := y - era*400
	mp := int(c.m) - 3
	if mp < 0 {
		mp += 12
	}
	doy := (153*mp+2)/5 + c.d - 1
	doe := yoe*365 + yoe/4 - yoe/100 + doy
	return era*146097 + doe - 719468
}

func civilFromEpochDay(n int) civil {
	z := n + 719468
	era := z
	if z < 0 {
		era = z - 146096
	}
	era /= 146097
	doe := z - era*146097
	yoe := (doe - doe/1460 + doe/36524 - doe/146096) / 365
	y := yoe + era*400
	doy := doe - (365*yoe + yoe/4 - yoe/100)
	mp := (5*doy + 2) / 153
	d := doy - (153*mp+2)/5 + 1
	m := mp + 3
	if m > 12 {
		m -= 12
		y++
	}
	return civil{y: y, m: time.Month(m), d: d}
}

func (c civil) addDays(n int) civil {
	return civilFromEpochDay(c.epochDay() + n)
}

func (c civil) weekday() time.Weekday {
	wd := (c.epochDay() + int(time.Thursday)) % 7
	if wd < 0 {
		wd += 7
	}
	return time.Weekday(wd)
}

func (c civil) before(o civil) bool {
	if c.y != o.y {
		return c.y < o.y
	}
	if c.m != o.m {
		return c.m < o.m
	}
	return c.d < o.d
}

func (c civil) valid() bool {
	if c.m < time.January || c.m > time.December {
		return false
	}
	return c.d >= 1 && c.d <= lastDayOfMonth(c.y, c.m)
}

func isLeapYear(y int) bool {
	return y%4 == 0 && (y%100 != 0 || y%400 == 0)
}

func lastDayOfMonth(y int, m time.Month) int {
	switch m {
	case time.April, time.June, time.September, time.November:
		return 30
	case time.February:
		if isLeapYear(y) {
			return 29
		}
		return 28
	}
	return 31
}

const (
	zoneSearchWindow = 8 * time.Hour
	zoneSearchStep   = time.Minute
)

func (c civil) at(hour, minute int, loc *time.Location) time.Time {
	guess := time.Date(c.y, c.m, c.d, hour, minute, 0, 0, loc)
	_, offGuess := guess.Zone()
	_, offEarlier := guess.Add(-zoneSearchWindow).Zone()
	if offGuess == offEarlier && civilOf(guess) == c && guess.Hour() == hour && guess.Minute() == minute {

		return guess
	}

	wanted := hour*60 + minute
	end := guess.Add(zoneSearchWindow)
	dayExists := false
	for t := guess.Add(-zoneSearchWindow); !t.After(end); t = t.Add(zoneSearchStep) {
		if civilOf(t) != c {
			continue
		}
		dayExists = true
		if t.Hour()*60+t.Minute() < wanted {
			continue
		}
		return t
	}

	if dayExists {

		return c.addDays(1).at(0, 0, loc)
	}

	return guess
}
