package main

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	pathpkg "path"
	"sort"
	"strconv"
	"strings"
	"testing"
)

const (
	cliImportPath    = "github.com/movsar/tt/internal/cli"
	apiImportPath    = "github.com/movsar/tt/internal/api"
	configImportPath = "github.com/movsar/tt/internal/config"
	storeImportPath  = "github.com/movsar/tt/internal/store"
)

type missingEscapeFinding struct {
	file        string
	scope       string
	sink        string
	provenance  string
	fingerprint string
	position    token.Position
	count       int
}

type missingEscapeException struct {
	file        string
	scope       string
	sink        string
	fingerprint string
	count       int
	why         string
}

type missingEscapeScope struct {
	name string
	node ast.Node
}

type missingEscapeAssignment struct {
	name string
	obj  *ast.Object
	expr ast.Expr
}

type missingEscapeAnalyzer struct {
	fset         *token.FileSet
	file         string
	imports      map[string]string
	taint        map[*ast.Object]map[string]bool
	dependencies map[*ast.Object]map[string]bool
	joined       map[*ast.Object]bool
}

var missingEscapeSourceFields = map[string]bool{
	"AssigneeUsername": true,
	"Body":             true,
	"ColumnID":         true,
	"ColumnName":       true,
	"Content":          true,
	"CopyID":           true,
	"CopyProjectID":    true,
	"DefaultProject":   true,
	"Desc":             true,
	"Detail":           true,
	"Etag":             true,
	"Err":              true,
	"Field":            true,
	"FromID":           true,
	"GroupID":          true,
	"ID":               true,
	"Id":               true,
	"ItemID":           true,
	"Kind":             true,
	"LastError":        true,
	"Method":           true,
	"Msg":              true,
	"Name":             true,
	"NewID":            true,
	"Note":             true,
	"OldID":            true,
	"OnBreakEnd":       true,
	"OnEnd":            true,
	"Op":               true,
	"Path":             true,
	"Permission":       true,
	"ProjectID":        true,
	"ProjectId":        true,
	"Reason":           true,
	"RepeatFlag":       true,
	"TaskID":           true,
	"TaskId":           true,
	"TimeZone":         true,
	"Title":            true,
	"Value":            true,
	"ViewMode":         true,
	"Stdout":           true,
	"Stderr":           true,
	"Killed":           true,
	"Code":             true,
}

var missingEscapeSourceParameters = map[string]bool{
	"anchor":      true,
	"answer":      true,
	"body":        true,
	"dir":         true,
	"message":     true,
	"msg":         true,
	"path":        true,
	"reason":      true,
	"redirectURI": true,
	"target":      true,
	"uri":         true,
	"url":         true,
}

var missingEscapeLocalSourceCalls = map[string]bool{
	"credentialsPath": true,
}

var missingEscapeLocalSanitizers = map[string]bool{
	"fixValue":  true,
	"moveName":  true,
	"quoteWord": true,
}

var missingEscapeLocalSanitizerReturns = map[string]string{
	"fixValue":  "cli.Foreign(s, 2+quotedRuneMax*utf8.RuneCountInString(s))",
	"moveName":  "cli.ReportTitle(s, w-1)",
	"quoteWord": "before + cli.ReportTitle(word, cli.Width-cli.DisplayWidth(before))",
}

var missingEscapeLocalSanitizerSignatures = map[string]string{
	"fixValue":  "func(s string) string",
	"moveName":  "func(s string, w int) string",
	"quoteWord": "func(before, word string) string",
}

var missingEscapeLocalSanitizerTextArguments = map[string]int{
	"fixValue":  0,
	"moveName":  0,
	"quoteWord": 1,
}

var missingEscapeStructuredRenderers = map[string]bool{
	"CardLines":  true,
	"ListLines":  true,
	"ReportLine": true,
}

var missingEscapeLocalLineBuilders = map[string]bool{
	"doctorNote":    true,
	"doctorSummary": true,
	"fixParagraph":  true,
	"moveParagraph": true,
	"syncParagraph": true,
}

