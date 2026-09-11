package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
	"time"

	"github.com/movsar/tt/internal/api"
	"github.com/movsar/tt/internal/app"
	"github.com/movsar/tt/internal/cli"
	"github.com/movsar/tt/internal/config"
	"github.com/movsar/tt/internal/focus"
	"github.com/movsar/tt/internal/store"
	"github.com/movsar/tt/internal/sync"
)

const syncUsage = "usage: tt sync [--retry-failed | --drop-parked] [--allow-project-drop]\n"

const dropParkedHint = `run "tt sync --drop-parked" to throw the parked entries away instead`

var errStoppedBeforeTheRaise = errors.New("stopped before the raise")

var errStoppedBeforeTheDrop = errors.New("stopped before the drop")

func cmdSync(ctx context.Context, stdout, stderr io.Writer, args []string, reports ...*invocation) int {
	var report *invocation
	if len(reports) != 0 && reports[0].jsonOutput {
		report = reports[0]
		report.resultSource = "server"
		report.resultData = map[string]any{"operation": "sync", "phase": "preflight"}
	}
	flags, err := parseSyncArgs(args)
	if err != nil {
		if report != nil {
			report.resultError = err
		}
		fmt.Fprintf(stderr, "tt: sync: %s\n", syncFailure(err))
		fmt.Fprint(stderr, syncUsage)
		return exitUsage
	}

	localWork := flags.retryFailed || flags.dropParked
	cfg := config.Default()
	cfg.FocusUpload.Enabled = false
	if len(reports) != 0 {
		loaded, configErr := reports[0].config()
		if configErr != nil {
			return reports[0].fail(configErr)
		}
		cfg = loaded
	}

	var token string
	if !localWork {
		token, err = syncToken()
		if err != nil {
			if report != nil {
				report.resultError = err
			}
			fmt.Fprintf(stderr, "tt: sync: %s\n", syncFailure(err))
			printIfInterrupted(ctx, stderr, queueUntouched)
			return exitError
		}
	}

	st, err := store.Open(ctx, "")
	if err != nil {
		if report != nil {
			report.resultError = err
		}
		fmt.Fprintf(stderr, "tt: sync: %s\n", syncFailure(err))
		printIfInterrupted(ctx, stderr, queueUntouched)
		return exitError
	}
	defer st.Close()
	unlock, lockErr := acquireSyncLock(st)
	if lockErr != nil {
		if report != nil {
			report.resultError = errors.New("another sync is running or its process lock is unavailable")
		}
		if errors.Is(lockErr, store.ErrLocked) {
			fmt.Fprintln(stderr, "tt: sync: another sync is already running")
		} else {
			fmt.Fprintln(stderr, "tt: sync: cannot acquire the process lock")
		}
		return exitError
	}
	defer unlock()

	if flags.retryFailed {

		n, err := st.RetryFailedConfirmed(context.WithoutCancel(ctx), func(creates int) error {
			warnAboutParkedCreates(stderr, creates)
			if interrupted(ctx) {
				return errStoppedBeforeTheRaise
			}
			return nil
		})
		if errors.Is(err, errStoppedBeforeTheRaise) {

			fmt.Fprint(stderr, nothingRaised)
			return exitInterrupted
		}
		if err != nil {

			if report != nil {
				report.resultError = err
			}
			fmt.Fprintf(stderr, "tt: sync: %s\n", syncFailure(err))
			printIfInterrupted(ctx, stderr, nothingRaised)
			return exitError
		}
		fmt.Fprintf(stdout, "put %d parked entry(s) back in line\n", n)
		if report != nil {
			report.setResultPart("requeued", n)
			report.resultChanged = n != 0
		}
	}

	if flags.dropParked {

		dropped, err := st.DropParkedConfirmed(context.WithoutCancel(ctx), func(creates int) error {
			warnAboutDroppedCreates(stderr, creates)
			if interrupted(ctx) {
				return errStoppedBeforeTheDrop
			}
			return nil
		})
		if errors.Is(err, errStoppedBeforeTheDrop) {

			fmt.Fprint(stderr, nothingDropped)
			return exitInterrupted
		}
		if err != nil {

			if report != nil {
				report.resultError = err
			}
			fmt.Fprintf(stderr, "tt: sync: %s\n", syncFailure(err))
			printIfInterrupted(ctx, stderr, nothingDropped)
			return exitError
		}

		printDroppedMutations(stdout, dropped, projectNamer(ctx, st))
		if report != nil {
			report.setResultPart("dropped", dropped)
			report.resultChanged = len(dropped) != 0
		}

		summariseDrop(stderr, dropped)
	}

	if localWork {
		token, err = syncToken()
		if err != nil {
			if report != nil {
				report.resultError = err
			}
			fmt.Fprintf(stderr, "tt: sync: %s\n", syncFailure(err))
			return exitError
		}
	}

	client := managedAPIClient(token)
	out := app.RunSyncWithOptions(ctx, st, cfg, func() (*api.Client, error) { return client, nil }, nil,
		app.SyncOptions{Tasks: sync.Options{AllowProjectDrop: flags.allowProjectDrop}, Catalogs: len(reports) != 0},
		func() (focus.TopicClient, error) { client, _, err := webClient(ctx); return client, err })
	resources, res, runErr := out.Resources, out.Tasks, out.Err
	if report != nil {
		report.setResultPart("phase", "finished")
		report.setResultPart("tasks", map[string]any{"pushed": res.Pushed, "requeued": res.Requeued, "failed": res.Failed, "parked_unsent": res.ParkedUnsent, "projects": res.Projects, "pulled": res.Pulled, "skipped": res.Skipped, "deleted": res.Deleted, "kept": res.Kept, "completed_dropped": res.CompletedDropped, "warnings": res.Warnings, "errors": jsonErrorMessages(res.Errors), "unsent": jsonErrorMessages(res.Unsent)})
		report.setResultPart("resources", map[string]any{"confirmed": resources.Confirmed, "held": resources.Failed, "errors": privateMessages(resources.Errors)})
		report.setResultPart("refreshes", out.Refreshes)
		report.setResultPart("focus_upload", map[string]any{"pending": out.FocusAttempted, "uploaded": out.Focus.Uploaded, "held": out.Focus.Held, "errors": jsonErrorMessages(out.Focus.Errors), "skipped": out.FocusSkipped})
		if runErr != nil {
			report.setResultPart("run_error", uiPrivateText(runErr.Error()))
			report.resultError = runErr
		}
		report.resultChanged = report.resultChanged || res.Pushed != 0 || res.Pulled != 0 || res.Deleted != 0 || resources.Confirmed != 0
		report.resultChanged = report.resultChanged || out.Focus.Uploaded != 0
		for _, refresh := range out.Refreshes {
			report.resultChanged = report.resultChanged || refresh.State == "refreshed"
		}
	}

	printSyncResult(stdout, res)
	if resources.Confirmed != 0 || resources.Failed != 0 {
		fmt.Fprintf(stdout, "resources: confirmed %d, held %d\n", resources.Confirmed, resources.Failed)
	}
	for _, message := range resources.Errors {
		cli.WriteLines(stderr, cli.Wrap("tt: sync: "+api.OneLine(uiPrivateText(message)), cli.Width))
	}
	for _, refresh := range out.Refreshes {
		cli.WriteLines(stdout, cli.Wrap(api.OneLine(uiPrivateText(refresh.Summary())), cli.Width))
	}
	if out.FocusAttempted {
		fmt.Fprintf(stdout, "focus: uploaded %d, held %d\n", out.Focus.Uploaded, out.Focus.Held)
	} else if len(reports) != 0 && out.FocusSkipped == "" {
		fmt.Fprintln(stdout, "focus: no sessions waiting for upload")
	}
	if len(reports) != 0 && out.FocusSkipped != "" {
		cli.WriteLines(stdout, cli.Wrap("Focus: "+out.FocusSkipped, cli.Width))
	}
	for _, err := range out.Focus.Errors {
		cli.WriteLines(stderr, cli.Wrap("tt: sync: "+api.OneLine(uiPrivateText(err.Error())), cli.Width))
	}

	stopped := interrupted(ctx)
	printSyncErrors(stderr, res.Errors, runErr, stopped)

	printSyncUnsent(stderr, res.Unsent)
	if stopped {
		printInterruptedQueue(stderr, res.Failed)
		return exitInterrupted
	}
	if len(resources.Errors) != 0 || out.RefreshFailed() || len(out.Focus.Errors) != 0 {
		return exitError
	}
	return syncExit(res, runErr)
}

