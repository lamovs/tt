package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/movsar/tt/internal/model"
	"github.com/movsar/tt/internal/store"
)

type Resolver struct {
	Store *store.Store

	Stdin  io.Reader
	Stderr io.Writer

	Stdout io.Writer

	Palette Palette

	Ask bool
}

const candidateLimit = 20

var (
	ErrNoReference = errors.New("no task given")

	ErrCancelled = errors.New("cancelled")
)

type NoMatchError struct {
	What  string
	Query string
	Scope string

	Known []string

	Omitted int
}

func (e *NoMatchError) Error() string {
	among := ""
	if e.Scope != "" {
		among = " among " + e.Scope
	}
	msg := refusalLine("no "+e.What+" matches ", e.Query, among)
	if len(e.Known) > 0 {
		msg += "\n" + knownLine(e.Known, e.Omitted)
	}
	return msg
}

func knownLine(names []string, omitted int) string {
	const opening = "there is: "

	pieces := make([]string, len(names))
	for i, name := range names {
		pieces[i] = escapeCell(name, nameInAnError)
	}

	for k := len(pieces); ; k-- {
		line := opening + strings.Join(pieces[:k], ", ") + andMore(len(names)-k+omitted)

		if DisplayWidth(line) <= Width || k <= 1 {
			return line
		}
	}
}

func andMore(n int) string {
	if n == 0 {
		return ""
	}
	return " and " + strconv.Itoa(n) + " more"
}

type AmbiguousError struct {
	What    string
	Query   string
	Choices []string

	Omitted int

	More bool
}

func (e *AmbiguousError) Error() string {
	total := len(e.Choices) + e.Omitted
	count := strconv.Itoa(total)
	if e.More {
		count = "more than " + count
	}

	named := ""
	if e.Omitted > 0 {
		named = ", the first " + strconv.Itoa(len(e.Choices))
	}
	msg := refusalLine("", e.Query, " matches "+count+" "+plural(e.What, total)+named+":")

	for _, c := range e.Choices {
		msg += "\n" + c
	}
	return msg
}

func plural(word string, n int) string {
	if n == 1 {
		return word
	}
	return word + "s"
}

const refusalPrefix = "tt: countdown: "

func refusalLine(before, query, after string) string {
	return before + ReportTitle(query, Width-len(refusalPrefix)-DisplayWidth(before)-DisplayWidth(after)) + after
}

func (r Resolver) Tasks(ctx context.Context, args []string, status store.StatusFilter) ([]string, error) {
	args = trimArgs(args)
	if len(args) == 0 {
		return nil, ErrNoReference
	}
	if allReferences(args) {
		if err := checkReferences(args); err != nil {
			return nil, err
		}
		return r.Store.ResolveRefs(ctx, args)
	}

	query := strings.Join(args, " ")

	found, err := r.Store.Tasks(ctx, store.TaskFilter{
		Search: query,
		Status: status,
		Limit:  candidateLimit + 1,
	})
	if err != nil {
		return nil, err
	}
	switch len(found) {
	case 0:
		return nil, &NoMatchError{What: "task", Query: query, Scope: scopeName(status)}
	case 1:
		return []string{found[0].Id}, nil
	}

	more := len(found) > candidateLimit
	if more {
		found = found[:candidateLimit]
	}
	choices, err := r.candidateLines(ctx, found)
	if err != nil {
		return nil, err
	}
	if !r.Ask {
		return nil, &AmbiguousError{What: "task", Query: query, Choices: choices, More: more}
	}
	id, err := r.choose(query, found, choices, more)
	if err != nil {
		return nil, err
	}
	return []string{id}, nil
}

func (r Resolver) choose(query string, found []model.Task, choices []string, more bool) (string, error) {

	ask := r.Stderr
	if ask == nil {
		ask = r.Stdout
	}

	fmt.Fprintln(ask, ReportLine("", query, fmt.Sprintf(" matches %d %s:", len(found), plural("task", len(found))), nil))
	if err := WriteLines(ask, choices); err != nil {
		return "", err
	}
	if more {
		fmt.Fprintf(ask, "(there are more; only the first %d are shown)\n", candidateLimit)
	}
	fmt.Fprint(ask, "which one? [1-"+strconv.Itoa(len(found))+", empty to cancel] ")

	line, _ := bufio.NewReader(r.Stdin).ReadString('\n')
	answer := strings.TrimSpace(line)
	if answer == "" {

		return "", ErrCancelled
	}
	n, err := strconv.Atoi(answer)
	if err != nil || n < 1 || n > len(found) {

		return "", errors.New(refusalLine("", answer,
			fmt.Sprintf(" is not one of the numbers offered (1-%d)", len(found))))
	}
	return found[n-1].Id, nil
}

func (r Resolver) candidateLines(ctx context.Context, found []model.Task) ([]string, error) {
	names, err := ProjectNames(ctx, r.Store)
	if err != nil {
		return nil, err
	}
	return ListLines(Rows(found, names), r.Palette, time.Now()), nil
}

func (r Resolver) Project(ctx context.Context, query string) (model.Project, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return model.Project{}, errors.New("no list given")
	}
	matches, err := r.Store.FindProjects(ctx, query)
	if err != nil {
		return model.Project{}, err
	}

	var exact []model.Project
	for _, p := range matches {
		if strings.EqualFold(strings.TrimSpace(p.Name), query) {
			exact = append(exact, p)
		}
	}
	if len(exact) == 1 {
		return exact[0], nil
	}
	switch len(matches) {
	case 0:
		all, err := r.Store.Projects(ctx)
		if err != nil {
			return model.Project{}, err
		}
		known, omitted := shortlist(projectNames(all))
		return model.Project{}, &NoMatchError{What: "list", Query: query, Known: known, Omitted: omitted}
	case 1:
		return matches[0], nil
	default:
		named, omitted := shortlist(projectNames(matches))
		return model.Project{}, &AmbiguousError{
			What: "list", Query: query, Choices: projectChoices(named), Omitted: omitted,
		}
	}
}

