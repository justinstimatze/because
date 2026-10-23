package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var today = time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)

const sample = "# id\tmatch\twhere\tcause\tsince\tuntil\tcheck\tsource\n" +
	"dependabot\tDependabot alerts are disabled\tout\tAlerts are off on purpose.\t2026-10-18\t2026-11-30\t-\tmemory\n" +
	"token\tDEPLOY_TOKEN\tin\tThis token was rotated; the new one is in the vault.\t2026-10-11\t-\t-\tnotes\n" +
	"cache\t(?i)widget cache.{0,60}off by default\treply\tThe widget cache is on by default since v2.\t2026-10-18\tnever\t-\tchangelog\n" +
	"old\tstale thing\tout\tLong gone.\t2026-08-01\t2026-09-01\t-\tx\n" +
	"\n# resolved\n" +
	"done\tdone thing\tout\tFixed.\t2026-10-01\t-\t-\tx · resolved 2026-10-10 (check exited 1)\n"

func setup(t *testing.T, table string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "because.tsv")
	if err := os.WriteFile(p, []byte(table), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BECAUSE_FILE", p)
	t.Setenv("BECAUSE_STATE_DIR", filepath.Join(dir, "state"))
	return p
}

func hook(t *testing.T, in map[string]any) string {
	t.Helper()
	b, _ := json.Marshal(in)
	var out bytes.Buffer
	if err := runHook(bytes.NewReader(b), &out, today); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

func TestParseRejectsAMalformedRowRatherThanSkippingIt(t *testing.T) {
	for name, line := range map[string]string{
		"too few fields": "a\tb\tout\tc\n",
		"bad where":      "a\tb\tsideways\tc\t2026-10-18\t-\t-\tx\n",
		"bad since":      "a\tb\tout\tc\tyesterday\t-\t-\tx\n",
		"bad regex":      "a\t(\tout\tc\t2026-10-18\t-\t-\tx\n",
	} {
		if _, err := Parse(strings.NewReader(line)); err == nil {
			t.Errorf("%s: parsed without error", name)
		}
	}
}

func TestRowsBelowResolvedMarkerNeverMatch(t *testing.T) {
	setup(t, sample)
	tb, err := load()
	if err != nil {
		t.Fatal(err)
	}
	if len(tb.Active) != 4 || len(tb.Resolved) != 1 {
		t.Fatalf("active=%d resolved=%d", len(tb.Active), len(tb.Resolved))
	}
	if hits := tb.Hits("out", "done thing", today); len(hits) != 0 {
		t.Fatalf("resolved row matched: %v", hits[0].ID)
	}
}

func TestALapsedRowStopsMatchingBeforeReviewMovesIt(t *testing.T) {
	setup(t, sample)
	tb, err := load()
	if err != nil {
		t.Fatal(err)
	}
	if hits := tb.Hits("out", "a stale thing appeared", today); len(hits) != 0 {
		t.Fatal("row past its until date still surfaced")
	}
}

func TestAnOpenRowNeverLapsesButGoesQuietWithoutHits(t *testing.T) {
	r := Row{Since: "2026-01-01"}
	later := time.Date(2026, 12, 1, 0, 0, 0, 0, time.Local)
	if r.Lapsed(later) {
		t.Fatal("open row lapsed")
	}
	if !r.Quiet("", later) {
		t.Fatal("open row with no hit in eleven months is not quiet")
	}
	if r.Quiet("2026-11-20", later) {
		t.Fatal("row hit eleven days ago is quiet")
	}
	if (Row{Since: "2026-01-01", Until: "never"}).Quiet("", later) {
		t.Fatal("a never row is quiet; never is the way to opt out")
	}
}

func TestASurfacedRowStampsItsLastHit(t *testing.T) {
	setup(t, sample)
	hook(t, map[string]any{
		"hook_event_name": "PreToolUse", "session_id": "s", "tool_name": "Bash",
		"tool_input": map[string]any{"command": "DEPLOY_TOKEN=abc123 ./deploy.sh"},
	})
	if got := loadHits()["token"]; got != today.Format(dateLayout) {
		t.Fatalf("last hit %q", got)
	}
}

func TestSkipDirectivesStopMatchesOnDocumentationOfACause(t *testing.T) {
	setup(t, "#! skip-tools ^mcp__linear\n#! skip-text open-asks\\.md|/memory/\n"+
		"pat\tDEPLOY_TOKEN\tin\tRotated.\t2026-10-11\t-\t-\tx\n"+
		"err\tDependabot alerts are disabled\tout\tOff on purpose.\t2026-10-18\t-\t-\tx\n")
	for name, in := range map[string]map[string]any{
		"ticket read": {"hook_event_name": "PostToolUse", "session_id": "a", "tool_name": "mcp__linear__get_issue",
			"tool_input": map[string]any{"id": "X-1"}, "tool_response": map[string]any{"text": "Dependabot alerts are disabled"}},
		"notes grep": {"hook_event_name": "PreToolUse", "session_id": "b", "tool_name": "Bash",
			"tool_input": map[string]any{"command": "rg DEPLOY_TOKEN ~/notes/open-asks.md"}},
	} {
		if out := hook(t, in); out != "" {
			t.Errorf("%s surfaced: %q", name, out)
		}
	}
	if out := hook(t, map[string]any{"hook_event_name": "PreToolUse", "session_id": "c", "tool_name": "Bash",
		"tool_input": map[string]any{"command": "DEPLOY_TOKEN=x ./deploy.sh"}}); out == "" {
		t.Fatal("a real use was skipped too")
	}
}

func TestAnUnknownDirectiveIsAnError(t *testing.T) {
	if _, err := Parse(strings.NewReader("#! skip-everything .\n")); err == nil {
		t.Fatal("unknown directive parsed")
	}
}

func TestPostToolUseInjectsTheCauseWithItsCheckOncePerSession(t *testing.T) {
	setup(t, sample)
	in := map[string]any{
		"hook_event_name": "PostToolUse", "session_id": "s1", "tool_name": "Bash",
		"tool_input":    map[string]any{"command": "gh run view 1 --log-failed"},
		"tool_response": map[string]any{"stdout": "HTTP 403: Dependabot alerts are disabled for this repository."},
	}
	out := hook(t, in)
	if !strings.Contains(out, "Alerts are off on purpose.") || !strings.Contains(out, `"hookEventName":"PostToolUse"`) {
		t.Fatalf("no injection: %q", out)
	}
	if strings.Contains(out, "decision") {
		t.Fatal("PostToolUse must add context, never block")
	}
	if again := hook(t, in); again != "" {
		t.Fatalf("second hit in the same session surfaced again: %q", again)
	}
	in["session_id"] = "s2"
	if fresh := hook(t, in); fresh == "" {
		t.Fatal("a new session did not see the row")
	}
}

func TestOutputMatchesUnescapedTextNotRawJSON(t *testing.T) {
	setup(t, "q\t\"quotedKey\":true\tout\tThe quoted key matched.\t2026-10-18\t-\t-\tx\n")
	out := hook(t, map[string]any{
		"hook_event_name": "PostToolUse", "session_id": "s", "tool_input": map[string]any{"command": "curl x"},
		"tool_response": map[string]any{"stdout": `{"error":{"quotedKey":true}}`},
	})
	if !strings.Contains(out, "The quoted key matched.") {
		t.Fatalf("quoted text inside stdout did not match: %q", out)
	}
}

func TestPreToolUseMatchesTheCommand(t *testing.T) {
	setup(t, sample)
	out := hook(t, map[string]any{
		"hook_event_name": "PreToolUse", "session_id": "s", "tool_name": "Bash",
		"tool_input": map[string]any{"command": "DEPLOY_TOKEN=abc123 ./deploy.sh"},
	})
	if !strings.Contains(out, "This token was rotated") {
		t.Fatalf("no injection: %q", out)
	}
}

func TestCallsOnTheTableItselfAreIgnored(t *testing.T) {
	p := setup(t, sample)
	out := hook(t, map[string]any{
		"hook_event_name": "PreToolUse", "session_id": "s", "tool_name": "Edit",
		"tool_input": map[string]any{"file_path": p, "new_string": "DEPLOY_TOKEN"},
	})
	if out != "" {
		t.Fatalf("editing the table triggered its own row: %q", out)
	}
}

func writeTranscript(t *testing.T, reply string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "t.jsonl")
	lines := []map[string]any{
		{"type": "user", "message": map[string]any{"content": "old prompt"}},
		{"type": "assistant", "message": map[string]any{"content": []map[string]any{{"type": "text", "text": "The widget cache is off by default (old turn)."}}}},
		{"type": "user", "message": map[string]any{"content": "new prompt"}},
		{"type": "assistant", "message": map[string]any{"content": []map[string]any{{"type": "tool_use", "name": "Bash"}}}},
		{"type": "user", "message": map[string]any{"content": []map[string]any{{"type": "tool_result", "content": "ok"}}}},
		{"type": "assistant", "message": map[string]any{"content": []map[string]any{{"type": "text", "text": reply}}}},
	}
	var b bytes.Buffer
	for _, l := range lines {
		j, _ := json.Marshal(l)
		b.Write(j)
		b.WriteByte('\n')
	}
	os.WriteFile(p, b.Bytes(), 0o644)
	return p
}

