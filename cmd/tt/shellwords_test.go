package main

import "testing"

func wordValues(cmds []simpleCommand) [][]string {
	out := make([][]string, len(cmds))
	for i, cmd := range cmds {
		vals := make([]string, len(cmd.words))
		for j, w := range cmd.words {
			vals[j] = w.value
		}
		out[i] = vals
	}
	return out
}

func equalWords(a, b [][]string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if len(a[i]) != len(b[i]) {
			return false
		}
		for j := range a[i] {
			if a[i][j] != b[i][j] {
				return false
			}
		}
	}
	return true
}

func TestSplitShellWordsModelled(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want [][]string
	}{
		{"empty string", "", nil},
		{"only separators", "  ;\t; \n ", nil},
		{"single bare word", "notify-send", [][]string{{"notify-send"}}},
		{"single quotes are literal", `echo 'a $b "c" \d'`, [][]string{{"echo", `a $b "c" \d`}}},
		{"double quotes keep spaces", `echo "a b"`, [][]string{{"echo", "a b"}}},
		{"double-quote escapes", "echo \"\\$x \\\"y\\\" \\\\z \\q\"", [][]string{{"echo", `$x "y" \z \q`}}},
		{"backslash escape outside quotes", `echo a\ b`, [][]string{{"echo", "a b"}}},
		{"line continuation outside quotes", "echo a\\\nb", [][]string{{"echo", "ab"}}},
		{"escaped = is not an assignment prefix", `X\=y "$TT_TASK"`, [][]string{{"X=y", "$TT_TASK"}}},
		{"escaped digit is not a file descriptor prefix", `cmd -m \2>/dev/null x`, [][]string{{"cmd", "-m", "2", "x"}}},
		{"escaped brace is an ordinary word", `notify-send \{ x`, [][]string{{"notify-send", "{", "x"}}},
		{"escaped reserved word names a program", `\if cmd`, [][]string{{"if", "cmd"}}},
		{"-- inside single quotes is not a terminator by value", `notify-send '--'`, [][]string{{"notify-send", "--"}}},
		{"-- inside double quotes is not a terminator by value", `notify-send "--"`, [][]string{{"notify-send", "--"}}},
		{"-- inside a longer word is not --", `notify-send "tt -- done"`, [][]string{{"notify-send", "tt -- done"}}},
		{"bare $NAME value is not expanded", `notify-send $TT_TASK`, [][]string{{"notify-send", "$TT_TASK"}}},
		{"quoted $NAME value is not expanded", `notify-send "$TT_TASK"`, [][]string{{"notify-send", "$TT_TASK"}}},
		{"semicolon splits commands", "echo a; echo b", [][]string{{"echo", "a"}, {"echo", "b"}}},
		{"newline splits commands", "echo a\necho b", [][]string{{"echo", "a"}, {"echo", "b"}}},
		{"& splits commands", "echo a & echo b", [][]string{{"echo", "a"}, {"echo", "b"}}},
		{"&& splits commands", "echo a && echo b", [][]string{{"echo", "a"}, {"echo", "b"}}},
		{"| splits commands", "echo a | echo b", [][]string{{"echo", "a"}, {"echo", "b"}}},
		{"|| splits commands", "echo a || echo b", [][]string{{"echo", "a"}, {"echo", "b"}}},
		{"run of separators makes no empty command", "echo a ;;; echo b", [][]string{{"echo", "a"}, {"echo", "b"}}},
		{"& inside quotes is not a separator", `notify-send -- "tt & co" "$TT_TASK"`, [][]string{{"notify-send", "--", "tt & co", "$TT_TASK"}}},
		{
			"parentheses split rather than nest",
			`(sleep 0; notify-send "$TT_TASK")`,
			[][]string{{"sleep", "0"}, {"notify-send", "$TT_TASK"}},
		},
		{"assignment prefix is not the program word", "DISPLAY=:0 notify-send x", [][]string{{"notify-send", "x"}}},
		{"two assignment prefixes", "A=1 B=2 cmd x", [][]string{{"cmd", "x"}}},
		{"= later in a word is an ordinary word", "notify-send --urgency=low", [][]string{{"notify-send", "--urgency=low"}}},
		{"redirection target is not a word", "dump.sh x >> /tmp/log", [][]string{{"dump.sh", "x"}}},
		{"line continuation before a redirection target", "dump.sh > \\\n /tmp/x \"$TT_TASK\"", [][]string{{"dump.sh", "$TT_TASK"}}},
		{"redirection before the command", ">out.txt echo hi", [][]string{{"echo", "hi"}}},
		{"digit-prefixed redirection", "notify-send 2>/dev/null x", [][]string{{"notify-send", "x"}}},
		{"digit run only counts at a word boundary", "echo foo2>bar", [][]string{{"echo", "foo2"}}},
		{"fd duplication leaves no stray word", "cmd 2>&1", [][]string{{"cmd"}}},
		{"<> operator", "cmd <>file", [][]string{{"cmd"}}},
		{"<& operator", "cmd <&3", [][]string{{"cmd"}}},
		{">& operator", "cmd >&2", [][]string{{"cmd"}}},
		{"<<< operator", "cmd <<<word", [][]string{{"cmd"}}},
		{"empty single-quoted word", "cmd ''", [][]string{{"cmd", ""}}},
		{"empty double-quoted word", `cmd ""`, [][]string{{"cmd", ""}}},
		{"escaped quote at a word boundary", `echo \"abc\"`, [][]string{{"echo", `"abc"`}}},
		{"nested different quote kinds in one word", `echo 'sq'"dq"bare`, [][]string{{"echo", "sqdqbare"}}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cmds, ok := splitShellWords(c.in)
			if !ok {
				t.Fatalf("splitShellWords(%q) ok = false, want true", c.in)
			}
			if got := wordValues(cmds); !equalWords(got, c.want) {
				t.Errorf("splitShellWords(%q) = %v, want %v", c.in, got, c.want)
			}
		})
	}
}

