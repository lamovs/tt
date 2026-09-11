package main

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/movsar/tt/internal/cli"
	"github.com/movsar/tt/internal/dates"
	"github.com/movsar/tt/internal/model"
	"github.com/movsar/tt/internal/schedule"
	"github.com/movsar/tt/internal/store"
)

func init() {
	register(command{help: cli.Help{
		Verb: "schedule", Summary: "set a timed task's start and planned duration",
		Examples: []cli.Example{
			{Cmd: `tt schedule 3 "14:00 + 1h30min" --preview`, What: "review start, end/due and cached overlaps without saving"},
			{Cmd: `tt schedule 3 "21.10.2026 14:00 + 90min"`, What: "use an explicit day, month and year"},
			{Cmd: `tt schedule 3 "tmr 14:00 + 1h30min" --zone Europe/Moscow`, What: "save start 14:00 and end/due 15:30"},
			{Cmd: `tt schedule 3 "14:00 + 90min" --allow-overlap`, What: "explicitly allow the reported overlaps"},
		},
		Sections: []cli.HelpSection{
			{Title: "Supported tasks", Items: []string{
				"schedule supports open, non-all-day tasks without repeat rules.",
				"Duration must be positive and use h or min, never m. In the start expression, +1m still means one calendar month.",
			}},
			{Title: "Start and time zone", Items: []string{
				"Start accepts a clock for today, a date plus clock, elapsed forms such as in 2h, or RFC3339 with an offset matching the selected IANA time zone.",
				"Nonexistent local times are rejected. Repeated clocks require an explicit offset.",
				"The zone defaults to a valid stored zone, then a verified system IANA zone. Otherwise, provide --zone.",
			}},
			{Title: "End and reminders", Items: []string{
				"End and due are start plus elapsed duration, not an independent deadline or focus duration.",
				"Existing reminders remain unchanged, although provider-side timing may change.",
			}},
			{Title: "Overlap review", Items: []string{
				"Review covers cached open timed tasks across all lists, including local changes and stored recurring intervals.",
				"It does not predict future recurrence instances or prove complete server availability. All-day and partial schedules are separate context; touching endpoints do not overlap.",
				"The preview is rechecked transactionally when saved. A stale preview is refused.",
				"Overlaps require an explicit save in a terminal or --allow-overlap. --preview never writes. Changes remain local until tt sync.",
			}},
		},
		SeeAlso: []string{"add", "due", "ui", "sync", "undo"},
	}, run: cmdSchedule})
}

type intervalOptions struct {
	zone           string
	preview, allow bool
}

func parseIntervalOptions(args []string) (intervalOptions, error) {
	var opt intervalOptions
	seen := map[string]bool{}
	for i := 0; i < len(args); i++ {
		if seen[args[i]] {
			return opt, fmt.Errorf("duplicate option %s", args[i])
		}
		seen[args[i]] = true
		switch args[i] {
		case "--preview":
			opt.preview = true
		case "--allow-overlap":
			opt.allow = true
		case "--zone":
			if i+1 >= len(args) {
				return opt, errors.New("--zone needs an IANA timezone")
			}
			i++
			opt.zone = args[i]
			if _, err := dates.IntervalZone(opt.zone); err != nil {
				return opt, err
			}
		default:
			return opt, fmt.Errorf("unknown option %s", args[i])
		}
	}
	return opt, nil
}

func cmdSchedule(inv *invocation) int {
	opt, err := parseIntervalOptions(inv.refinements)
	if err != nil {
		return inv.intervalInputError(err)
	}
	if inv.interrupted() {
		return exitInterrupted
	}
	st, err := inv.openStore()
	if err != nil {
		return inv.fail(err)
	}
	defer st.Close()
	now := time.Now()

	ids, expression, code, ok := inv.resolveTasksAndValue(st, inv.data, store.StatusOpen, "START + DURATION", func(s string) error {
		parts := strings.Split(strings.Join(strings.Fields(s), " "), " + ")
		if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" {
			return errors.New("expected START + DURATION")
		}
		if _, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(parts[0])); err == nil {
			_, err = dates.ParseElapsed(parts[1])
			return err
		}
		_, err := dates.ParseInterval(s, "UTC", now)
		return err
	})
	if !ok {
		return code
	}
	if len(ids) != 1 {
		return inv.misuse(oneTaskOnly, len(ids))
	}
	original, err := st.Task(inv.ctx, ids[0])
	if err != nil {
		return inv.fail(err)
	}
	zone := opt.zone
	if zone == "" {
		zone = dates.DefaultIntervalZone(original.TimeZone)
	}
	interval, err := dates.ParseInterval(expression, zone, now)
	if err != nil {
		return inv.intervalInputError(err)
	}
	edit, err := schedule.IntervalEdit(original, interval)
	if err != nil {
		return inv.fail(err)
	}
	_, err = inv.saveInterval(st, &original, edit, model.Task{}, opt)
	if err != nil {
		return inv.fail(err)
	}
	return exitOK
}

func (inv *invocation) saveInterval(st *store.Store, original *model.Task, edit model.TaskEdit, create model.Task, opt intervalOptions) (store.TaskMutationOutcome, error) {
	p, err := st.PrepareInterval(inv.ctx, original, edit, create)
	if err != nil {
		return store.TaskMutationOutcome{}, err
	}
	if opt.preview {
		if inv.jsonOutput {
			inv.resultData = map[string]any{"preview": true, "task": p.Task, "overlaps": p.Overlaps, "context": p.Context, "last_sync": p.LastSync}
			return store.TaskMutationOutcome{}, nil
		}
		return store.TaskMutationOutcome{}, cli.WriteLines(inv.stdout, cli.IntervalLines(p))
	}
	if err := cli.ReviewInterval(inv.ctx, p, inv.stdin, inv.stderr, canAsk(inv), opt.allow); err != nil {
		return store.TaskMutationOutcome{}, err
	}
	out, err := st.ApplyInterval(inv.ctx, p, true)
	if err == nil {
		inv.recordTask(out.Task, out.Changed, false)
		fmt.Fprintln(inv.stdout, cli.ReportLine("scheduled ", out.Task.Title, " locally; sync to confirm", nil))
	}
	return out, err
}

func (inv *invocation) intervalInputError(err error) int {
	return inv.misuse("%s", cli.Foreign(err.Error(), cli.Width-len("tt: "+inv.verb+": ")))
}
