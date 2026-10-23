package main

import (
	"bufio"
	"fmt"
	"io"
	"regexp"
	"regexp/syntax"
	"strings"
	"time"
)

// A Row is one known cause: when an agent sees text matching Match in the place Where names, the
// symptom is explained by Cause. Rows are retired, never deleted: a row below the resolvedMarker
// line is kept for history and never matches.
type Row struct {
	ID     string
	Match  *regexp.Regexp
	Raw    string // the regex as written, for lint and review output
	Where  string // "in" (tool input), "out" (tool output) or "reply" (assistant text at Stop)
	Cause  string
	Since  string // YYYY-MM-DD
	Until  string // YYYY-MM-DD, "never", or empty (open: no end date, flagged by review once quiet)
	Check  string // shell command; exit 0 means the cause still holds
	Source string
	Line   int
	// need holds lowercase literals of which any match must contain at least one, or nil when the
	// regex has none worth checking. Hits skips the regex when none of them is in the text.
	need []string
}

const (
	resolvedMarker = "# resolved"
	directive      = "#!"
	quietLife      = 60 * 24 * time.Hour
	dateLayout     = "2006-01-02"
	columns        = 8
)

var wheres = map[string]bool{"in": true, "out": true, "reply": true}

// Table is the parsed file: Active rows match, Resolved rows are history. Lines keeps the file's
// text so review can rewrite it without disturbing comments or ordering.
type Table struct {
	Active   []Row
	Resolved []Row
	Lines    []string
	// SkipTools and SkipText come from `#! skip-tools REGEX` and `#! skip-text REGEX` lines. A tool
	// call whose name matches SkipTools, or whose input matches SkipText, is not matched at all: it
	// reads documentation of causes (a ticket, a notes file) rather than meeting a symptom.
	SkipTools *regexp.Regexp
	SkipText  *regexp.Regexp
}