func TestSplitShellWordsRefusals(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"command substitution", "echo $(date)"},
		{"command substitution mid double quote", `echo "hi $(date)"`},
		{"backquote substitution", "echo `date`"},
		{"dollar-single-quoted string", `argv $'--' y`},
		{"dollar-double-quoted string", `argv $"x" y`},
		{"process substitution <(", "diff <(a) <(b)"},
		{"process substitution >(", "tee >(cat)"},
		{"heredoc", "cat <<EOF"},
		{"heredoc is not <<<", "cat <<E"},
		{"combined &>", "cmd &>file"},
		{"combined &>>", "cmd &>>file"},
		{"combined |&", "cmd1 |& cmd2"},
		{"unterminated single quote", "echo 'abc"},
		{"unterminated double quote", `echo "abc`},
		{"trailing lone backslash", `echo abc\`},
		{"parameter expansion with a modifier", "echo ${TT_TASK:-none}"},
		{"parameter expansion with a suffix modifier", "echo ${TT_TASK#x}"},
		{"empty parameter braces", "echo ${}"},
		{"unclosed parameter braces", "echo ${TT_TASK"},
		{"lone { word", "notify-send { echo hi ; }"},
		{"lone } word", "echo }"},
		{"reserved word in program position", "if cmd"},
		{"redirection with nothing after it at end of string", "cmd >"},
		{"redirection whose target is only a line continuation", "dump.sh > \\\n"},
		{"digit-prefixed redirection with nothing after it", "cmd 2>"},
		{"redirection followed by a separator", "cmd > ;"},
		{"redirection followed by another operator", "cmd > > x"},
		{"unmodelled construct in the second command of a chain", "echo hi; echo $(date)"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cmds, ok := splitShellWords(c.in)
			if ok {
				t.Fatalf("splitShellWords(%q) ok = true, want false (cmds=%v)", c.in, cmds)
			}
			if cmds != nil {
				t.Errorf("splitShellWords(%q) returned commands on refusal: %v", c.in, cmds)
			}
		})
	}
}

func TestSplitShellWordsSegments(t *testing.T) {
	in := `echo "$TT_TASK"`
	cmds, ok := splitShellWords(in)
	if !ok || len(cmds) != 1 || len(cmds[0].words) != 2 {
		t.Fatalf("splitShellWords(%q) = %v, %v, want one command of two words", in, cmds, ok)
	}
	w := cmds[0].words[1]
	if w.value != "$TT_TASK" {
		t.Fatalf("value = %q, want $TT_TASK", w.value)
	}
	if len(w.segs) != 1 || w.segs[0].q != quoteDouble {
		t.Fatalf("segs = %+v, want one quoteDouble segment", w.segs)
	}
	if got := in[w.segs[0].from:w.segs[0].to]; got != "$TT_TASK" {
		t.Errorf("segment covers %q, want $TT_TASK", got)
	}

	in = `echo 'sq'"dq"bare`
	cmds, ok = splitShellWords(in)
	if !ok || len(cmds) != 1 || len(cmds[0].words) != 2 {
		t.Fatalf("splitShellWords(%q) = %v, %v, want one command of two words", in, cmds, ok)
	}
	w = cmds[0].words[1]
	wantSeg := []struct {
		q    quoting
		text string
	}{
		{quoteSingle, "sq"},
		{quoteDouble, "dq"},
		{quoteNone, "bare"},
	}
	if len(w.segs) != len(wantSeg) {
		t.Fatalf("segs = %+v, want %d segments", w.segs, len(wantSeg))
	}
	for i, want := range wantSeg {
		if w.segs[i].q != want.q {
			t.Errorf("segs[%d].q = %v, want %v", i, w.segs[i].q, want.q)
		}
		if got := in[w.segs[i].from:w.segs[i].to]; got != want.text {
			t.Errorf("segs[%d] covers %q, want %q", i, got, want.text)
		}
	}
}

func TestSplitShellWordsAssignmentRequiresUnquoted(t *testing.T) {
	cmds, ok := splitShellWords("X=y env")
	if !ok || len(cmds) != 1 || len(cmds[0].words) != 1 || cmds[0].words[0].value != "env" {
		t.Fatalf(`splitShellWords("X=y env") = %v, %v, want one command with only "env"`, cmds, ok)
	}

	cmds, ok = splitShellWords(`"X=y" env`)
	if !ok || len(cmds) != 1 || len(cmds[0].words) != 2 {
		t.Fatalf(`splitShellWords("\"X=y\" env") = %v, %v, want a two-word command`, cmds, ok)
	}
	if got := wordValues(cmds); !equalWords(got, [][]string{{"X=y", "env"}}) {
		t.Errorf(`quoted "X=y" was stripped as an assignment: words = %v`, got)
	}
}

func TestSplitShellWordsReservedWordRequiresUnquoted(t *testing.T) {
	if _, ok := splitShellWords("if cmd"); ok {
		t.Errorf(`splitShellWords("if cmd") ok = true, want false`)
	}
	cmds, ok := splitShellWords(`"if" cmd`)
	if !ok {
		t.Fatalf(`splitShellWords("\"if\" cmd") ok = false, want true`)
	}
	if got := wordValues(cmds); !equalWords(got, [][]string{{"if", "cmd"}}) {
		t.Errorf(`quoted "if" was treated as reserved: words = %v`, got)
	}
}

func TestSplitShellWordsLineContinuationStartsNoWord(t *testing.T) {
	cmds, ok := splitShellWords("argv -m \\\n \"$TT_TASK\"")
	if !ok {
		t.Fatalf("splitShellWords ok = false, want true")
	}
	if got := wordValues(cmds); !equalWords(got, [][]string{{"argv", "-m", "$TT_TASK"}}) {
		t.Errorf("words = %v, want the continuation to leave no word behind", got)
	}

	cmds, ok = splitShellWords(`argv -i "" "$TT_TASK"`)
	if !ok {
		t.Fatalf("splitShellWords ok = false, want true")
	}
	if got := wordValues(cmds); !equalWords(got, [][]string{{"argv", "-i", "", "$TT_TASK"}}) {
		t.Errorf("words = %v, want the explicit empty argument to stay a word", got)
	}
}

func TestSplitShellWordsEscapeHasItsOwnQuoting(t *testing.T) {
	in := `echo \$TT_TASK`
	cmds, ok := splitShellWords(in)
	if !ok || len(cmds) != 1 || len(cmds[0].words) != 2 {
		t.Fatalf("splitShellWords(%q) = %v, %v, want one command of two words", in, cmds, ok)
	}
	w := cmds[0].words[1]
	if w.value != "$TT_TASK" {
		t.Fatalf("value = %q, want $TT_TASK", w.value)
	}
	wantSeg := []struct {
		q    quoting
		text string
	}{
		{quoteEscaped, "$"},
		{quoteNone, "TT_TASK"},
	}
	if len(w.segs) != len(wantSeg) {
		t.Fatalf("segs = %+v, want %d segments", w.segs, len(wantSeg))
	}
	for i, want := range wantSeg {
		if w.segs[i].q != want.q {
			t.Errorf("segs[%d].q = %v, want %v", i, w.segs[i].q, want.q)
		}
		if got := in[w.segs[i].from:w.segs[i].to]; got != want.text {
			t.Errorf("segs[%d] covers %q, want %q", i, got, want.text)
		}
	}
}
