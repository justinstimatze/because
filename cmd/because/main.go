// because surfaces known causes at the moment their symptom appears.
//
// The table (~/.claude/because.tsv, or $BECAUSE_FILE) holds one row per known cause, keyed by the
// text an agent will actually see: an error string in tool output, a token name in a command, a
// premise phrase in a reply. As a Claude Code hook it matches each row against that text and
// injects the cause, framed as a hypothesis with the command that confirms it. Rows expire, and
// `because review` retires the ones whose check no longer holds.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strings"
	"time"
)

// version is set by `make` from the git tag; a `go install …@vX.Y.Z` build reads it from the module.
var version = "dev"

func versionString() string {
	if version == "dev" {
		if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
			return bi.Main.Version
		}
	}
	return version
}

const (
	maxHits     = 3
	maxHaystack = 64 * 1024
	checkLimit  = 20 * time.Second
	seenLife    = 14 * 24 * time.Hour
	maxRowChars = 800
	maxActive   = 500
	maxMessage  = 9000
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "hook":
		err = runHook(os.Stdin, os.Stdout, time.Now())
	case "review":
		err = runReview(os.Stdout, len(os.Args) > 2 && os.Args[2] == "--apply", time.Now())
	case "lint":
		err = runLint(os.Stdout, time.Now())
	case "match":
		if len(os.Args) != 4 {
			err = fmt.Errorf("usage: because match <in|out|reply> <text>")
			break
		}
		err = runMatch(os.Stdout, os.Args[2], os.Args[3], time.Now())
	case "path":
		fmt.Println(tablePath())
	case "--version", "version":
		fmt.Println(versionString())
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "because:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage: because <command>
  hook              Claude Code hook: reads the event JSON on stdin (PreToolUse, PostToolUse, Stop, SessionStart)
  review [--apply]  run each active row's check; list (or with --apply, retire) rows that lapsed or no longer hold
  lint              parse the table and report its rows
  match WHERE TEXT  show which rows TEXT would surface (WHERE is in, out or reply)
  path              print the table's path`)
}