func TestMissingEscapeCheckerRejectsForeignFlows(t *testing.T) {
	cases := []struct {
		name string
		src  string
	}{
		{
			name: "direct percent s",
			src:  `package main; import "fmt"; func f(err error) { fmt.Printf("%s", err.Error()) }`,
		},
		{
			name: "concatenation",
			src:  `package main; import "fmt"; func f(task struct{ Title string }) { fmt.Print("task: " + task.Title) }`,
		},
		{
			name: "one alias",
			src:  `package main; import "fmt"; func f(task struct{ Title string }) { title := task.Title; fmt.Print(title) }`,
		},
		{
			name: "multiple aliases",
			src:  `package main; import "fmt"; func f(task struct{ Title string }) { a := task.Title; b := a; fmt.Print(b) }`,
		},
		{
			name: "constant branch cannot clear taint",
			src: `package main; import "fmt"; func f(task struct{ Title string }, use bool) {
				var title string; if use { title = task.Title } else { title = "own" }; fmt.Print(title)
			}`,
		},
		{
			name: "second field beside bounded first",
			src: `package main; import ("fmt"; cli "github.com/movsar/tt/internal/cli")
				func f(task struct{ ID, Title string }) { fmt.Printf("%s %s", cli.Foreign(task.ID, 20), task.Title) }`,
		},
		{
			name: "new package initializer",
			src:  `package main; import ("fmt"; "os"); var summary = fmt.Sprintf("%s", os.Getenv("X"))`,
		},
		{
			name: "initializer function literal",
			src: `package main; import ("fmt"; "os")
				var handler = func() string { return fmt.Sprintf("%s", os.Getenv("X")) }`,
		},
		{
			name: "misleading sanitizer import",
			src: `package main; import ("fmt"; cli "example.com/not-cli")
				func f(task struct{ Title string }) { fmt.Print(cli.Foreign(task.Title, 20)) }`,
		},
		{
			name: "local sanitizer shadow",
			src: `package main; import ("fmt"; cli "github.com/movsar/tt/internal/cli")
				type fake struct{}; func (fake) Foreign(s string, _ int) string { return s }
				func f(task struct{ Title string }) { cli := fake{}; fmt.Print(cli.Foreign(task.Title, 20)) }`,
		},
		{
			name: "generic helper is not a sanitizer",
			src: `package main; import "fmt"; func passthrough(s string) string { return s }
				func f(task struct{ Title string }) { fmt.Print(passthrough(task.Title)) }`,
		},
		{
			name: "local sanitizer keeps raw prefix foreign",
			src: `package main; import "fmt"; func quoteWord(before, word string) string { return before + word }
				func f(task struct{ Title string }) { fmt.Print(quoteWord(task.Title, "own")) }`,
		},
		{
			name: "numeric verb does not sanitize a string",
			src:  `package main; import "fmt"; func f(task struct{ ID string }) { fmt.Printf("%d", task.ID) }`,
		},
		{
			name: "shadowed numeric builtin is a generic helper",
			src: `package main; import "fmt"; func len(s string) string { return s }
				func f(task struct{ ID string }) { fmt.Print(len(task.ID)) }`,
		},
		{
			name: "generic scalar method receiver without arguments",
			src: `package main; import "fmt"; type text string
				func (x text) passthrough() string { return string(x) }
				func f(task struct{ Title string }) { x := text(task.Title); fmt.Print(x.passthrough()) }`,
		},
		{
			name: "generic scalar method receiver with argument",
			src: `package main; import "fmt"; type text string
				func (x text) passthrough(s string) string { return string(x) + s }
				func f(task struct{ Title string }) { x := text(task.Title); fmt.Print(x.passthrough("own")) }`,
		},
		{
			name: "parenthesized source",
			src:  `package main; import ("fmt"; "os"); func f() { fmt.Print((os.Getenv)("X")) }`,
		},
		{
			name: "parenthesized sink",
			src:  `package main; import "fmt"; func f(task struct{ Title string }) { (fmt.Print)(task.Title) }`,
		},
		{
			name: "parenthesized error method",
			src:  `package main; import "fmt"; func f(err error) { fmt.Print((err.Error)()) }`,
		},
		{
			name: "bare error parameter",
			src:  `package main; import "fmt"; func f(cause error) { fmt.Printf("%v", cause) }`,
		},
		{
			name: "bare local error result",
			src:  `package main; import ("fmt"; "errors"); func f() { err := errors.New("foreign error"); fmt.Print(err) }`,
		},
		{
			name: "error field",
			src:  `package main; import "fmt"; func f(result struct{ Err error }) { fmt.Print(result.Err) }`,
		},
		{
			name: "generic second result",
			src: `package main; import "fmt"; func passthrough(s string) (int, string) { return 0, s }
				func f(task struct{ Title string }) { _, x := passthrough(task.Title); fmt.Print(x) }`,
		},
		{
			name: "generic second var result",
			src: `package main; import "fmt"; func passthrough(s string) (int, string) { return 0, s }
				func f(task struct{ Title string }) { var _, x = passthrough(task.Title); fmt.Print(x) }`,
		},
		{
			name: "wrappers and join propagate",
			src: `package main; import ("fmt"; "strings")
				func f(task struct{ Title string }) { fmt.Print(strings.Join([]string{string((task.Title))}, ",")) }`,
		},
		{
			name: "zero argument config path source",
			src: `package main; import ("fmt"; config "github.com/movsar/tt/internal/config")
				func f() { fmt.Print(config.Path()) }`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := fixtureMissingEscapeFindings(t, c.src); len(got) == 0 {
				t.Fatal("checker accepted an unescaped foreign flow")
			}
		})
	}
}

func TestMissingEscapeCheckerFormattingSinkContract(t *testing.T) {
	t.Run("ordinary and initializer constructors are sinks", func(t *testing.T) {
		for _, src := range []string{
			`package main; import "fmt"; func fresh(task struct{ Title string }) string { return fmt.Sprintf("%s", task.Title) }`,
			`package main; import "fmt"; var task struct{ Title string }; var summary = fmt.Sprintf("%s", task.Title)`,
		} {
			if got := fixtureMissingEscapeFindings(t, src); len(got) == 0 {
				t.Fatal("checker accepted an unescaped fmt constructor")
			}
		}
	})

	t.Run("only a genuine sanitizer protects a constructor path", func(t *testing.T) {
		good := `package main; import ("fmt"; cli "github.com/movsar/tt/internal/cli")
			func f(task struct{ Title string }) string { return cli.Foreign(fmt.Sprintf("%s", task.Title), 20) }`
		if got := fixtureMissingEscapeFindings(t, good); len(got) != 0 {
			t.Fatalf("genuine sanitizer produced findings: %v", got)
		}
		bad := `package main; import ("fmt"; cli "example.com/not-cli")
			func f(task struct{ Title string }) string { return cli.Foreign(fmt.Sprintf("%s", task.Title), 20) }`
		if got := fixtureMissingEscapeFindings(t, bad); len(got) == 0 {
			t.Fatal("misleading sanitizer import protected a constructor")
		}
		inside := `package main; import ("fmt"; cli "github.com/movsar/tt/internal/cli")
			func f(task struct{ Title string }) string { return fmt.Sprintf("%s", cli.Foreign(task.Title, 20)) }`
		if got := fixtureMissingEscapeFindings(t, inside); len(got) != 0 {
			t.Fatalf("sanitized formatter argument produced findings: %v", got)
		}
		joined := `package main; import ("fmt"; "strings"; cli "github.com/movsar/tt/internal/cli")
			func f(task struct{ Title string }) string {
				return cli.Foreign(strings.Join([]string{fmt.Sprintf("%s", task.Title)}, ","), 20)
			}`
		if got := fixtureMissingEscapeFindings(t, joined); len(got) != 0 {
			t.Fatalf("protected pure Join/composite path produced findings: %v", got)
		}
		parenthesized := `package main; import ("fmt"; cli "github.com/movsar/tt/internal/cli")
			func f(task struct{ Title string }) string { return (cli.Foreign)((fmt.Sprintf)("%s", task.Title), 20) }`
		if got := fixtureMissingEscapeFindings(t, parenthesized); len(got) != 0 {
			t.Fatalf("parenthesized genuine sanitizer produced findings: %v", got)
		}
	})

	t.Run("sanitization is per argument", func(t *testing.T) {
		for _, src := range []string{
			`package main; import ("fmt"; cli "github.com/movsar/tt/internal/cli"); func f(task struct{ ID, Title string }) { _ = fmt.Sprintf("%s %s", cli.Foreign(task.Title, 20), task.ID) }`,
			`package main; import ("fmt"; cli "github.com/movsar/tt/internal/cli"); func f(task struct{ ID, Title string }) { fmt.Printf("%s %s", cli.Foreign(task.Title, 20), task.ID) }`,
		} {
			if got := fixtureMissingEscapeFindings(t, src); len(got) == 0 {
				t.Fatal("checker accepted a raw sibling argument")
			}
		}
	})

	t.Run("aliases helpers and join remain foreign", func(t *testing.T) {
		cases := []string{
			`package main; import "fmt"; func f(task struct{ Title string }) { a := task.Title; b := a; _ = fmt.Sprintf("%s", b) }`,
			`package main; import "fmt"; func passthrough(s string) string { return s }; func f(task struct{ Title string }) { _ = fmt.Sprintf("%s", passthrough(task.Title)) }`,
			`package main; import ("fmt"; "strings"); func f(task struct{ Title string }) { fmt.Print(strings.Join([]string{task.Title}, ",")) }`,
		}
		for _, src := range cases {
			if got := fixtureMissingEscapeFindings(t, src); len(got) == 0 {
				t.Fatal("checker accepted a propagated formatter value")
			}
		}
	})

	t.Run("sanitizer syntax does not hide an executed print", func(t *testing.T) {
		src := `package main; import ("fmt"; cli "github.com/movsar/tt/internal/cli")
			func f(task struct{ Title string }) string { return cli.Foreign(func() string { fmt.Printf("%s", task.Title); return "own" }(), 20) }`
		if got := fixtureMissingEscapeFindings(t, src); len(got) == 0 {
			t.Fatal("sanitizer syntax hid an independently executed print")
		}
	})
}