func syncExit(res sync.Result, runErr error) int {
	if !app.TaskSyncSucceeded(res, runErr) {
		return exitError
	}
	return exitOK
}

func printSyncErrors(w io.Writer, errs []error, runErr error, stopped bool) {
	fromInterrupt := func(err error) bool {
		var parked *sync.ParkedError
		if errors.As(err, &parked) {
			return false
		}
		return stopped && errors.Is(err, context.Canceled)
	}
	for _, e := range errs {
		if fromInterrupt(e) {
			continue
		}
		if writeAuthorizationAdvice(w, "tt: sync: ", e) {
			continue
		}
		fmt.Fprintf(w, "tt: sync: %s\n", syncPassFailure(e))
	}
	if runErr != nil && !fromInterrupt(runErr) {
		if !writeAuthorizationAdvice(w, "tt: sync: ", runErr) {
			fmt.Fprintf(w, "tt: sync: %s\n", syncPassFailure(runErr))
		}
	}
}

func printSyncUnsent(w io.Writer, unsent []error) {
	for _, e := range unsent {
		cli.WriteLines(w, cli.Wrap(fullReportAtom(e.Error()), cli.Width))
	}
}

func syncPassFailure(err error) string {
	msg := err.Error()
	prose, ok := strings.CutSuffix(msg, "; "+sync.RetryHint)
	if !ok {
		return fullReportAtom(msg)
	}
	return strings.Join(syncHintLines(fullReportAtom(prose), sync.RetryHint, syncMessageWidth), "\n")
}

