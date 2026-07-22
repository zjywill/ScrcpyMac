package adb

import "strings"

// Quote is Python's shlex.quote, reimplemented exactly.
//
// It is the single most fragile escaping rule in the port: phone_type and
// phone_paste build a device-side shell command by string concatenation, and the
// quoting is what keeps Chinese text, spaces, quotes and shell metacharacters
// from being reinterpreted by /system/bin/sh on the phone.
//
// The safe set is Python's re.compile(r"[^\w@%+=:,./-]", re.ASCII). re.ASCII
// makes \w exactly [A-Za-z0-9_], so the complete set of characters that may
// appear unquoted is:
//
//	A-Z  a-z  0-9  _  @  %  +  =  :  ,  .  /  -
//
// Everything else forces single-quoting — space, $, backtick, ", ', \, ;, |, &,
// <, >, (, ), {, }, [, ], !, #, *, ?, ~, ^, newline, tab, and **every non-ASCII
// rune**. Getting that last part backwards is what would silently break Chinese
// paste, so the scan is byte-wise: any byte >= 0x80 is unsafe, which is exactly
// what re.ASCII means for a UTF-8 string.
//
// An embedded single quote becomes '"'"' — close, escaped quote, reopen. The
// empty string becomes a bare pair of single quotes: an empty argument, not
// nothing at all.
//
// The result is concatenated into a command string that is handed to adb as ONE
// argv element; the device shell does the unquoting. Never quote a second time
// for the host — exec.Command passes argv directly and no host shell is
// involved, exactly like Python's subprocess.run(list).
func Quote(s string) string {
	if s == "" {
		return "''"
	}
	safe := true
	for i := 0; i < len(s); i++ {
		if !shellSafe[s[i]] {
			safe = false
			break
		}
	}
	if safe {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

// shellSafe[b] is true for the bytes shlex.quote leaves unquoted.
var shellSafe = func() (table [256]bool) {
	const extra = "_@%+=:,./-"
	for b := byte('a'); b <= 'z'; b++ {
		table[b] = true
	}
	for b := byte('A'); b <= 'Z'; b++ {
		table[b] = true
	}
	for b := byte('0'); b <= '9'; b++ {
		table[b] = true
	}
	for i := 0; i < len(extra); i++ {
		table[extra[i]] = true
	}
	return table
}()

// EscapeInputText applies the `input text` escaping actions.py performs before
// quoting: `%` becomes `%25`, then a space becomes `%s`.
//
// THE ORDER IS LOAD-BEARING. Doing it the other way round corrupts every string
// containing a space: the `%s` written for the space would itself be rewritten
// to `%25s`. Together with Quote the full command is
//
//	"input text " + adb.Quote(adb.EscapeInputText(text))
//
// so an ASCII string with spaces ends up unquoted (`hello%sworld`, because `%`
// and `s` are both in the safe set) while anything non-ASCII is single-quoted.
//
// Android's `input text` decodes `%s` back to a space; other `%XX` sequences are
// not decoded on most Android versions, so a literal `%` really is typed as
// `%25` on many devices. That is a pre-existing fidelity bug in the Python and
// it is replicated deliberately — it is also the only escape that keeps spaces
// working, and phone_type's own description already says to use phone_paste for
// anything that is not short ASCII.
func EscapeInputText(text string) string {
	return strings.ReplaceAll(strings.ReplaceAll(text, "%", "%25"), " ", "%s")
}
