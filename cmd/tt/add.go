package main

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/movsar/tt/internal/cli"
	"github.com/movsar/tt/internal/config"
	"github.com/movsar/tt/internal/dates"
	"github.com/movsar/tt/internal/model"
	"github.com/movsar/tt/internal/schedule"
	"github.com/movsar/tt/internal/store"
)

func init() {
	register(command{
		help: cli.Help{
			Verb:    "add",
			Summary: "create a task",
			Examples: []cli.Example{
				{Cmd: `tt add Work --schedule "tmr 14:00 + 1h30min" --preview`, What: "review a planned interval and cached overlaps"},
				{Cmd: "tt add Call --in 2h", What: "due in two hours, with a reminder at that time"},
				{Cmd: "tt add Call --in 1h30min", What: "in an hour and a half"},
				{Cmd: "tt add Call --at tmr 09:00", What: "tomorrow at nine, with a reminder"},
				{Cmd: "tt add Call -d +2h --remind at", What: "the explicit form; -d in 2h works too"},
				{Cmd: "tt add Call -d today 18:00 --remind -10min --remind at", What: "remind ten minutes before, then at six"},
				{Cmd: "tt add meeting notes -e markdown", What: "create from a prefilled Markdown document in VISUAL or EDITOR"},
				{Cmd: "tt add call the dentist", What: "a task in the default list"},
				{Cmd: "tt add call the dentist -P work", What: "in the list named or matching \"work\""},
				{Cmd: "tt add pay rent -d eom", What: "due the last day of this month"},
				{Cmd: "tt add standup -d fri 9:00 -p high", What: "a date with a time, and a priority"},
				{Cmd: "tt add trip --item passport --item tickets", What: "with an ordered checklist"},
				{Cmd: "tt add report --repeat weekly --remind at", What: "with a repeat and reminder"},
			},
			Sections: []cli.HelpSection{
				{Title: "Scheduling", Items: []string{
					"--schedule takes one quoted START + DURATION. The end becomes the due date. --zone, --preview and --allow-overlap behave as in tt schedule.",
					"--schedule cannot be combined with -d, --at, --in, --repeat or -e, and it does not add a reminder automatically.",
					"--in accepts elapsed durations such as 2h, 30min and 1h30min. The forms in 2h and +2h are equivalent.",
					"--at accepts a date with a time, or a bare clock for today. Both --in and --at add a reminder at the due time.",
					"Choose only one of -d, --in and --at. A date without a time is all-day. +1m means one calendar month; minutes use min.",
					"With --in or --at, extra --remind values come before the automatic at reminder. Do not repeat at.",
				}},
				{Title: "Task destination and title", Items: []string{
					"Without -P, default_project resolves a cached ID or legacy name. Configure it with tt config default-project. Missing or unusable defaults refuse creation.",
					"The title is everything before the first flag, joined with single spaces.",
					"Put -- before a title that starts with a dash, for example: tt add -- -5 min plank.",
				}},
				{Title: "Checklist, reminders and editor", Items: []string{
					"Each --item and --remind value must be one shell argument. Repeat the flag to preserve ordered values; quote multiword checklist titles.",
					"-e markdown opens a prefilled document. New records may be TEXT or NOTE. An unchanged document creates nothing; failures preserve the draft.",
					"In tt ui, press a for a task or n for a note, then Ctrl+E to edit the Markdown draft.",
				}},
				{Title: "Repeat and saving", Items: []string{
					"Use --repeat at most once. Quote RRULE values that contain semicolons.",
					"Changes remain local until tt sync. Saving a reminder does not prove that a device notification was delivered.",
				}},
			},
			SeeAlso: []string{"schedule", "edit", "ui", "show", "due", "remind", "repeat", "sync"},
		},
		run: cmdAdd,
	})
}

const (
	addFlagProject  = "-P"
	addFlagDate     = "-d"
	addFlagPriority = "-p"
	addFlagItem     = "--item"
	addFlagRepeat   = "--repeat"
	addFlagRemind   = "--remind"
	addFlagEditor   = "-e"
	addFlagIn       = "--in"
	addFlagAt       = "--at"
)

func addIsFlag(s string) bool {
	switch s {
	case addFlagProject, addFlagDate, addFlagPriority, addFlagItem, addFlagRepeat, addFlagRemind, addFlagEditor, addFlagIn, addFlagAt, "--schedule", "--zone", "--preview", "--allow-overlap":
		return true
	default:
		return false
	}
}

type addOptions struct {
	interval        string
	intervalOptions intervalOptions
	project         string
	hasProject      bool
	dateWords       []string
	hasDate         bool
	priority        string
	hasPriority     bool
	items           []string
	repeat          string
	hasRepeat       bool
	reminders       []string
	editor          bool
	reminderDate    bool
}