func TestStopBlocksOnceOnAStalePremiseInThisTurnsProse(t *testing.T) {
	setup(t, sample)
	tp := writeTranscript(t, "The widget cache is off by default, so every request hits the database.")
	out := hook(t, map[string]any{"hook_event_name": "Stop", "session_id": "s", "transcript_path": tp})
	if !strings.Contains(out, `"decision":"block"`) || !strings.Contains(out, "on by default since v2") {
		t.Fatalf("no block: %q", out)
	}
	if retry := hook(t, map[string]any{"hook_event_name": "Stop", "session_id": "s2", "transcript_path": tp, "stop_hook_active": true}); retry != "" {
		t.Fatalf("blocked the retry: %q", retry)
	}
}

func TestStopIgnoresEarlierTurnsAndCodeSpans(t *testing.T) {
	setup(t, sample)
	tp := writeTranscript(t, "The old ticket says `widget cache is off by default`, which is stale.")
	if out := hook(t, map[string]any{"hook_event_name": "Stop", "session_id": "s", "transcript_path": tp}); out != "" {
		t.Fatalf("blocked on a quote or an earlier turn: %q", out)
	}
}

func TestSessionStartSaysNothingUnlessARowLapsed(t *testing.T) {
	setup(t, strings.Replace(sample, "old\tstale thing", "# old\tstale thing", 1))
	if out := hook(t, map[string]any{"hook_event_name": "SessionStart"}); out != "" {
		t.Fatalf("spent tokens with nothing lapsed: %q", out)
	}
	setup(t, sample)
	if out := hook(t, map[string]any{"hook_event_name": "SessionStart"}); !strings.Contains(out, "1 known-cause row") {
		t.Fatalf("lapsed row not reported: %q", out)
	}
}

