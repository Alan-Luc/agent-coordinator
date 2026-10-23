# agent-coordinator

A local macOS CLI and Bubble Tea dashboard for independent native Pi sessions.
Each job has its own hidden tmux server. Closing the dashboard does not stop an
established session; restarting it discovers existing jobs.

## Prerequisites and build

- macOS, Go 1.27+, tmux 3.5+, and native Pi on `PATH`.
- An ordinary terminal outside tmux for Enter/Observe. Listing and starting jobs
  also work inside an existing tmux client.
- Node 22.19+ for the optional Pi extension (development tests use Node 24).

```sh
go build -o bin/agent-coordinator ./cmd/agent-coordinator
export PATH="$PWD/bin:$PATH"
agent-coordinator
```

The coordinator executable must have an absolute path containing only letters,
digits, `_`, `.`, `/`, and `-`, to avoid tmux command-parser ambiguity.

## CLI

Names match `[a-z0-9][a-z0-9-]{0,47}`. Put the name **before** command options.
Directories must already exist. Tasks are nonblank UTF-8, at most 1 MiB.

```sh
printf '%s\n' 'Inspect the tests and explain what they cover.' |
  agent-coordinator start inspect --cwd "$PWD" --task-file - --json
agent-coordinator start another --cwd "$PWD" --task-file /private/path/task.txt
agent-coordinator list --json
agent-coordinator doctor inspect --json
agent-coordinator enter inspect
agent-coordinator observe inspect
```

Enter attaches to the same Pi process. Multiple writable clients can type at
once; nobody is evicted. Observe is read-only, including keyboard, paste and
mouse input, and does not resize the session. It shows the current screen, not
scrollback. **F12 Return** detaches only your client and is reserved from Pi.

`continue NAME` explicitly sends SIGCONT to the verified foreground Pi process
group after confirmation; it does not resume a conversation file.
`repair-socket NAME` confirms and signals the recorded owned live server to
restore a missing socket. Neither action proves Pi is ready or work succeeded.
Use `doctor NAME` for the next safe action.

`forget NAME` retires an unclaimed or verified ended attempt, never a live claimed
Pi process. `forget --id UUID` addresses an old retired attempt after name reuse.
Revocation fences delayed bootstraps; small UUID records and locks remain for the
installation lifetime. Unknown ownership refuses destructive operations.

Exit codes: `0` completed/submitted action, `1` operational failure, `2` invalid
usage/unsupported terminal, `3` duplicate name/lock timeout, `75` uncertainty.
After an uncertain launch, inspect `list`/`doctor`; **do not blindly retry**.
A live terminal or exit status zero is not task success. Readiness remains
`unverified` and task outcome `unknown`.

## Optional Pi tool

The repository is a Pi package exposing one typed `coordinator` tool:

```json
{"request":{"action":"start","name":"inspect","cwd":".","task":"Inspect the tests."}}
{"request":{"action":"list"}}
{"request":{"action":"doctor","name":"inspect"}}
```

The installed CLI must be on Pi's `PATH`. Relative directories resolve against
that calling Pi session. Task text goes to CLI stdin, never shell interpolation
or process arguments. Tool cancellation terminates/reaps its CLI child, not an
established tmux/Pi job; an interrupted start may still have launched.

Personal installation is optional and requires your separate authorization:

```sh
pi install /absolute/path/to/agent-coordinator
```

Then reload/restart Pi. Loading the extension registers the tool but starts no
work. The tool exposes no terminal attachment, signals, cleanup, later messaging
or automated approvals. Native project trust, hooks and approval/question UI
remain authoritative. Tool arguments are recorded in Pi's transcript; this is
**not** a secret-input channel.

## State, privacy and limits

State uses `AC_STATE_DIR`, otherwise `$XDG_STATE_HOME/agent-coordinator`, otherwise
`$HOME/.local/state/agent-coordinator`. Managed directories are `0700`, files
`0600`; private sockets live under canonical `/tmp/ac-<uid>-<token>/<uuid>/`.
Do not manually edit or delete active state/socket paths.

Pi receives a native `@file` envelope containing the fixed header
`Initial task supplied by agent-coordinator:` plus a blank line and the exact
supplied task body. The envelope is not a security delimiter. Task files,
transcripts and terminal output contain task text; routine coordinator errors and
argv do not. Native input hooks may transform input normally.

Jobs share their directories and can make conflicting edits. This is not
isolation, scheduling, semantic progress tracking or supervision. Sleep affects
availability; shutdown/reboot stops work. There is no automatic restart, retry or
recovery. A live process whose server ended may require manual intervention.
Ownership checks guard accidental stale/PID-reused resources, not hostile code
running as the same OS account.

## Checks

```sh
go test ./internal/coordinator/... ./internal/tmux/... ./internal/tui/...
go test -tags=integration ./integration/...
npm ci --ignore-scripts
npm test
go test ./...
go vet ./...
```

Integration fixtures use fake Pi with actual private tmux/PTY clients. They do
not prove native Pi ingestion, trust UI or physical-key/terminal compatibility;
those require separate native acceptance. Personal-package installation is not
performed by tests.
