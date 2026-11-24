package store

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/rolandpj1968/shunter/state"
)

// The line files are the queue at rest as three plain lists, the shape a
// script queue keeps (docs/cutover.md): the rollback path out of Shunter,
// and a way in. They name changes only; heads and bases are read from the
// forge again on the way in, and every line passes the gate again.
//
//	waiting.list   one line a line, in queue order: b <change>... [alone]
//	               (held lines too, in their places)
//	held.list      b <change>... -- <reason>
//	parked.list    park <id> [all] -- <restore condition> -- <reason>
//	               followed by its lines: b <change>... [alone]
//
// Blank lines and lines starting with # are comments. A change is named as
// the forge names it; a leading # is dropped. What a line file cannot carry
// is dropped on the way out: a line's fix flag, and a hold placed on a line
// that is also parked (the park is kept).
const (
	WaitingFile = "waiting.list"
	HeldFile    = "held.list"
	ParkedFile  = "parked.list"
)

// LineFile is the three lists.
type LineFile struct {
	Waiting []PlainLine `json:"waiting"`
	Held    []PlainHold `json:"held,omitempty"`
	Parks   []PlainPark `json:"parks,omitempty"`
}

// PlainLine is one line: its changes in stack order.
type PlainLine struct {
	Changes []state.ChangeID `json:"changes"`
	Alone   bool             `json:"alone,omitempty"`
}

// PlainHold is a hold on the waiting line that has these changes.
type PlainHold struct {
	Changes []state.ChangeID `json:"changes"`
	Reason  string           `json:"reason"`
}

// PlainPark is a park in force and the lines under it.
type PlainPark struct {
	ID     state.ParkID `json:"id"`
	All    bool         `json:"all,omitempty"`
	Cond   string       `json:"cond"`
	Reason string       `json:"reason,omitempty"`
	Lines  []PlainLine  `json:"lines,omitempty"`
}

const sep = " -- "

// ToLines writes a document as line files.
func ToLines(d Document) (LineFile, error) {
	lf := LineFile{Waiting: []PlainLine{}}
	parks := map[state.ParkID]int{}
	for _, p := range d.Parks {
		if err := checkField(string(p.ID), "park id"); err != nil {
			return LineFile{}, err
		}
		if p.Cond == "" || strings.Contains(p.Cond, sep) || strings.ContainsAny(p.Cond, "\r\n") {
			return LineFile{}, fmt.Errorf("lines: park %s: its restore condition %q cannot be written on one line", p.ID, p.Cond)
		}
		parks[p.ID] = len(lf.Parks)
		lf.Parks = append(lf.Parks, PlainPark{ID: p.ID, All: p.All, Cond: p.Cond, Reason: oneLine(p.Reason)})
	}
	for _, l := range d.Lines {
		pl := PlainLine{Alone: l.Alone}
		for _, c := range l.Changes {
			if err := checkField(string(c.ID), "change"); err != nil {
				return LineFile{}, err
			}
			pl.Changes = append(pl.Changes, c.ID)
		}
		if len(pl.Changes) == 0 {
			return LineFile{}, fmt.Errorf("lines: line %s has no changes", l.ID)
		}
		if l.State == "parked" {
			i, ok := parks[l.Park]
			if !ok {
				return LineFile{}, fmt.Errorf("lines: line %s is parked under %q, which is not in the document", l.ID, l.Park)
			}
			lf.Parks[i].Lines = append(lf.Parks[i].Lines, pl)
			continue
		}
		lf.Waiting = append(lf.Waiting, pl)
		if l.State == "held" {
			lf.Held = append(lf.Held, PlainHold{Changes: pl.Changes, Reason: oneLine(l.Reason)})
		}
	}
	return lf, nil
}

func checkField(s, what string) error {
	if s == "" || strings.ContainsAny(s, " \t\r\n#") || s == "alone" || s == "--" || s == "all" {
		return fmt.Errorf("lines: %s %q cannot be written in a line file", what, s)
	}
	return nil
}

func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	return strings.ReplaceAll(s, sep, " - ")
}

// Described is what the forge says of one change, for FromLines.
type Described struct {
	ID   state.ChangeID
	Head state.Head
	// Base is the change it is stacked on, or empty.
	Base state.ChangeID
}