func (r Resolver) DefaultProject(ctx context.Context, name string) (model.Project, error) {
	projects, err := r.Store.Projects(ctx)
	if err != nil {
		return model.Project{}, err
	}
	p, err := ResolveDefaultProject(projects, name)
	if err != nil {
		return model.Project{}, fmt.Errorf("%w\n%s", err, defaultProjectHint)
	}
	return p, nil
}

const defaultProjectHint = `that target comes from the default_project key; run "tt config default-project" to choose it`

type ListNameMatch int

const (
	ListNameNone ListNameMatch = iota
	ListNameOne
	ListNameSeveral
)

func MatchListName(names []string, want string) (ListNameMatch, []string) {
	want = strings.TrimSpace(want)
	if want == "" {
		return ListNameNone, nil
	}
	matches := store.MatchNames(names, want)

	var exact []string
	for _, name := range matches {
		if strings.EqualFold(strings.TrimSpace(name), want) {
			exact = append(exact, name)
		}
	}
	if len(exact) == 1 {
		return ListNameOne, exact
	}
	switch len(matches) {
	case 0:
		return ListNameNone, nil
	case 1:
		return ListNameOne, matches
	default:
		return ListNameSeveral, matches
	}
}

func SplitValue(args []string, what string, parse ValueParser) (ref []string, value string, err error) {
	args = trimArgs(args)
	switch len(args) {
	case 0:
		return nil, "", fmt.Errorf("name a task and a %s", what)
	case 1:
		if kind, _ := classifyRef(args[0]); kind != refNone {
			return nil, "", fmt.Errorf("%q names a task and no %s; the %s comes last", args[0], what, what)
		}
		return nil, "", fmt.Errorf("name a task and a %s", what)
	}

	reasons := make([]error, len(args))
	for start := 1; start < len(args); start++ {
		candidate := strings.Join(args[start:], " ")
		if reasons[start] = parse(candidate); reasons[start] == nil {
			return args[:start], candidate, nil
		}
	}

	return nil, "", fmt.Errorf("%s\n%w",
		refusalLine("could not read a "+what+" at the end of ", strings.Join(args, " "), ":"),
		reasons[blame(args)])
}

type ValueParser func(value string) error

func blame(args []string) int {
	if kind, _ := classifyRef(args[0]); kind != refNone {
		return 1
	}
	return len(args) - 1
}

func trimArgs(args []string) []string {
	out := make([]string, 0, len(args))
	for _, a := range args {
		if a = strings.TrimSpace(a); a != "" {
			out = append(out, a)
		}
	}
	return out
}

func allReferences(args []string) bool {
	for _, a := range args {
		kind, err := classifyRef(a)
		if kind == refNone && err == nil {
			return false
		}
	}
	return true
}

func checkReferences(args []string) error {
	for _, a := range args {
		if _, err := classifyRef(a); err != nil {
			return err
		}
	}
	return nil
}

type refKind int

const (
	refNone refKind = iota
	refNumber
	refID
)

func classifyRef(s string) (refKind, error) {
	if isTaskID(s) {
		return refID, nil
	}
	lo, hi, hyphen := strings.Cut(s, "-")
	from, err := strconv.Atoi(lo)
	if err != nil {
		return refNone, nil
	}
	if hyphen && hi == "" {
		return refNumber, fmt.Errorf("%q is a range with nothing after the dash; write both ends, as in \"2-5\"", s)
	}
	to := from
	if hyphen {
		if to, err = strconv.Atoi(hi); err != nil {
			return refNone, nil
		}
	}
	if from < 1 || to < 1 {
		return refNumber, fmt.Errorf("%q: the first task in a listing is 1", s)
	}
	if to < from {
		return refNumber, fmt.Errorf("%q runs backwards; write it as %q", s, fmt.Sprintf("%d-%d", to, from))
	}
	return refNumber, nil
}

const idMinLen = 16

const localIDBodyLen = 16

func isTaskID(s string) bool {
	if body, ok := strings.CutPrefix(s, store.LocalIDPrefix); ok {
		return len(body) == localIDBodyLen && isHexRun(body)
	}
	return len(s) >= idMinLen && isHexRun(s) && !isListingPosition(s)
}

func isHexRun(s string) bool {
	for _, c := range s {
		if !strings.ContainsRune("0123456789abcdefABCDEF", c) {
			return false
		}
	}
	return true
}

func isListingPosition(s string) bool {
	_, err := strconv.Atoi(s)
	return err == nil
}

func scopeName(status store.StatusFilter) string {
	switch status {
	case store.StatusOpen:
		return "the open tasks"
	case store.StatusDone:
		return "the completed tasks"
	default:
		return ""
	}
}

func projectNames(projects []model.Project) []string {
	names := make([]string, len(projects))
	for i, p := range projects {
		names[i] = p.Name
	}
	return names
}

const namesInAnError = 12

const nameInAnError = maxProject

func shortlist(names []string) ([]string, int) {
	if len(names) <= namesInAnError {
		return names, 0
	}

	return names[:namesInAnError:namesInAnError], len(names) - namesInAnError
}

func projectChoices(names []string) []string {
	lines := make([]string, len(names))
	for i, name := range names {
		lines[i] = escapeCell(name, Width)
	}
	return lines
}
