# agent-coordinator

A local macOS CLI and Bubble Tea dashboard for independent native Pi sessions.
Each job has its own hidden tmux server. Closing the dashboard does not stop an
established session; restarting it discovers existing jobs.

## Prerequisites and build

- macOS, Go 1.27+, tmux 3.5+, and native Pi on `PATH`.
- An ordinary terminal outside tmux for native Enter/full-screen CLI Observe.
  Listing, starting jobs and read-only dashboard capture also work inside tmux.
- Node 22.19+ for the optional Pi extension (development tests use Node 24).

```sh
go build -o bin/agent-coordinator ./cmd/agent-coordinator
export PATH="$PWD/bin:$PATH"
agent-coordinator
```

The coordinator executable must have an absolute path containing only letters,
digits, `_`, `.`, `/`, and `-`, to avoid tmux command-parser ambiguity.

## Dashboard

The session list stays visible beside a live, read-only **Observe** pane. **Enter**
opens that same native Pi session; **F12 Return** detaches only your client.
Observe captures the current screen without forwarding input or resizing Pi.
Use **o** to toggle it in a narrow terminal.

The dashboard refreshes automatically. Confirmed exited sessions disappear from
its active list, but their diagnostic records remain in `list`/`doctor`. Unknown
or unreachable sessions stay visible; refresh never deletes records.

For a selected compatible session, **m** composes literal steering, **s** requests
a human-confirmed graceful stop, and **X** separately confirms force-stop. A busy
Pi may defer graceful shutdown; a timeout is uncertain and never escalates.
Force-stop uses SIGKILL on the freshly verified foreground Pi group, can interrupt
writes, and may leave detached tools running. Neither stop means work succeeded.

Agent reports are labeled assertions, separate from process facts. Observed
extension dialogs and explicit agent requests flag attention independently.
Ending a dialog does not clear an agent's unresolved request. Idle is not an input
request. Startup trust prompts and dialogs outside Pi's extension observer may be
unobserved: **attention is unknown**, not an all-clear. Open the native session
to inspect. Sessions without the compatible extension cannot report/control yet;
reload them deliberately, never by an automatic restart.

## CLI

Names match `[a-z0-9][a-z0-9-]{0,47}`. Put the name **before** command options.
Directories must already exist. Tasks are nonblank UTF-8, at most 1 MiB.

```sh
printf '%s\n' 'Inspect the tests and explain what they cover.' |
  agent-coordinator start inspect --cwd "$PWD" --task-file - --json
agent-coordinator start another --cwd "$PWD" --task-file /private/path/task.txt
agent-coordinator list --json
agent-coordinator list --session-info --json
agent-coordinator doctor inspect --json
agent-coordinator enter inspect
agent-coordinator observe inspect
```

Default `start`/`list`/`doctor` JSON retains the legacy response shape for running
older extensions. `list` and `doctor` accept `--session-info` to include fresh
extension reports. The current Pi tool requests that read capability and falls
back to legacy reads when an older CLI does not support it; mutations never retry.

Enter attaches to the same Pi process. Multiple writable clients can type at
once; nobody is evicted. Observe is read-only, including keyboard, paste and
mouse input, and does not resize the session. It shows the current screen, not
scrollback. **F12 Return** detaches only your client and is reserved from Pi.

`continue NAME` explicitly sends SIGCONT to the verified foreground Pi process
group after confirmation; it does not resume a conversation file.
`repair-socket NAME` confirms and signals the recorded owned live server to
restore a missing socket. Neither action proves Pi is ready or work succeeded.
Use `doctor NAME` for the next safe action.

`stop UUID` requests graceful native shutdown after terminal confirmation.
`force-stop UUID` requires its own fresh confirmation and verifies the exact
foreground process group before SIGKILL. Both target immutable UUIDs, not names.

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

The tool also accepts immutable UUID-targeted `send` (`id`, `text`, `request_id`),
`stop` (`id`), and `force_stop` (`id`). Use a fresh UUID request ID for a new send;
reusing it cannot repeat the side effect. Stop operations require actual native
human confirmation in the initiating Pi session; model-supplied booleans are not
consent, and non-interactive calls are refused. Force-stop is a separate action,
not a graceful-stop fallback.

A managed agent can publish its own `report` (`summary`, optional `attention`).
A string requests attention; `null` explicitly clears the reported request;
omitting it preserves the prior request. Reports do not verify task success.

The installed CLI must be on Pi's `PATH`. Relative directories resolve against
that calling Pi session. Task and steering text go to CLI stdin, never shell
interpolation or process arguments. Tool cancellation terminates/reaps its CLI child, not an
established tmux/Pi job; an interrupted start may still have launched.

Personal installation is optional and requires your separate authorization:

```sh
pi install /absolute/path/to/agent-coordinator
```

Then reload/restart Pi. Loading without a session only registers capabilities.
A verified managed job starts its private controller at native session startup;
unmanaged or inherited descendant sessions cannot claim another job's endpoint.
The tool exposes no terminal attachment, cleanup or automated approvals.
Native project trust, hooks and approval/question UI remain authoritative.
A send receipt means only that the native message API was invoked, not that the
model received, acted on or completed it. A lost response is uncertain: inspect
the target, never automatically retry. Pending controls are fenced to the native
session/runtime generation, not merely a reusable name. Tool arguments are
recorded in Pi's transcript; this is **not** a secret-input channel.

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

New jobs identify the macOS boot with `kern.bootsessionuuid`, not the adjustable
boot wall clock. Their manifests declare the identity scheme explicitly, so older
binaries refuse those records rather than misclassify a UUID as a past boot.
Legacy clock-based records remain visible as unknown; the new coordinator will
not attach, signal or retire claimed work whose boot identity it cannot prove.
Existing native processes and attached clients keep running. There is no automatic
record migration, restart or personal-state rewrite.

Jobs share their directories and can make conflicting edits. This is not
isolation, scheduling, independently verified task completion or supervision. Sleep affects
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