// FromLines turns line files into a document for Import, reading each
// line's heads and base with describe. A line is named by its first change,
// as an offer from the CLI is; its base is the line that holds its first
// change's base, when the files hold one anywhere. Lines keep the files'
// order, except that a base comes before the lines stacked on it: a script
// queue's list may name a stacked line first, and an offer needs its base.
func FromLines(lf LineFile, describe func([]state.ChangeID) ([]Described, error)) (Document, error) {
	d := Document{Format: ExportFormat, Lines: []ExportLine{}}
	held := map[string]string{}
	for _, h := range lf.Held {
		held[key(h.Changes)] = h.Reason
	}
	owner := map[state.ChangeID]state.LineID{}
	baseOf := map[state.LineID]state.ChangeID{}
	add := func(pl PlainLine, st string, park state.ParkID) error {
		if len(pl.Changes) == 0 {
			return errors.New("lines: a line with no changes")
		}
		info, err := describe(pl.Changes)
		if err != nil {
			return fmt.Errorf("lines: describing %s: %w", key(pl.Changes), err)
		}
		if len(info) != len(pl.Changes) {
			return fmt.Errorf("lines: the forge described %d of %d changes of %s", len(info), len(pl.Changes), key(pl.Changes))
		}
		e := ExportLine{ID: state.LineID(pl.Changes[0]), Alone: pl.Alone, State: st, Park: park}
		for i, c := range info {
			if c.ID != pl.Changes[i] {
				return fmt.Errorf("lines: asked for %s, the forge described %s", pl.Changes[i], c.ID)
			}
			e.Changes = append(e.Changes, state.Change{ID: c.ID, Head: c.Head})
		}
		baseOf[e.ID] = info[0].Base
		if r, ok := held[key(pl.Changes)]; ok && st == "waiting" {
			e.State, e.Reason = "held", r
			delete(held, key(pl.Changes))
		}
		for _, c := range pl.Changes {
			if _, dup := owner[c]; dup {
				return fmt.Errorf("lines: change %s is in two lines", c)
			}
			owner[c] = e.ID
		}
		d.Lines = append(d.Lines, e)
		return nil
	}
	for _, pl := range lf.Waiting {
		if err := add(pl, "waiting", ""); err != nil {
			return Document{}, err
		}
	}
	for _, p := range lf.Parks {
		d.Parks = append(d.Parks, ExportPark{ID: p.ID, All: p.All, Cond: p.Cond, Reason: p.Reason})
		for _, pl := range p.Lines {
			if err := add(pl, "parked", p.ID); err != nil {
				return Document{}, err
			}
		}
	}
	for k := range held {
		return Document{}, fmt.Errorf("lines: a hold on %s, which is no waiting line", k)
	}
	byID := map[state.LineID]*ExportLine{}
	for i := range d.Lines {
		l := &d.Lines[i]
		if b := owner[baseOf[l.ID]]; b != l.ID {
			l.Base = b
		}
		byID[l.ID] = l
	}
	ordered := make([]ExportLine, 0, len(d.Lines))
	placed := map[state.LineID]bool{}
	var place func(l *ExportLine)
	place = func(l *ExportLine) {
		if placed[l.ID] {
			return
		}
		// Marked first, so that a cycle of bases (which the forge's
		// ancestry rules out) ends here and is refused by the offer.
		placed[l.ID] = true
		if b := byID[l.Base]; b != nil {
			place(b)
		}
		ordered = append(ordered, *l)
	}
	for i := range d.Lines {
		place(&d.Lines[i])
	}
	d.Lines = ordered
	return d, nil
}

func key(ids []state.ChangeID) string {
	s := make([]string, len(ids))
	for i, id := range ids {
		s[i] = string(id)
	}
	return strings.Join(s, " ")
}

func (pl PlainLine) text() string {
	s := "b " + key(pl.Changes)
	if pl.Alone {
		s += " alone"
	}
	return s
}

// Write writes the three files into dir, each atomically.
func (lf LineFile) Write(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	var w, h, p strings.Builder
	w.WriteString("# Waiting lines, in queue order: b <change>... [alone]\n")
	for _, l := range lf.Waiting {
		w.WriteString(l.text() + "\n")
	}
	h.WriteString("# Held lines: b <change>... -- <reason>\n")
	for _, x := range lf.Held {
		h.WriteString("b " + key(x.Changes) + sep + x.Reason + "\n")
	}
	p.WriteString("# Parks: park <id> [all] -- <restore condition> -- <reason>, then its lines\n")
	for _, x := range lf.Parks {
		all := ""
		if x.All {
			all = " all"
		}
		p.WriteString("park " + string(x.ID) + all + sep + x.Cond + sep + x.Reason + "\n")
		for _, l := range x.Lines {
			p.WriteString(l.text() + "\n")
		}
	}
	for name, body := range map[string]string{WaitingFile: w.String(), HeldFile: h.String(), ParkedFile: p.String()} {
		if err := WriteFileAtomic(filepath.Join(dir, name), []byte(body)); err != nil {
			return err
		}
	}
	return nil
}

