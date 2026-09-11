package model

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

type Duration time.Duration

const durationHint = `want "25m", "1h30m", "90s" or "0"`

func ParseDuration(s string) (Duration, error) {
	in := strings.TrimSpace(s)
	fail := func() (Duration, error) {
		return 0, fmt.Errorf("invalid duration %q (%s)", s, durationHint)
	}
	outOfRange := func() (Duration, error) {
		return 0, fmt.Errorf("invalid duration %q: value out of range", s)
	}
	if in == "" {
		return fail()
	}
	if in == "0" {
		return 0, nil
	}

	units := map[byte]time.Duration{'h': time.Hour, 'm': time.Minute, 's': time.Second}
	order := "hms"

	const maxSeconds = int64(1<<63-1) / int64(time.Second)
	var totalSec int64
	next := 0
	for len(in) > 0 {
		digits := 0
		for digits < len(in) && in[digits] >= '0' && in[digits] <= '9' {
			digits++
		}
		if digits == 0 || digits == len(in) {
			return fail()
		}
		n, err := strconv.ParseInt(in[:digits], 10, 64)
		if err != nil {
			return outOfRange()
		}
		unit := in[digits]
		mul, ok := units[unit]
		if !ok {
			return fail()
		}
		pos := strings.IndexByte(order, unit)
		if pos < next {
			return fail()
		}
		next = pos + 1

		perUnit := int64(mul / time.Second)
		if n > maxSeconds/perUnit || totalSec > maxSeconds-n*perUnit {
			return outOfRange()
		}
		totalSec += n * perUnit
		in = in[digits+1:]
	}
	return Duration(totalSec * int64(time.Second)), nil
}

func (d Duration) String() string {
	v := time.Duration(d)
	if v == 0 {
		return "0"
	}
	sign := ""
	if v < 0 {
		sign, v = "-", -v
	}
	if v%time.Second != 0 {
		return sign + v.String()
	}
	secs := int64(v / time.Second)
	var b strings.Builder
	b.WriteString(sign)
	for _, part := range []struct {
		size int64
		unit string
	}{{3600, "h"}, {60, "m"}, {1, "s"}} {
		if n := secs / part.size; n > 0 {
			fmt.Fprintf(&b, "%d%s", n, part.unit)
			secs -= n * part.size
		}
	}
	return b.String()
}

func (d Duration) Duration() time.Duration { return time.Duration(d) }

func (d Duration) Seconds() int { return int(time.Duration(d) / time.Second) }

func (d Duration) IsOff() bool { return d == 0 }

func (d Duration) MarshalText() ([]byte, error) { return []byte(d.String()), nil }

func (d *Duration) UnmarshalText(b []byte) error {
	v, err := ParseDuration(string(b))
	if err != nil {
		return err
	}
	*d = v
	return nil
}
