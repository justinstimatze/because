package main

import (
	"bufio"
	"fmt"
	"io"
	"regexp"
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
	Until  string // YYYY-MM-DD, "never", or empty (Since + defaultLife)
	Check  string // shell command; exit 0 means the cause still holds
	Source string
	Line   int
}

const (
	resolvedMarker = "# resolved"
	defaultLife    = 30 * 24 * time.Hour
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
	return row, nil
}

// Expiry is the last day the row is in force, or the zero time for "never".
func (r Row) Expiry() time.Time {
	if r.Until == "never" {
		return time.Time{}
	}
	if r.Until != "" {
		t, _ := time.Parse(dateLayout, r.Until)
		return t
	}
	s, _ := time.Parse(dateLayout, r.Since)
	return s.Add(defaultLife)
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
	for _, r := range t.Active {
		if r.Where == where && !r.Lapsed(today) && r.Match.MatchString(text) {
			out = append(out, r)
		}
	}
	return out
}