func TestMissingEscapeCheckerRejectsUnsupportedDotImports(t *testing.T) {
	for _, src := range []string{
		`package main; import . "fmt"; func f(task struct{ Title string }) { Print(task.Title) }`,
		`package main; import ("fmt"; . "os"); func f() { fmt.Print(Getenv("X")) }`,
	} {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, "fixture.go", src, parser.AllErrors)
		if err != nil {
			t.Fatalf("parse fixture: %v", err)
		}
		if problems := missingEscapeDotImportProblems(map[string]*ast.File{"fixture.go": f}); len(problems) == 0 {
			t.Fatal("checker accepted a dot-imported boundary package")
		}
	}
}

func TestMissingEscapeCheckerAcceptsSafeFlows(t *testing.T) {
	cases := []struct {
		name string
		src  string
	}{
		{
			name: "foreign",
			src: `package main; import ("fmt"; cli "github.com/movsar/tt/internal/cli")
				func f(task struct{ Title string }) { fmt.Print(cli.Foreign(task.Title, 20)) }`,
		},
		{
			name: "aliased report title import",
			src: `package main; import ("fmt"; safe "github.com/movsar/tt/internal/cli")
				func f(task struct{ Title string }) { fmt.Print(safe.ReportTitle(task.Title, 20)) }`,
		},
		{
			name: "static and numeric",
			src:  `package main; import "fmt"; func f(n int) { fmt.Printf("own %d", n) }`,
		},
		{
			name: "strconv quote",
			src: `package main; import ("fmt"; "strconv")
				func f(task struct{ Title string }) { fmt.Print(strconv.Quote(task.Title)) }`,
		},
		{
			name: "escape only width exception",
			src: `package main; import ("fmt"; api "github.com/movsar/tt/internal/api")
				func f(task struct{ ID string }) { fmt.Print(api.OneLine(task.ID)) }`,
		},
		{
			name: "local sanitizer text argument",
			src: `package main; import "fmt"; func quoteWord(before, word string) string { return before + word }
				func f(task struct{ Title string }) { fmt.Print(quoteWord("own: ", task.Title)) }`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := fixtureMissingEscapeFindings(t, c.src); len(got) != 0 {
				t.Fatalf("safe flow produced findings: %v", got)
			}
		})
	}
}

func TestMissingEscapeExceptionsAreExactAndBidirectional(t *testing.T) {
	src := `package main; import "fmt"; func f(task struct{ Title string }) { value := task.Title; fmt.Print(value) }`
	findings := fixtureMissingEscapeFindings(t, src)
	if len(findings) != 1 {
		t.Fatalf("got %d findings, want 1", len(findings))
	}
	exact := exceptionForFinding(findings[0], "reviewed fixture exception")
	if problems := missingEscapeInventoryProblems(findings, []missingEscapeException{exact}); len(problems) != 0 {
		t.Fatalf("exact exception produced problems: %v", problems)
	}

	t.Run("stale", func(t *testing.T) {
		if problems := missingEscapeInventoryProblems(nil, []missingEscapeException{exact}); len(problems) == 0 {
			t.Fatal("stale exception was accepted")
		}
	})
	t.Run("duplicate", func(t *testing.T) {
		if problems := missingEscapeInventoryProblems(findings, []missingEscapeException{exact, exact}); len(problems) == 0 {
			t.Fatal("duplicate exception was accepted")
		}
	})
	t.Run("changed source through alias", func(t *testing.T) {
		changed := fixtureMissingEscapeFindings(t,
			`package main; import "fmt"; func f(task struct{ Name string }) { value := task.Name; fmt.Print(value) }`)
		problems := missingEscapeInventoryProblems(changed, []missingEscapeException{exact})
		if len(problems) < 2 {
			t.Fatalf("changed alias source problems = %v, want unapproved and stale", problems)
		}
	})
	t.Run("occurrence count", func(t *testing.T) {
		changed := exact
		changed.count++
		if problems := missingEscapeInventoryProblems(findings, []missingEscapeException{changed}); len(problems) == 0 {
			t.Fatal("changed occurrence count was accepted")
		}
	})
}

