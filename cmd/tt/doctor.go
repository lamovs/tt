package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/movsar/tt/internal/api"
	"github.com/movsar/tt/internal/cli"
	"github.com/movsar/tt/internal/config"
	"github.com/movsar/tt/internal/model"
	"github.com/movsar/tt/internal/store"
	"github.com/movsar/tt/internal/sync"
)

const tokenEnvVar = "TT_TOKEN"

type checkStatus int

const (
	statusOK checkStatus = iota
	statusWarn
	statusFail

	statusSkipped
)

func (s checkStatus) label() string {
	switch s {
	case statusWarn:
		return "warn"
	case statusFail:
		return "fail"
	case statusSkipped:
		return "skip"
	default:
		return "ok"
	}
}

type checkResult struct {
	status  checkStatus
	summary string

	notes []string
}

const doctorUsage = "usage: tt doctor [--fix]\n"

const maxDoctorListed = 20

func cmdDoctor(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, args []string) int {
	data, refinements := splitArgs(args)
	if len(data) != 0 {
		fmt.Fprintln(stderr, quoteWord("tt: doctor: unexpected argument ", data[0]))
		fmt.Fprint(stderr, doctorUsage)
		return exitUsage
	}

	fix := false
	for _, r := range refinements {
		if r != "--fix" {
			fmt.Fprintln(stderr, quoteWord("tt: doctor: unknown option ", r))
			fmt.Fprint(stderr, doctorUsage)
			return exitUsage
		}
		fix = true
	}
	if fix {
		return doctorFix(ctx, stdin, stdout, stderr)
	}
	return runDoctorChecks(ctx, stdout)
}

type doctorCache struct {
	read        bool
	lists       int
	names       []string
	projects    []model.Project
	configWrite configWriteObservation
}

func runDoctorChecks(ctx context.Context, stdout io.Writer) int {
	return runDoctorChecksWithWriteProbe(ctx, stdout, observeConfigWrite)
}

func runDoctorChecksWithWriteProbe(ctx context.Context, stdout io.Writer, probe func() configWriteObservation) int {

	results := collectDoctorChecks(ctx, probe)

	if interrupted(ctx) {
		return exitInterrupted
	}

	return doctorReport(stdout, results)
}

func doctorReport(w io.Writer, results []checkResult) int {
	var ok, warn, fail, skipped int
	for _, r := range results {
		printCheck(w, r)
		switch r.status {
		case statusOK:
			ok++
		case statusWarn:
			warn++
		case statusFail:
			fail++
		case statusSkipped:
			skipped++
		}
	}
	fmt.Fprintf(w, "\n%d ok, %d warn, %d fail", ok, warn, fail)
	if skipped > 0 {
		fmt.Fprintf(w, ", %d skip", skipped)
	}
	fmt.Fprintln(w)
	if fail > 0 {
		return exitError
	}
	return exitOK
}

func printCheck(w io.Writer, r checkResult) {
	summary := doctorSummary(r.summary)
	fmt.Fprintf(w, "%-4s %s\n", r.status.label(), summary[0])
	for _, line := range summary[1:] {
		fmt.Fprintln(w, line)
	}
	for _, line := range r.notes {
		fmt.Fprintln(w, line)
	}
}

const (
	doctorIndent    = "     "
	doctorTextWidth = cli.Width - len(doctorIndent)

	doctorNestedIndent    = "       "
	doctorNestedTextWidth = cli.Width - len(doctorNestedIndent)
)

func doctorSummary(s string) []string {
	lines := cli.Wrap(s, doctorTextWidth)
	for i := 1; i < len(lines); i++ {
		lines[i] = doctorIndent + lines[i]
	}
	return lines
}

func fullReportAtom(s string) string {
	quoted := cli.Foreign(s, 2+quotedRuneMax*utf8.RuneCountInString(s))
	var atom strings.Builder
	atom.Grow(len(quoted))
	for i, r := range quoted {
		if i == 0 || i+utf8.RuneLen(r) == len(quoted) || !unicode.IsSpace(r) {
			atom.WriteRune(r)
			continue
		}
		switch {
		case r == ' ':
			atom.WriteString(`\x20`)
		case r <= 0xffff:
			fmt.Fprintf(&atom, `\u%04x`, r)
		default:
			fmt.Fprintf(&atom, `\U%08x`, r)
		}
	}
	return atom.String()
}

func doctorNote(s string) []string {
	return doctorIndented(doctorIndent, doctorTextWidth, s)
}

func doctorNested(s string) []string {
	return doctorIndented(doctorNestedIndent, doctorNestedTextWidth, s)
}

func doctorHint(s string) []string {
	return []string{doctorIndent + s}
}

func doctorIndented(indent string, w int, s string) []string {
	lines := cli.Wrap(s, w)
	for i, line := range lines {
		if line == "" {
			continue
		}
		lines[i] = indent + line
	}
	return lines
}

type doctorToken struct {
	value   string
	fromEnv bool
}

func (t doctorToken) where() string {
	if t.fromEnv {
		return "$" + tokenEnvVar
	}
	return "the token file"
}

const (
	loginHint      = `run "tt login"`
	loginTokenHint = `run "tt login token"`
)

func loginLines(cerr error) []string {
	return append(doctorHint(loginHint), blockedLoginLines(cerr)...)
}

type tokenFlaw int

const (
	tokenFlawNone tokenFlaw = iota
	tokenUnsendable
	tokenNotCredential
)

func inspectTokenValue(v string) (flaw tokenFlaw, kind string) {

	for i := 0; i < len(v); i++ {
		b := v[i]
		if (b < 0x20 && b != '\t') || b == 0x7f {
			switch {
			case b == '\n' || b == '\r':
				return tokenUnsendable, "a line break"
			case b == 0x00:
				return tokenUnsendable, "a NUL"
			default:
				return tokenUnsendable, "a control character"
			}
		}
	}

	for i := 0; i < len(v); i++ {
		b := v[i]
		switch {
		case b >= 'A' && b <= 'Z', b >= 'a' && b <= 'z', b >= '0' && b <= '9':
		case b == '-', b == '.', b == '_', b == '~', b == '+', b == '/', b == '=':
		default:
			return tokenNotCredential, ""
		}
	}
	return tokenFlawNone, ""
}

func unsendableTokenLines(kind string) []string {
	cause := "a file written half-way, or one that was never text, is what usually reads like this"
	if pastedAcrossLines(kind) {
		cause = "a token pasted across two lines is the usual way one gets in"
	}
	return doctorNote("nothing was sent: a control character cannot go in a header, and " + cause)
}

func unsendableEnvWayOut(kind string) []string {
	how := "to the token and nothing else"
	if pastedAcrossLines(kind) {
		how = "to the token on one line"
	}
	return doctorNote(fmt.Sprintf("set $%s %s, or unset it to fall back to the token file", tokenEnvVar, how))
}

func pastedAcrossLines(kind string) bool { return kind == "a line break" }

func notCredentialLines() []string {
	return doctorNote("a bearer token is written in letters, digits and the characters - . _ ~ + / = " +
		"and this value has something else in it; tt sends it as it stands and the server has the " +
		"last word, but an error page or the output of a command that did not produce a token is " +
		"what usually reads like this")
}

const tokenNotShapedClause = ", and what is in it is not shaped like a token"

type diskCredentialState struct {
	dir               credentialDirInfo
	dirErr            error
	token             api.TokenInfo
	tokenErr          error
	creds             appCredentials
	credsInfo         credentialFileInfo
	credsOK           bool
	credsErr          error
	tokenTargetDir    credentialDirInfo
	tokenTargetDirErr error
	credsTargetDir    credentialDirInfo
	credsTargetDirErr error
}

func inspectDiskCredentials() diskCredentialState {
	var d diskCredentialState
	d.dir, d.dirErr = inspectCredentialDir()
	d.token, d.tokenErr = api.LoadToken()
	d.creds, d.credsInfo, d.credsOK, d.credsErr = loadCredentialsInfo()
	if d.tokenErr == nil && d.token.PathVerified && d.token.ResolvedPath != d.token.Path {
		d.tokenTargetDir, d.tokenTargetDirErr = inspectCredentialDirAt(filepath.Dir(d.token.ResolvedPath))
	}
	if d.credsInfo.Read && d.credsInfo.PathVerified && d.credsInfo.ResolvedPath != d.credsInfo.Path {
		d.credsTargetDir, d.credsTargetDirErr = inspectCredentialDirAt(filepath.Dir(d.credsInfo.ResolvedPath))
	}
	return d
}

func resolvedCredential(path, resolved string, verified bool) string {
	if !verified {
		return fullReportAtom(path) + " (opened target could not be identified after the read)"
	}
	if resolved == path {
		return fullReportAtom(path)
	}
	return fmt.Sprintf("%s (target %s)", fullReportAtom(path), fullReportAtom(resolved))
}

func credentialPermissionNotes(d diskCredentialState) (notes []string, finding bool) {
	if d.dirErr != nil && !errors.Is(d.dirErr, fs.ErrNotExist) {
		finding = true
		notes = append(notes, doctorNote("the directory for saved credentials cannot be inspected: "+fullReportAtom(d.dirErr.Error()))...)
	} else if d.dirErr == nil && d.dir.Insecure {
		finding = true
		notes = append(notes, doctorNote(fmt.Sprintf("credential directory %s has mode %04o; want 0700 so other local accounts cannot inspect it",
			resolvedCredential(d.dir.Path, d.dir.ResolvedPath, d.dir.PathVerified), d.dir.Mode))...)
	}
	if d.tokenErr == nil && d.token.Insecure {
		finding = true
		notes = append(notes, doctorNote(fmt.Sprintf("saved token %s has mode %04o; want 0600 so other local accounts cannot read it",
			resolvedCredential(d.token.Path, d.token.ResolvedPath, d.token.PathVerified), d.token.Mode))...)
	}
	if d.credsInfo.Read && d.credsInfo.Insecure {
		finding = true
		notes = append(notes, doctorNote(fmt.Sprintf("saved app credentials %s have mode %04o; want 0600 so other local accounts cannot read them",
			resolvedCredential(d.credsInfo.Path, d.credsInfo.ResolvedPath, d.credsInfo.PathVerified), d.credsInfo.Mode))...)
	}
	for _, target := range []struct {
		name string
		dir  credentialDirInfo
		err  error
	}{
		{"saved token target", d.tokenTargetDir, d.tokenTargetDirErr},
		{"saved app credentials target", d.credsTargetDir, d.credsTargetDirErr},
	} {
		if target.err != nil {
			finding = true
			notes = append(notes, doctorNote(target.name+" directory cannot be inspected: "+fullReportAtom(target.err.Error()))...)
			continue
		}
		if target.dir.Path != "" && target.dir.Insecure {
			finding = true
			notes = append(notes, doctorNote(fmt.Sprintf("%s directory %s has mode %04o; want 0700 so other local accounts cannot inspect it",
				target.name, resolvedCredential(target.dir.Path, target.dir.ResolvedPath, target.dir.PathVerified), target.dir.Mode))...)
		}
	}
	return notes, finding
}