func TestSessionStartReportsABrokenTableLoudly(t *testing.T) {
	setup(t, "not\ta\trow\n")
	if out := hook(t, map[string]any{"hook_event_name": "SessionStart"}); !strings.Contains(out, "does not parse") {
		t.Fatalf("broken table was silent: %q", out)
	}
}

func TestReviewRetiresFailedChecksAndLapsedRowsKeepsUnrunnableOnes(t *testing.T) {
	p := setup(t, "holds\tx\tout\tc\t2026-10-18\tnever\ttrue\ts\n"+
		"fails\ty\tout\tc\t2026-10-18\tnever\tfalse\ts\n"+
		"missing\tz\tout\tc\t2026-10-18\tnever\tno-such-binary-because-test\ts\n"+
		"lapsed\tw\tout\tc\t2026-08-01\t2026-09-01\t-\ts\n")
	var out bytes.Buffer
	if err := runReview(&out, true, today); err != nil {
		t.Fatal(err)
	}
	tb, err := load()
	if err != nil {
		t.Fatal(err)
	}
	var active, resolved []string
	for _, r := range tb.Active {
		active = append(active, r.ID)
	}
	for _, r := range tb.Resolved {
		resolved = append(resolved, r.ID)
	}
	if strings.Join(active, ",") != "holds,missing" || strings.Join(resolved, ",") != "fails,lapsed" {
		t.Fatalf("active=%v resolved=%v\n%s", active, resolved, out.String())
	}
	b, _ := os.ReadFile(p)
	if !strings.Contains(string(b), "resolved 2026-10-18 (check exited 1)") {
		t.Fatalf("source not stamped:\n%s", b)
	}
}

func TestTheUntilDayItselfIsStillInForce(t *testing.T) {
	r := Row{Since: "2026-10-01", Until: "2026-10-18"}
	if r.Lapsed(time.Date(2026, 10, 18, 23, 0, 0, 0, time.Local)) {
		t.Fatal("row lapsed on its own until day")
	}
	if !r.Lapsed(time.Date(2026, 10, 19, 0, 1, 0, 0, time.Local)) {
		t.Fatal("row still in force the day after until")
	}
}