// ReadLines reads the three files from dir. The held and parked files may
// be missing; the waiting file may not.
func ReadLines(dir string) (LineFile, error) {
	var lf LineFile
	var err error
	f, err := os.Open(filepath.Join(dir, WaitingFile))
	if err != nil {
		return lf, err
	}
	lf.Waiting, err = ParseWaiting(f)
	f.Close()
	if err != nil {
		return lf, fmt.Errorf("%s: %w", WaitingFile, err)
	}
	if f, err := os.Open(filepath.Join(dir, HeldFile)); err == nil {
		lf.Held, err = ParseHeld(f)
		f.Close()
		if err != nil {
			return lf, fmt.Errorf("%s: %w", HeldFile, err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return lf, err
	}
	if f, err := os.Open(filepath.Join(dir, ParkedFile)); err == nil {
		lf.Parks, err = ParseParked(f)
		f.Close()
		if err != nil {
			return lf, fmt.Errorf("%s: %w", ParkedFile, err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return lf, err
	}
	return lf, nil
}

// each calls f with every line of r that is not blank or a comment, and
// its number.
func each(r io.Reader, f func(n int, s string) error) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	n := 0
	for sc.Scan() {
		n++
		s := strings.TrimSpace(sc.Text())
		if s == "" || strings.HasPrefix(s, "#") {
			continue
		}
		if err := f(n, s); err != nil {
			return fmt.Errorf("line %d: %w", n, err)
		}
	}
	return sc.Err()
}

func parseLine(s string) (PlainLine, error) {
	f := strings.Fields(s)
	if len(f) < 2 || f[0] != "b" {
		return PlainLine{}, fmt.Errorf("%q: want b <change>... [alone]", s)
	}
	var pl PlainLine
	for i, x := range f[1:] {
		if x == "alone" && i == len(f)-2 && i > 0 {
			pl.Alone = true
			continue
		}
		x = strings.TrimPrefix(x, "#")
		if err := checkField(x, "change"); err != nil {
			return PlainLine{}, err
		}
		pl.Changes = append(pl.Changes, state.ChangeID(x))
	}
	if len(pl.Changes) == 0 {
		return PlainLine{}, fmt.Errorf("%q names no change", s)
	}
	return pl, nil
}

// ParseWaiting reads a waiting list.
func ParseWaiting(r io.Reader) ([]PlainLine, error) {
	out := []PlainLine{}
	err := each(r, func(_ int, s string) error {
		pl, err := parseLine(s)
		out = append(out, pl)
		return err
	})
	return out, err
}

// ParseHeld reads a held list.
func ParseHeld(r io.Reader) ([]PlainHold, error) {
	var out []PlainHold
	err := each(r, func(_ int, s string) error {
		l, reason, ok := strings.Cut(s, sep)
		reason = strings.TrimSpace(reason)
		if !ok || reason == "" {
			return fmt.Errorf("%q: a hold needs its reason: b <change>... -- <reason>", s)
		}
		pl, err := parseLine(l)
		if err != nil {
			return err
		}
		if pl.Alone {
			return fmt.Errorf("%q: a hold names the line's changes only", s)
		}
		out = append(out, PlainHold{Changes: pl.Changes, Reason: reason})
		return nil
	})
	return out, err
}

// ParseParked reads a parked list.
func ParseParked(r io.Reader) ([]PlainPark, error) {
	var out []PlainPark
	err := each(r, func(_ int, s string) error {
		if strings.HasPrefix(s, "park ") {
			parts := strings.SplitN(s, sep, 3)
			if len(parts) < 2 {
				return fmt.Errorf("%q: want park <id> [all] -- <restore condition> -- <reason>", s)
			}
			head := strings.Fields(parts[0])
			p := PlainPark{Cond: strings.TrimSpace(parts[1])}
			switch {
			case len(head) == 2:
			case len(head) == 3 && head[2] == "all":
				p.All = true
			default:
				return fmt.Errorf("%q: want park <id> [all]", parts[0])
			}
			p.ID = state.ParkID(head[1])
			if len(parts) == 3 {
				p.Reason = strings.TrimSpace(parts[2])
			}
			if p.Cond == "" {
				return fmt.Errorf("park %s has no restore condition", p.ID)
			}
			out = append(out, p)
			return nil
		}
		if len(out) == 0 {
			return fmt.Errorf("%q: a parked line comes after its park", s)
		}
		pl, err := parseLine(s)
		if err != nil {
			return err
		}
		out[len(out)-1].Lines = append(out[len(out)-1].Lines, pl)
		return nil
	})
	return out, err
}