func unusedDiskCredentialNotes(d diskCredentialState) (notes []string, finding bool) {
	if d.tokenErr != nil {
		if !errors.Is(d.tokenErr, fs.ErrNotExist) || errors.Is(d.tokenErr, api.ErrUnusableLink) {
			finding = true
			notes = append(notes, doctorNote("the unused saved token is not usable: "+fullReportAtom(d.tokenErr.Error()))...)
		}
	} else {
		switch flaw, kind := inspectTokenValue(d.token.Value); {
		case d.token.Value == "":
			finding = true
			notes = append(notes, doctorNote("the unused saved token file is empty")...)
		case flaw == tokenUnsendable:
			finding = true
			notes = append(notes, doctorNote("the unused saved token has "+kind+" in it and cannot be sent")...)
		case flaw == tokenNotCredential:
			finding = true
			notes = append(notes, doctorNote("the unused saved token is not shaped like a bearer token")...)
		}
	}
	if d.credsErr != nil {
		if !errors.Is(d.credsErr, fs.ErrNotExist) || errors.Is(d.credsErr, api.ErrUnusableLink) {
			finding = true
			notes = append(notes, doctorNote("the unused saved app credentials are not usable: "+fullReportAtom(d.credsErr.Error()))...)
		}
	}
	permissionNotes, insecure := credentialPermissionNotes(d)
	return append(notes, permissionNotes...), finding || insecure
}

func checkToken() (checkResult, doctorToken) {
	if err := localAuthBlock(); err != nil {
		return checkResult{status: statusFail, summary: "token: local authorization is blocked", notes: doctorNote(fullReportAtom(err.Error()))}, doctorToken{}
	}
	disk := inspectDiskCredentials()
	if v := strings.TrimSpace(os.Getenv(tokenEnvVar)); v != "" {
		diskNotes, diskFinding := unusedDiskCredentialNotes(disk)
		switch flaw, kind := inspectTokenValue(v); flaw {
		case tokenUnsendable:
			notes := append(unsendableTokenLines(kind), unsendableEnvWayOut(kind)...)
			notes = append(notes, diskNotes...)
			return checkResult{
				status:  statusFail,
				summary: fmt.Sprintf("token: $%s has %s in it and cannot be sent", tokenEnvVar, kind),
				notes:   notes,
			}, doctorToken{}
		case tokenNotCredential:
			return checkResult{
				status:  statusWarn,
				summary: fmt.Sprintf("token: using $%s", tokenEnvVar) + tokenNotShapedClause,
				notes:   append(notCredentialLines(), diskNotes...),
			}, doctorToken{value: v, fromEnv: true}
		}
		status := statusOK
		if diskFinding {
			status = statusWarn
		}
		return checkResult{status: status, summary: fmt.Sprintf("token: using $%s", tokenEnvVar), notes: diskNotes}, doctorToken{value: v, fromEnv: true}
	}

	creds, ok, cerr := disk.creds, disk.credsOK, disk.credsErr
	info, err := disk.token, disk.tokenErr
	permissionNotes, insecure := credentialPermissionNotes(disk)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) && !errors.Is(err, api.ErrUnusableLink) {
			return checkResult{status: statusFail, summary: "token: not found", notes: append(permissionNotes, loginLines(cerr)...)}, doctorToken{}
		}
		return checkResult{status: statusFail, summary: "token: " + fullReportAtom(err.Error()), notes: append(permissionNotes, blockedLoginLines(cerr)...)}, doctorToken{}
	}

	if info.Value == "" {
		summary := fmt.Sprintf("token: %s is empty", fullReportAtom(info.Path))
		return checkResult{status: statusFail, summary: summary, notes: append(permissionNotes, loginLines(cerr)...)}, doctorToken{}
	}

	flaw, kind := inspectTokenValue(info.Value)
	if flaw == tokenUnsendable {
		notes := append(unsendableTokenLines(kind), permissionNotes...)
		notes = append(notes, loginLines(cerr)...)
		return checkResult{
			status:  statusFail,
			summary: fmt.Sprintf("token: %s has %s in it and cannot be sent", fullReportAtom(info.Path), kind),
			notes:   notes,
		}, doctorToken{}
	}

	status := statusOK
	summary := fmt.Sprintf("token: %s (mode %04o)", resolvedCredential(info.Path, info.ResolvedPath, info.PathVerified), info.Mode)
	notes := permissionNotes
	if insecure {
		status = statusWarn
	}

	needLogin := false
	if flaw == tokenNotCredential {
		status = statusWarn
		summary += tokenNotShapedClause
		notes = append(notes, notCredentialLines()...)
		needLogin = cerr == nil
	}

	switch {
	case cerr != nil:
		status = statusWarn
		notes = append(notes, credentialsUnreadableLines(cerr,
			"or removed first; whether a refresh token was saved cannot be told either")...)
	case !ok || !hasRefreshToken(creds.RefreshToken):

		notes = append(notes, doctorNote("no refresh token saved: once this token stops working, another login "+
			"will be needed because it cannot be renewed")...)
	}
	if needLogin {
		notes = append(notes, loginLines(cerr)...)
	}
	return checkResult{status: status, summary: summary, notes: notes}, doctorToken{value: info.Value}
}

func credentialsUnreadableLines(err error, tail string) []string {
	return doctorNote("the saved app credentials cannot be read: " + fullReportAtom(err.Error()) + "\n" +
		"\"tt login\" reads that same file and stops on it, so it has to be repaired " + tail)
}

func blockedLoginLines(cerr error) []string {
	if cerr == nil {
		return nil
	}
	return append(credentialsUnreadableLines(cerr, "or removed before that login can be run; a token you already have can be saved without reading that file:"),
		doctorHint(loginTokenHint)...)
}

var newAPIClient = func(token string) *api.Client { return managedAPIClient(token) }

const apiCheckCall = "GET /open/v1/project"

const allowProjectDropHint = `run "tt sync ` + sync.AllowProjectDropFlag + `"`

const apiDetailBudget = doctorTextWidth - len("(),")

func apiDetail(err error, sent string) string {
	var (
		lead string
		body string
	)
	var se *api.StatusError
	var de *api.DecodeError
	var large *api.ResponseTooLargeError
	switch {
	case errors.As(err, &se):
		lead = strconv.Itoa(se.StatusCode)
		body = se.Body
	case errors.As(err, &de) && errors.Is(de.Err, api.ErrIncompleteAnswer):
		lead = fmt.Sprintf("http %d: the answer carried nothing this call can use", de.StatusCode)
		body = de.Body
	case errors.As(err, &de):
		lead = fmt.Sprintf("http %d: the answer could not be decoded", de.StatusCode)
		body = de.Body
	case errors.As(err, &large):
		lead = fmt.Sprintf("http %d: over the %d byte limit this client reads", large.StatusCode, large.Limit)
	default:

		lead = "no detail this check can read"
	}
	if body == "" {
		return lead
	}
	if len(sent) >= 16 {
		body = strings.ReplaceAll(body, sent, "<the token tt sent>")
	}
	const separator = ": "
	return lead + separator + cli.Foreign(body, apiDetailBudget-len(lead)-len(separator))
}

