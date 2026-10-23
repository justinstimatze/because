# because

A [Claude Code](https://docs.claude.com/en/docs/claude-code) hook that surfaces a known cause at the
moment its symptom appears.

Notes an agent keeps for itself only help when it thinks to look them up, and the miss happens when
it doesn't know it should: a 403 gets investigated as a mystery while the reason sits in a notes
file from last week. With `because`, each time you learn why a symptom happens, you add one row to a
table, keyed on the exact text the agent will next see. The hook then matches that row where the
text arrives:

| where   | hook        | matched against                                         | effect                                         |
| ------- | ----------- | ------------------------------------------------------- | ---------------------------------------------- |
| `out`   | PostToolUse | every string in the tool's JSON output, unescaped       | adds the cause to the agent's context          |
| `in`    | PreToolUse  | the tool's input: the command, or the file content      | adds the cause to the agent's context          |
| `reply` | Stop        | the reply the agent just wrote, minus code spans/fences | blocks the reply once so the agent can revise  |

A row surfaces at most once per session, at most three rows per hit. Each is framed as a hypothesis
with its check command; whether to run it is the agent's call. At session start the hook
stays quiet unless some row has passed its until date, gone quiet or gone loud (below), and then it
says so
in one line.

## The table

`~/.claude/because.tsv` (or `$BECAUSE_FILE`), created and edited by hand. One row per line, eight
tab-separated fields; blank lines and `#` comments are ignored:

    id  match  where  cause  since  until  check  source

- `id` is unique across the file.
- `match` is an RE2 regex (`(?i)` for case-insensitive).
- `where` is `in`, `out` or `reply`, from the table above.
- `since` is the `YYYY-MM-DD` date the cause was learned. It is required.
- `until` is `YYYY-MM-DD` for a hard end (the row stops matching after that day), `never`, or `-`
  for open: no end date, but the row is flagged once it goes quiet.
- `check` is a shell command whose exit 0 means the cause still holds, or `-` for none. It runs
  under `sh -c` from your home directory, so use absolute or `~` paths.
- `source` is free text telling the agent where the cause came from, or `-`.

Rows are never deleted. `because review --apply` moves a row whose until date has passed, or whose
check exits non-zero, below a `# resolved` line and stamps its source with the date and reason. A
check that cannot run (timeout, missing binary) keeps its row. After a review the file ends like
this:

    # resolved
    old-proxy	407 Proxy Authentication	out	The corporate proxy needs a token.	2026-09-01	2026-10-01	-	setup notes · resolved 2026-10-02 (past until 2026-10-01)

An open row that has not surfaced in 60 days (counting from `since` if it never has) is quiet. It
keeps matching, because an old cause can still be the right one, but `lint`, `review` and the
session-start line list it until you give it an until date, mark it `never`, or move it under
`# resolved` yourself. That is the pressure that keeps the table relevant; nothing retires on age
alone.

The opposite signal is loud: a row that surfaced in more than five sessions in 30 days. That
usually means the match is wider than the cause, so `lint` and `review` list it for narrowing. A
row whose error genuinely keeps recurring can be marked `never`, which exempts it.

### Skipping documentation

A cause gets written down in tickets and notes, so reading those would match the row that
documents it. Two optional lines at the top of the table stop that:

    #! skip-tools ^mcp__linear
    #! skip-text open-asks\.md|/memory/

`skip-tools` is a regex over the tool name; a matching call is not checked at all. `skip-text` is a
regex over the call's input, such as a command that greps a notes file. Each may appear once.

The hook records which rows a session has seen under `$BECAUSE_STATE_DIR` (default
`because/` in your user cache directory, `~/.cache/because` on Linux), and removes those records
after 14 days. Each time a row surfaces, a dated line goes in `hits.tsv` there; quiet and loud
are measured from it, and lines older than 30 days are dropped except each row's latest.

## Example

A row that explains a failure the agent would otherwise debug:

    staging-down	(?i)staging.{0,80}502 Bad Gateway	out	Staging is down for the database migration; prod is unaffected.	2026-10-18	2026-10-25	! curl -sf https://staging.example.com/healthz	migration plan

Check it before relying on the hook:

    $ because match out 'GET https://staging.example.com/api -> 502 Bad Gateway'
    Matches: Treat each as a hypothesis until its check runs:
    - [staging-down] since 2026-10-18: Staging is down for the database migration; prod is unaffected. Confirm with: `! curl -sf https://staging.example.com/healthz` (exit 0 = still true). Source: migration plan.

When a tool's output contains that text in a session, the agent sees the same lines under "That
output matches a known cause."

## Commands

    because hook              the hook entry point (reads the event JSON on stdin)
    because lint              parse the table and list rows with their expiry
    because review [--apply]  run checks; list or retire rows that no longer hold
    because match WHERE TEXT  show which rows TEXT would surface
    because path              print the table's path

`lint`, `review` and `match` work on the table directly and do not need the hook registered.

## Install

Requires Go 1.26 or later, on Linux or macOS.

    go install github.com/justinstimatze/because/cmd/because@latest

or `make install` from a clone, which runs `go install` with the version stamped from the git tag.
Then register the hook in `~/.claude/settings.json`. The `mcp__.*` matcher covers output from every
MCP server's tools:

```json
{
  "hooks": {
    "PreToolUse":   [{ "matcher": "Bash|Write|Edit",        "hooks": [{ "type": "command", "command": "because hook", "timeout": 5 }] }],
    "PostToolUse":  [{ "matcher": "Bash|mcp__.*|WebFetch",  "hooks": [{ "type": "command", "command": "because hook", "timeout": 5 }] }],
    "Stop":         [{ "hooks": [{ "type": "command", "command": "because hook", "timeout": 5 }] }],
    "SessionStart": [{ "hooks": [{ "type": "command", "command": "because hook", "timeout": 5 }] }]
  }
}
```

Until `~/.claude/because.tsv` exists, the hook notes the missing file on stderr and lets every call
through. A table that fails to parse is reported at session start.

## Limits

Each hooked call reads and compiles the whole table, then checks every active row for that hook
against at most 64 KiB of text (the first and last 32 KiB of longer output). A row's regex runs only
when the text contains one of the literals any match would need, so a row costs a substring scan on
most calls: on 64 KiB of build output a call takes about 6 ms with a few dozen rows and 40 ms with
a thousand. A hit injects at most three rows, each row once per session, cut to 9,000 characters so
Claude Code delivers it inline rather than moving it to a file. `because lint` warns past 500 active
rows or a row over 800 characters.

## Security

`because review` runs each row's `check` with `sh -c`, so treat the table as code. See
[SECURITY.md](SECURITY.md).

## Prior art

The idea is an ITIL [known error database](https://en.wikipedia.org/wiki/Known_error): diagnosed
causes and their workarounds, looked up by symptom before anyone investigates. `because` moves the
lookup to the moment the symptom text appears, so nobody has to remember to search. The row schema follows [Alertmanager silences](https://prometheus.io/docs/alerting/latest/alertmanager/#silences):
matchers, a time window, a comment and a creator. Marking a row resolved instead of deleting it
follows [Nygard's architecture decision records](https://cognitect.com/blog/2011/11/15/documenting-architecture-decisions)
and the validity windows on facts in [Graphiti](https://github.com/getzep/graphiti).

## License

MIT