func TestMissingEscapeDirectSourceDependenciesInvalidateExceptions(t *testing.T) {
	cases := []struct {
		name   string
		before string
		after  string
	}{
		{
			name:   "error receiver initializer",
			before: `package main; import ("fmt"; "errors"); func f(task struct{ Title string }) { err := errors.New("own"); fmt.Print(err.Error()) }`,
			after:  `package main; import ("fmt"; "errors"); func f(task struct{ Title string }) { err := errors.New(task.Title); fmt.Print(err.Error()) }`,
		},
		{
			name:   "environment key initializer",
			before: `package main; import ("fmt"; "os"); func f() { key := "A"; fmt.Print(os.Getenv(key)) }`,
			after:  `package main; import ("fmt"; "os"); func f() { key := "B"; fmt.Print(os.Getenv(key)) }`,
		},
		{
			name:   "environment direct key control",
			before: `package main; import ("fmt"; "os"); func f() { fmt.Print(os.Getenv("A")) }`,
			after:  `package main; import ("fmt"; "os"); func f() { fmt.Print(os.Getenv("B")) }`,
		},
		{
			name:   "error local alias",
			before: `package main; import ("fmt"; "errors"); func f(task struct{ Title string }) { err := errors.New("own"); value := err.Error(); fmt.Print(value) }`,
			after:  `package main; import ("fmt"; "errors"); func f(task struct{ Title string }) { err := errors.New(task.Title); value := err.Error(); fmt.Print(value) }`,
		},
		{
			name:   "selected field receiver alias",
			before: `package main; import "fmt"; func f(a, b struct{ Body string }) { x := a; fmt.Print(x.Body) }`,
			after:  `package main; import "fmt"; func f(a, b struct{ Body string }) { x := b; fmt.Print(x.Body) }`,
		},
		{
			name:   "selected field receiver literal",
			before: `package main; import "fmt"; func f() { x := struct{ Body string }{Body: "own"}; fmt.Print(x.Body) }`,
			after:  `package main; import "fmt"; func f() { x := struct{ Body string }{Body: "changed"}; fmt.Print(x.Body) }`,
		},
		{
			name:   "scalar selected source control",
			before: `package main; import "fmt"; func f(a, b struct{ Body string }) { x := a.Body; fmt.Print(x) }`,
			after:  `package main; import "fmt"; func f(a, b struct{ Body string }) { x := b.Body; fmt.Print(x) }`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			before := fixtureMissingEscapeFindings(t, c.before)
			after := fixtureMissingEscapeFindings(t, c.after)
			if len(before) != 1 || len(after) != 1 {
				t.Fatalf("finding counts = %d and %d, want 1 and 1", len(before), len(after))
			}
			exception := exceptionForFinding(before[0], "reviewed fixture exception")
			if problems := missingEscapeInventoryProblems(after, []missingEscapeException{exception}); len(problems) < 2 {
				t.Fatalf("changed source dependency problems = %v, want unapproved and stale", problems)
			}
		})
	}

	t.Run("unchanged selected field receiver control", func(t *testing.T) {
		src := `package main; import "fmt"; func f(a struct{ Body string }) { x := a; fmt.Print(x.Body) }`
		before := fixtureMissingEscapeFindings(t, src)
		after := fixtureMissingEscapeFindings(t, src)
		if len(before) != 1 || len(after) != 1 {
			t.Fatalf("finding counts = %d and %d, want 1 and 1", len(before), len(after))
		}
		exception := exceptionForFinding(before[0], "reviewed fixture exception")
		if problems := missingEscapeInventoryProblems(after, []missingEscapeException{exception}); len(problems) != 0 {
			t.Fatalf("unchanged receiver dependency problems = %v, want none", problems)
		}
	})
}

func TestMissingEscapeNamedBoundariesRejectStaleEntries(t *testing.T) {
	problems := missingEscapeMissingNames(map[string]bool{"live": true},
		map[string]bool{"live": true, "removed": true}, "fixture sanitizer")
	if len(problems) != 1 || !strings.Contains(problems[0], "removed") {
		t.Fatalf("missing boundary problems = %v, want the stale name", problems)
	}
}

func TestMissingEscapeLocalSanitizerContractRejectsPassthrough(t *testing.T) {
	for _, src := range []string{
		`package main; import cli "github.com/movsar/tt/internal/cli"; func fixValue(s string) string { return s }`,
		`package main; import cli "example.com/not-cli"; func fixValue(s string) string { return cli.Foreign(s, 2+quotedRuneMax*utf8.RuneCountInString(s)) }`,
		`package main; var cli struct{ Foreign func(string, int) string }; func fixValue(s string) string { return cli.Foreign(s, 2+quotedRuneMax*utf8.RuneCountInString(s)) }`,
		`package main; import cli "github.com/movsar/tt/internal/cli"; func quoteWord(word, before string) string { return before + cli.ReportTitle(word, cli.Width-utf8.RuneCountInString(before)) }`,
	} {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, "fixture.go", src, parser.AllErrors)
		if err != nil {
			t.Fatalf("parse fixture: %v", err)
		}
		problems := missingEscapeSanitizerContractProblems(map[string]*ast.File{"fixture.go": f})
		if len(problems) == 0 {
			t.Fatal("changed local sanitizer contract was accepted")
		}
	}
}

func fixtureMissingEscapeFindings(t *testing.T, src string) []missingEscapeFinding {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "fixture.go", src, parser.AllErrors)
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	return findMissingEscapes(fset, map[string]*ast.File{"fixture.go": f})
}

func parseMissingEscapePackage(t *testing.T, fset *token.FileSet) map[string]*ast.File {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read the package directory: %v", err)
	}
	files := map[string]*ast.File{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, parser.AllErrors)
		if err != nil {
			t.Fatalf("parse %s with identifier resolution: %v", name, err)
		}
		files[name] = f
	}
	if len(files) == 0 {
		t.Fatal("parsed no production files in the package directory")
	}
	return files
}

func findMissingEscapes(fset *token.FileSet, files map[string]*ast.File) []missingEscapeFinding {
	var raw []missingEscapeFinding
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		f := files[name]
		imports := missingEscapeImports(f)
		for _, scope := range missingEscapeScopes(f) {
			a := missingEscapeAnalyzer{
				fset:         fset,
				file:         name,
				imports:      imports,
				taint:        map[*ast.Object]map[string]bool{},
				dependencies: map[*ast.Object]map[string]bool{},
				joined:       map[*ast.Object]bool{},
			}
			a.seedSourceParameters(scope.node)
			a.propagateAssignments(scope.node)
			raw = append(raw, a.findInScope(scope)...)
		}
	}

	byKey := map[string]missingEscapeFinding{}
	for _, finding := range raw {
		key := missingEscapeFindingKey(finding)
		if old, ok := byKey[key]; ok {
			old.count++
			byKey[key] = old
			continue
		}
		finding.count = 1
		byKey[key] = finding
	}
	out := make([]missingEscapeFinding, 0, len(byKey))
	for _, finding := range byKey {
		out = append(out, finding)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].file != out[j].file {
			return out[i].file < out[j].file
		}
		if out[i].scope != out[j].scope {
			return out[i].scope < out[j].scope
		}
		if out[i].sink != out[j].sink {
			return out[i].sink < out[j].sink
		}
		return out[i].provenance < out[j].provenance
	})
	return out
}