func checkAPI(ctx context.Context, tok doctorToken, cache doctorCache) checkResult {
	if tok.value == "" {

		return checkResult{status: statusSkipped, summary: "api: not tried, there is no usable token"}
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	client := newAPIClient(tok.value)
	projects, err := client.ListProjects(ctx)
	if err == nil {

		if len(projects) > 0 {
			return checkResult{status: statusOK, summary: fmt.Sprintf("api: %s answered, %d list(s)", apiCheckCall, len(projects))}
		}

		if cache.read && cache.lists >= 2 {
			return checkResult{
				status: statusWarn,
				summary: fmt.Sprintf("api: %s answered with no lists at all while the cache holds %d list(s)",
					apiCheckCall, cache.lists),
				notes: append(doctorNote("tt sync refuses an answer that carries no lists rather than empty the "+
					"cache, so the refresh stops here and the cache keeps what it has; if you deleted those "+
					"lists yourself:"),
					doctorHint(allowProjectDropHint)...),
			}
		}
		return checkResult{status: statusOK, summary: fmt.Sprintf("api: %s answered, no lists at all", apiCheckCall)}
	}

	var se *api.StatusError
	if errors.As(err, &se) {
		switch {
		case se.StatusCode == 401:

			return checkResult{
				status: statusFail,
				summary: fmt.Sprintf("api: %s: the token was rejected (%s), check %s",
					apiCheckCall, apiDetail(err, tok.value), tok.where()),
			}
		case se.StatusCode == 403:

			return checkResult{
				status:  statusFail,
				summary: fmt.Sprintf("api: %s: the request was refused (%s)", apiCheckCall, apiDetail(err, tok.value)),
				notes: doctorNote("something refused this request with the token tt sent: TickTick, or whatever " +
					"is standing in for it on this network. A token whose scopes do not reach this endpoint, an " +
					"account locked out, and a proxy that refuses this host all answer this way, and the answer " +
					"does not say which"),
			}
		case se.StatusCode == 404:

			return checkResult{
				status:  statusFail,
				summary: fmt.Sprintf("api: %s: there is no such endpoint (%s)", apiCheckCall, apiDetail(err, tok.value)),
				notes: doctorNote("this is the endpoint every refresh starts from, and whatever answered for the " +
					"API says it does not have it - the server itself, or something standing in front of it on " +
					"this network; tt cannot sync while that stands"),
			}
		case se.StatusCode == 429:

			return checkResult{
				status: statusWarn,
				summary: fmt.Sprintf("api: %s: whatever answered asked tt to come back later (%s), tt works offline",
					apiCheckCall, apiDetail(err, tok.value)),
			}
		case se.StatusCode >= 500:
			return checkResult{
				status:  statusWarn,
				summary: fmt.Sprintf("api: %s: whatever answered failed (%s), tt works offline", apiCheckCall, apiDetail(err, tok.value)),
			}
		default:

			return checkResult{
				status:  statusWarn,
				summary: fmt.Sprintf("api: %s: unexpected answer (%s), tt works offline", apiCheckCall, apiDetail(err, tok.value)),
			}
		}
	}

	var de *api.DecodeError
	if errors.As(err, &de) {
		return checkResult{
			status: statusWarn,
			summary: fmt.Sprintf("api: %s: the answer could not be read (%s), tt works offline",
				apiCheckCall, apiDetail(err, tok.value)),
		}
	}
	var large *api.ResponseTooLargeError
	if errors.As(err, &large) {
		return checkResult{
			status: statusWarn,
			summary: fmt.Sprintf("api: %s: the answer is larger than tt reads (%s), tt works offline",
				apiCheckCall, apiDetail(err, tok.value)),
		}
	}

	return checkResult{
		status:  statusWarn,
		summary: "api: unreachable, tt works offline",
		notes: append(doctorNote("the failure as it came back, quoted and cut to at most three "+
			"lines of this report. Not all of it is tt's, none of it is advice from tt, and "+
			"none of it can act."),
			unreachableAnswerLines(err, tok.value)...),
	}
}

const apiAnswerMaxLines = 3

func unreachableAnswerLines(err error, sent string) []string {
	widths := make([]int, apiAnswerMaxLines)
	for i := range widths {
		widths[i] = doctorNestedTextWidth
	}
	budget := -(apiAnswerMaxLines - 1) * (quotedRuneMax - 1)
	for _, w := range widths {
		budget += w
	}
	answer := err.Error()
	if len(sent) >= 16 {
		answer = strings.ReplaceAll(answer, sent, "<the token tt sent>")
	}
	lines := cutToColumns(cli.Foreign(answer, budget), widths)
	for i := range lines {
		lines[i] = doctorNestedIndent + lines[i]
	}
	return lines
}

const cacheAsideNote = "what the server sent comes back on the next sync, but the outbox in the same " +
	"file does not - it is the one copy of the changes the server has not been told about yet, so move " +
	"this file aside rather than deleting it if you have to start over"

const (
	cacheDriverLead = "SQLite reports "
	cacheStoreLead  = "the store reports "
)

const cacheDriverBudget = doctorTextWidth - len(cacheDriverLead)

const cacheStoreBudget = doctorTextWidth - len(cacheStoreLead)

func checkCache(ctx context.Context) (checkResult, doctorCache) {

	var cache doctorCache

	path, err := store.DefaultPath()
	if err != nil {

		return checkResult{status: statusFail, summary: fmt.Sprintf("cache: cannot locate the cache: %v", err)}, cache
	}

	insp, err := store.Inspect(ctx, path)
	if err != nil {
		return cacheOpenVerdict(path, err), cache
	}
	defer insp.Close()
	st := insp.Store

	problems, sound, err := cacheIntegrity(ctx, st)
	if err != nil {

		return cacheOpenVerdict(path, err), cache
	}
	if !sound {
		return checkResult{
			status:  statusFail,
			summary: fmt.Sprintf("cache: %s: the database file is damaged", fullReportAtom(path)),
			notes:   append(integrityProblemLines(problems), doctorNote(cacheAsideNote)...),
		}, cache
	}

	found, hasMeta, hasRow, err := insp.Version(ctx)
	if err != nil {
		return cacheVersionUnreadable(path, err), cache
	}
	schema, done := cacheSchemaVerdict(path, found, hasMeta, hasRow, insp.SchemaLatest)
	if done {
		return schema, cache
	}

	status, notes := schema.status, schema.notes

	difference, err := st.CompareSchema(ctx, found)
	if err != nil {
		return cacheCompareFailure(path, err), cache
	}
	shape, done := cacheDifferenceVerdict(path, found, difference)
	if done {

		shape.notes = append(notes, shape.notes...)
		return shape, cache
	}
	if shape.status > status {
		status = shape.status
	}
	notes = append(notes, shape.notes...)

	foreignKeys, foreignErr := st.ForeignKeyViolations(ctx)
	if foreignErr != nil {
		return cacheReadFailure(path, "the declared relationships", cacheStoreLead, foreignErr), cache
	}
	uncachedProjects, projectReferenceErr := st.UncachedTaskProjects(ctx)
	if projectReferenceErr != nil {
		return cacheReadFailure(path, "the task-to-project references", cacheStoreLead, projectReferenceErr), cache
	}
	danglingItems, itemReferenceErr := st.DanglingItemTasks(ctx)
	if itemReferenceErr != nil {
		return cacheReadFailure(path, "the item-to-task references", cacheStoreLead, itemReferenceErr), cache
	}

	var tasks int
	if err := st.DB().QueryRowContext(ctx, `SELECT count(*) FROM tasks`).Scan(&tasks); err != nil {
		return cacheReadFailure(path, "the task count", cacheDriverLead, err), cache
	}
	names, lists, projects, err := cacheListNames(ctx, st)
	if err != nil {
		return cacheReadFailure(path, "the list names", cacheDriverLead, err), cache
	}
	cache = doctorCache{read: true, lists: lists, names: names, projects: projects}
	counts, err := st.OutboxCounts(ctx)
	if err != nil {
		return cacheReadFailure(path, "the outbox counts", cacheStoreLead, err), cache
	}

	var parkedCreates int
	if counts.Failed > 0 {
		if parkedCreates, err = st.FailedCreates(ctx); err != nil {
			return cacheReadFailure(path, "the parked creates", cacheStoreLead, err), cache
		}
	}

	orphans, err := st.OrphanedLocalTasks(ctx)
	if err != nil {
		return cacheReadFailure(path, "the rows made offline", cacheStoreLead, err), cache
	}
	parents, parentReferenceErr := st.UncachedTaskParents(ctx)
	if parentReferenceErr != nil {
		return cacheReadFailure(path, "the task-to-parent references", cacheStoreLead, parentReferenceErr), cache
	}

	age, syncNote, syncFinding := lastSyncAge(ctx, st)
	summary := fmt.Sprintf("cache: %s (schema v%d, %d task(s), %d list(s), synced %s, %d queued)",
		fullReportAtom(path), found, tasks, lists, age, counts.Pending+counts.Inflight)

	if syncNote != "" {
		if syncFinding {
			status = statusWarn
		}
		notes = append(notes, doctorNote(syncNote)...)
	}
	if counts.Failed > 0 {
		status = statusWarn

		notes = append(notes,
			doctorNote(fmt.Sprintf("%d outbox entry(s) failed and need attention", counts.Failed))...)

		if parkedCreates > 0 {
			notes = append(notes,
				doctorNote(fmt.Sprintf("%d parked create(s) among them: putting a create back in line can "+
					"make a second copy of the task there, and nothing lets the server spot the repeat",
					parkedCreates))...)
		}
		notes = append(notes, doctorHint(sync.RetryHint)...)
		notes = append(notes, doctorHint(dropParkedHint)...)
	}
	if counts.Unknown > 0 {
		status = statusWarn
		notes = append(notes, doctorNote(fmt.Sprintf("%d outbox entry(s) have an unknown state; they are not included in the queued or failed totals and doctor changes none of them",
			counts.Unknown))...)
	}
	if len(foreignKeys) > 0 {
		status = statusWarn
		notes = append(notes, doctorNote(fmt.Sprintf("%d declared foreign-key relationship(s) are broken in the cache", len(foreignKeys)))...)
		for _, v := range foreignKeys[:min(len(foreignKeys), maxDoctorListed)] {
			row := "without a rowid"
			if v.RowID.Valid {
				row = fmt.Sprintf("row %d", v.RowID.Int64)
			}
			notes = append(notes, doctorNested(fmt.Sprintf("%s %s references missing parent %s (constraint %d)",
				fullReportAtom(v.Table), row, fullReportAtom(v.Parent), v.Constraint))...)
		}
		if rest := len(foreignKeys) - maxDoctorListed; rest > 0 {
			notes = append(notes, doctorNested(fmt.Sprintf("and %d more, not listed here", rest))...)
		}
	}
	if len(danglingItems) > 0 {
		status = statusWarn
		notes = append(notes, doctorNote(fmt.Sprintf("%d checklist item(s) reference a task row that is absent", len(danglingItems)))...)
		for _, v := range danglingItems[:min(len(danglingItems), maxDoctorListed)] {
			notes = append(notes, doctorNested(fmt.Sprintf("item %s of task %s", fullReportAtom(v.ItemID), fullReportAtom(v.TaskID)))...)
		}
		if rest := len(danglingItems) - maxDoctorListed; rest > 0 {
			notes = append(notes, doctorNested(fmt.Sprintf("and %d more, not listed here", rest))...)
		}
	}
	if len(uncachedProjects) > 0 {
		status = statusWarn
		notes = append(notes, doctorNote(fmt.Sprintf("%d task(s) reference a project that is absent from this local cache; a partial or stale project cache can cause this and it does not prove server corruption",
			len(uncachedProjects)))...)
		for _, v := range uncachedProjects[:min(len(uncachedProjects), maxDoctorListed)] {
			notes = append(notes, doctorNested(fmt.Sprintf("task %s references uncached project %s", fullReportAtom(v.TaskID), fullReportAtom(v.ProjectID)))...)
		}
		if rest := len(uncachedProjects) - maxDoctorListed; rest > 0 {
			notes = append(notes, doctorNested(fmt.Sprintf("and %d more, not listed here", rest))...)
		}
	}
	if len(parents) > 0 {
		status = statusWarn
		notes = append(notes, doctorNote(fmt.Sprintf("%d task parent reference(s) point outside this local cache; a partial or stale cache can cause this and it does not prove server corruption", len(parents)))...)
		for _, ref := range parents[:min(len(parents), maxDoctorListed)] {
			source := "cached task"
			if ref.QueueSeq != 0 {
				source = fmt.Sprintf("queued change %d of task", ref.QueueSeq)
			}
			if ref.Baseline {
				source = fmt.Sprintf("baseline of queued change %d of task", ref.QueueSeq)
			}
			notes = append(notes, doctorNested(fmt.Sprintf("%s %s references uncached parent %s", source, fullReportAtom(ref.TaskID), fullReportAtom(ref.ParentID)))...)
		}
		if rest := len(parents) - maxDoctorListed; rest > 0 {
			notes = append(notes, doctorNested(fmt.Sprintf("and %d more, not listed here", rest))...)
		}
		notes = append(notes, doctorNote("Review the task and sync queue before changing a link. Doctor reports these references without repairing them.")...)
	}
	if len(orphans) > 0 {
		status = statusWarn

		notes = append(notes,
			doctorNote(fmt.Sprintf("%d task(s) made offline will never be sent: no create is left in the queue",
				len(orphans)))...)
		for _, t := range orphans[:min(len(orphans), maxDoctorListed)] {
			notes = append(notes, doctorNestedIndent+orphanedTaskLine(t, doctorNestedTextWidth))
		}

		if rest := len(orphans) - maxDoctorListed; rest > 0 {
			notes = append(notes, doctorNested(fmt.Sprintf("and %d more, not listed here", rest))...)
		}
		notes = append(notes,
			doctorNote("they are shown rather than removed: the row is the only copy tt has of what "+
				"was written, and the server may hold one as well - a create answered with an id that "+
				"looks local leaves the task on the server with tt unable to address it, and a cache "+
				"written before tt stopped leaving such rows behind has them for the plainer reason "+
				"that nothing swept them up")...)
	}
	return checkResult{status: status, summary: summary, notes: notes}, cache
}

func cacheOpenVerdict(path string, err error) checkResult {
	switch {
	case errors.Is(err, store.ErrNoCache):

		return checkResult{
			status:  statusSkipped,
			summary: fmt.Sprintf("cache: no cache at %s yet", fullReportAtom(path)),
			notes: doctorNote("nothing has been written into it yet, and doctor does not create one: " +
				"the first tt command that needs the cache does"),
		}
	case errors.Is(err, store.ErrNotAFile):
		return checkResult{
			status:  statusFail,
			summary: fmt.Sprintf("cache: %s is %s", fullReportAtom(path), cacheFileKind(path)),
			notes: doctorNote("doctor does not open it: a cache tt made is an ordinary file, and " +
				"opening what is not one can wait for ever - a read of a named pipe waits for whoever " +
				"writes to it, and a check that waits for ever has nothing to report"),
		}
	case errors.Is(err, store.ErrUnusableSidecar):
		return checkResult{
			status:  statusFail,
			summary: fmt.Sprintf("cache: %s has a sidecar doctor cannot trust", fullReportAtom(path)),
			notes: append(doctorNote("doctor refuses to report cache counts because SQLite can ignore an invalid WAL and answer from an older main file; "+
				"nothing was repaired or removed: "+fullReportAtom(err.Error())), doctorNote(cacheAsideNote)...),
		}
	case errors.Is(err, store.ErrCannotLook):

		reason := error(err)
		var perr *fs.PathError
		if errors.As(err, &perr) {
			reason = perr.Err
		}
		return checkResult{status: statusFail, summary: fmt.Sprintf("cache: cannot look at %s: %s", fullReportAtom(path), fullReportAtom(reason.Error()))}
	case errors.Is(err, store.ErrBadMigrations):

		return checkResult{
			status:  statusFail,
			summary: "cache: this tt cannot read its own migrations, so nothing was asked of the cache",
			notes: append(doctorNote(err.Error()),
				doctorNote(fmt.Sprintf("the file at %s was not opened and nothing here is a finding "+
					"about it: what is wrong is the tt doing the complaining", fullReportAtom(path)))...),
		}
	case runEnded(err):
		return cacheRunEnded(path)
	}
	return cacheUnreadable(path, err)
}

func runEnded(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

func cacheRunEnded(path string) checkResult {
	return checkResult{
		status:  statusFail,
		summary: fmt.Sprintf("cache: %s: the run ended before the check finished reading it", fullReportAtom(path)),
		notes: doctorNote("nothing here is a finding about the cache: the context this check was given " +
			"is done, and what was left to ask of the file was never asked - at the open that is all of " +
			"it, and further down it is whatever the check had not got to"),
	}
}

func cacheFileKind(path string) string {
	const plain = "not a regular file"
	fi, err := os.Stat(path)
	if err != nil {
		return plain
	}
	switch m := fi.Mode(); {
	case m.IsDir():
		return "a directory, " + plain
	case m&fs.ModeNamedPipe != 0:
		return "a named pipe, " + plain
	case m&fs.ModeSocket != 0:
		return "a socket, " + plain
	case m&fs.ModeDevice != 0:
		return "a device, " + plain
	}
	return plain
}

func cacheUnreadable(path string, err error) checkResult {
	notes := doctorNote(cacheDriverLead + cli.Foreign(err.Error(), cacheDriverBudget))
	if store.IsReadOnlyRefusal(err) {
		notes = append(notes, doctorNote("doctor opens the cache read-only and writes nothing to it: "+
			"SQLite answers this way when reading the file would take a write it is not allowed - the "+
			"scratch files it needs beside the cache, which is what a directory this user cannot write "+
			"looks like from in here, or a journal beside it that would have to be rolled back first")...)
	}
	return checkResult{status: statusFail, summary: fmt.Sprintf("cache: cannot read %s", fullReportAtom(path)), notes: notes}
}

func cacheIntegrity(ctx context.Context, st *store.Store) (problems []string, sound bool, err error) {
	rows, err := st.DB().QueryContext(ctx, `PRAGMA integrity_check`)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {

			problems = append(problems, err.Error())
			break
		}
		problems = append(problems, s)
	}
	answered := rows.Err()
	if answered != nil && len(problems) == 0 {
		problems = append(problems, answered.Error())
	}
	return problems, answered == nil && len(problems) == 1 && problems[0] == "ok", nil
}

func integrityProblemLines(problems []string) []string {
	if len(problems) == 0 {
		return doctorNote("SQLite answered the check with neither a problem nor the word ok")
	}
	lines := doctorNote(fmt.Sprintf("SQLite reports %d problem(s) with it:", len(problems)))
	for _, p := range problems[:min(len(problems), maxDoctorListed)] {
		lines = append(lines, doctorNestedIndent+cli.Foreign(p, doctorNestedTextWidth))
	}
	if rest := len(problems) - maxDoctorListed; rest > 0 {
		lines = append(lines, doctorNested(fmt.Sprintf("and %d more, not listed here", rest))...)
	}
	return lines
}

func cacheVersionUnreadable(path string, err error) checkResult {
	if runEnded(err) {
		return cacheRunEnded(path)
	}
	return checkResult{
		status:  statusFail,
		summary: fmt.Sprintf("cache: %s: the schema version in its meta table could not be read", fullReportAtom(path)),
		notes:   doctorNote(cacheStoreLead + cli.Foreign(err.Error(), cacheStoreBudget)),
	}
}

func cacheReadFailure(path, what, lead string, err error) checkResult {
	if runEnded(err) {
		return cacheRunEnded(path)
	}
	return checkResult{
		status:  statusFail,
		summary: fmt.Sprintf("cache: %s: %s could not be read", fullReportAtom(path), what),
		notes:   doctorNote(lead + cli.Foreign(err.Error(), doctorTextWidth-len(lead))),
	}
}

func cacheCompareFailure(path string, err error) checkResult {
	switch {
	case runEnded(err):
		return cacheRunEnded(path)
	case errors.Is(err, store.ErrBadReferenceSchema):
		return checkResult{
			status: statusFail,
			summary: "cache: this tt could not build the schema it compares a cache against, " +
				"so no comparison was made",

			notes: append(doctorNote(cacheStoreLead+cli.Foreign(
				strings.TrimPrefix(err.Error(), store.ErrBadReferenceSchema.Error()+": "),
				cacheStoreBudget)),
				doctorNote(fmt.Sprintf("nothing here is a finding about %s: what would not build is a "+
					"schema this build makes in memory out of its own migrations, and nothing the file "+
					"said is in question", fullReportAtom(path)))...),
		}
	}
	return cacheReadFailure(path, "the schema the file holds", cacheStoreLead, err)
}

func cacheSchemaVerdict(path string, found int, hasMeta, hasRow bool, latest int) (checkResult, bool) {
	switch {
	case !hasMeta:
		return checkResult{
			status:  statusFail,
			summary: fmt.Sprintf("cache: %s has no meta table, so it is not a cache tt wrote", fullReportAtom(path)),
		}, true
	case !hasRow:
		return checkResult{
			status:  statusFail,
			summary: fmt.Sprintf("cache: %s carries no schema version: its meta table has no schema_version row", fullReportAtom(path)),
		}, true
	case found < 1:

		return checkResult{
			status: statusFail,
			summary: fmt.Sprintf("cache: %s: its meta table gives the schema version as %d, "+
				"and this build describes 1..%d", fullReportAtom(path), found, latest),
			notes: doctorNote("nothing tt writes puts a number below 1 there, so the file was edited by " +
				"hand, half-restored from a backup or written by something that is not tt - and the next " +
				"command that opens the cache does not put it right: the migration step applies every " +
				"migration above the version it finds, so it would run the first one over tables that " +
				"are already there and stop on the first table it cannot create twice"),
		}, true
	case found > latest:

		return checkResult{
			status:  statusFail,
			summary: fmt.Sprintf("cache: %s (schema v%d, newer than this build reads)", fullReportAtom(path), found),
			notes: doctorNote((&store.VersionError{
				Found: found, Supported: latest, Path: fullReportAtom(path),
			}).Error()),
		}, true
	case found < latest:

		return checkResult{
			status: statusWarn,
			notes: doctorNote(fmt.Sprintf("this cache is at schema v%d and this build of tt carries v%d: "+
				"doctor does not apply the migration, the next command that opens the cache does - and "+
				"after that a tt older than this one will refuse the file", found, latest)),
		}, false
	}
	return checkResult{status: statusOK}, false
}

func cacheDifferenceVerdict(path string, v int, d store.SchemaDifference) (checkResult, bool) {
	if len(d.Blocking()) > 0 {

		notes := doctorNote(fmt.Sprintf("compared against what this build's migrations 1..%d produce, "+
			"%d thing(s) are missing:", v, len(d.Missing)))
		notes = append(notes, schemaObjectLines(d.Missing)...)
		if len(d.Extra) > 0 {
			notes = append(notes, doctorNote(fmt.Sprintf("and %d thing(s) are there that they do not produce:",
				len(d.Extra)))...)
			notes = append(notes, schemaObjectLines(d.Extra)...)
		}
		notes = append(notes, doctorNote("the version is written in the cache's own meta table and nothing "+
			"checks it against the tables as the cache is opened, so a file edited by hand, half-restored "+
			"from a backup or written by something that is not tt carries a version its tables do not "+
			"implement - and so does a cache tt itself wrote, if a migration body in this build was "+
			"changed after it shipped")...)
		notes = append(notes, doctorNote(cacheAsideNote)...)
		return checkResult{
			status:  statusFail,
			summary: fmt.Sprintf("cache: %s: it does not have everything schema v%d describes", fullReportAtom(path), v),
			notes:   notes,
		}, true
	}

	var notes []string
	if len(d.Missing) > 0 {
		notes = append(notes, doctorNote(fmt.Sprintf("%d thing(s) schema v%d has are missing, none of them "+
			"a table or a column:", len(d.Missing), v))...)
		notes = append(notes, schemaObjectLines(d.Missing)...)

		notes = append(notes, doctorNote("what is missing is not something an answer is read out of: "+
			"an index is a plan rather than an answer, and tt reads the same rows without it, "+
			"more slowly")...)
	}
	if len(d.Extra) > 0 {
		notes = append(notes, doctorNote(fmt.Sprintf("this cache has %d thing(s) that this build's "+
			"migrations 1..%d do not produce:", len(d.Extra), v))...)
		notes = append(notes, schemaObjectLines(d.Extra)...)
		notes = append(notes, doctorNote("these extra objects were found and listed; this check has not "+
			"established their effect on ordinary operations")...)
	}
	if len(notes) == 0 {
		return checkResult{status: statusOK}, false
	}
	return checkResult{status: statusWarn, notes: notes}, false
}

func schemaObjectLines(objects []store.SchemaObject) []string {
	var lines []string
	for _, o := range objects[:min(len(objects), maxDoctorListed)] {
		lines = append(lines, doctorNestedIndent+o.Kind+" "+
			cli.Foreign(o.Name, doctorNestedTextWidth-len(o.Kind)-1))
	}
	if rest := len(objects) - maxDoctorListed; rest > 0 {
		lines = append(lines, doctorNested(fmt.Sprintf("and %d more, not listed here", rest))...)
	}
	return lines
}

func cacheListNames(ctx context.Context, st *store.Store) (names []string, lists int, projects []model.Project, err error) {
	rows, err := st.DB().QueryContext(ctx, `SELECT name, id, kind, closed FROM projects ORDER BY name`)
	if err != nil {
		return nil, 0, nil, err
	}
	defer rows.Close()
	for rows.Next() {

		var name, id, kind sql.NullString
		var closed sql.NullBool
		if err := rows.Scan(&name, &id, &kind, &closed); err != nil {
			return nil, 0, nil, err
		}
		projects = append(projects, model.Project{Id: id.String, Name: name.String, Kind: kind.String, Closed: closed.Bool})
		lists++
		if name.Valid && name.String != "" {
			names = append(names, name.String)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, 0, nil, err
	}
	return names, lists, projects, nil
}

func orphanedTaskLine(t store.OrphanedLocalTask, w int) string {
	id := cli.ReportTitle(t.ID, 2+quotedRuneMax*utf8.RuneCountInString(t.ID))
	tail := fmt.Sprintf(" (%s)", id)
	if !t.CreatedTime.IsZero() {
		tail = fmt.Sprintf(" (%s, created %s)", id, stamp(t.CreatedTime.Time))
	}
	if t.Title == "" {
		return "(no title)" + tail
	}
	return cli.ReportTitle(t.Title, w-len(tail)) + tail
}

const durationHorizon = time.Duration(math.MaxInt64)

var doctorNow = time.Now

func lastSyncAge(ctx context.Context, st *store.Store) (age, note string, finding bool) {
	v, ok, err := st.Meta(ctx, sync.LastSyncKey)
	switch {
	case runEnded(err):

		return "not asked", fmt.Sprintf("the %s stamp was not asked for: the context this check was "+
			"given is done, and nothing here is a finding about the cache", sync.LastSyncKey), false
	case err != nil:
		return "unknown", lastSyncReadFailure(err), true
	case !ok:
		return "never", "", false
	}

	t, perr := model.ParseStoreTime(v)
	if perr != nil || t.IsZero() {
		return "unknown", fmt.Sprintf("the %s stamp in the cache reads %s, which is not a timestamp",
			sync.LastSyncKey, cli.ReportTitle(v, doctorTextWidth-1)), true
	}

	now := doctorNow()
	switch {
	case t.Time.After(now.Add(durationHorizon)):
		return "in the future", fmt.Sprintf("the %s stamp reads %s, which is further ahead of this clock "+
			"than tt measures an age over, so a scheduled sync is not due until the clock catches up",
			sync.LastSyncKey, cli.ReportTitle(v, doctorTextWidth-1)), true
	case t.Time.Before(now.Add(-durationHorizon)):

		return "further back than this clock measures", fmt.Sprintf("the %s stamp reads %s, which is "+
			"further behind this clock than tt measures an age over: nothing tt writes puts a stamp "+
			"there, so something else wrote it or this clock has moved",
			sync.LastSyncKey, cli.ReportTitle(v, doctorTextWidth-1)), true
	}
	switch since := now.Sub(t.Time); {
	case since >= time.Second:

		return model.Duration(since.Truncate(time.Second)).String() + " ago", "", false
	case since > -time.Second:

		return "just now", "", false
	default:
		return "in the future", fmt.Sprintf("the %s stamp is %s ahead of this clock, so a scheduled sync is not due until the clock catches up",
			sync.LastSyncKey, model.Duration((-since).Truncate(time.Second))), true
	}
}

func lastSyncReadFailure(err error) string {
	return cacheStoreLead + cli.Foreign(err.Error(), cacheStoreBudget)
}

const defaultProjectKey = "default_project"

const configJoinedBudget = doctorTextWidth - 1

const configRefusedBudget = doctorTextWidth - 1

const configProblemBudget = doctorTextWidth

type configOrigin int

const (
	originFile configOrigin = iota

	originSilentFile

	originNoFile

	originBrokenLink

	originNoFileNoWrite
)

func checkConfig(cache doctorCache) checkResult {
	path, err := config.Path()
	if err != nil {
		return checkResult{status: statusFail, summary: "config: " + err.Error()}
	}

	info, statErr := os.Lstat(path)
	switch {
	case errors.Is(statErr, fs.ErrNotExist):
		return configNotThere(path, cache)
	case statErr != nil:
		return checkResult{status: statusFail, summary: "config: " + fullReportAtom(statErr.Error())}
	case info.Mode()&fs.ModeSymlink != 0:

		if _, serr := os.Stat(path); serr != nil {
			if errors.Is(serr, fs.ErrNotExist) {
				return configLinkWithNoTarget(path, cache)
			}
			return checkResult{status: statusFail, summary: "config: " + fullReportAtom(serr.Error())}
		}
	}

	cfg, err := config.LoadFile(path)
	if err != nil {
		result := configUnusable(path, err)
		if cache.configWrite.refused() {
			result.notes = append(result.notes,
				configWriteRefusal(cache.configWrite.anchor, cache.configWrite.err, true)...)
		}
		return result
	}

	opened, origin := []config.DoubleOpen(nil), originFile
	if src, rerr := os.ReadFile(path); rerr == nil {
		opened = config.DoubleOpenedTables(src)
		if !config.SetsRootKey(src, defaultProjectKey) {
			origin = originSilentFile
		}
	}

	status := statusOK
	clauses := []string{"found, tt reads it"}
	var notes []string
	if cache.configWrite.refused() {
		status = statusWarn
		clauses = append(clauses, "its target directory refused a config-write probe")
		notes = append(notes, configWriteRefusal(cache.configWrite.anchor, cache.configWrite.err, true)...)
	}
	if len(opened) > 0 {
		status = statusWarn
		clauses = append(clauses, doubleOpenClause(opened))
		notes = append(notes, doubleOpenNotes(opened, cache.configWrite.refused())...)
	}
	clause, settings, warn := defaultProjectVerdict(cache, cfg.DefaultProject, origin)
	if cfg.DefaultFocus.TaskID != "" {
		focusClause, focusNotes, focusWarn := defaultFocusVerdict(cfg.DefaultFocus)
		clauses = append(clauses, focusClause)
		settings = append(settings, focusNotes...)
		warn = warn || focusWarn
	}
	if warn {
		status = statusWarn
	}
	return checkResult{
		status:  status,
		summary: configSummary(path, append(clauses, clause)),
		notes:   append(notes, settings...),
	}
}

func configSummary(path string, clauses []string) string {
	return fmt.Sprintf("config: %s (%s)", fullReportAtom(path), strings.Join(clauses, ", "))
}

func configNotThere(path string, cache doctorCache) checkResult {
	status := statusOK
	clauses := []string{"not found, using defaults"}
	origin := originNoFile
	var notes []string
	if cache.configWrite.refused() {
		status = statusWarn
		clauses = append(clauses, "its target directory refused a config-write probe")
		notes = append(notes,
			configWriteRefusal(cache.configWrite.anchor, cache.configWrite.err, false)...)
		origin = originNoFileNoWrite
	}
	clause, settings, warn := defaultProjectVerdict(cache, config.Default().DefaultProject, origin)
	if warn {
		status = statusWarn
	}
	return checkResult{
		status:  status,
		summary: configSummary(path, append(clauses, clause)),
		notes:   append(notes, settings...),
	}
}

func configLinkWithNoTarget(path string, cache doctorCache) checkResult {
	clauses := []string{"a symbolic link with no target, using defaults"}
	notes := configLinkNotes(path, cache.configWrite.refused())
	if cache.configWrite.refused() {
		clauses = append(clauses, "its target directory refused a config-write probe")
		notes = append(notes,
			configWriteRefusal(cache.configWrite.anchor, cache.configWrite.err, false)...)
	}

	clause, settings, _ := defaultProjectVerdict(cache, config.Default().DefaultProject, originBrokenLink)
	return checkResult{
		status:  statusWarn,
		summary: configSummary(path, append(clauses, clause)),
		notes:   append(notes, settings...),
	}
}

func configLinkNotes(path string, writeRefused bool) []string {
	notes := doctorNote("a symbolic link is at that path and following it reaches no file, so nothing " +
		"is read from it and every setting is the default")

	if target, err := os.Readlink(path); err == nil {
		notes = append(notes, doctorNote("it points at "+fullReportAtom(target))...)
	}
	if writeRefused {
		notes = append(notes, doctorNote("\"tt config --init\" refuses a path that has something at it")...)
	} else {
		notes = append(notes, doctorNote("creating the file at the end of that link, or removing the link, is "+
			"what settles it; --fix writes to the file at the end of it and leaves the link alone, while "+
			"\"tt config --init\" refuses a path that has something at it")...)
	}
	return notes
}

func configUnusable(path string, err error) checkResult {
	var cerr *config.Error
	if !errors.As(err, &cerr) {
		reason := err.Error()
		var perr *fs.PathError
		if errors.As(err, &perr) && perr.Path == path {
			reason = perr.Err.Error()
		}
		return checkResult{
			status:  statusFail,
			summary: fmt.Sprintf("config: %s: cannot be read", fullReportAtom(path)),
			notes:   doctorNote(fullReportAtom(reason)),
		}
	}
	var notes []string
	for _, p := range cerr.Problems {

		lead := ""
		if p.Line > 0 {
			lead = fmt.Sprintf("line %d: ", p.Line)
		}
		notes = append(notes, doctorNote(lead+cli.Foreign(p.Msg, configProblemBudget-len(lead)))...)
	}
	return checkResult{status: statusFail, summary: fmt.Sprintf("config: %s: invalid", fullReportAtom(path)), notes: notes}
}

func doubleOpenClause(opened []config.DoubleOpen) string {
	names := make([]string, 0, len(opened))
	for _, o := range opened {
		names = append(names, o.Table)
	}
	return "the " + andList(names) + " settings are not strict TOML 1.0"
}

func andList(words []string) string {
	if len(words) < 2 {
		return strings.Join(words, "")
	}
	return strings.Join(words[:len(words)-1], ", ") + " and " + words[len(words)-1]
}

func doubleOpenNotes(opened []config.DoubleOpen, writeRefused bool) []string {
	var notes []string
	for _, o := range opened {
		header := "[" + o.Table + "]"
		notes = append(notes, doctorNote(fmt.Sprintf("%s is opened twice: a dotted key at the root opens "+
			"it where it stands, and a %s header further down opens it again", header, header))...)
		keys := make([]string, 0, len(o.Dotted))
		for _, k := range o.Dotted {
			keys = append(keys, cli.Foreign(k, configJoinedBudget))
		}
		notes = append(notes, doctorNote("written as dotted keys at the root: "+strings.Join(keys, ", "))...)
		if !writeRefused {
			notes = append(notes, doctorNote("moving those under the "+header+" header by hand is what settles it")...)
		}
	}
	return append(notes, doctorNote(fmt.Sprintf("TOML 1.0 allows a table to be opened only once, so tt reads "+
		"this file and a strict reader does not - python's tomllib refuses it with \"Cannot declare "+
		"('%s',) twice\"", opened[0].Table))...)
}

func defaultProjectVerdict(cache doctorCache, name string, origin configOrigin) (clause string, notes []string, warn bool) {
	id, query, refErr := config.ProjectReference(name)
	if refErr != nil {
		return "default_project is invalid", doctorNote(refErr.Error()), true
	}
	if id != "" && cache.read && cache.lists > 0 {
		p, err := cli.ResolveDefaultProject(cache.projects, name)
		if err != nil {
			return "default_project is unavailable in the cache", doctorNote(err.Error()), true
		}
		return "default_project matches one list by ID", doctorNote("Current cached name: " + configListNames([]string{p.Name}) + "; ID selection survives a rename. Server availability is not verified by the cache."), false
	}

	value := cli.Foreign(name, doctorTextWidth)
	switch {
	case !cache.read:

		return "default_project not checked", doctorNote(fmt.Sprintf("default_project is %s and was not "+
			"checked: the cache check above did not get as far as the list names", value)), false
	case len(cache.names) == 0 && cache.lists > 0:

		return "default_project not checked", doctorNote(fmt.Sprintf("default_project is %s and was not "+
			"checked: the cache holds %d list(s) and not one of them carries a name, so there is no "+
			"name here for it to match; a sync is what fills them in", value, cache.lists)), false
	case len(cache.names) == 0:
		return "default_project not checked", doctorNote(fmt.Sprintf("default_project is %s and was not "+
			"checked: the cache holds no lists yet, and a sync is what fills it", value)), false
	}

	switch kind, matches := cli.MatchListName(cache.names, query); kind {
	case cli.ListNameOne:
		if len(cache.projects) > 0 {
			if _, err := cli.ResolveDefaultProject(cache.projects, name); err != nil {
				return "default_project is unusable in the cache", doctorNote(err.Error()), true
			}
		}

		return "default_project matches one list", nil, false
	case cli.ListNameSeveral:
		notes = doctorNote(fmt.Sprintf("default_project is %s and %d lists match it: %s",
			value, len(matches), configListNames(matches)))
		return fmt.Sprintf("default_project matches %d lists", len(matches)),
			append(notes, doctorNote("\"tt add\" refuses a name that matches more than one list; the name "+
				"must resolve to one cached list, and an exact name shared by several lists is still ambiguous")...), true
	}

	notes = doctorNote(fmt.Sprintf("default_project is %s and no list in the cache has that name", value))
	if sentence := originSentence(origin); sentence != "" {
		notes = append(notes, doctorNote(sentence)...)
	}

	notes = append(notes, doctorNote("the list names in the cache: "+configListNames(cache.names))...)
	if unnamed := cache.lists - len(cache.names); unnamed > 0 {
		notes = append(notes, doctorNote(fmt.Sprintf("%d cache row(s) counted above carry no name and cannot be listed",
			unnamed))...)
	}
	notes = append(notes, doctorNote("that name is what \"tt add\" uses when no list is given, so it fails "+
		"the same way until the name or the list changes")...)
	notes = append(notes, doctorNote("a list renamed on the server reaches the cache with a sync")...)
	notes = append(notes, doctorNote("The cache may be stale; this does not confirm removal on the server. Choose a stable ID with tt config default-project.")...)
	if out := originWayOut(origin, cache.configWrite.refused()); out != "" {
		notes = append(notes, doctorNote(out)...)
	}
	return "default_project matches no list", notes, true
}

func originSentence(origin configOrigin) string {
	switch origin {
	case originSilentFile:
		return "the config file does not set it, so this is the name tt ships with"
	case originNoFile, originNoFileNoWrite:

		return "nothing is at that path to set it, so this is the name tt ships with"
	case originBrokenLink:

		return "nothing tt can read sets it, so this is the name tt ships with"
	}
	return ""
}

func originWayOut(origin configOrigin, writeRefused bool) string {
	if writeRefused {
		return ""
	}
	switch origin {
	case originNoFile:
		return "\"tt config --init\" writes a starter config at that path; that key set to one of those " +
			"names in it settles it"
	case originBrokenLink, originNoFileNoWrite:
		return ""
	}
	return "that key set to one of those names, in the file above, settles it"
}

func configListNames(names []string) string {
	shown := names[:min(len(names), maxDoctorListed)]
	quoted := make([]string, 0, len(shown))
	for _, name := range shown {
		quoted = append(quoted, cli.Foreign(name, configJoinedBudget))
	}
	line := strings.Join(quoted, ", ")
	if rest := len(names) - len(shown); rest > 0 {
		line += fmt.Sprintf(" and %d more", rest)
	}
	return line
}

var (
	errNotADirectory     = errors.New("is not a directory, so no directory can be made at it")
	errDanglingComponent = errors.New("is a symbolic link that leads nowhere, so no directory can be made at it")
)

var errNotProbed = errors.New("the walk stopped above the config directory without asking")

type configWriteObservation struct {
	state  configWriteState
	anchor string
	err    error
}

type configWriteState uint8

const (
	configWriteUnknown configWriteState = iota
	configWriteSucceeded
	configWriteRefused
)

func (o configWriteObservation) refused() bool {
	return o.state == configWriteRefused
}

func observeConfigWrite() configWriteObservation {
	return observeConfigWriteWithProbe(configWritable)
}

func observeConfigWriteWithProbe(probe func(string) (string, error)) configWriteObservation {
	path, err := config.Path()
	if err != nil {
		return configWriteObservation{state: configWriteUnknown}
	}
	target, err := resolveConfigPath(path)
	if err != nil {
		return configWriteObservation{state: configWriteUnknown}
	}
	return observeConfigWriteAtWithProbe(target, probe)
}

func observeConfigWriteAt(target string) configWriteObservation {
	return observeConfigWriteAtWithProbe(target, configWritable)
}

func observeConfigWriteAtWithProbe(target string, probe func(string) (string, error)) configWriteObservation {
	anchor, err := probe(target)
	switch {
	case err == nil:
		return configWriteObservation{state: configWriteSucceeded, anchor: anchor}
	case errors.Is(err, errNotProbed):
		return configWriteObservation{state: configWriteUnknown, anchor: anchor}
	default:
		return configWriteObservation{state: configWriteRefused, anchor: anchor, err: err}
	}
}

func configWritable(path string) (string, error) {
	dir := filepath.Dir(path)
	floor := filepath.Dir(dir)
	for {
		_, err := os.Lstat(dir)
		switch {
		case errors.Is(err, fs.ErrNotExist):

			if dir == floor {
				return dir, errNotProbed
			}
			dir = filepath.Dir(dir)
			continue
		case err != nil:
			return dir, err
		}
		info, serr := os.Stat(dir)
		switch {
		case errors.Is(serr, fs.ErrNotExist):
			return dir, errDanglingComponent
		case serr != nil:
			return dir, serr
		case !info.IsDir():
			return dir, errNotADirectory
		}

		f, cerr := os.CreateTemp(dir, ".tt-doctor-*")
		if cerr != nil {
			return dir, cerr
		}

		f.Close()
		os.Remove(f.Name())
		return dir, nil
	}
}

func configWriteRefusal(anchor string, err error, existing bool) []string {
	dir := fullReportAtom(anchor)
	reason := dir + " " + err.Error()
	var perr *fs.PathError
	if errors.As(err, &perr) {
		reason = dir + ": " + perr.Err.Error()
	}
	var notes []string
	if existing {
		notes = doctorNote("the config path exists, but a probe for a replacement file in its target " +
			"directory was refused: " + reason)
		notes = append(notes, doctorNote("what is at that path was left unchanged; a later attempt "+
			"rechecks the target directory instead of relying on this observation")...)
	} else {
		notes = doctorNote("no config can be read at that path, and its target directory refused a probe " +
			"for a file that could become one: " + reason)
		notes = append(notes, doctorNote("until that changes tt has no readable config and cannot be given "+
			"one: every setting is the default, and a config written by hand or by any tt command is "+
			"refused in that directory")...)
	}

	switch {
	case errors.Is(err, errDanglingComponent):
		return append(notes, doctorNote("making the directory that link points at, or removing the link, "+
			"is what settles it")...)
	case errors.Is(err, errNotADirectory):
		return append(notes, doctorNote("moving what is at that path out of the way, or pointing "+
			"$XDG_CONFIG_HOME at a directory that can be written, is what settles it")...)
	}
	return append(notes, doctorNote("making that directory writable, or pointing $XDG_CONFIG_HOME at one "+
		"that can be written, is what settles it")...)
}

func checkNotify() checkResult {
	return checkNotifyWithConfigWrite(configWriteObservation{state: configWriteUnknown})
}

func checkNotifyWithConfigWrite(write configWriteObservation) checkResult {
	cfg, err := config.Load()
	if err != nil {
		var cerr *config.Error
		if errors.As(err, &cerr) {
			return checkResult{status: statusWarn, summary: "notify: config is invalid, see the config check above"}
		}
		return checkResult{status: statusWarn, summary: "notify: config cannot be read, see the config check above"}
	}
	if !shellAvailable() {

		return checkResult{status: statusWarn, summary: "notify: " + noShellNotice(cfg)}
	}

	unchecked := uncheckedCommands(cfg)
	remarks := append(quotedPlaceholderLines(unchecked, write.refused()), openOptionListLines(unchecked)...)
	if len(unchecked) > 0 {

		remarks = append(uncheckedRuleLines(), remarks...)
	}

	machine := thisMachine()

	src := configSource()

	groups := notifyGroups(notifySettingOutcomes(cfg))
	lead := notifySummaryGroup(groups)
	report := notifyReport{
		src:                 src,
		machine:             machine,
		onEndSet:            cfg.Timer.OnEnd != "",
		onEndCommand:        cfg.Timer.OnEnd,
		readyProgramMissing: readyProgramNotHere(groups, machine),
		writeRefused:        write.refused(),
	}

	notes := notifyOutcomeLines(groups[lead], report)
	for i, g := range groups {
		if i == lead {
			continue
		}
		notes = append(notes, doctorNote(notifyNotice(g, machine))...)
		notes = append(notes, notifyOutcomeLines(g, report)...)
	}
	notes = append(notes, remarks...)
	notes = append(notes, notifyWayOutLines(groups[lead], report)...)

	return checkResult{
		status:  groups[lead].kind.status(),
		summary: "notify: " + notifyNotice(groups[lead], machine),
		notes:   notes,
	}
}

func noShellNotice(cfg config.Config) string {
	var keys []string
	for _, cmd := range notifyCommands(cfg) {
		if cmd.command != "" {
			keys = append(keys, cmd.key)
		}
	}
	if len(keys) == 0 {
		return noShellSentence
	}
	list, plural := notifyKeyList(keys)
	holds := "holds"
	if plural {
		holds = "hold"
	}
	return noShellSentence + "; " + list + " " + holds + " one"
}

const noShellSentence = "tt runs a notification command through sh, and there is none on this PATH"

type notifyOutcomeKind int

const (
	notifyUnresolved notifyOutcomeKind = iota
	notifyUnset
	notifyNoBus
	notifyUnanswered
	notifyUnchecked
	notifyResolves
)

func (k notifyOutcomeKind) status() checkStatus {
	if k == notifyResolves {
		return statusOK
	}
	return statusWarn
}

type notifySettingOutcome struct {
	key     string
	kind    notifyOutcomeKind
	program string
	sys     string
	ready   bool
}

type notifyGroup struct {
	notifySettingOutcome
	keys []string
}

func notifySettingOutcomes(cfg config.Config) []notifySettingOutcome {
	var out []notifySettingOutcome
	for _, cmd := range notifyCommands(cfg) {
		switch {
		case cmd.command != "":
			out = append(out, notifyOutcomeFor(cmd))
		case cmd.key == onEndKey:
			out = append(out, notifySettingOutcome{key: cmd.key, kind: notifyUnset})
		}
	}
	return out
}

func notifyOutcomeFor(cmd notifyCommand) notifySettingOutcome {
	word := notifyFirstWord(cmd.command)
	if isReadyCommand(cmd.command) {
		out := notifySettingOutcome{key: cmd.key, program: word, sys: readyCommandSystem(cmd.command), ready: true}
		switch probeWord(word) {
		case wordResolves:
			if cmd.command == config.NotifyLinux && !sessionBusPresent() {
				out.kind = notifyNoBus
			} else {
				out.kind = notifyResolves
			}
		case wordDoesNotResolve:
			out.kind = notifyUnresolved
		default:
			out.kind = notifyUnanswered
		}
		return out
	}
	if isProgramWord(word) && probeWord(word) == wordDoesNotResolve {
		return notifySettingOutcome{key: cmd.key, kind: notifyUnresolved, program: word}
	}
	return notifySettingOutcome{key: cmd.key, kind: notifyUnchecked}
}

func notifyFirstWord(command string) string {
	command = strings.TrimLeft(command, " \t")
	if i := strings.IndexAny(command, " \t"); i >= 0 {
		return command[:i]
	}
	return command
}

func isProgramWord(word string) bool {
	if word == "" {
		return false
	}
	for i := range len(word) {
		switch b := word[i]; {
		case b >= 'a' && b <= 'z', b >= 'A' && b <= 'Z', b >= '0' && b <= '9':
		case b == '_', b == '.', b == '+', b == '-':
		default:
			return false
		}
	}
	return true
}

func readyCommandSystem(command string) string {
	if command == config.NotifyMacOS {
		return "macos"
	}
	return "notify-send"
}

func notifyGroups(outcomes []notifySettingOutcome) []notifyGroup {
	var groups []notifyGroup
	for _, o := range outcomes {
		at := -1
		for i, g := range groups {
			if g.kind == o.kind && g.program == o.program && g.ready == o.ready {
				at = i
				break
			}
		}
		if at < 0 {
			groups = append(groups, notifyGroup{notifySettingOutcome: o, keys: []string{o.key}})
			continue
		}
		groups[at].keys = append(groups[at].keys, o.key)
	}
	return groups
}

func notifySummaryGroup(groups []notifyGroup) int {
	worst := statusOK
	for _, g := range groups {
		worst = max(worst, g.kind.status())
	}
	lead := 0
	for i, g := range groups {
		if g.kind.status() != worst {
			continue
		}
		if groups[lead].kind.status() != worst || g.kind < groups[lead].kind {
			lead = i
		}
	}
	return lead
}

func notifyNotice(g notifyGroup, m notifyMachine) string {
	list, plural := notifyKeyList(g.keys)
	switch g.kind {
	case notifyUnset:
		return list + " is not set"
	case notifyUnchecked:
		return uncheckedNotice(g.keys)
	case notifyUnresolved:
		if g.ready {
			return readyCommandNotice(list, plural, g.sys, g.program, m)
		}
		return unresolvedProgramNotice(list, plural, g.program)
	case notifyNoBus:
		return noSessionBusNotice(list, plural, g.sys)
	case notifyUnanswered:
		return unansweredProbeNotice(list, plural, g.sys, g.program)
	default:
		runs := "runs"
		if plural {
			runs = "run"
		}

		return list + " " + runs + " " + g.program + ", which resolves on this system"
	}
}

func notifyKeyList(keys []string) (string, bool) {
	return strings.Join(keys, " and "), len(keys) > 1
}

func readyCommandNotice(list string, plural bool, sys, program string, m notifyMachine) string {
	holds := "holds"
	if plural {
		holds = "hold"
	}
	lead := list + " " + holds + " the ready command for " + systemLabel(sys)
	if readyCommandSuitsMachine(sys, m) {
		return lead + ", and " + program + " does not resolve on this system"
	}
	return lead + ", and this is " + detectedSystemLabel(m) + ": " + program + " does not resolve here"
}

func readyCommandSuitsMachine(sys string, m notifyMachine) bool {
	if m.system != "unknown" {
		return sys == m.system
	}
	return sys == "notify-send" && m.goos == "linux"
}

func detectedSystemLabel(m notifyMachine) string {
	if label, ok := machineLabels[m.system]; ok {
		return label
	}
	if label, ok := goosLabels[m.goos]; ok {
		return label
	}
	return "a system tt does not recognise"
}

var machineLabels = map[string]string{
	"macos": systemLabel("macos"),
	"wsl":   systemLabel("wsl"),
}

var goosLabels = map[string]string{
	"linux":   "Linux",
	"windows": "Windows",
}

func unresolvedProgramNotice(list string, plural bool, program string) string {
	runs := "runs"
	if plural {
		runs = "run"
	}
	return list + " " + runs + " " + cli.ReportTitle(program, doctorTextWidth-1) +
		", which does not resolve on this system"
}

func noSessionBusNotice(list string, plural bool, sys string) string {
	holds := "holds"
	if plural {
		holds = "hold"
	}
	return list + " " + holds + " the ready command for " + systemLabel(sys) +
		", and this session names no bus for it: $" + busAddressVar + " is empty and there is nothing at $" +
		runtimeDirVar + "/bus"
}

func unansweredProbeNotice(list string, plural bool, sys, program string) string {
	holds := "holds"
	if plural {
		holds = "hold"
	}
	return list + " " + holds + " the ready command for " + systemLabel(sys) +
		", and whether " + program + " resolves here is unanswered: the shell that was asked did not come back"
}

func noReadyCommandNotice(m notifyMachine) string {
	if m.system == "unknown" && m.goos == "linux" {
		return "the only ready command tt has for a Linux box needs notify-send, and there is none on this PATH"
	}
	return "no ready command for this system"
}

type notifyReport struct {
	src                 []byte
	machine             notifyMachine
	onEndSet            bool
	onEndCommand        string
	readyProgramMissing bool
	writeRefused        bool
}

func readyProgramNotHere(groups []notifyGroup, m notifyMachine) bool {
	for _, g := range groups {
		if g.ready && g.kind == notifyUnresolved && g.sys == m.system {
			return true
		}
	}
	return false
}

func readyProgramMissingNotice(sys, hint string) string {
	return "the ready command for " + systemLabel(sys) + " needs " + notifyFirstWord(hint) +
		", which does not resolve here, so there is none to offer"
}

func readyProgramMissingWayOut(sys, hint string) string {
	return readyProgramMissingNotice(sys, hint) +
		": what a missing program takes is a PATH or an install, and no setting supplies it"
}

func notifyOutcomeLines(g notifyGroup, r notifyReport) []string {
	if g.kind != notifyUnset {
		return nil
	}
	byHand := func(notice string) []string {
		if r.writeRefused {
			return append(doctorNote(notice), doctorNote("available variables: "+varList())...)
		}
		return append(doctorNote(notice+"; set timer.on_end manually"),
			doctorNote("available variables: "+varList())...)
	}
	hint, ok := notifyHints[r.machine.system]
	if !ok {
		return byHand(noReadyCommandNotice(r.machine))
	}
	if r.readyProgramMissing {
		return byHand(readyProgramMissingNotice(r.machine.system, hint))
	}
	return onEndSuggestion(r.machine.system, hint, r.src, r.writeRefused)
}

func notifyWayOutLines(g notifyGroup, r notifyReport) []string {
	switch g.kind {
	case notifyResolves:
		if !notifyTestExercises(g.keys) {
			if r.writeRefused {
				return doctorNote("what resolves is the program and not the notification, and " + notifyTestScope)
			}
			return doctorNote("what resolves is the program and not the notification, and " + notifyTestRunsOnEnd)
		}
		return append(doctorNote("what resolves is the program and not the notification: "+
			"whether one appears on screen is what this answers:"), doctorHint(notifyTestHint)...)
	case notifyUnanswered:
		if !notifyTestExercises(g.keys) {
			if r.writeRefused {
				return doctorNote("nothing was established about the command here, and " + notifyTestScope)
			}
			return doctorNote("nothing was established about the command here, and " + notifyTestRunsOnEnd)
		}
		return append(doctorNote("nothing was established about the command here; what runs it and "+
			"reports what came back is this:"), doctorHint(notifyTestHint)...)
	case notifyNoBus:
		if readyCommandSuitsMachine(g.sys, r.machine) {
			return nil
		}
		return readyCommandLines(g.keys, r)
	case notifyUnresolved, notifyUnchecked:
		return readyCommandLines(g.keys, r)
	}
	return nil
}

func notifyTestExercises(keys []string) bool {
	return slices.Contains(keys, onEndKey)
}

const notifyTestScope = `"tt notify test" runs what ` + onEndKey + ` holds and nothing else`

const notifyTestRunsOnEnd = notifyTestScope +
	`, so putting this command there for a run is what would put the same question to it`

const notifyTestHint = `run "tt notify test"`

type notifyCommand struct{ key, command string }

const (
	onEndKey      = config.TimerTable + ".on_end"
	onBreakEndKey = config.TimerTable + ".on_break_end"
)

func notifyCommands(cfg config.Config) []notifyCommand {
	return []notifyCommand{
		{onEndKey, cfg.Timer.OnEnd},
		{onBreakEndKey, cfg.Timer.OnBreakEnd},
	}
}

func uncheckedCommands(cfg config.Config) []notifyCommand {
	var out []notifyCommand
	for _, cmd := range notifyCommands(cfg) {
		if cmd.command == "" || isReadyCommand(cmd.command) {
			continue
		}
		out = append(out, cmd)
	}
	return out
}

func isReadyCommand(command string) bool {
	return command == config.NotifyMacOS || command == config.NotifyLinux
}

func uncheckedNotice(keys []string) string {
	if len(keys) == 1 {
		return keys[0] + " runs a command tt did not write and cannot check"
	}
	return strings.Join(keys, " and ") + " run commands tt did not write and cannot check"
}

func uncheckedRuleLines() []string {
	return doctorNote("tt does not read shell commands, so what one of them hands the notifier is not " +
		"something it can confirm; the rule the ready commands are built on has two halves - a value " +
		"has to arrive as an argument, and an argument counts as a value only once the program has " +
		"stopped looking for options")
}

func quotedPlaceholderLines(cmds []notifyCommand, writeRefused bool) []string {
	var bareLines, singleLines []string
	for _, cmd := range cmds {
		bare, single := quotedPlaceholders(cmd.command)
		if len(bare) > 0 {
			bareLines = append(bareLines, doctorNote(cmd.key+": "+strings.Join(bare, ", ")+" in quotes")...)
		}
		if len(single) > 0 {
			singleLines = append(singleLines, doctorNote(cmd.key+": "+strings.Join(single, ", ")+" in single quotes")...)
		}
	}

	var lines []string
	if len(bareLines) > 0 {
		lines = append(lines, bareLines...)
		explanation := "a placeholder brings its own quotes with it, so quotes of your own around it " +
			"close and reopen and leave the value bare - split on spaces and globbed"
		if !writeRefused {
			explanation += "; inside quotes of your own write the environment form instead, and outside " +
				"them the placeholder as it stands"
		}
		lines = append(lines, doctorNote(explanation)...)
	}
	if len(singleLines) > 0 {
		lines = append(lines, singleLines...)
		lines = append(lines, doctorNote("a placeholder in single quotes is not a reference at all - the "+
			"notifier is handed the text of the variable rather than its value")...)
	}
	return lines
}

func openOptionListLines(cmds []notifyCommand) []string {
	var lines []string
	for _, cmd := range cmds {
		if passesValueWithOptionsOpen(cmd.command) {
			lines = append(lines, doctorNote(cmd.key+": a value goes out with the option list still open")...)
		}
	}
	if len(lines) == 0 {
		return nil
	}
	return append(lines, doctorNote("a task title comes down from the server and can begin with a dash, "+
		"which a notifier still reading options takes for an option of its own rather than the title it "+
		"was passed as; \"--\" is what ends the option list and goes before the values, and a notifier "+
		"that has no \"--\" needs the value to arrive as the argument of an option instead")...)
}

func dottedTimerWayOutLines() []string {
	lines := doctorNote(dottedTimerByHand)
	lines = append(lines, doctorHint(dottedTimerSetting)...)
	lines = append(lines, doctorNote(dottedTimerByFix)...)
	return append(lines, doctorHint(runFixHint)...)
}

func readyCommandLines(keys []string, r notifyReport) []string {
	sys := r.machine.system
	customOnEnd := r.onEndSet && slices.Contains(keys, onEndKey)
	supportedCustomRepair := false
	if customOnEnd {
		_, supportedCustomRepair, _ = repairOnEndCommand(r.onEndCommand)
	}
	dotted := config.DottedTimerKeys(r.src)
	if supportedCustomRepair && len(dotted) > 0 {
		lines := doctorNote("timer.on_end has a supported whole-word quote repair, but nothing here " +
			"can be copied as it stands: the config writes " +
			strings.Join(dotted, ", ") + " at the root rather than under a " + timerHeader +
			" header, and --fix refuses the file for it")
		if r.writeRefused {
			return lines
		}
		return append(lines, dottedTimerWayOutLines()...)
	}
	if supportedCustomRepair {
		if r.writeRefused {
			return doctorNote("timer.on_end has a supported whole-word quote repair, but the config " +
				"write probe was refused")
		}
		lines := doctorNote("--fix preserves the current timer.on_end command and removes only its " +
			"redundant whole-word placeholder quotes; run \"tt doctor --fix\" to review the complete " +
			"escaped old and repaired values before deciding whether to write it")
		if others := notifyKeysBesides(keys, onEndKey); len(others) > 0 {
			list, _ := notifyKeyList(others)
			lines = append(lines, doctorNote(fixWritesOnEndOnly+list+" has to be set by hand")...)
		}
		return lines
	}
	hint, ok := notifyHints[sys]
	if !ok {
		return doctorNote(noReadyCommandNotice(r.machine) + "; available variables: " + varList())
	}
	if r.readyProgramMissing {

		if !r.onEndSet {
			return nil
		}
		return doctorNote(readyProgramMissingWayOut(sys, hint))
	}
	if len(dotted) > 0 {
		lines := doctorNote("there is a ready command for " + systemLabel(sys) + " that ends its option " +
			"list, but nothing here can be copied as it stands: the config writes " +
			strings.Join(dotted, ", ") + " at the root rather than under a " + timerHeader +
			" header, and --fix refuses the file for it")
		if r.writeRefused {
			return lines
		}
		return append(lines, dottedTimerWayOutLines()...)
	}
	if customOnEnd {
		if r.writeRefused {
			return doctorNote("there is a ready command for " + systemLabel(sys) +
				", but replacing the current timer.on_end requires a manual config edit")
		}
		lines := append(
			doctorNote("the current timer.on_end is outside --fix's narrow repair; replace it by hand "+
				"with the ready command for "+systemLabel(sys)+":"),
			doctorNestedIndent+"on_end = "+config.QuoteTOMLString(hint))
		if others := notifyKeysBesides(keys, onEndKey); len(others) > 0 {
			list, _ := notifyKeyList(others)
			lines = append(lines, doctorNote(fixWritesOnEndOnly+list+" has to be set by hand")...)
		}
		return lines
	}

	if !slices.Contains(keys, onEndKey) {
		if r.writeRefused {
			return doctorNote("there is a ready command for " + systemLabel(sys) +
				" that ends its option list")
		}
		list, _ := notifyKeyList(keys)
		lines := "there is a ready command for " + systemLabel(sys) + " that ends its option " +
			"list, but " + fixWritesOnEndOnly + list + " has to be set by hand"
		if r.onEndSet {
			lines += ", and the block that would stand here is not printed: what it sets is on_end, " +
				"the file already gives that key a value, and a second one under it defines it twice"
		}
		return doctorNote(lines)
	}
	if r.writeRefused {
		return doctorNote("there is a ready command for " + systemLabel(sys) +
			" that ends its option list")
	}

	lines := append(
		doctorNote("the ready command for "+systemLabel(sys)+" ends its option list, and "+
			"--fix puts it in on_end:"),
		doctorNestedIndent+"on_end = "+config.QuoteTOMLString(hint))
	if others := notifyKeysBesides(keys, onEndKey); len(others) > 0 {
		list, _ := notifyKeyList(others)
		return append(lines, doctorNote(fixWritesOnEndOnly+list+" has to be set by hand")...)
	}
	return lines
}

const fixWritesOnEndOnly = "--fix writes on_end and no other key, so "

func notifyKeysBesides(keys []string, key string) []string {
	var out []string
	for _, k := range keys {
		if k != key {
			out = append(out, k)
		}
	}
	return out
}

func passesValueWithOptionsOpen(command string) bool {
	if isReadyCommand(command) {
		return false
	}
	return valueGoesOutWithOptionsOpen(command)
}

func valueGoesOutWithOptionsOpen(command string) bool {
	substituted, spans := applyBraceTemplateSpans(command)
	cmds, ok := splitShellWords(substituted)
	if !ok {
		return false
	}
	for _, cmd := range cmds {
		for i, w := range cmd.words {
			if i == 0 {
				continue
			}
			refs := notifyValueRefs(substituted, spans, w)
			if len(refs) == 0 || !passedWholeOrDashed(w.value, refs) {
				continue
			}
			if optionListEndedIn(cmd.words[:i]) || looksLikeOption(cmd.words[i-1]) {
				continue
			}
			return true
		}
	}
	return false
}

func optionListEndedIn(before []word) bool {
	for _, w := range before {
		if w.value == "--" {
			return true
		}
	}
	return false
}

func looksLikeOption(w word) bool {
	v := w.value
	if len(v) < 2 || v[0] != '-' || v == "--" {
		return false
	}
	dashes := 0
	for dashes < len(v) && v[dashes] == '-' {
		dashes++
	}
	return !strings.Contains(v[dashes:], "=")
}

func passedWholeOrDashed(value string, refs []string) bool {
	for _, ref := range refs {
		rest, ok := strings.CutSuffix(value, ref)
		if ok && strings.Trim(rest, "-") == "" {
			return true
		}
	}
	return false
}

func notifyValueRefs(s string, spans []templateSpan, w word) []string {
	var refs []string
	for _, span := range spans {
		if q, ok := spanQuotingIn(w, span); ok && q != quoteSingle {
			refs = append(refs, s[span.from+1:span.to-1])
		}
	}
	for _, sg := range w.segs {
		if sg.q == quoteSingle || sg.q == quoteEscaped {
			continue
		}
		refs = append(refs, environmentRefsIn(s, sg)...)
	}
	return refs
}

func environmentRefsIn(s string, sg segment) []string {
	var refs []string
	for i := sg.from; i < sg.to; i++ {
		if s[i] != '$' {
			continue
		}
		j := i + 1
		braced := j < sg.to && s[j] == '{'
		if braced {
			j++
		}
		start := j
		for j < sg.to && isIdentByte(s[j]) {
			j++
		}
		name := s[start:j]
		if braced {
			if j >= sg.to || s[j] != '}' {
				continue
			}
			j++
		}
		if slices.Contains(notifyVarNames, name) {
			refs = append(refs, s[i:j])
		}
	}
	return refs
}

func spanQuotingIn(w word, span templateSpan) (quoting, bool) {
	for _, sg := range w.segs {
		if sg.from <= span.from+1 && span.to-1 <= sg.to {
			return sg.q, true
		}
	}
	return quoteNone, false
}

func spanLanding(cmds []simpleCommand, span templateSpan) ([]word, int, quoting, bool) {
	for _, cmd := range cmds {
		for i, w := range cmd.words {
			if q, ok := spanQuotingIn(w, span); ok {
				return cmd.words, i, q, true
			}
		}
	}
	return nil, 0, quoteNone, false
}

func quotedPlaceholders(command string) (bare, single []string) {
	substituted, spans := applyBraceTemplateSpans(command)
	if len(spans) == 0 {
		return nil, nil
	}
	cmds, ok := splitShellWords(substituted)
	if !ok {
		return nil, nil
	}
	for _, span := range spans {
		words, at, q, found := spanLanding(cmds, span)
		if !found {
			continue
		}
		switch q {
		case quoteDouble:

		case quoteNone:

			if at > 0 {
				bare = appendMatchOnce(bare, span.match)
			}
		case quoteSingle:

			if at > 0 && !looksLikeOption(words[at-1]) {
				single = appendMatchOnce(single, span.match)
			}
		}
	}
	return bare, single
}

func appendMatchOnce(list []string, match string) []string {
	if slices.Contains(list, match) {
		return list
	}
	return append(list, match)
}

func onEndSuggestion(sys, hint string, src []byte, writeRefused bool) []string {
	if dotted := config.DottedTimerKeys(src); len(dotted) > 0 {
		lead := "suggested for " + systemLabel(sys) + ", by hand: the config writes "
		if writeRefused {
			lead = "the config writes "
		}
		lines := doctorNote(lead + strings.Join(dotted, ", ") + " at the root rather than under a " + timerHeader +
			" header, and --fix writes on_end under such a header and nowhere else - " +
			"this file has none, and adding one while those keys stand at the root would define the " +
			"table both ways, which is invalid TOML 1.0 - tt's own parser happens to accept such a " +
			"file and other readers do not, so --fix refuses the file as it stands")
		if writeRefused {
			return lines
		}
		return append(lines, dottedTimerWayOutLines()...)
	}
	if writeRefused {
		return doctorNote("available variables: " + varList())
	}

	lines := doctorNote("suggested for " + systemLabel(sys) + ":")
	lines = append(lines, doctorHint(runFixHint)...)
	setting := doctorNestedIndent + "on_end = " + config.QuoteTOMLString(hint)
	if config.OpensTimerTableIn(src) {
		return append(append(lines, doctorNote("or add this to the "+timerHeader+" section of the config file:")...), setting)
	}
	return append(append(lines, doctorNote("or add this to the end of the config file:")...),
		doctorNestedIndent+timerHeader, setting)
}

func configSource() []byte {
	path, err := config.Path()
	if err != nil {
		return nil
	}
	src, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	return src
}

func varList() string {
	names := make([]string, len(notifyVarNames))
	for i, n := range notifyVarNames {
		names[i] = "$" + n
	}
	return strings.Join(names, ", ")
}

func collectDoctorChecks(ctx context.Context, probe func() configWriteObservation) []checkResult {
	tokenResult, token := checkToken()
	cacheResult, cache := checkCache(ctx)
	cache.configWrite = probe()
	checks := []checkResult{
		tokenResult,
		checkAPI(ctx, token, cache),
		cacheResult,
		checkConfig(cache),
		checkNotifyWithConfigWrite(cache.configWrite),
		checkAI(ctx),
	}
	if codex, ok := checkCodexInstructions(); ok {
		checks = append(checks, codex)
	}
	return checks
}