func tablePath() string {
	if p := os.Getenv("BECAUSE_FILE"); p != "" {
		return p
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude", "because.tsv")
}

func load() (*Table, error) {
	f, err := os.Open(tablePath())
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return Parse(f)
}

type hookInput struct {
	Event          string          `json:"hook_event_name"`
	SessionID      string          `json:"session_id"`
	ToolName       string          `json:"tool_name"`
	ToolInput      json.RawMessage `json:"tool_input"`
	ToolResponse   json.RawMessage `json:"tool_response"`
	TranscriptPath string          `json:"transcript_path"`
	StopHookActive bool            `json:"stop_hook_active"`
}

// runHook never fails the session: a broken table or unreadable input is reported on stderr and at
// SessionStart, and the tool call goes ahead.
func runHook(in io.Reader, out io.Writer, now time.Time) error {
	var h hookInput
	if err := json.NewDecoder(in).Decode(&h); err != nil {
		fmt.Fprintln(os.Stderr, "because: bad hook input:", err)
		return nil
	}
	t, err := load()
	if err != nil {
		if h.Event == "SessionStart" && !os.IsNotExist(err) {
			emitContext(out, h.Event, fmt.Sprintf("because: the known-causes table at %s does not parse, so nothing in it will surface: %v", tablePath(), err))
		} else {
			fmt.Fprintln(os.Stderr, "because:", err)
		}
		return nil
	}
	switch h.Event {
	case "SessionStart":
		sweepSeen(now)
		hits := loadHits()
		lapsed, quiet := 0, 0
		for _, r := range t.Active {
			switch {
			case r.Lapsed(now):
				lapsed++
			case r.Quiet(hits[r.ID], now):
				quiet++
			}
		}
		var parts []string
		if lapsed > 0 {
			parts = append(parts, fmt.Sprintf("%d known-cause row(s) are past their until date and no longer surface", lapsed))
		}
		if quiet > 0 {
			parts = append(parts, fmt.Sprintf("%d open row(s) have not surfaced in %d days", quiet, int(quietLife.Hours()/24)))
		}
		if len(parts) > 0 {
			emitContext(out, h.Event, "because: "+strings.Join(parts, "; ")+". Run `because review` to retire, renew or date them.")
		}
	case "PreToolUse":
		input := haystack(h.ToolInput)
		if touchesTable(h.ToolInput) || t.Skip(h.ToolName, input) {
			return nil
		}
		hits := unseen(h.SessionID, t.Hits("in", input, now), now)
		if len(hits) > 0 {
			emitContext(out, h.Event, render(hits, "This tool call names something with a known cause."))
		}
	case "PostToolUse":
		if touchesTable(h.ToolInput) || t.Skip(h.ToolName, haystack(h.ToolInput)) {
			return nil
		}
		hits := unseen(h.SessionID, t.Hits("out", haystack(h.ToolResponse), now), now)
		if len(hits) > 0 {
			emitContext(out, h.Event, render(hits, "That output matches a known cause."))
		}
	case "Stop":
		if h.StopHookActive || h.TranscriptPath == "" {
			return nil
		}
		text, err := lastTurnText(h.TranscriptPath)
		if err != nil || text == "" {
			return nil
		}
		hits := unseen(h.SessionID, t.Hits("reply", prose(text), now), now)
		if len(hits) > 0 {
			msg := render(hits, "Your reply rests on a premise with a recorded correction.") +
				"\nRevise the reply if the correction applies; if it doesn't, stop as you were."
			return json.NewEncoder(out).Encode(map[string]string{"decision": "block", "reason": msg})
		}
	}
	return nil
}

func emitContext(out io.Writer, event, msg string) {
	_ = json.NewEncoder(out).Encode(map[string]any{
		"hookSpecificOutput": map[string]string{"hookEventName": event, "additionalContext": msg},
	})
}

// render frames each row as a hypothesis with the command that tests it, because a remembered cause
// acted on without its check is worse than no memory at all.
func render(rows []Row, lead string) string {
	var b strings.Builder
	b.WriteString(lead + " Treat each as a hypothesis until its check runs:\n")
	for _, r := range rows {
		fmt.Fprintf(&b, "- [%s] since %s: %s", r.ID, r.Since, r.Cause)
		if r.Check != "" {
			fmt.Fprintf(&b, " Confirm with: `%s` (exit 0 = still true).", r.Check)
		}
		if r.Source != "" {
			fmt.Fprintf(&b, " Source: %s.", r.Source)
		}
		b.WriteString("\n")
	}
	s := strings.TrimRight(b.String(), "\n")
	// Claude Code moves hook text over 10,000 characters to a file the agent is not asked to read.
	if len(s) > maxMessage {
		s = s[:maxMessage] + " …"
	}
	return s
}

// touchesTable is true for a call that reads or edits the table itself, whose text matches every
// row's own regex by construction.
func touchesTable(raw json.RawMessage) bool {
	var in struct {
		FilePath string `json:"file_path"`
		Command  string `json:"command"`
	}
	_ = json.Unmarshal(raw, &in)
	p := tablePath()
	return in.FilePath == p || strings.Contains(in.Command, filepath.Base(p)) || strings.HasPrefix(strings.TrimSpace(in.Command), "because ")
}

// haystack flattens a JSON value to the text an agent reads: every string leaf, unescaped, joined
// by newlines in key order, capped at maxHaystack. Matching the raw JSON would see `\"x\"` where the
// agent sees `"x"`.
func haystack(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return capped(string(raw))
	}
	var b strings.Builder
	var walk func(any)
	walk = func(x any) {
		switch t := x.(type) {
		case string:
			b.WriteString(t)
			b.WriteByte('\n')
		case []any:
			for _, e := range t {
				walk(e)
			}
		case map[string]any:
			keys := make([]string, 0, len(t))
			for k := range t {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				walk(t[k])
			}
		}
	}
	walk(v)
	return capped(b.String())
}