func TestOutputMatchesAnErrorAtTheEndOfALongLog(t *testing.T) {
	setup(t, "tail\tFATAL: disk full\tout\tThe runner disk fills on full builds.\t2026-10-18\t-\t-\tx\n")
	out := hook(t, map[string]any{
		"hook_event_name": "PostToolUse", "session_id": "s", "tool_input": map[string]any{"command": "make"},
		"tool_response": map[string]any{"stdout": strings.Repeat("compiling...\n", 20000) + "FATAL: disk full\n"},
	})
	if !strings.Contains(out, "runner disk") {
		t.Fatalf("error at the end of a long log was cut off: %q", out)
	}
}

func TestReviewWritesThroughASymlinkAndKeepsTheMode(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real.tsv")
	if err := os.WriteFile(real, []byte("gone\tx\tout\tc\t2026-10-18\tnever\tfalse\ts\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "because.tsv")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BECAUSE_FILE", link)
	if err := runReview(&bytes.Buffer{}, true, today); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("review replaced the symlink with a file")
	}
	fi, _ := os.Stat(real)
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode changed to %v", fi.Mode().Perm())
	}
	if b, _ := os.ReadFile(real); !strings.Contains(string(b), "# resolved") {
		t.Fatalf("target not rewritten:\n%s", b)
	}
}

func TestACheckThatLeavesABackgroundProcessDoesNotHang(t *testing.T) {
	start := time.Now()
	code, err := runCheck("(sleep 5 &); true")
	if err != nil || code != 0 {
		t.Fatalf("code=%d err=%v", code, err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("check waited %s on a background child", d)
	}
}

func TestAnInjectionStaysUnderClaudeCodesHookTextCap(t *testing.T) {
	long := strings.Repeat("x", 5000)
	setup(t, "a\tboom\tout\t"+long+"\t2026-10-18\t-\t-\ts\nb\tboom\tout\t"+long+"\t2026-10-18\t-\t-\ts\n")
	out := hook(t, map[string]any{
		"hook_event_name": "PostToolUse", "session_id": "s", "tool_input": map[string]any{"command": "x"},
		"tool_response": map[string]any{"stdout": "boom"},
	})
	var v struct {
		H struct {
			C string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(out), &v); err != nil || v.H.C == "" {
		t.Fatalf("no context: %q", out)
	}
	if len(v.H.C) >= 10000 {
		t.Fatalf("context is %d characters", len(v.H.C))
	}
}

func TestThePrefilterNeverRejectsARealMatch(t *testing.T) {
	for _, c := range []struct{ re, text string }{
		{`Dependabot alerts are disabled`, "HTTP 403: Dependabot alerts are disabled for this repository."},
		{`(?i)widget cache.{0,60}off by default`, "The WIDGET CACHE is OFF by default"},
		{`(?i)staging.{0,80}502 Bad Gateway`, "GET https://staging.example.com -> 502 bad gateway"},
		{`(foo|barbaz)qux`, "fooqux"},
		{`(abc)+def`, "abcabcdef"},
		{`x?yz`, "yz"},
		{`ERR_[A-Z]+_TIMEOUT`, "ERR_DNS_TIMEOUT"},
		{`(?i)kelvin`, "Kelvin"},
	} {
		row, err := parseRow("r\t"+c.re+"\tout\tc\t2026-10-18\tnever\t-\ts", 1)
		if err != nil {
			t.Fatal(err)
		}
		if !row.Match.MatchString(c.text) {
			t.Fatalf("%s: test text does not match the regex itself", c.re)
		}
		tb := &Table{Active: []Row{row}}
		if len(tb.Hits("out", c.text, today)) != 1 {
			t.Errorf("%s: prefilter %q rejected %q", c.re, row.need, c.text)
		}
	}
}

func TestThePrefilterSkipsRowsWhoseLiteralIsAbsent(t *testing.T) {
	row, _ := parseRow("r\t(?i)staging.{0,80}502 Bad Gateway\tout\tc\t2026-10-18\tnever\t-\ts", 1)
	if len(row.need) == 0 {
		t.Fatal("no literal extracted")
	}
	if containsAny(strings.ToLower("compiling...\nok\n"), row.need) {
		t.Fatal("prefilter passed text with neither literal")
	}
}

func BenchmarkHitsOnALongLog(b *testing.B) {
	var sb strings.Builder
	for i := range 200 {
		fmt.Fprintf(&sb, "r%d\t(?i)some unusual error text number %d.{0,40}here\tout\tc\t2026-10-20\tnever\t-\ts\n", i, i)
	}
	tb, err := Parse(strings.NewReader(sb.String()))
	if err != nil {
		b.Fatal(err)
	}
	text := capped(strings.Repeat("compiling...\n", 6000))
	for b.Loop() {
		tb.Hits("out", text, today)
	}
}