func (a *missingEscapeAnalyzer) seedSourceParameters(node ast.Node) {
	ast.Inspect(node, func(n ast.Node) bool {
		var params *ast.FieldList
		switch n := n.(type) {
		case *ast.FuncDecl:
			params = n.Type.Params
		case *ast.FuncLit:
			params = n.Type.Params
		default:
			return true
		}
		if params == nil {
			return true
		}
		for _, field := range params.List {
			isError := false
			if id, ok := field.Type.(*ast.Ident); ok {
				isError = id.Name == "error" && id.Obj == nil
			}
			for _, id := range field.Names {
				if id.Obj != nil && (missingEscapeSourceParameters[id.Name] || isError) {
					kind := "param:"
					if isError {
						kind = "error-param:"
					}
					mergeMissingEscapeTaint(a.taint, id.Obj,
						map[string]bool{"source:" + kind + id.Name: true})
				}
			}
		}
		return true
	})
}

func missingEscapeScopes(f *ast.File) []missingEscapeScope {
	var scopes []missingEscapeScope
	for _, declaration := range f.Decls {
		switch declaration := declaration.(type) {
		case *ast.FuncDecl:
			scopes = append(scopes, missingEscapeScope{name: declaration.Name.Name, node: declaration})
		case *ast.GenDecl:
			if declaration.Tok != token.VAR {
				continue
			}
			for _, raw := range declaration.Specs {
				spec, ok := raw.(*ast.ValueSpec)
				if !ok || len(spec.Names) == 0 {
					continue
				}
				allNames := make([]string, len(spec.Names))
				for i, name := range spec.Names {
					allNames[i] = name.Name
				}
				for i, value := range spec.Values {
					name := strings.Join(allNames, ",")
					if len(spec.Names) == len(spec.Values) {
						name = spec.Names[i].Name
					}
					scopes = append(scopes, missingEscapeScope{name: "var:" + name, node: value})
				}
			}
		}
	}
	return scopes
}

func missingEscapeImports(f *ast.File) map[string]string {
	imports := map[string]string{}
	for _, spec := range f.Imports {
		path, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			continue
		}
		name := pathpkg.Base(path)
		if spec.Name != nil {
			name = spec.Name.Name
		}
		if name == "_" || name == "." {
			continue
		}
		imports[name] = path
	}
	return imports
}

func missingEscapeBoundaryProblems(files map[string]*ast.File) []string {
	declared := map[string]bool{}
	for _, file := range files {
		forEachFunc(file, func(name string, _ *ast.FuncDecl) {
			declared[name] = true
		})
	}
	var problems []string
	problems = append(problems, missingEscapeDotImportProblems(files)...)
	problems = append(problems, missingEscapeMissingNames(declared,
		missingEscapeLocalSourceCalls, "local source")...)
	problems = append(problems, missingEscapeMissingNames(declared,
		missingEscapeLocalSanitizers, "local sanitizer")...)
	problems = append(problems, missingEscapeSanitizerContractProblems(files)...)
	problems = append(problems, missingEscapeMissingNames(declared,
		missingEscapeLocalLineBuilders, "local line builder")...)
	sort.Strings(problems)
	return problems
}

func missingEscapeSanitizerContractProblems(files map[string]*ast.File) []string {
	seen := map[string]int{}
	var problems []string
	for fileName, file := range files {
		imports := missingEscapeImports(file)
		for _, raw := range file.Decls {
			decl, ok := raw.(*ast.FuncDecl)
			if !ok || !missingEscapeLocalSanitizers[decl.Name.Name] {
				continue
			}
			name := decl.Name.Name
			seen[name]++
			if imports["cli"] != cliImportPath {
				problems = append(problems, fmt.Sprintf("%s local sanitizer %s no longer resolves cli to %s", fileName, name, cliImportPath))
			}
			if decl.Recv != nil || missingEscapeNodeText(decl.Type) != missingEscapeLocalSanitizerSignatures[name] {
				problems = append(problems, fmt.Sprintf("%s local sanitizer %s no longer has its reviewed ordered signature", fileName, name))
			}
			if decl.Body == nil || len(decl.Body.List) != 1 {
				problems = append(problems, fmt.Sprintf("%s local sanitizer %s no longer has its reviewed one-return body", fileName, name))
				continue
			}
			ret, ok := decl.Body.List[0].(*ast.ReturnStmt)
			if !ok || len(ret.Results) != 1 || missingEscapeNodeText(ret.Results[0]) != missingEscapeLocalSanitizerReturns[name] {
				problems = append(problems, fmt.Sprintf("%s local sanitizer %s no longer has its reviewed return expression", fileName, name))
			}
		}
	}
	for name := range missingEscapeLocalSanitizers {
		if seen[name] > 1 {
			problems = append(problems, fmt.Sprintf("missing-escape local sanitizer %s has %d declarations", name, seen[name]))
		}
	}
	sort.Strings(problems)
	return problems
}

func missingEscapeDotImportProblems(files map[string]*ast.File) []string {
	boundaryPaths := map[string]bool{
		"fmt":            true,
		"os":             true,
		"strconv":        true,
		"strings":        true,
		cliImportPath:    true,
		apiImportPath:    true,
		configImportPath: true,
		storeImportPath:  true,
	}
	var problems []string
	for name, file := range files {
		for _, spec := range file.Imports {
			if spec.Name == nil || spec.Name.Name != "." {
				continue
			}
			path, err := strconv.Unquote(spec.Path.Value)
			if err == nil && boundaryPaths[path] {
				problems = append(problems, fmt.Sprintf("%s dot-imports missing-escape boundary package %s; use a resolved package name",
					name, path))
			}
		}
	}
	sort.Strings(problems)
	return problems
}

func missingEscapeMissingNames(declared, boundaries map[string]bool, kind string) []string {
	var problems []string
	for name := range boundaries {
		if !declared[name] {
			problems = append(problems, fmt.Sprintf("missing-escape %s %s is no longer declared", kind, name))
		}
	}
	sort.Strings(problems)
	return problems
}

