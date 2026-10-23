# Security

## Reporting a vulnerability

Email **justin@justinstimatze.com** with `because-security` in the subject.
Please do not file public issues for suspected vulnerabilities.

## Threat model

`because` runs locally and opens no network connection.

**What it reads.** The table (`~/.claude/because.tsv` or `$BECAUSE_FILE`), the hook event JSON on
stdin, and at `Stop` the session transcript Claude Code names in `transcript_path`.

**What it runs.** `because review` runs each row's `check` field through `sh -c` from your home
directory, with a 20-second timeout and its output discarded. The table is therefore code: anyone who can write to it can run
commands as you the next time you review. Keep it in your home directory with your own permissions,
and read a row's `check` before adding a table someone else wrote. The hook itself never runs a
check; it only prints the command as text for the agent to consider.

**What it writes.** `because review --apply` rewrites the table in place, through a temporary file
and a rename, and only after the rewritten text parses. It follows a symlinked table to its target
and keeps the file's permissions. The hook appends seen row ids to
`seen-<session id>` under `$BECAUSE_STATE_DIR` (default: your user cache directory, `because/`),
mode 0600, so a row surfaces once per session.

**What it emits.** A row's cause, check and source, injected into the agent's context or, at `Stop`,
returned as a block reason. That text is whatever the table says, so the table is also prompt input:
the same trust you give a `CLAUDE.md` file.