// capped keeps the head and the tail of a long text, since a build or test log puts its error at
// the end.
func capped(s string) string {
	if len(s) > maxHaystack {
		return s[:maxHaystack/2] + "\n" + s[len(s)-maxHaystack/2:]
	}
	return s
}

// unseen keeps the first maxHits rows this session has not been shown, and records them, so one
// cause surfaces once per session rather than on every matching call. It also stamps each shown
// row's last-hit date, which is what review's quiet check reads.
func unseen(session string, rows []Row, now time.Time) []Row {
	if len(rows) == 0 {
		return nil
	}
	dir := stateDir()
	path := filepath.Join(dir, "seen-"+safe(session))
	seen := map[string]bool{}
	if b, err := os.ReadFile(path); err == nil {
		for _, id := range strings.Fields(string(b)) {
			seen[id] = true
		}
	}
	var out []Row
	for _, r := range rows {
		if !seen[r.ID] && len(out) < maxHits {
			out = append(out, r)
		}
	}
	if len(out) > 0 && session != "" {
		_ = os.MkdirAll(dir, 0o700)
		if f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
			for _, r := range out {
				fmt.Fprintln(f, r.ID)
			}
			f.Close()
		}
		recordHits(out, now)
	}
	return out
}

func hitsPath() string { return filepath.Join(stateDir(), "hits.tsv") }

// loadHits reads the last date each row surfaced, as id → YYYY-MM-DD.
func loadHits() map[string]string {
	m := map[string]string{}
	b, err := os.ReadFile(hitsPath())
	if err != nil {
		return m
	}
	for _, line := range strings.Split(string(b), "\n") {
		if id, day, ok := strings.Cut(line, "\t"); ok {
			m[id] = day
		}
	}
	return m
}