// Parse reads the tab-separated table. A malformed row is an error naming its line, never a silent
// skip: a row that never matches looks exactly like a cause that never recurs.
func Parse(r io.Reader) (*Table, error) {
	t := &Table{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	resolved := false
	n := 0
	for sc.Scan() {
		n++
		line := sc.Text()
		t.Lines = append(t.Lines, line)
		trim := strings.TrimSpace(line)
		if strings.EqualFold(trim, resolvedMarker) {
			resolved = true
			continue
		}
		if strings.HasPrefix(trim, directive) {
			if err := t.parseDirective(strings.TrimSpace(trim[len(directive):]), n); err != nil {
				return nil, err
			}
			continue
		}
		if trim == "" || strings.HasPrefix(trim, "#") {
			continue
		}
		row, err := parseRow(line, n)
		if err != nil {
			return nil, err
		}
		if resolved {
			t.Resolved = append(t.Resolved, row)
		} else {
			t.Active = append(t.Active, row)
		}
	}
	return t, sc.Err()
}

func (t *Table) parseDirective(d string, n int) error {
	name, arg, _ := strings.Cut(d, " ")
	arg = strings.TrimSpace(arg)
	var dst **regexp.Regexp
	switch name {
	case "skip-tools":
		dst = &t.SkipTools
	case "skip-text":
		dst = &t.SkipText
	default:
		return fmt.Errorf("line %d: unknown directive %q (want skip-tools or skip-text)", n, name)
	}
	if arg == "" {
		return fmt.Errorf("line %d: %s needs a regex", n, name)
	}
	if *dst != nil {
		return fmt.Errorf("line %d: %s given twice; join the patterns with |", n, name)
	}
	re, err := regexp.Compile(arg)
	if err != nil {
		return fmt.Errorf("line %d: %s: %v", n, name, err)
	}
	*dst = re
	return nil
}

// Skip reports whether a tool call is one whose text should not be matched.
func (t *Table) Skip(tool, input string) bool {
	return (t.SkipTools != nil && tool != "" && t.SkipTools.MatchString(tool)) ||
		(t.SkipText != nil && t.SkipText.MatchString(input))
}

func parseRow(line string, n int) (Row, error) {
	f := strings.Split(line, "\t")
	if len(f) != columns {
		return Row{}, fmt.Errorf("line %d: want %d tab-separated fields (id match where cause since until check source), got %d", n, columns, len(f))
	}
	for i := range f {
		f[i] = strings.TrimSpace(f[i])
	}
	for _, i := range []int{5, 6, 7} { // "-" spells an empty optional field
		if f[i] == "-" {
			f[i] = ""
		}
	}
	row := Row{ID: f[0], Raw: f[1], Where: f[2], Cause: f[3], Since: f[4], Until: f[5], Check: f[6], Source: f[7], Line: n}
	if row.ID == "" || row.Raw == "" || row.Cause == "" {
		return Row{}, fmt.Errorf("line %d: id, match and cause are required", n)
	}
	if !wheres[row.Where] {
		return Row{}, fmt.Errorf("line %d (%s): where must be in, out or reply, got %q", n, row.ID, row.Where)
	}
	if _, err := time.Parse(dateLayout, row.Since); err != nil {
		return Row{}, fmt.Errorf("line %d (%s): since must be YYYY-MM-DD, got %q", n, row.ID, row.Since)
	}
	if row.Until != "" && row.Until != "never" {
		if _, err := time.Parse(dateLayout, row.Until); err != nil {
			return Row{}, fmt.Errorf("line %d (%s): until must be YYYY-MM-DD, never, or empty, got %q", n, row.ID, row.Until)
		}
	}
	re, err := regexp.Compile(row.Raw)
	if err != nil {
		return Row{}, fmt.Errorf("line %d (%s): match: %v", n, row.ID, err)
	}
	row.Match = re
	if ast, err := syntax.Parse(row.Raw, syntax.Perl); err == nil {
		row.need = required(ast.Simplify())
	}
	return row, nil
}

// minNeed is the shortest literal worth a substring check; shorter ones appear in most text anyway.
const minNeed = 3

// required returns lowercase literals of which every match of re contains at least one, or nil if
// it cannot say. Lowercasing both sides keeps it a necessary condition for (?i) patterns too.
func required(re *syntax.Regexp) []string {
	switch re.Op {
	case syntax.OpLiteral:
		if s := strings.ToLower(string(re.Rune)); len(s) >= minNeed {
			return []string{s}
		}
	case syntax.OpCapture, syntax.OpPlus:
		return required(re.Sub[0])
	case syntax.OpRepeat:
		if re.Min >= 1 {
			return required(re.Sub[0])
		}
	case syntax.OpConcat:
		// Any one child's set is a valid condition; take the one whose shortest literal is longest.
		var best []string
		for _, sub := range re.Sub {
			if s := required(sub); s != nil && (best == nil || shortest(s) > shortest(best)) {
				best = s
			}
		}
		return best
	case syntax.OpAlternate:
		var all []string
		for _, sub := range re.Sub {
			s := required(sub)
			if s == nil {
				return nil
			}
			all = append(all, s...)
		}
		return all
	}
	return nil
}

func shortest(s []string) int {
	n := len(s[0])
	for _, x := range s[1:] {
		n = min(n, len(x))
	}
	return n
}

// Expiry is the last day the row is in force, or the zero time for "never" and open rows.
func (r Row) Expiry() time.Time {
	if r.Until == "never" || r.Until == "" {
		return time.Time{}
	}
	t, _ := time.Parse(dateLayout, r.Until)
	return t
}

// Quiet reports whether an open row has gone quietLife without surfacing, counting from its last
// hit or, if it never surfaced, from Since. Quiet rows still match; review lists them so each one
// gets an until date, `never`, or retirement on purpose.
func (r Row) Quiet(lastHit string, today time.Time) bool {
	if r.Until != "" {
		return false
	}
	from := r.Since
	if lastHit > from {
		from = lastHit
	}
	f, err := time.Parse(dateLayout, from)
	return err == nil && today.Format(dateLayout) > f.Add(quietLife).Format(dateLayout)
}

// Lapsed reports whether today's local date is past the row's expiry. A lapsed row stops matching
// at once, even before review moves it, so a stale cause is never injected as current. Dates compare
// as strings, so the until day itself is in force all day in local time.
func (r Row) Lapsed(today time.Time) bool {
	e := r.Expiry()
	return !e.IsZero() && today.Format(dateLayout) > e.Format(dateLayout)
}

// Hits returns the active, unlapsed rows for where whose regex matches text, in file order.
func (t *Table) Hits(where, text string, today time.Time) []Row {
	var out []Row
	lower := ""
	for _, r := range t.Active {
		if r.Where != where || r.Lapsed(today) {
			continue
		}
		if r.need != nil {
			if lower == "" {
				lower = strings.ToLower(text)
			}
			if !containsAny(lower, r.need) {
				continue
			}
		}
		if r.Match.MatchString(text) {
			out = append(out, r)
		}
	}
	return out
}

func containsAny(s string, subs []string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