const syncMessageWidth = cli.Width - len("tt: sync: ")

func syncParagraph(s string) string {
	return strings.Join(cli.Wrap(s, syncMessageWidth), "\n")
}

func syncFailure(err error) string {
	msg := err.Error()
	prose, ok := strings.CutSuffix(msg, "; "+sync.RetryHint)
	if !ok {
		return syncParagraph(msg)
	}
	return strings.Join(syncHintLines(prose, sync.RetryHint, syncMessageWidth), "\n")
}

func syncHintLines(prose, hint string, width int) []string {
	return append(cli.Wrap(prose, width), hint)
}

const (
	queueUntouched = "nothing was sent, and the queue is exactly as it was\n"
	nothingRaised  = "nothing was put back in line, and the parked entries are exactly where they were\n"
	nothingDropped = "nothing was thrown away, and the parked entries are exactly where they were\n"
)

func printIfInterrupted(ctx context.Context, w io.Writer, line string) {
	if !interrupted(ctx) {
		return
	}
	fmt.Fprint(w, line)
}

func printInterruptedQueue(w io.Writer, failed int) {
	if failed > 0 {
		cli.WriteLines(w, syncHintLines(
			fmt.Sprintf("%d entry(s) were parked and will not go out on the next pass:", failed),
			sync.RetryHint, cli.Width))
		fmt.Fprint(w, "whatever else was not sent is still in the queue and goes out on the next pass\n")
		return
	}

	cli.WriteLines(w, cli.Wrap(
		"the changes that were not sent are still in the queue and go out on the next pass",
		cli.Width))
}

func warnAboutParkedCreates(w io.Writer, creates int) {
	if creates <= 0 {
		return
	}
	fmt.Fprintf(w, "tt: sync: %d parked create(s) will be sent again, which cannot be undone\n", creates)
	fmt.Fprint(w, "a parked create may have got through before it was parked, and nothing on the\n")
	fmt.Fprint(w, "entry says whether it did; nothing lets the server recognise the repeat either,\n")
	fmt.Fprint(w, "so a second copy may appear there\n")
}

func warnAboutDroppedCreates(w io.Writer, creates int) {
	if creates <= 0 {
		return
	}
	fmt.Fprintf(w, "tt: sync: %d parked create(s) will be thrown away, which cannot be undone\n", creates)
	fmt.Fprint(w, "a create made offline is the only plan there was to put its task on the server,\n")
	fmt.Fprint(w, "so throwing one away takes the task with it; where the create had been taken up\n")
	fmt.Fprint(w, "for sending, the server may hold a copy of its own - the lines below say which\n")
	fmt.Fprint(w, "of them had been taken up; stored parking detail is not shown\n")
}