func recordHits(rows []Row, now time.Time) {
	m := loadHits()
	for _, r := range rows {
		m[r.ID] = now.Format(dateLayout)
	}
	ids := make([]string, 0, len(m))
	for id := range m {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var b strings.Builder
	for _, id := range ids {
		fmt.Fprintf(&b, "%s\t%s\n", id, m[id])
	}
	tmp := hitsPath() + ".tmp"
	if os.WriteFile(tmp, []byte(b.String()), 0o600) == nil {
		_ = os.Rename(tmp, hitsPath())
	}
}

// sweepSeen removes per-session seen files untouched for seenLife, so the state directory does not
// grow by one file per session forever.
func sweepSeen(now time.Time) {
	paths, _ := filepath.Glob(filepath.Join(stateDir(), "seen-*"))
	for _, p := range paths {
		if fi, err := os.Stat(p); err == nil && now.Sub(fi.ModTime()) > seenLife {
			_ = os.Remove(p)
		}
	}
}

func stateDir() string {
	if d := os.Getenv("BECAUSE_STATE_DIR"); d != "" {
		return d
	}
	d, err := os.UserCacheDir()
	if err != nil {
		d = os.TempDir()
	}
	return filepath.Join(d, "because")
}

func safe(s string) string {
	if s == "" {
		return "nosession"
	}
	return strings.Map(func(r rune) rune {
		if r == '/' || r == '\\' || r == '.' {
			return '_'
		}
		return r
	}, s)
}

// lastTurnText returns the assistant text written since the last real user prompt (a prompt, not a
// tool result), reading the transcript forward and resetting at each prompt.
func lastTurnText(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	type part struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	type entry struct {
		Type    string `json:"type"`
		Message struct {
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}
	var parts []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
	for sc.Scan() {
		var e entry
		if json.Unmarshal(sc.Bytes(), &e) != nil {
			continue
		}
		c := e.Message.Content
		switch e.Type {
		case "user":
			var s string
			if json.Unmarshal(c, &s) == nil {
				parts = nil
				continue
			}
			var ps []part
			if json.Unmarshal(c, &ps) == nil {
				for _, p := range ps {
					if p.Type == "text" {
						parts = nil
						break
					}
				}
			}
		case "assistant":
			var ps []part
			if json.Unmarshal(c, &ps) == nil {
				for _, p := range ps {
					if p.Type == "text" {
						parts = append(parts, p.Text)
					}
				}
			}
		}
	}
	return strings.Join(parts, "\n"), sc.Err()
}

// prose drops fenced blocks and inline code spans, so a quoted log line or command never counts as
// the reply asserting it.
func prose(text string) string {
	var b strings.Builder
	fenced := false
	for _, line := range strings.Split(text, "\n") {
		if t := strings.TrimSpace(line); strings.HasPrefix(t, "```") || strings.HasPrefix(t, "~~~") {
			fenced = !fenced
			continue
		}
		if fenced {
			continue
		}
		in := false
		for _, r := range line {
			if r == '`' {
				in = !in
				continue
			}
			if !in {
				b.WriteRune(r)
			}
		}
		b.WriteByte('\n')
	}
	return b.String()
}

func runLint(out io.Writer, now time.Time) error {
	t, err := load()
	if err != nil {
		return err
	}
	ids := map[string]int{}
	for _, r := range append(append([]Row{}, t.Active...), t.Resolved...) {
		if prev, ok := ids[r.ID]; ok {
			return fmt.Errorf("line %d: id %q already used on line %d", r.Line, r.ID, prev)
		}
		ids[r.ID] = r.Line
	}
	hits := loadHits()
	for _, r := range t.Active {
		state := "active"
		switch {
		case r.Lapsed(now):
			state = "LAPSED"
		case r.Quiet(hits[r.ID], now):
			state = "QUIET"
		}
		fmt.Fprintf(out, "%-6s %-24s %-5s until %-10s last hit %s\n", state, r.ID, r.Where, expiryText(r), orNone(hits[r.ID]))
	}
	fmt.Fprintf(out, "%d active, %d resolved\n", len(t.Active), len(t.Resolved))
	for _, r := range t.Active {
		if n := len(r.Cause) + len(r.Check) + len(r.Source); n > maxRowChars {
			fmt.Fprintf(out, "warning: %s injects %d characters; keep a row under %d so three fit well inside one hook message\n", r.ID, n, maxRowChars)
		}
	}
	if len(t.Active) > maxActive {
		fmt.Fprintf(out, "warning: %d active rows; every hooked call parses all of them; retire rows that no longer earn a place past %d\n", len(t.Active), maxActive)
	}
	return nil
}

func runMatch(out io.Writer, where, text string, now time.Time) error {
	t, err := load()
	if err != nil {
		return err
	}
	if where == "reply" {
		text = prose(text)
	}
	hits := t.Hits(where, text, now)
	if len(hits) == 0 {
		fmt.Fprintln(out, "no row matches")
		return nil
	}
	fmt.Fprintln(out, render(hits, "Matches:"))
	return nil
}

type verdict struct {
	row    Row
	retire bool
	quiet  bool
	why    string
}

// runReview runs every active row's check. A row retires when its until date has passed or its
// check exits non-zero; a check that cannot run (timeout, missing binary) is reported and the row
// kept, since "could not test" is not "no longer true".
func runReview(out io.Writer, apply bool, now time.Time) error {
	t, err := load()
	if err != nil {
		return err
	}
	hits := loadHits()
	var vs []verdict
	for _, r := range t.Active {
		v := verdict{row: r}
		quiet := r.Quiet(hits[r.ID], now)
		switch {
		case r.Lapsed(now):
			v.retire, v.why = true, "past until "+r.Expiry().Format(dateLayout)
		case r.Check != "":
			code, err := runCheck(r.Check)
			switch {
			case err != nil:
				v.why = "check could not run: " + err.Error()
			case code != 0:
				v.retire, v.why = true, fmt.Sprintf("check exited %d", code)
			default:
				v.why = "check holds"
			}
		default:
			v.why = "no check; in force until " + expiryText(r)
		}
		if quiet && !v.retire {
			v.quiet = true
			v.why += fmt.Sprintf("; quiet: no hit since %s", orNone(hits[r.ID]))
		}
		vs = append(vs, v)
	}
	retiring, quiet := 0, 0
	for _, v := range vs {
		mark := "keep  "
		if v.quiet {
			mark, quiet = "QUIET ", quiet+1
		}
		if v.retire {
			mark, retiring = "RETIRE", retiring+1
		}
		fmt.Fprintf(out, "%s %-24s %s\n", mark, v.row.ID, v.why)
	}
	if quiet > 0 {
		fmt.Fprintf(out, "%d open row(s) quiet for %d days. Each still matches; give it an until date, `never`, or a failing check, or move it under %q yourself.\n", quiet, int(quietLife.Hours()/24), resolvedMarker)
	}
	if retiring == 0 || !apply {
		if retiring > 0 {
			fmt.Fprintf(out, "%d row(s) would retire; rerun with --apply to move them under %q.\n", retiring, resolvedMarker)
		}
		return nil
	}
	return retire(t, vs, now)
}

func expiryText(r Row) string {
	switch {
	case r.Until == "":
		return "open"
	case r.Until == "never":
		return "never"
	}
	return r.Until
}

func orNone(day string) string {
	if day == "" {
		return "none"
	}
	return day
}

func runCheck(cmd string) (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), checkLimit)
	defer cancel()
	c := exec.CommandContext(ctx, "sh", "-c", cmd)
	// nil Stdout and Stderr go to the null device with no pipe, so a check that leaves a background
	// process running cannot hold Wait open past the timeout.
	c.Stdout, c.Stderr = nil, nil
	c.WaitDelay = time.Second
	c.Dir, _ = os.UserHomeDir()
	err := c.Run()
	if ctx.Err() != nil {
		return -1, fmt.Errorf("timed out after %s", checkLimit)
	}
	if ee, ok := err.(*exec.ExitError); ok {
		if ee.ExitCode() == 127 {
			return -1, fmt.Errorf("command not found")
		}
		return ee.ExitCode(), nil
	}
	return 0, err
}

