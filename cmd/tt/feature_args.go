package main

import (
	"strconv"

	"github.com/movsar/tt/internal/schedule"
	"github.com/movsar/tt/internal/store"
)

func parseRepeatRule(value string) (string, error) { return schedule.ParseRepeat(value) }

func (inv *invocation) oneFeatureTask(st *store.Store, ref string) (string, int, bool) {
	ids, code, ok := inv.resolveTasks(st, []string{ref}, store.StatusAll)
	if !ok {
		return "", code, false
	}
	if len(ids) != 1 {
		return "", inv.misuse(oneTaskOnly, len(ids)), false
	}
	return ids[0], exitOK, true
}

func positivePosition(value string) (int, error) {
	position, err := strconv.Atoi(value)
	if err != nil || position < 1 {
		return 0, &invalidItemPosition{value: value}
	}
	return position, nil
}

type invalidItemPosition struct {
	value string
}

func (*invalidItemPosition) Error() string {
	return "position must be a positive integer"
}