func printDroppedMutations(w io.Writer, dropped []store.DroppedMutation, projectName func(string) string) {
	if len(dropped) == 0 {
		fmt.Fprint(w, "nothing was parked, so nothing was thrown away\n")
		return
	}
	fmt.Fprintf(w, "threw away %d parked entry(s), and these lines are the only record of them:\n", len(dropped))
	var survived bool
	for _, m := range dropped {

		lead := fmt.Sprintf("  entry %d: ", m.Seq)
		room := cli.Width - cli.DisplayWidth(lead) - 1 -
			max(droppedTitleFloor, cli.DisplayWidth(droppedTaskName(m, droppedTitleFloor)))

		head := lead + cli.ReportTitle(m.Op, max(room, droppedTitleFloor)) + " "
		fmt.Fprintf(w, "%s%s\n", head, droppedTaskName(m, cli.Width-cli.DisplayWidth(head)))
		cli.WriteLines(w, dropRecordLines(droppedStamps(m)))
		if m.Reason != "" {

			cli.WriteLines(w, dropRecordLines([]string{droppedReasonOmission}))
		}
		if m.ChecklistRecoveryPending {
			cli.WriteLines(w, dropRecordLines([]string{
				"checklist recovery is pending; run tt sync for a sound fresh baseline; old checklist undo is retained but cannot be replayed across this discarded history, and tt undo --skip moves past it",
			}))
		}
		if m.TaskRemoved {

			fmt.Fprint(w, droppedTaskGone(m))

			if where := droppedTaskProject(m, projectName,
				cli.Width-len("    the list it was in: ")); where != "" {
				fmt.Fprintf(w, "    the list it was in: %s\n", where)
			}
			continue
		}
		if addressedToTheServer(m) {
			survived = true
		}
	}
	if survived {

		fmt.Fprint(w, "the changes above that were for tasks the server knows will not go out, and\n")
		fmt.Fprint(w, "the next pull brings each of those tasks back in line with what the server has\n")
	}
}

const droppedReasonOmission = "stored parking detail is omitted"

func cutToColumns(s string, widths []int) []string {
	left := []rune(s)
	out := make([]string, 0, len(widths))
	for _, w := range widths {
		if len(left) == 0 {
			break
		}
		if len(left) <= w {
			out = append(out, string(left))
			return out
		}
		cut := escapeBoundary(left, w)
		if cut == 0 {

			cut = w
		}
		out = append(out, string(left[:cut]))
		left = left[cut:]
	}
	return out
}

func escapeBoundary(piece []rune, w int) int {
	var at int
	for at < w {
		n := quotedRuneLen(piece[at:])
		if at+n > w {
			break
		}
		at += n
	}
	return at
}

const quotedRuneMax = 10

func quotedRuneLen(piece []rune) int {
	if len(piece) < 2 || piece[0] != '\\' {
		return 1
	}
	n := 2
	switch piece[1] {
	case 'x':
		n = 4
	case 'u':
		n = 6
	case 'U':
		n = quotedRuneMax
	}
	return min(n, len(piece))
}

const dropRecordIndent = "    "

func dropRecordLines(lines []string) []string {
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		out = append(out, dropRecordIndent+line)
	}
	return out
}

func addressedToTheServer(m store.DroppedMutation) bool {
	return m.TaskID != "" && !store.IsLocalID(m.TaskID)
}

func droppedTaskGone(m store.DroppedMutation) string {
	const head = "    the task itself went with it, and whatever else was queued for it: it was\n"
	if !m.Requested {
		return head +
			"    made offline, and nothing here records the create as ever having been\n" +
			"    taken up for sending, so as far as this cache knows the server never\n" +
			"    had the task, and nothing here will mention it again\n"
	}
	return head +
		"    made offline, and the create had been taken up for sending - so whether\n" +
		"    the server made the task before the entry was parked is not something this\n" +
		"    record can say.\n" +
		"    If it did make it, that copy comes in with a later pull as a task of its\n" +
		"    own, under an id nothing here has ever seen\n"
}

func summariseDrop(w io.Writer, dropped []store.DroppedMutation) {
	if len(dropped) == 0 {
		return
	}

	fmt.Fprintf(w, "tt: sync: %s\n", syncParagraph(fmt.Sprintf(
		"%d parked entry(s) thrown away, %d task(s) went with them - see stdout",
		len(dropped), tasksGoneWith(dropped))))
}

func tasksGoneWith(dropped []store.DroppedMutation) int {
	var n int
	for _, m := range dropped {
		if m.TaskRemoved {
			n++
		}
	}
	return n
}