func (a *missingEscapeAnalyzer) propagateAssignments(node ast.Node) {
	var assignments []missingEscapeAssignment
	ast.Inspect(node, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.AssignStmt:
			for i, raw := range n.Lhs {
				id, ok := raw.(*ast.Ident)
				if !ok || id.Obj == nil || len(n.Rhs) == 0 {
					continue
				}
				rhs := n.Rhs[0]
				if len(n.Lhs) == len(n.Rhs) {
					rhs = n.Rhs[i]
				}
				assignments = append(assignments, missingEscapeAssignment{name: id.Name, obj: id.Obj, expr: rhs})
			}
		case *ast.ValueSpec:
			for i, id := range n.Names {
				if id.Obj == nil || len(n.Values) == 0 {
					continue
				}
				rhs := n.Values[0]
				if len(n.Names) == len(n.Values) {
					rhs = n.Values[i]
				}
				assignments = append(assignments, missingEscapeAssignment{name: id.Name, obj: id.Obj, expr: rhs})
			}
		}
		return true
	})

	for changed := true; changed; {
		changed = false
		for _, assignment := range assignments {
			dependency := a.expressionDependencies(assignment.expr)
			if dependency == nil {
				dependency = map[string]bool{}
			}
			dependency["assign:"+assignment.name+"="+missingEscapeNodeText(assignment.expr)] = true
			if mergeMissingEscapeTaint(a.dependencies, assignment.obj, dependency) {
				changed = true
			}
		}
	}

	for changed := true; changed; {
		changed = false
		for _, assignment := range assignments {
			provenance := a.expressionTaint(assignment.expr)
			if len(provenance) == 0 {
				continue
			}
			provenance["assign:"+assignment.name+"="+missingEscapeNodeText(assignment.expr)] = true
			if mergeMissingEscapeTaint(a.taint, assignment.obj, provenance) {
				changed = true
			}
			if !a.joined[assignment.obj] && a.isConcatenatedExpression(assignment.expr) {
				a.joined[assignment.obj] = true
				changed = true
			}
		}
	}
}

func (a *missingEscapeAnalyzer) expressionDependencies(expr ast.Expr) map[string]bool {
	out := map[string]bool{}
	ast.Inspect(expr, func(n ast.Node) bool {
		id, ok := n.(*ast.Ident)
		if !ok || id.Obj == nil {
			return true
		}
		for item := range a.dependencies[id.Obj] {
			out[item] = true
		}
		return true
	})
	if len(out) == 0 {
		return nil
	}
	return out
}

func (a *missingEscapeAnalyzer) findInScope(scope missingEscapeScope) []missingEscapeFinding {
	protected := map[*ast.CallExpr]bool{}
	ast.Inspect(scope.node, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if index, ok := a.sanitizerTextArgument(call); ok && index < len(call.Args) {
			a.protectConstructorPath(call.Args[index], protected)
		}
		return true
	})

	var findings []missingEscapeFinding
	ast.Inspect(scope.node, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.CallExpr:
			if args, ok := a.sinkArguments(n, protected); ok {
				for _, arg := range args {
					findings = append(findings, a.findingFor(scope.name, n, a.expressionTaint(arg))...)
				}
			}
		case *ast.ReturnStmt:
			for _, result := range n.Results {
				if a.isConcatenatedExpression(result) {
					findings = append(findings, a.findingFor(scope.name, result, a.expressionTaint(result))...)
				}
			}
		}
		return true
	})
	return findings
}

func (a *missingEscapeAnalyzer) isConcatenatedExpression(expr ast.Expr) bool {
	switch expr := expr.(type) {
	case *ast.ParenExpr:
		return a.isConcatenatedExpression(expr.X)
	case *ast.BinaryExpr:
		return expr.Op == token.ADD
	case *ast.Ident:
		return expr.Obj != nil && a.joined[expr.Obj]
	case *ast.CallExpr:
		path, name, ok := a.importedCall(expr)
		return ok && path == "strings" && name == "Join"
	}
	return false
}

func (a *missingEscapeAnalyzer) findingFor(scope string, sink ast.Node, provenance map[string]bool) []missingEscapeFinding {
	if len(provenance) == 0 {
		return nil
	}
	source := joinMissingEscapeSet(provenance)
	return []missingEscapeFinding{{
		file:        a.file,
		scope:       scope,
		sink:        missingEscapeNodeText(sink),
		provenance:  source,
		fingerprint: missingEscapeFingerprint(source),
		position:    a.fset.Position(sink.Pos()),
	}}
}

func (a *missingEscapeAnalyzer) sinkArguments(call *ast.CallExpr, protected map[*ast.CallExpr]bool) ([]ast.Expr, bool) {
	if path, name, ok := a.importedCall(call); ok && path == "fmt" {
		constructors := map[string]bool{"Sprintf": true, "Sprint": true, "Sprintln": true}
		if constructors[name] && protected[call] {
			return nil, false
		}
		skip := 0
		switch name {
		case "Fprint", "Fprintf", "Fprintln":
			skip = 1
		case "Append", "Appendf", "Appendln":
			skip = 1
		case "Print", "Printf", "Println", "Sprint", "Sprintf", "Sprintln", "Errorf":
		default:
			return nil, false
		}
		if skip >= len(call.Args) {
			return nil, true
		}
		return call.Args[skip:], true
	}

	if selector, ok := missingEscapeUnparen(call.Fun).(*ast.SelectorExpr); ok {
		switch selector.Sel.Name {
		case "Write", "WriteString":
			return call.Args, true
		}
	}
	if id, ok := missingEscapeUnparen(call.Fun).(*ast.Ident); ok && missingEscapeLocalLineBuilders[id.Name] &&
		(id.Obj == nil || id.Obj.Kind == ast.Fun) {
		return call.Args, true
	}
	if path, name, ok := a.importedCall(call); ok && path == cliImportPath {
		switch name {
		case "Wrap", "WriteLines":
			return call.Args, true
		}
	}
	return nil, false
}

func (a *missingEscapeAnalyzer) protectConstructorPath(expr ast.Expr, protected map[*ast.CallExpr]bool) {
	switch expr := expr.(type) {
	case *ast.ParenExpr:
		a.protectConstructorPath(expr.X, protected)
	case *ast.BinaryExpr:
		if expr.Op == token.ADD {
			a.protectConstructorPath(expr.X, protected)
			a.protectConstructorPath(expr.Y, protected)
		}
	case *ast.CallExpr:
		if a.isStringConversion(expr) {
			if len(expr.Args) == 1 {
				a.protectConstructorPath(expr.Args[0], protected)
			}
			return
		}
		if path, name, ok := a.importedCall(expr); ok && path == "fmt" &&
			(name == "Sprintf" || name == "Sprint" || name == "Sprintln") {
			protected[expr] = true
			for _, arg := range expr.Args {
				a.protectConstructorPath(arg, protected)
			}
			return
		}
		if path, name, ok := a.importedCall(expr); ok && path == "strings" && name == "Join" {
			for _, arg := range expr.Args {
				a.protectConstructorPath(arg, protected)
			}
		}
	case *ast.CompositeLit:
		for _, element := range expr.Elts {
			switch element := element.(type) {
			case *ast.KeyValueExpr:
				a.protectConstructorPath(element.Value, protected)
			case ast.Expr:
				a.protectConstructorPath(element, protected)
			}
		}
	}
}