// retire rewrites the table with each retiring row moved under the resolved marker, its source
// stamped with the date and reason. Everything else keeps its place.
func retire(t *Table, vs []verdict, now time.Time) error {
	gone := map[int]string{}
	for _, v := range vs {
		if v.retire {
			gone[v.row.Line] = v.why
		}
	}
	var keep, moved []string
	hasMarker := false
	for i, line := range t.Lines {
		if why, ok := gone[i+1]; ok {
			f := strings.Split(line, "\t")
			f[len(f)-1] = strings.TrimSpace(f[len(f)-1]) + fmt.Sprintf(" · resolved %s (%s)", now.Format(dateLayout), why)
			moved = append(moved, strings.Join(f, "\t"))
			continue
		}
		if strings.EqualFold(strings.TrimSpace(line), resolvedMarker) {
			hasMarker = true
		}
		keep = append(keep, line)
	}
	if !hasMarker {
		keep = append(keep, "", resolvedMarker)
	}
	keep = append(keep, moved...)
	var buf bytes.Buffer
	buf.WriteString(strings.Join(keep, "\n") + "\n")
	if _, err := Parse(bytes.NewReader(buf.Bytes())); err != nil {
		return fmt.Errorf("rewritten table would not parse, left unchanged: %w", err)
	}
	// Write through a symlink (a table kept in a dotfiles repo) rather than replacing it, and keep
	// the file's mode.
	p, err := filepath.EvalSymlinks(tablePath())
	if err != nil {
		return err
	}
	fi, err := os.Stat(p)
	if err != nil {
		return err
	}
	tmp := p + ".tmp"
	_ = os.Remove(tmp)
	if err := os.WriteFile(tmp, buf.Bytes(), fi.Mode().Perm()); err != nil {
		return err
	}
	if err := os.Chmod(tmp, fi.Mode().Perm()); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}
