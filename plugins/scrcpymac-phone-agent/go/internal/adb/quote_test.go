package adb

import "testing"

// TestQuoteMatchesShlexQuote pins Quote against output captured from the real
// Python interpreter (python3 -c 'import shlex; print(shlex.quote(s))'). Every
// expectation below was produced that way, not reasoned about.
func TestQuoteMatchesShlexQuote(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"plain ascii", "abc", "abc"},
		{"the whole safe set", "a-b_c.d/e:f,g+h=i@j%k", "a-b_c.d/e:f,g+h=i@j%k"},
		{"digits", "0", "0"},
		{"leading dashes are not special", "--flag", "--flag"},
		{"empty string becomes an empty argument", "", "''"},
		{"space forces quoting", "a b", "'a b'"},
		{"tab forces quoting", "tab\tx", "'tab\tx'"},
		{"newline forces quoting", "a\nb", "'a\nb'"},
		{"dollar is neutralised", "$HOME", "'$HOME'"},
		{"backtick", "`id`", "'`id`'"},
		{"semicolon", "a;b", "'a;b'"},
		{"pipe", "a|b", "'a|b'"},
		{"ampersand", "a&b", "'a&b'"},
		{"redirection", "a<b>c", "'a<b>c'"},
		{"glob", "*", "'*'"},
		{"tilde", "~x", "'~x'"},
		{"parens", "(x)", "'(x)'"},
		{"braces", "{}", "'{}'"},
		{"brackets", "[]", "'[]'"},
		{"bang", "!", "'!'"},
		{"hash", "#", "'#'"},
		{"question", "?", "'?'"},
		{"caret", "^", "'^'"},
		{"backslash", `\`, `'\'`},
		{"double quotes", `"q"`, `'"q"'`},
		{"single quote is closed, escaped and reopened", "it's", `'it'"'"'s'`},
		{"several single quotes", "a'b'c", `'a'"'"'b'"'"'c'`},
		{"a string of only quotes", "''", `''"'"''"'"''`},
		// re.ASCII is what makes every non-ASCII rune unsafe. Getting this
		// backwards is what would silently break Chinese paste.
		{"chinese", "你好", "'你好'"},
		{"chinese with a space", "你好 世界", "'你好 世界'"},
		{"emoji", "emoji😀", "'emoji😀'"},
		{"non-breaking space", "a b", "'a b'"},
		{"percent alone is safe", "%", "%"},
		{"percent with a space is not", "% ", "'% '"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Quote(tt.in); got != tt.want {
				t.Errorf("Quote(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestEscapeInputTextOrdering(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"ascii is untouched", "abc", "abc"},
		{"space becomes %s", "a b", "a%sb"},
		{"percent becomes %25 first", "%", "%25"},
		// The order is what makes this reversible: a literal "%s" the user typed
		// must not survive as a space.
		{"a literal %s is escaped, not turned into a space", "hello%sworld", "hello%25sworld"},
		{"percent then space", "% ", "%25%s"},
		{"multiple spaces", "a b c", "a%sb%sc"},
		{"chinese with a space", "你好 世界", "你好%s世界"},
		{"empty", "", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := EscapeInputText(tt.in); got != tt.want {
				t.Errorf("EscapeInputText(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestInputTextCommand pins the full composition phone_type performs, again
// against real shlex.quote output. This is the string that reaches
// /system/bin/sh on the device as one argv element.
func TestInputTextCommand(t *testing.T) {
	tests := []struct {
		name string
		text string
		want string
	}{
		{"ascii with spaces ends up unquoted", "hello world", "input text hello%sworld"},
		{"safe set survives", "a-b_c.d/e:f,g+h=i@j%k", "input text a-b_c.d/e:f,g+h=i@j%25k"},
		{"chinese is single-quoted", "你好 世界", "input text '你好%s世界'"},
		{"emoji", "emoji😀", "input text 'emoji😀'"},
		{"apostrophe", "it's", `input text 'it'"'"'s'`},
		{"metacharacters are inert", "$(rm -rf /);echo", `input text '$(rm%s-rf%s/);echo'`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := "input text " + Quote(EscapeInputText(tt.text))
			if got != tt.want {
				t.Errorf("type command for %q =\n %s\nwant %s", tt.text, got, tt.want)
			}
		})
	}
}

// TestClipboardCommand pins phone_paste's device command. Unlike type_text it
// does NOT pre-escape, so text goes to the clipboard verbatim.
func TestClipboardCommand(t *testing.T) {
	tests := []struct {
		name string
		text string
		want string
	}{
		{"chinese keeps its spaces", "你好 世界", "cmd clipboard set-text '你好 世界'"},
		{"ascii sentence", "hello world", "cmd clipboard set-text 'hello world'"},
		{"apostrophe", "it's", `cmd clipboard set-text 'it'"'"'s'`},
		{"injection attempt is inert", "x'; reboot; '", `cmd clipboard set-text 'x'"'"'; reboot; '"'"''`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := "cmd clipboard set-text " + Quote(tt.text)
			if got != tt.want {
				t.Errorf("paste command for %q =\n %s\nwant %s", tt.text, got, tt.want)
			}
		})
	}
}

func FuzzQuoteRoundTripsThroughSh(f *testing.F) {
	// Whatever Quote produces must be a single sh word that unquotes back to the
	// original. The device runs /system/bin/sh, but the quoting rules exercised
	// here (single quotes, '"'"' splicing) are plain POSIX.
	for _, seed := range []string{"", "abc", "a b", "it's", "你好 世界", "$HOME", "a\nb", `\`, "%"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, s string) {
		quoted := Quote(s)
		if got := unquoteSh(t, quoted); got != s {
			t.Errorf("Quote(%q) = %q, which unquotes to %q", s, quoted, got)
		}
	})
}

// unquoteSh reverses POSIX single-quoting: the string is either bare or a
// sequence of '...' segments spliced with escaped quotes.
func unquoteSh(t *testing.T, s string) string {
	t.Helper()
	var out []byte
	inQuotes := false
	for i := 0; i < len(s); i++ {
		switch {
		case s[i] == '\'':
			inQuotes = !inQuotes
		case !inQuotes && s[i] == '"':
			// The '"'"' splice: a double-quoted single quote.
			if i+2 < len(s) && s[i+1] == '\'' && s[i+2] == '"' {
				out = append(out, '\'')
				i += 2
				continue
			}
			t.Fatalf("unexpected double quote in %q at %d", s, i)
		default:
			out = append(out, s[i])
		}
	}
	if inQuotes {
		t.Fatalf("unbalanced quotes in %q", s)
	}
	return string(out)
}