func (a *missingEscapeAnalyzer) expressionTaint(expr ast.Expr) map[string]bool {
	if expr == nil {
		return nil
	}
	switch expr := expr.(type) {
	case *ast.Ident:
		out := cloneMissingEscapeSet(a.taint[expr.Obj])
		if missingEscapeErrorName(expr.Name) {
			if out == nil {
				out = map[string]bool{}
			}
			out["source:error-ident:"+expr.Name] = true
			for item := range a.dependencies[expr.Obj] {
				out[item] = true
			}
		}
		return out
	case *ast.BasicLit:
		return nil
	case *ast.ParenExpr:
		return a.expressionTaint(expr.X)
	case *ast.SelectorExpr:
		if id, ok := expr.X.(*ast.Ident); ok && id.Obj == nil && a.imports[id.Name] != "" {
			return nil
		}
		if missingEscapeSourceFields[expr.Sel.Name] {
			out := map[string]bool{"source:" + missingEscapeNodeText(expr): true}
			for item := range a.expressionTaint(expr.X) {
				out[item] = true
			}
			for item := range a.expressionDependencies(expr.X) {
				out[item] = true
			}
			return out
		}
		return nil
	case *ast.BinaryExpr:
		if expr.Op != token.ADD {
			return nil
		}
		return flowMissingEscapeTaint(expr, a.expressionTaint(expr.X), a.expressionTaint(expr.Y))
	case *ast.UnaryExpr:
		return a.expressionTaint(expr.X)
	case *ast.StarExpr:
		return a.expressionTaint(expr.X)
	case *ast.IndexExpr:
		return flowMissingEscapeTaint(expr, a.expressionTaint(expr.X), a.expressionTaint(expr.Index))
	case *ast.IndexListExpr:
		sets := []map[string]bool{a.expressionTaint(expr.X)}
		for _, index := range expr.Indices {
			sets = append(sets, a.expressionTaint(index))
		}
		return flowMissingEscapeTaint(expr, sets...)
	case *ast.SliceExpr:
		return flowMissingEscapeTaint(expr, a.expressionTaint(expr.X), a.expressionTaint(expr.Low),
			a.expressionTaint(expr.High), a.expressionTaint(expr.Max))
	case *ast.TypeAssertExpr:
		return a.expressionTaint(expr.X)
	case *ast.CompositeLit:
		var sets []map[string]bool
		for _, element := range expr.Elts {
			switch element := element.(type) {
			case *ast.KeyValueExpr:
				sets = append(sets, a.expressionTaint(element.Value))
			case ast.Expr:
				sets = append(sets, a.expressionTaint(element))
			}
		}
		return flowMissingEscapeTaint(expr, sets...)
	case *ast.CallExpr:
		if _, ok := a.sanitizerTextArgument(expr); ok {
			return a.taintOutsideSanitizerText(expr)
		}
		if a.isStructuredRenderer(expr) {
			return nil
		}
		if a.isDirectSourceCall(expr) {
			return a.directSourceTaint(expr)
		}
		if a.isNumericCall(expr) {
			return nil
		}
		sets := make([]map[string]bool, 0, len(expr.Args))
		if selector, ok := missingEscapeUnparen(expr.Fun).(*ast.SelectorExpr); ok {
			if id, imported := selector.X.(*ast.Ident); !imported || id.Obj != nil || a.imports[id.Name] == "" {
				sets = append(sets, a.expressionTaint(selector.X))
			}
		}
		for _, arg := range expr.Args {
			sets = append(sets, a.expressionTaint(arg))
		}
		return flowMissingEscapeTaint(expr, sets...)
	}
	return nil
}

func missingEscapeErrorName(name string) bool {
	lower := strings.ToLower(name)
	return lower != "stderr" && strings.HasSuffix(lower, "err")
}

func (a *missingEscapeAnalyzer) directSourceTaint(call *ast.CallExpr) map[string]bool {
	out := map[string]bool{"source:" + missingEscapeNodeText(call): true}
	var dependencies []ast.Expr
	if selector, ok := missingEscapeUnparen(call.Fun).(*ast.SelectorExpr); ok {
		if id, imported := selector.X.(*ast.Ident); !imported || id.Obj != nil || a.imports[id.Name] == "" {
			dependencies = append(dependencies, selector.X)
		}
	}
	dependencies = append(dependencies, call.Args...)
	for _, dependency := range dependencies {
		for item := range a.expressionTaint(dependency) {
			out[item] = true
		}
		for item := range a.expressionDependencies(dependency) {
			out[item] = true
		}
	}
	return out
}

func (a *missingEscapeAnalyzer) taintOutsideSanitizerText(call *ast.CallExpr) map[string]bool {
	index, _ := a.sanitizerTextArgument(call)
	var sets []map[string]bool
	for i, arg := range call.Args {
		if i == index {
			continue
		}
		sets = append(sets, a.expressionTaint(arg))
	}
	return flowMissingEscapeTaint(call, sets...)
}

func (a *missingEscapeAnalyzer) sanitizerTextArgument(call *ast.CallExpr) (int, bool) {
	if path, name, ok := a.importedCall(call); ok {
		switch path {
		case cliImportPath:
			switch name {
			case "Foreign", "ReportTitle":
				return 0, true
			case "ReportLine":
				return 1, true
			}
		case apiImportPath:
			if name == "OneLine" {
				return 0, true
			}
		case "strconv":
			if name == "Quote" {
				return 0, true
			}
		}
	}
	if id, ok := missingEscapeUnparen(call.Fun).(*ast.Ident); ok && missingEscapeLocalSanitizers[id.Name] &&
		(id.Obj == nil || id.Obj.Kind == ast.Fun) {
		return missingEscapeLocalSanitizerTextArguments[id.Name], true
	}
	return 0, false
}

func (a *missingEscapeAnalyzer) isStructuredRenderer(call *ast.CallExpr) bool {
	path, name, ok := a.importedCall(call)
	return ok && path == cliImportPath && missingEscapeStructuredRenderers[name]
}