func parseAddOptions(refinements []string) (addOptions, error) {
	var opt addOptions
	i := 0
	for i < len(refinements) {
		switch refinements[i] {
		case "--schedule":
			if opt.interval != "" || i+1 >= len(refinements) || strings.TrimSpace(refinements[i+1]) == "" {
				return opt, errors.New("--schedule needs one quoted START + DURATION")
			}
			opt.interval = refinements[i+1]
			i += 2
		case "--zone", "--preview", "--allow-overlap":
			end := i + 1
			if refinements[i] == "--zone" {
				end++
			}
			if end > len(refinements) {
				return opt, errors.New("--zone needs an IANA timezone")
			}
			parsed, err := parseIntervalOptions(refinements[i:end])
			if err != nil {
				return opt, err
			}
			if parsed.zone != "" {
				opt.intervalOptions.zone = parsed.zone
			}
			opt.intervalOptions.preview = opt.intervalOptions.preview || parsed.preview
			opt.intervalOptions.allow = opt.intervalOptions.allow || parsed.allow
			i = end
		case addFlagEditor:
			if opt.editor || i+1 >= len(refinements) || refinements[i+1] != "markdown" {
				return opt, errors.New("-e requires markdown and may be given only once")
			}
			opt.editor = true
			i += 2
		case addFlagProject:
			if i+1 >= len(refinements) {
				return opt, fmt.Errorf("%s needs a list name", addFlagProject)
			}
			opt.project, opt.hasProject = refinements[i+1], true
			i += 2
		case addFlagPriority:
			if i+1 >= len(refinements) {
				return opt, fmt.Errorf("%s needs a priority (%s)", addFlagPriority, strings.Join(model.PriorityNames(), ", "))
			}
			opt.priority, opt.hasPriority = refinements[i+1], true
			i += 2
		case addFlagDate, addFlagIn, addFlagAt:
			flag := refinements[i]
			if opt.reminderDate || (flag != addFlagDate && opt.hasDate) {
				return opt, errors.New("choose only one of -d, --in and --at")
			}
			j := i + 1
			for j < len(refinements) && !addIsFlag(refinements[j]) {
				j++
			}
			if j == i+1 {
				return opt, fmt.Errorf("%s needs a date", flag)
			}
			opt.dateWords, opt.hasDate = refinements[i+1:j], true
			if flag != addFlagDate {
				opt.reminderDate = true
				if flag == addFlagIn {
					value := strings.ToLower(strings.Join(strings.Fields(strings.Join(opt.dateWords, " ")), " "))
					if strings.HasPrefix(value, "in ") {
						value = strings.TrimSpace(value[3:])
					}
					value = strings.TrimPrefix(value, "+")
					if _, err := dates.ParseElapsed(value); err != nil {
						return opt, err
					}
					opt.dateWords = []string{"in " + value}
				}
			}
			i = j
		case addFlagItem:
			if i+1 >= len(refinements) || addIsFlag(refinements[i+1]) {
				return opt, fmt.Errorf("%s needs an item title", addFlagItem)
			}
			if strings.TrimSpace(refinements[i+1]) == "" {
				return opt, fmt.Errorf("%s needs a nonempty item title", addFlagItem)
			}
			opt.items = append(opt.items, refinements[i+1])
			i += 2
		case addFlagRepeat:
			if opt.hasRepeat {
				return opt, fmt.Errorf("%s may be given only once", addFlagRepeat)
			}
			if i+1 >= len(refinements) || addIsFlag(refinements[i+1]) {
				return opt, fmt.Errorf("%s needs a repeat rule", addFlagRepeat)
			}
			repeat, err := parseRepeatRule(refinements[i+1])
			if err != nil {
				return opt, err
			}
			opt.repeat, opt.hasRepeat = repeat, true
			i += 2
		case addFlagRemind:
			if i+1 >= len(refinements) || addIsFlag(refinements[i+1]) {
				return opt, fmt.Errorf("%s needs a reminder trigger", addFlagRemind)
			}
			reminder, err := schedule.ParseReminder(refinements[i+1])
			if err != nil {
				return opt, err
			}
			opt.reminders = append(opt.reminders, reminder)
			i += 2
		default:
			return opt, addUnknownOption{opt: refinements[i]}
		}
	}
	if opt.reminderDate {
		opt.reminders = append(opt.reminders, "TRIGGER:PT0S")
	}
	if opt.interval != "" && (opt.hasDate || opt.hasRepeat || opt.editor) {
		return opt, errors.New("--schedule cannot combine with -d, --at, --in, --repeat or -e; edit the saved task afterwards")
	}
	if opt.interval == "" && opt.intervalOptions != (intervalOptions{}) {
		return opt, errors.New("--zone, --preview and --allow-overlap need --schedule")
	}
	if err := validateReminderList(opt.reminders); err != nil {
		return opt, err
	}
	return opt, nil
}

