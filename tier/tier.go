// Package tier chooses a line's check tier by what it changes (PROTOCOL.md
// §5.4): the full tier, or the light tier for inert content, which no build,
// test or generator reads.
//
// The classifier can only err towards the full tier. A change is light only
// when every one of its files is known, matches a pattern declared inert, and
// matches no pattern declared read. No files, a file list that could not be
// read, a path it cannot make sense of, a file under no inert pattern: full.
// So adding a file to a change never lightens it, and a rule left out costs
// time, never a check that should have run.
package tier

import (
	"path"
	"strings"
)

// Tier is a check tier. The zero value is Full: what is not classified is
// checked in full.
type Tier uint8

const (
	Full  Tier = iota // every check
	Light             // the light check: inert content only
)

func (t Tier) String() string {
	if t == Light {
		return "light"
	}
	return "full"
}

// Rules say what is inert. A pattern is a slash-separated path whose
// segments are path.Match patterns; a segment "**" matches any number of
// segments, none included. "docs/**" is everything under docs; "**/*.md" is
// every Markdown file.
type Rules struct {
	// Inert are the patterns of content nothing builds, tests or generates
	// from.
	Inert []string
	// Read are patterns of files that something does read, though an inert
	// pattern also matches them: a document a test checks, a page a
	// generator embeds. A file matching one is never inert.
	Read []string
}

// Configured reports whether any content is declared inert. With no rules
// every change is full.
func (r Rules) Configured() bool { return len(r.Inert) > 0 }

// Classify gives the tier of a change that touches files: every path it
// adds, changes, removes or renames, the old name of a rename included.
func (r Rules) Classify(files []string) Tier {
	if !r.Configured() || len(files) == 0 {
		return Full
	}
	for _, f := range files {
		if !clean(f) || !matchAny(r.Inert, f) || matchAny(r.Read, f) {
			return Full
		}
	}
	return Light
}

// clean reports whether f is a plain relative path: nothing a pattern could
// be fooled by.
func clean(f string) bool {
	if f == "" || strings.HasPrefix(f, "/") || strings.ContainsAny(f, "\\\x00\n") || path.Clean(f) != f {
		return false
	}
	for _, seg := range strings.Split(f, "/") {
		if seg == ".." || seg == "." || seg == "" {
			return false
		}
	}
	return true
}

func matchAny(patterns []string, f string) bool {
	for _, p := range patterns {
		if Match(p, f) {
			return true
		}
	}
	return false
}

// Match reports whether the path matches the pattern. A malformed pattern
// matches nothing.
func Match(pattern, f string) bool {
	if pattern == "" {
		return false
	}
	return match(strings.Split(pattern, "/"), strings.Split(f, "/"))
}

func match(pat, segs []string) bool {
	for len(pat) > 0 {
		if pat[0] == "**" {
			for i := 0; i <= len(segs); i++ {
				if match(pat[1:], segs[i:]) {
					return true
				}
			}
			return false
		}
		if len(segs) == 0 {
			return false
		}
		if ok, err := path.Match(pat[0], segs[0]); err != nil || !ok {
			return false
		}
		pat, segs = pat[1:], segs[1:]
	}
	return len(segs) == 0
}
