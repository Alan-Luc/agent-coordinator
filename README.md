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

A dense session table sits beside **Observe · READ ONLY** at 72×16 and above.
Wide windows add activity and agent-report columns; smaller terminals use a
compact fallback. Below 72×12, resize for full identity and recovery text.
The dashboard uses a fixed Kanagawa Ink palette. Selection uses `>` even without
color; `[INPUT]` marks an observed native prompt, `[AGENT]` an explicit agent
request, and `[?]` unknown observation.

Press **h** for centered keyboard help, then **Esc**, **h**, or **q** to close it.
**Ctrl+C** quits from either view. The compact footer keeps shared-input and
readiness/outcome guidance visible; action errors and confirmation warnings are
not hidden in help.

**Enter** opens that same native Pi session; **F12 Return** detaches only your
client. Observe captures the current screen, not scrollback, without forwarding
input or resizing Pi. It preserves validated colors and basic text styles while
filtering terminal-control sequences; captured content is not an attention or
control-authority source. **o** hides only the output; selected facts and attention
remain visible. Forms show the active field and retain literal multiline drafts;
confirmations label the full target identity and never default to yes.

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

## Manager communication

In an interactive Pi with a persisted conversation, opt in explicitly:

```text
/coordinator manage on
/coordinator adopt WORKER-UUID
/coordinator release WORKER-UUID
/coordinator manage off
```

Jobs started by an enabled manager are associated before launch. Adoption requires
confirmation of the exact existing worker; it never takes over another manager's
assignment. Release refuses outstanding requests. Model-requested `manage`
(`enabled`) and `adopt` (`id`) require native human confirmation. New, forked and
cloned conversations do not inherit enrollment. Turning management off pauses
delivery without stopping workers or deleting requests.

A verified assigned worker uses `publish` (`message_id`, `kind`, `text`) for a
`question`, `blocker` or reported `result`. Caller and manager identity come from
the adapter, not model arguments. Unassigned workers receive an error, not a claim
that Alan was notified. Routine progress continues to use `report` and does not
wake the manager model. Their sanitized summaries appear in a bounded,
unverified worker-report widget.

The open manager checks its inbox every two seconds. New publications are batched
into attributed follow-up notices: idle Pi starts a turn; busy Pi queues behind
its current work. Worker text is **not Alan's instruction or permission**. The
manager reads `inbox` (optional `cursor`), then explicitly `ack`s `message_ids`.
`escalate` (`message_id`, `note`) surfaces a persistent human-attention request.
`reply` (`message_id`, `request_id`, `text`) sends correlated plain text to the
saved originating runtime. `resolve` (`message_id`, `note`) explicitly closes a
request; its originating worker can also resolve it. Delivery, acknowledgement,
reply and resolution are separate. Replies cannot answer native consent dialogs:
enter that worker session when actual native interaction is needed.

Persistence means the backend saved the publication, not that a model read it.
Notice attempts are saved before the void native API call; they are not delivery
receipts. Unacknowledged notices may appear again after reconnect. Exactly-once
model execution is not promised. A duplicate retained publication ID with the
same payload is idempotent; conflicting content is refused. Reply claims are saved
before dispatch: an incomplete claim is uncertain and is never automatically
redispatched. Definite rejections are retained separately and replay the same error,
not another send. Explicit resolution can close a request despite an uncertain
historical reply; replay protection lasts while that message is retained.
A stale worker generation refuses reply without clearing the
request or retargeting another runtime. Inspect before making a fresh explicit
send. Worker reports/results never prove successful work.

Workers continue if the manager closes. Pending/escalated counts remain visible
in the manager and dashboard, including requests from ended jobs. Reopen the same
conversation to reconnect; a competing live or unprovable runtime lease refuses
ownership. Acknowledged unresolved requests remain outstanding without repeated
model wake-ups. Native prompt observation is partial and may miss brief dialogs;
stale, startup and unsupported observation is unknown. There is no always-on
service or desktop-notification delivery while Pi is closed.

Read-only diagnostics: `agent-coordinator messages --json` (page with `--cursor`).
Communication uses a separate private versioned `communication/state.json`;
unknown schemas fail closed. Limits are 4 KiB UTF-8 publication text, 16 unresolved
requests per worker, 64 per manager, 16 managers, 128 bindings/messages and a
1 MiB encoded document. Admission reserves delivery metadata space; lifecycle
writes retain the hard document bound and can evict eligible records to drain
accepted requests. Managed starts retain the public 1 MiB task limit, including
when JSON escaping makes their internal request larger. Unresolved requests and unacknowledged results are never
evicted to accept new work. Resolved messages and acknowledged results are kept
at most seven days and may be evicted sooner under pressure. Full queues and
persistence failures are explicit; `forget` cannot silently remove outstanding
requests. This is a small local queue, not a secret-input channel or fleet system.
Optional terminal-notifier is not required or installed by this package.

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