type addUnknownOption struct{ opt string }

func (e addUnknownOption) Error() string { return fmt.Sprintf("unknown option %q", e.opt) }

func cmdAdd(inv *invocation) int {
	title := strings.TrimSpace(strings.Join(inv.data, " "))
	opt, err := parseAddOptions(inv.refinements)
	if err != nil {
		var unknown addUnknownOption
		if errors.As(err, &unknown) {
			return inv.misuseWord("unknown option ", unknown.opt)
		}
		return inv.misuse("%v", err)
	}
	if title == "" && !opt.editor {
		return inv.misuse("no title given")
	}

	task := model.Task{Title: title}
	if opt.interval != "" {
		zone := opt.intervalOptions.zone
		if zone == "" {
			zone = dates.DefaultIntervalZone("")
		}
		interval, err := dates.ParseInterval(opt.interval, zone, time.Now())
		if err != nil {
			return inv.misuse("%v", err)
		}
		task.StartDate, task.DueDate, task.TimeZone = interval.Start, interval.End, interval.Zone
	}
	if len(opt.items) != 0 {
		task.Items = make([]model.Item, len(opt.items))
		for i, item := range opt.items {
			task.Items[i].Title = item
		}
	}
	if opt.hasRepeat {
		task.RepeatFlag = opt.repeat
	}
	if len(opt.reminders) != 0 {
		task.Reminders = append([]string(nil), opt.reminders...)
	}

	if opt.hasDate {
		var d dates.Result
		var err error
		if opt.reminderDate {
			d, err = schedule.ReminderDate(strings.Join(opt.dateWords, " "), time.Now())
		} else {
			d, err = dates.ParseNow(strings.Join(opt.dateWords, " "))
		}
		if err != nil {
			return inv.misuse("%v", err)
		}

		if !d.Clear {
			task.DueDate = d.Time
			task.IsAllDay = d.AllDay
		}
	}

	if opt.hasPriority {
		p, err := model.ParsePriority(opt.priority)
		if err != nil {
			return inv.misuse("%v", err)
		}
		task.Priority = p
	}

	st, err := inv.openStore()
	if err != nil {
		return inv.fail(err)
	}
	defer st.Close()

	resolver := inv.resolver(st)
	var project model.Project
	if opt.hasProject {
		project, err = resolver.Project(inv.ctx, opt.project)
	} else {
		cfg, cfgErr := config.Load()
		if cfgErr != nil {
			return inv.fail(cfgErr)
		}
		project, err = resolver.DefaultProject(inv.ctx, cfg.DefaultProject)
	}
	if err != nil {
		return inv.fail(err)
	}
	task.ProjectId = project.Id
	if reason := project.CreateUnavailable(); reason != "" {
		return inv.fail(errors.New(reason + "; sync or choose another list"))
	}
	if opt.editor {
		task.Kind = "TEXT"
		draft, err := inv.openTaskEditor(task)
		if err != nil {
			return inv.editorFailure(draft, err)
		}
		if !draft.Changed {
			return inv.editorDone(draft, "unchanged; no task created")
		}
		next, _, err := editedTaskDocument(task, draft.Data, true)
		if err != nil {
			return inv.editorFailure(draft, err)
		}
		if reflect.DeepEqual(next, task) {
			return inv.editorDone(draft, "unchanged; no task created")
		}
		created, err := st.CreateTask(inv.ctx, next)
		if err != nil {
			return inv.editorFailure(draft, err)
		}
		where := " to " + cli.ReportTitle(project.Name, cli.Width/4)
		return inv.editorDone(draft, cli.ReportLine("added ", created.Title, where, nil))
	}

	if opt.interval != "" {
		_, err := inv.saveInterval(st, nil, model.TaskEdit{}, task, opt.intervalOptions)
		if err != nil {
			return inv.fail(err)
		}
		return exitOK
	}

	existing, err := st.Tasks(inv.ctx, store.TaskFilter{Search: title, Status: store.StatusAll})
	if err != nil {
		return inv.fail(err)
	}
	for _, t := range existing {
		if strings.EqualFold(strings.TrimSpace(t.Title), title) {
			if inv.jsonOutput {
				inv.resultWarnings = append(inv.resultWarnings, "a task with this exact title already exists")
			}
			fmt.Fprintln(inv.stdout, cli.ReportLine("note: a task called ", title, " already exists", nil))
			break
		}
	}

	created, err := st.CreateTask(inv.ctx, task)
	if err != nil {
		return inv.fail(err)
	}
	inv.recordTask(created, true, false)

	where := " to " + cli.ReportTitle(project.Name, cli.Width/4)
	fmt.Fprintln(inv.stdout, cli.ReportLine("added ", created.Title, where, nil))
	return exitOK
}