func (a *missingEscapeAnalyzer) isDirectSourceCall(call *ast.CallExpr) bool {
	if selector, ok := missingEscapeUnparen(call.Fun).(*ast.SelectorExpr); ok && selector.Sel.Name == "Error" && len(call.Args) == 0 {
		return true
	}
	if path, name, ok := a.importedCall(call); ok {
		if path == "os" && (name == "Getenv" || name == "LookupEnv" || name == "UserHomeDir") {
			return true
		}
		if path == configImportPath && name == "Path" {
			return true
		}
		if path == apiImportPath && name == "TokenPath" {
			return true
		}
		if path == storeImportPath && name == "DefaultPath" {
			return true
		}
	}
	if id, ok := missingEscapeUnparen(call.Fun).(*ast.Ident); ok && missingEscapeLocalSourceCalls[id.Name] &&
		(id.Obj == nil || id.Obj.Kind == ast.Fun) {
		return true
	}
	return false
}

func (a *missingEscapeAnalyzer) isNumericCall(call *ast.CallExpr) bool {
	if id, ok := missingEscapeUnparen(call.Fun).(*ast.Ident); ok {
		if id.Obj != nil {
			return false
		}
		switch id.Name {
		case "cap", "len", "int", "int8", "int16", "int32", "int64",
			"uint", "uint8", "uint16", "uint32", "uint64", "uintptr",
			"float32", "float64", "complex64", "complex128":
			return true
		}
	}
	if path, name, ok := a.importedCall(call); ok && path == "unicode/utf8" {
		return name == "RuneCount" || name == "RuneCountInString"
	}
	return false
}

func (a *missingEscapeAnalyzer) isStringConversion(call *ast.CallExpr) bool {
	id, ok := missingEscapeUnparen(call.Fun).(*ast.Ident)
	return ok && id.Name == "string" && id.Obj == nil
}

func (a *missingEscapeAnalyzer) importedCall(call *ast.CallExpr) (string, string, bool) {
	selector, ok := missingEscapeUnparen(call.Fun).(*ast.SelectorExpr)
	if !ok {
		return "", "", false
	}
	id, ok := selector.X.(*ast.Ident)
	if !ok || id.Obj != nil {
		return "", "", false
	}
	path := a.imports[id.Name]
	if path == "" {
		return "", "", false
	}
	return path, selector.Sel.Name, true
}

func missingEscapeUnparen(expr ast.Expr) ast.Expr {
	for {
		paren, ok := expr.(*ast.ParenExpr)
		if !ok {
			return expr
		}
		expr = paren.X
	}
}

func flowMissingEscapeTaint(node ast.Node, sets ...map[string]bool) map[string]bool {
	out := map[string]bool{}
	for _, set := range sets {
		for item := range set {
			out[item] = true
		}
	}
	if len(out) == 0 {
		return nil
	}
	out["flow:"+missingEscapeNodeText(node)] = true
	return out
}

func mergeMissingEscapeTaint(all map[*ast.Object]map[string]bool, obj *ast.Object, add map[string]bool) bool {
	if obj == nil || len(add) == 0 {
		return false
	}
	if all[obj] == nil {
		all[obj] = map[string]bool{}
	}
	changed := false
	for item := range add {
		if !all[obj][item] {
			all[obj][item] = true
			changed = true
		}
	}
	return changed
}

func cloneMissingEscapeSet(in map[string]bool) map[string]bool {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]bool, len(in))
	for item := range in {
		out[item] = true
	}
	return out
}

func joinMissingEscapeSet(in map[string]bool) string {
	items := make([]string, 0, len(in))
	for item := range in {
		items = append(items, item)
	}
	sort.Strings(items)
	return strings.Join(items, " | ")
}

func missingEscapeNodeText(node ast.Node) string {
	var out bytes.Buffer
	if err := (&printer.Config{Mode: printer.RawFormat, Tabwidth: 8}).Fprint(&out, token.NewFileSet(), node); err != nil {
		panic(fmt.Sprintf("print AST node: %v", err))
	}
	return strings.TrimSpace(out.String())
}

func missingEscapeFingerprint(provenance string) string {
	sum := sha256.Sum256([]byte(provenance))
	return fmt.Sprintf("%x", sum)
}

func missingEscapeFindingKey(finding missingEscapeFinding) string {
	return strings.Join([]string{finding.file, finding.scope, finding.sink, finding.fingerprint}, "\x00")
}

func missingEscapeExceptionKey(exception missingEscapeException) string {
	return strings.Join([]string{exception.file, exception.scope, exception.sink, exception.fingerprint}, "\x00")
}

func exceptionForFinding(finding missingEscapeFinding, why string) missingEscapeException {
	return missingEscapeException{
		file:        finding.file,
		scope:       finding.scope,
		sink:        finding.sink,
		fingerprint: finding.fingerprint,
		count:       finding.count,
		why:         why,
	}
}

func missingEscapeInventoryProblems(findings []missingEscapeFinding, exceptions []missingEscapeException) []string {
	found := map[string]missingEscapeFinding{}
	for _, finding := range findings {
		found[missingEscapeFindingKey(finding)] = finding
	}
	excused := map[string]missingEscapeException{}
	var problems []string
	for _, exception := range exceptions {
		key := missingEscapeExceptionKey(exception)
		if _, duplicate := excused[key]; duplicate {
			problems = append(problems, fmt.Sprintf("duplicate missing-escape exception for %s %s: %s",
				exception.file, exception.scope, exception.sink))
			continue
		}
		excused[key] = exception
		if exception.file == "" || exception.scope == "" || exception.sink == "" ||
			exception.fingerprint == "" || exception.count < 1 || exception.why == "" {
			problems = append(problems, fmt.Sprintf("incomplete missing-escape exception for %s %s", exception.file, exception.scope))
		}
	}
	for key, finding := range found {
		exception, ok := excused[key]
		if !ok {
			problems = append(problems, fmt.Sprintf("%s: %s reaches %s without a reviewed escape boundary; provenance %s; fingerprint %s",
				finding.position, finding.scope, finding.sink, finding.provenance, finding.fingerprint))
			continue
		}
		if exception.count != finding.count {
			problems = append(problems, fmt.Sprintf("missing-escape exception for %s %s expects %d occurrence(s), found %d",
				finding.file, finding.scope, exception.count, finding.count))
		}
	}
	for key, exception := range excused {
		if _, ok := found[key]; !ok {
			problems = append(problems, fmt.Sprintf("stale missing-escape exception for %s %s: %s; source or sink no longer matches",
				exception.file, exception.scope, exception.sink))
		}
	}
	sort.Strings(problems)
	return problems
}