func projectNamer(ctx context.Context, st *store.Store) func(string) string {
	ctx = context.WithoutCancel(ctx)
	known := map[string]string{}
	return func(id string) string {
		if id == "" {
			return ""
		}
		if name, ok := known[id]; ok {
			return name
		}
		var name string
		if p, err := st.Project(ctx, id); err == nil {
			name = p.Name
		}
		known[id] = name
		return name
	}
}

func droppedTaskProject(m store.DroppedMutation, projectName func(string) string, w int) string {
	if m.ProjectID == "" {
		return ""
	}
	if name := projectName(m.ProjectID); name != "" {
		return cli.ReportTitle(name, w)
	}
	return api.OneLine(m.ProjectID)
}

func droppedTaskName(m store.DroppedMutation, w int) string {
	switch {
	case m.Title != "" && w >= droppedTitleFloor:
		return cli.ReportTitle(m.Title, w)
	case m.TaskID != "":
		return "task " + api.OneLine(m.TaskID)
	case m.Title != "":

		return cli.ReportTitle(m.Title, droppedTitleFloor)
	default:
		return "(no task recorded)"
	}
}

const droppedTitleFloor = 6

func droppedStamps(m store.DroppedMutation) []string {
	if m.ParkedAt.IsZero() {
		return []string{
			fmt.Sprintf("queued %s, %d attempt(s)", stamp(m.QueuedAt), m.Attempts),
			"parked at a moment this cache does not record",
		}
	}
	return []string{fmt.Sprintf("queued %s, parked %s, %d attempt(s)",
		stamp(m.QueuedAt), stamp(m.ParkedAt), m.Attempts)}
}

func stamp(t time.Time) string {
	return t.Local().Format("2006-01-02 15:04:05")
}

type syncArgs struct {
	retryFailed bool

	dropParked bool

	allowProjectDrop bool
}

func parseSyncArgs(args []string) (syncArgs, error) {
	var out syncArgs

	const prefix = "tt: sync: "
	data, refinements := splitArgs(args)
	if len(data) != 0 {
		return syncArgs{}, errors.New(strings.TrimPrefix(
			quoteWord(prefix+"unexpected argument ", data[0]), prefix))
	}
	for _, r := range refinements {
		switch r {
		case "--retry-failed":
			out.retryFailed = true
		case "--drop-parked":
			out.dropParked = true
		case sync.AllowProjectDropFlag:
			out.allowProjectDrop = true
		default:
			return syncArgs{}, errors.New(strings.TrimPrefix(
				quoteWord(prefix+"unknown option ", r), prefix))
		}
	}

	if out.retryFailed && out.dropParked {
		return syncArgs{}, errors.New("--retry-failed and --drop-parked ask opposite things of the same entries")
	}
	return out, nil
}

func printSyncResult(w io.Writer, res sync.Result) {
	fmt.Fprintf(w, "pushed %d, requeued %d, failed %d\n", res.Pushed, res.Requeued, res.Failed)

	if n := res.ParkedUnsent; n > 0 {
		cli.WriteLines(w, syncHintLines(
			fmt.Sprintf("%d of those were parked before the pass could send them:", n),
			sync.RetryHint, cli.Width))
	}

	fmt.Fprintf(w, "pulled %d task(s) from %d list(s), %d skipped, %d deleted, %d kept\n",
		res.Pulled, res.Projects, res.Skipped, res.Deleted, res.Kept)

	if n := res.CompletedDropped; n > 0 {
		fmt.Fprintf(w, "%d completed task(s) went with the lists the server no longer has,\n", n)
		fmt.Fprint(w, "and nothing brings those back\n")
	}

	if n := res.WarningCount(); n > 0 {
		fmt.Fprintf(w, "%d field(s) the server sent could not be read, see \"tt doctor\"\n", n)
	}
}

func syncToken() (string, error) {
	if v := strings.TrimSpace(os.Getenv(tokenEnvVar)); v != "" {
		return checkedAuthToken(v)
	}
	info, err := api.LoadToken()
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", errors.New("no token found, run \"tt login\"")
		}
		return "", err
	}
	if info.Value == "" {
		return "", fmt.Errorf("token file %s is empty, run \"tt login\"", info.Path)
	}
	return checkedAuthToken(info.Value)
}
