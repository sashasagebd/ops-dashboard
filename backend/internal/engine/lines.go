package engine

import (
	"bytes"
	"regexp"
	"strings"
	"unicode/utf8"
)

// MaxLineBytes is the longest log line kept; anything past it is cut off.
// Generous for real output, but bounds what a single line (a minified file
// printed by a build step, say) can cost the database and the browser.
const MaxLineBytes = 4096

// lineWriter turns text written in arbitrary pieces (Docker sends build
// output and container logs in chunks that needn't end at a newline) into
// whole lines, passed to emit without the newline.
type lineWriter struct {
	buf  []byte
	emit func(string)
}

func (w *lineWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	rest := w.buf
	for {
		i := bytes.IndexByte(rest, '\n')
		if i < 0 {
			break
		}
		w.emit(string(rest[:i]))
		rest = rest[i+1:]
	}
	// A "line" that never ends would otherwise grow without limit: pass on
	// what there is once it's well past the longest line kept.
	if len(rest) > 4*MaxLineBytes {
		w.emit(string(rest))
		rest = nil
	}
	w.buf = append(w.buf[:0], rest...)
	return len(p), nil
}

// Flush passes on a last line that didn't end with a newline.
func (w *lineWriter) Flush() {
	if len(w.buf) > 0 {
		w.emit(string(w.buf))
		w.buf = w.buf[:0]
	}
}

// ansiEscape matches terminal escape sequences (colours, cursor movement),
// which tools such as npm print and which mean nothing in a web page.
var ansiEscape = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`)

// cleanLine makes a line of program output fit for storing and showing: what
// a terminal would end up displaying for a line redrawn with carriage
// returns (progress bars), without escape sequences, as valid UTF-8 without
// NUL bytes (Postgres text can't hold them), at most MaxLineBytes long.
func cleanLine(s string) string {
	s = strings.TrimRight(s, "\r")
	if i := strings.LastIndexByte(s, '\r'); i >= 0 {
		s = s[i+1:]
	}
	s = ansiEscape.ReplaceAllString(s, "")
	s = strings.ReplaceAll(s, "\x00", "")
	s = strings.ToValidUTF8(s, "�")
	if len(s) > MaxLineBytes {
		cut := MaxLineBytes
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		s = s[:cut] + " …(line cut)"
	}
	return s
}
