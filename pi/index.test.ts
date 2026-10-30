import assert from "node:assert/strict";
import { chmod, mkdir, mkdtemp, readFile, realpath, rm, writeFile } from "node:fs/promises";
import { delimiter, join, resolve } from "node:path";
import { tmpdir } from "node:os";
import test from "node:test";
import { Value } from "typebox/value";
import { cliArguments, coordinatorTool, parseResult, registerCoordinator, runCli } from "./index.ts";
import { parseControlRequest, updateAgentReport, updateObservedActivity, updateObservedPrompt, verifyInheritedJob } from "./session-control.ts";
import type { SessionStatus } from "./control-protocol.ts";

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

function commandLog(text: string): string[][] {
  return text.trim().split("\n").map((line) => {
    const parsed: unknown = JSON.parse(line);
    assert.ok(Array.isArray(parsed));
    const args: string[] = [];
    for (const value of parsed) { assert.ok(typeof value === "string"); args.push(value); }
    return args;
  });
}

function errorCode(value: unknown): string {
  assert.ok(isRecord(value) && isRecord(value.details) && isRecord(value.details.error));
  const code = value.details.error.code;
  assert.ok(typeof code === "string");
  return code;
}

function register() {
  let registrations = 0;
  let registeredCoordinator = false;
  registerCoordinator({ registerTool(tool) { registrations += 1; registeredCoordinator = tool === coordinatorTool; } });
  assert.ok(registeredCoordinator, "registerCoordinator should pass the coordinator tool to the host");
  return { tool: coordinatorTool, registrations };
}

test("report tools retain their own controller when another runtime registers", async () => {
  function bindReport(status: SessionStatus) {
    let execute: (...args: unknown[]) => unknown = () => { throw new Error("tool not registered"); };
    registerCoordinator({ registerTool(tool) { execute = (...args) => Reflect.apply(tool.execute, undefined, args); } }, {
      status: () => status,
      report: async () => status,
    });
    return () => execute("call", { request: { action: "report", summary: "working" } }, undefined, undefined, { sessionManager: { getSessionId: () => status.session_id } });
  }
  const first: SessionStatus = { protocol: 1, job_id: "first", owner: "owner", session_id: "native-a", generation: "runtime-a", pid: 1, updated_at: 1, activity: "idle" };
  const invokeFirst = bindReport(first);
  bindReport({ ...first, job_id: "second", session_id: "native-b", generation: "runtime-b" });
  const result = await invokeFirst();
  assert.ok(isRecord(result) && isRecord(result.structuredContent) && isRecord(result.structuredContent.reported));
  assert.equal(result.structuredContent.reported.job_id, "first", "another runtime must not retarget a report");
});

test("control requests accept only the contract actions", () => {
  const { tool } = register();
  assert.ok(tool);
  for (const request of [
    { action: "send", id: "123e4567-e89b-42d3-a456-426614174000", text: "literal", request_id: "123e4567-e89b-42d3-a456-426614174001" },
    { action: "report", summary: "working" },
    { action: "report", summary: "working", attention: null },
    { action: "stop", id: "123e4567-e89b-42d3-a456-426614174000" },
    { action: "force_stop", id: "123e4567-e89b-42d3-a456-426614174000" },
  ]) assert.equal(Value.Check(tool.parameters, { request }), true, `${request.action} request should be accepted`);
  assert.equal(Value.Check(tool.parameters, { request: { action: "report", summary: "x", extra: true } }), false);
});

test("control protocol strictly bounds UTF-8 text and rejects unknown fields", () => {
  const base = { protocol: 1, job_id: "123e4567-e89b-42d3-a456-426614174000", session_id: "native", generation: "123e4567-e89b-42d3-a456-426614174001", request_id: "123e4567-e89b-42d3-a456-426614174002" };
  assert.equal(parseControlRequest({ ...base, action: "send", text: "é".repeat(32768) })?.action, "send");
  assert.equal(parseControlRequest({ ...base, action: "send", text: "é".repeat(32769) }), undefined);
  assert.equal(parseControlRequest({ ...base, action: "send", text: "\uD800" }), undefined);
  assert.equal(parseControlRequest({ ...base, action: "send", text: "ok", extra: true }), undefined);
  assert.equal(parseControlRequest({ ...base, action: "stop", text: "unexpected" }), undefined);
});

test("agent report attention is independent from native prompt observation", () => {
  const status: SessionStatus = { protocol: 1, job_id: "job", owner: "owner", session_id: "native", generation: "generation", pid: 1, updated_at: 1, activity: "unknown" };
  updateObservedActivity(status, "idle");
  assert.equal(status.prompt, undefined, "idle is not an input request");
  updateObservedActivity(status, "busy");
  updateObservedPrompt(status, { kind: "custom", title: "Permission" });
  assert.ok(updateAgentReport(status, "Build running", "Choose a strategy"));
  updateObservedActivity(status, "idle");
  updateObservedPrompt(status, undefined);
  assert.equal(status.prompt, undefined, "native prompt end clears only native attention");
  assert.equal(updateAgentReport(status, "Still running", undefined)?.attention, "Choose a strategy");
  assert.equal(updateAgentReport(status, "Resolved", null)?.attention, undefined);
  assert.equal(status.prompt, undefined);
  assert.equal(updateAgentReport(status, "x".repeat(1025), null), undefined);
});

test("inherited job activation requires exact recorded pane PID", async () => {
  const root = await mkdtemp(join(await realpath(tmpdir()), "coordinator-session-control-"));
  const id = "123e4567-e89b-42d3-a456-426614174000";
  const uid = process.getuid?.();
  assert.ok(typeof uid === "number");
  const owner = `${Date.now().toString(16).padStart(8, "0").slice(-8)}${Math.random().toString(16).slice(2, 10)}`;
  const jobs = join(root, "jobs");
  const job = join(jobs, id);
  const socketRoot = join(await realpath("/tmp"), `ac-${uid}-${owner}`);
  const socketDir = join(socketRoot, id);
  const previous = new Map(["AC_STATE_DIR", "AC_OWNER", "AC_JOB"].map((key) => [key, process.env[key]]));
  try {
    await Promise.all([jobs, socketRoot].map((path) => mkdir(path, { mode: 0o700 })));
    await Promise.all([job, socketDir].map((path) => mkdir(path, { mode: 0o700 })));
    await writeFile(join(root, "owner.json"), JSON.stringify({ schema: 1, token: owner }), { mode: 0o600 });
    const bootId = "3e322019-1073-4f52-afd0-abed9a17ce70";
    const identity = { boot_id: bootId, pid: process.pid, uid, start_sec: 1, start_usec: 0 };
    const manifestPath = join(job, "manifest.json");
    const manifest = { schema: 1, id, name: "test-job", owner, cwd: root, pi: "/bin/pi", tmux: "/bin/tmux", coordinator: "/bin/coordinator", created: new Date().toISOString(), boot_id: bootId, protocol: "1", socket: join(socketDir, "s") };
    await writeFile(manifestPath, JSON.stringify(manifest), { mode: 0o600 });
    const launchPath = join(job, "launch.json");
    await writeFile(launchPath, JSON.stringify({ schema: 1, phase: "exec-intent", server: { ...identity, pid: process.pid + 1 }, pane: identity }), { mode: 0o600 });
    process.env.AC_STATE_DIR = root;
    process.env.AC_OWNER = owner;
    process.env.AC_JOB = id;
    assert.equal(await verifyInheritedJob(), undefined, "legacy manifest without an explicit boot identity scheme must not activate");
    await writeFile(manifestPath, JSON.stringify({ ...manifest, boot_id_scheme: "darwin-boot-session-uuid" }));
    assert.ok(await verifyInheritedJob(), "recorded pane process with the stable boot identity may activate");
    await writeFile(manifestPath, JSON.stringify({ ...manifest, boot_id_scheme: "unknown-scheme" }));
    assert.equal(await verifyInheritedJob(), undefined, "unknown boot identity scheme must not activate");
    await writeFile(manifestPath, JSON.stringify({ ...manifest, boot_id_scheme: "darwin-boot-session-uuid", boot_id: "not-a-uuid" }));
    assert.equal(await verifyInheritedJob(), undefined, "malformed stable boot identity must not activate");
    await writeFile(manifestPath, JSON.stringify({ ...manifest, boot_id_scheme: "darwin-boot-session-uuid" }));
    await writeFile(launchPath, JSON.stringify({ schema: 1, phase: "exec-intent", server: { ...identity, pid: process.pid + 1 }, pane: { ...identity, pid: process.pid + 1 } }), { mode: 0o600 });
    assert.equal(await verifyInheritedJob(), undefined, "descendant must not activate its parent's endpoint");
    await writeFile(launchPath, JSON.stringify({ schema: 1, phase: "exec-intent", server: { ...identity, pid: process.pid + 1 }, pane: identity }), { mode: 0o600 });
    await writeFile(manifestPath, JSON.stringify({ ...manifest, protocol: "2", boot_id_scheme: "darwin-boot-session-uuid" }));
    assert.equal(await verifyInheritedJob(), undefined, "protocol2 without intent and role must fail closed");
    const fileObservation = { Path: "/managed/file", Hash: "a".repeat(64), Size: 1, Mode: 0, UID: uid, Dev: 1, Ino: 1 };
    const observation = { Launcher: fileObservation, Current: fileObservation, Package: fileObservation, BinLink: { Path: "/managed/link", Target: "target", UID: uid, Dev: 1, Ino: 1 }, Entry: fileObservation, Version: "1.4.0" };
    await writeFile(join(job, "intent.json"), JSON.stringify({ schema: 1, kind: "interactive", terminal_profile: "transparent-v1", argv_sha256: "a".repeat(64), native_pi: { Executable: "/bin/pi", Observation: observation } }), { mode: 0o600 });
    await writeFile(join(job, "launch-argv.json"), JSON.stringify({ schema: 1, argv: [] }), { mode: 0o600 });
    await writeFile(join(job, "role.json"), JSON.stringify({ schema: 1, role: "standalone" }), { mode: 0o600 });
    await rm(join(job, "launch-argv.json"));
    const protocol2 = await verifyInheritedJob();
    assert.equal(protocol2?.protocol, "2", "well-shaped protocol2 activation retains recorded pane identity");
    assert.deepEqual(protocol2?.pane, identity);
    await writeFile(join(job, "launch-argv.json"), JSON.stringify({ schema: 1, argv: [] }), { mode: 0o600 });
    assert.equal(await verifyInheritedJob(), undefined, "leftover launch payload must refuse activation after bootstrap");
    await rm(join(job, "launch-argv.json"));
    await writeFile(join(job, "role.json"), JSON.stringify({ schema: 1, role: "unexpected" }), { mode: 0o600 });
    assert.equal(await verifyInheritedJob(), undefined, "unsupported role fails closed");
  } finally {
    for (const [key, value] of previous) value === undefined ? delete process.env[key] : process.env[key] = value;
    await rm(root, { recursive: true, force: true });
    await rm(socketRoot, { recursive: true, force: true });
  }
});

test("stop refuses absent UI or an already-aborted call before CLI mutation", async () => {
  const { tool } = register();
  assert.ok(tool);
  const request = { request: { action: "stop", id: "123e4567-e89b-42d3-a456-426614174000" } };
  const noUi = await Reflect.apply(tool.execute, undefined, ["call", request, undefined, undefined, { cwd: tmpdir(), hasUI: false }]);
  assert.equal(errorCode(noUi), "confirmation_unavailable");
  const aborted = new AbortController();
  aborted.abort();
  const noMutation = await Reflect.apply(tool.execute, undefined, ["call", request, aborted.signal, undefined, { cwd: tmpdir(), hasUI: true }]);
  assert.equal(errorCode(noMutation), "confirmation_unavailable");
});

test("stop denial and session replacement never reach the mutating CLI phase", async () => {
  const directory = await mkdtemp(join(tmpdir(), "coordinator-stop-confirm-"));
  const executable = join(directory, "coordinate");
  const capture = join(directory, "commands");
  const previousPath = process.env.PATH;
  const previousCapture = process.env.CONTROL_CAPTURE;
  const id = "123e4567-e89b-42d3-a456-426614174000";
  const uid = process.getuid?.();
  assert.ok(typeof uid === "number");
  const target = { id, session_id: "123e4567-e89b-42d3-a456-426614174001", generation: "123e4567-e89b-42d3-a456-426614174002", pane: { boot_id: "boot", pid: 123, uid, start_sec: 1, start_usec: 0 } };
  try {
    await writeFile(executable, `#!/usr/bin/env node\nconst fs=require('node:fs');const args=process.argv.slice(2);fs.appendFileSync(process.env.CONTROL_CAPTURE,JSON.stringify(args)+'\\n');if(args[0]==='_target')process.stdout.write(JSON.stringify(${JSON.stringify(target)}));\n`);
    await chmod(executable, 0o700);
    process.env.PATH = `${directory}${delimiter}${previousPath ?? ""}`;
    process.env.CONTROL_CAPTURE = capture;
    const { tool } = register();
    assert.ok(tool);
    const context = { cwd: directory, hasUI: true, sessionManager: { getSessionId: () => "initiator" }, ui: { confirm: async () => false } };
    const denied = await Reflect.apply(tool.execute, undefined, ["call", { request: { action: "stop", id } }, undefined, undefined, context]);
    assert.equal(errorCode(denied), "confirmation_cancelled");
    assert.deepEqual(commandLog(await readFile(capture, "utf8")), [["_target", id, "--json"]]);

    await writeFile(capture, "");
    let sessionReads = 0;
    const replacedContext = { ...context, sessionManager: { getSessionId: () => ++sessionReads === 1 ? "initiator" : "replaced" }, ui: { confirm: async () => true } };
    const replaced = await Reflect.apply(tool.execute, undefined, ["call", { request: { action: "stop", id } }, undefined, undefined, replacedContext]);
    assert.equal(errorCode(replaced), "confirmation_cancelled");
    assert.deepEqual(commandLog(await readFile(capture, "utf8")), [["_target", id, "--json"]]);
  } finally {
    if (previousPath === undefined) delete process.env.PATH; else process.env.PATH = previousPath;
    if (previousCapture === undefined) delete process.env.CONTROL_CAPTURE; else process.env.CONTROL_CAPTURE = previousCapture;
    await rm(directory, { recursive: true, force: true });
  }
});

test("uncertain send response is never automatically retried", async () => {
  const directory = await mkdtemp(join(tmpdir(), "coordinator-send-uncertain-"));
  const executable = join(directory, "coordinate");
  const capture = join(directory, "commands");
  const previousPath = process.env.PATH;
  const previousCapture = process.env.CONTROL_CAPTURE;
  const id = "123e4567-e89b-42d3-a456-426614174000";
  const uid = process.getuid?.();
  assert.ok(typeof uid === "number");
  const target = { id, session_id: "123e4567-e89b-42d3-a456-426614174001", generation: "123e4567-e89b-42d3-a456-426614174002", pane: { boot_id: "boot", pid: 123, uid, start_sec: 1, start_usec: 0 } };
  try {
    await writeFile(executable, `#!/usr/bin/env node\nconst fs=require('node:fs');const args=process.argv.slice(2);fs.appendFileSync(process.env.CONTROL_CAPTURE,JSON.stringify(args)+'\\n');if(args[0]==='_target')process.stdout.write(JSON.stringify(${JSON.stringify(target)}));else process.stdout.write('invalid');\n`);
    await chmod(executable, 0o700);
    process.env.PATH = `${directory}${delimiter}${previousPath ?? ""}`;
    process.env.CONTROL_CAPTURE = capture;
    const { tool } = register();
    assert.ok(tool);
    const result = await Reflect.apply(tool.execute, undefined, ["call", { request: { action: "send", id, text: "literal 世界", request_id: "123e4567-e89b-42d3-a456-426614174003" } }, undefined, undefined, { cwd: directory }]);
    assert.equal(errorCode(result), "uncertain");
    const commands = commandLog(await readFile(capture, "utf8"));
    assert.equal(commands.filter((args) => args[0] === "send").length, 1);
    assert.deepEqual(commands.map((args) => args[0]), ["_target", "send"]);
  } finally {
    if (previousPath === undefined) delete process.env.PATH; else process.env.PATH = previousPath;
    if (previousCapture === undefined) delete process.env.CONTROL_CAPTURE; else process.env.CONTROL_CAPTURE = previousCapture;
    await rm(directory, { recursive: true, force: true });
  }
});

test("extension registers one strict Coordinate request tool without spawning", () => {
  const { tool, registrations } = register();
  assert.equal(registrations, 1);
  assert.equal(tool?.name, "coordinate");
  assert.equal(tool?.label, "Coordinate");
  assert.ok(tool);
  assert.equal(Value.Check(tool.parameters, { request: { action: "list" } }), true);
  assert.equal(Value.Check(tool.parameters, { request: { action: "list", extra: true } }), false);
  assert.equal(Value.Check(tool.parameters, { action: "list" }), false);
  assert.equal(Value.Check(tool.parameters, { request: { action: "run" } }), false);
});

test("rich reads fall back only for old-CLI invalid_usage and never retry mutations", async () => {
  const directory = await mkdtemp(join(tmpdir(), "coordinator-legacy-read-"));
  const executable = join(directory, "coordinate");
  const capture = join(directory, "commands");
  const previousPath = process.env.PATH;
  const previousCapture = process.env.CONTROL_CAPTURE;
  try {
    await writeFile(executable, `#!/usr/bin/env node\nconst fs=require('node:fs');const args=process.argv.slice(2);fs.appendFileSync(process.env.CONTROL_CAPTURE,JSON.stringify(args)+'\\n');if(args.includes('--session-info')){process.stdout.write(JSON.stringify({error:{code:'invalid_usage',message:'unknown flag',uncertain:false}}));process.exit(2)}process.stdout.write(JSON.stringify({jobs:[]}));\n`);
    await chmod(executable, 0o700);
    process.env.PATH = `${directory}${delimiter}${previousPath ?? ""}`;
    process.env.CONTROL_CAPTURE = capture;
    const { tool } = register();
    await Reflect.apply(tool.execute, undefined, ["call", { request: { action: "list" } }, undefined, undefined, { cwd: directory }]);
    assert.deepEqual(commandLog(await readFile(capture, "utf8")), [["list", "--session-info", "--json"], ["list", "--json"]]);
  } finally {
    if (previousPath === undefined) delete process.env.PATH; else process.env.PATH = previousPath;
    if (previousCapture === undefined) delete process.env.CONTROL_CAPTURE; else process.env.CONTROL_CAPTURE = previousCapture;
    await rm(directory, { recursive: true, force: true });
  }
});

test("fixed action vectors keep malicious task text exclusively on stdin", () => {
  const task = "\uFEFFUnicode @mention\n</file>; touch /tmp/should-not-run";
  assert.deepEqual(cliArguments({ action: "start", name: "job-a", cwd: "work dir", task }, "/caller/project"), {
    argv: ["start", "job-a", "--cwd", resolve("/caller/project", "work dir"), "--task-file", "-", "--json"],
    cwd: "/caller/project", input: task,
  });
  assert.deepEqual(cliArguments({ action: "list" }, "/caller"), { argv: ["list", "--session-info", "--json"], cwd: "/caller" });
  assert.deepEqual(cliArguments({ action: "doctor", name: "job-a" }, "/caller"), { argv: ["doctor", "job-a", "--session-info", "--json"], cwd: "/caller" });
});

test("validates action response shapes and rejects malformed protocol", () => {
  const job = { id: "id", name: "job", launch: "submitted", terminal: "live", pi_readiness: "unverified", task_outcome: "unknown", clients: 0, retired: false, next_action: "none" };
  assert.deepEqual(parseResult(JSON.stringify(job), "start"), job);
  assert.deepEqual(parseResult(JSON.stringify({ jobs: [job] }), "list"), { jobs: [job] });
  assert.throws(() => parseResult(JSON.stringify({ jobs: [] }), "doctor"), /wrong response/);
  assert.throws(() => parseResult("not json", "list"), /invalid JSON/);
  assert.throws(() => parseResult(JSON.stringify({ id: "id" }), "start"), /does not match/);
  for (const invalid of [
    { ...job, pi_readiness: "ready" },
    { ...job, task_outcome: "success" },
    { ...job, clients: -1 },
  ]) assert.throws(() => parseResult(JSON.stringify(invalid), "start"), /does not match/);
  const unknown = { ...job, name: "" };
  assert.deepEqual(parseResult(JSON.stringify({ jobs: [unknown] }), "list"), { jobs: [unknown] });
});

test("tool executes against the PATH CLI with cwd and task confined to their channels", async () => {
  const directory = await mkdtemp(join(tmpdir(), "coordinator-tool-"));
  const executable = join(directory, "coordinate");
  const capture = join(directory, "stdin");
  const previousPath = process.env.PATH;
  const previousCapture = process.env.CAPTURE;
  const previousStarted = process.env.STARTED;
  const task = "\uFEFFliteral @x\n</file>; $(touch /tmp/not-run)";
  const job = { id: "id", name: "job", launch: "submitted", terminal: "live", pi_readiness: "unverified", task_outcome: "unknown", clients: 0, retired: false, next_action: "none" };
  try {
    await writeFile(executable, `#!/bin/sh\ncat > "$CAPTURE"\nprintf '%s' '${JSON.stringify(job)}'\n`);
    await chmod(executable, 0o700);
    process.env.PATH = `${directory}${delimiter}${previousPath ?? ""}`;
    process.env.CAPTURE = capture;
    const { tool } = register();
    assert.ok(tool);
    const result = await Reflect.apply(tool.execute, undefined, ["call", { request: { action: "start", name: "job", cwd: "workspace", task } }, undefined, undefined, { cwd: directory }]);
    assert.deepEqual(result, { content: [{ type: "text", text: JSON.stringify(job) }], details: job, structuredContent: job, });
    assert.equal(await readFile(capture, "utf8"), task);

    const preAborted = new AbortController();
    preAborted.abort();
    const notSubmitted = await Reflect.apply(tool.execute, undefined, ["call", { request: { action: "start", name: "job", cwd: ".", task } }, preAborted.signal, undefined, { cwd: directory }]);
    assert.deepEqual(notSubmitted, {
      content: [{ type: "text", text: JSON.stringify({ error: { code: "coordinator_error", message: "Coordinate request was cancelled before launch.", name: "job", uncertain: false } }) }],
      details: { error: { code: "coordinator_error", message: "Coordinate request was cancelled before launch.", name: "job", uncertain: false } },
      structuredContent: { error: { code: "coordinator_error", message: "Coordinate request was cancelled before launch.", name: "job", uncertain: false } },
      isError: true,
    });
    assert.equal(await readFile(capture, "utf8"), task);

    const startedFile = join(directory, "started");
    process.env.STARTED = startedFile;
    await writeFile(executable, "#!/usr/bin/env node\nrequire('node:fs').writeFileSync(process.env.STARTED, 'yes'); setInterval(() => {}, 50);\n");
    const afterSpawn = new AbortController();
    const submitted = Reflect.apply(tool.execute, undefined, ["call", { request: { action: "start", name: "job", cwd: ".", task } }, afterSpawn.signal, undefined, { cwd: directory }]);
    for (let attempt = 0; attempt < 100; attempt += 1) {
      try { await readFile(startedFile, "utf8"); break; }
      catch { await new Promise((resolvePromise) => setTimeout(resolvePromise, 10)); }
    }
    assert.equal(await readFile(startedFile, "utf8"), "yes");
    afterSpawn.abort();
    const uncertainAbort = { error: { code: "uncertain", message: "Coordinate request cancelled after submission; inspect list/doctor before retrying.", name: "job", uncertain: true } };
    assert.deepEqual(await submitted, { content: [{ type: "text", text: JSON.stringify(uncertainAbort) }], details: uncertainAbort, structuredContent: uncertainAbort, isError: true });

    await writeFile(executable, `#!/bin/sh\nprintf '%s' '${JSON.stringify({ error: { code: "uncertain", message: "inspect", id: "known-id", name: "job", uncertain: true } })}'\nexit 75\n`);
    const uncertain = await Reflect.apply(tool.execute, undefined, ["call", { request: { action: "start", name: "job", cwd: ".", task } }, undefined, undefined, { cwd: directory }]);
    assert.deepEqual(uncertain, {
      content: [{ type: "text", text: JSON.stringify({ error: { code: "uncertain", message: "Launch may have been submitted; inspect list/doctor before retrying.", id: "known-id", name: "job", uncertain: true } }) }],
      details: { error: { code: "uncertain", message: "Launch may have been submitted; inspect list/doctor before retrying.", id: "known-id", name: "job", uncertain: true } },
      structuredContent: { error: { code: "uncertain", message: "Launch may have been submitted; inspect list/doctor before retrying.", id: "known-id", name: "job", uncertain: true } },
      isError: true,
    });

    await rm(executable);
    process.env.PATH = directory;
    const missing = await Reflect.apply(tool.execute, undefined, ["call", { request: { action: "start", name: "job", cwd: ".", task } }, undefined, undefined, { cwd: directory }]);
    assert.deepEqual(missing, {
      content: [{ type: "text", text: JSON.stringify({ error: { code: "coordinator_error", message: "coordinate was not found on PATH; install the Coordinate CLI first.", name: "job", uncertain: false } }) }],
      details: { error: { code: "coordinator_error", message: "coordinate was not found on PATH; install the Coordinate CLI first.", name: "job", uncertain: false } },
      structuredContent: { error: { code: "coordinator_error", message: "coordinate was not found on PATH; install the Coordinate CLI first.", name: "job", uncertain: false } },
      isError: true,
    });
  } finally {
    if (previousPath === undefined) delete process.env.PATH;
    else process.env.PATH = previousPath;
    if (previousCapture === undefined) delete process.env.CAPTURE;
    else process.env.CAPTURE = previousCapture;
    if (previousStarted === undefined) delete process.env.STARTED;
    else process.env.STARTED = previousStarted;
    await rm(directory, { recursive: true, force: true });
  }
});

test("runner reports an early CLI stdin close as a spawned failure", async () => {
  const directory = await mkdtemp(join(tmpdir(), "coordinator-stdin-"));
  const executable = join(directory, "fake-cli");
  try {
    await writeFile(executable, "#!/bin/sh\nexit 0\n");
    await chmod(executable, 0o700);
    await assert.rejects(runCli(executable, [], directory, "x".repeat(1 << 20)), (error) => error instanceof Error && error.message.includes("closed stdin") && "spawned" in error && error.spawned === true);
  } finally {
    await rm(directory, { recursive: true, force: true });
  }
});

test("abort does not wait for a descendant holding inherited output pipes", async () => {
  const directory = await mkdtemp(join(tmpdir(), "coordinator-pipe-"));
  const executable = join(directory, "fake-cli");
  const pidFile = join(directory, "descendant.pid");
  const cliPidFile = join(directory, "cli.pid");
  const previousPidFile = process.env.DESCENDANT_PID_FILE;
  const previousCliPidFile = process.env.CLI_PID_FILE;
  let deadlineTimer: NodeJS.Timeout | undefined;
  try {
    await writeFile(executable, "#!/bin/sh\nprintf '%s' $$ > \"$CLI_PID_FILE\"\nnode -e 'setInterval(() => {}, 50)' &\nprintf '%s' $! > \"$DESCENDANT_PID_FILE\"\nwait\n");
    await chmod(executable, 0o700);
    process.env.DESCENDANT_PID_FILE = pidFile;
    process.env.CLI_PID_FILE = cliPidFile;
    const controller = new AbortController();
    const running = runCli(executable, [], directory, undefined, controller.signal);
    const deadline = new Promise<never>((_resolvePromise, rejectPromise) => { deadlineTimer = setTimeout(() => rejectPromise(new Error("runner did not close inherited pipes")), 1000); });
    const boundedRun = Promise.race([running, deadline]);
    const cancellation = assert.rejects(boundedRun, /cancelled after submission/);
    let descendantPid = 0;
    for (let attempt = 0; attempt < 100 && descendantPid === 0; attempt += 1) {
      try { descendantPid = Number(await readFile(pidFile, "utf8")); }
      catch { await new Promise((resolvePromise) => setTimeout(resolvePromise, 10)); }
    }
    assert.ok(descendantPid > 0, "fixture descendant should start before cancellation");
    controller.abort();
    await new Promise((resolvePromise) => setTimeout(resolvePromise, 150));
    process.kill(descendantPid, 0);
    const cliPid = Number(await readFile(cliPidFile, "utf8"));
    assert.throws(() => process.kill(cliPid, 0), { code: "ESRCH" });
    await cancellation;
  } finally {
    clearTimeout(deadlineTimer);
    try {
      const descendantPid = Number(await readFile(pidFile, "utf8"));
      if (Number.isInteger(descendantPid) && descendantPid > 0) process.kill(descendantPid, "SIGTERM");
    } catch { /* The test child did not start or already exited. */ }
    if (previousPidFile === undefined) delete process.env.DESCENDANT_PID_FILE;
    else process.env.DESCENDANT_PID_FILE = previousPidFile;
    if (previousCliPidFile === undefined) delete process.env.CLI_PID_FILE;
    else process.env.CLI_PID_FILE = previousCliPidFile;
    await rm(directory, { recursive: true, force: true });
  }
});

test("runner bounds output, aborts and times out only its CLI child", async () => {
  const directory = await mkdtemp(join(tmpdir(), "coordinator-tool-"));
  const executable = join(directory, "fake-cli");
  try {
    await writeFile(executable, "#!/bin/sh\nprintf ok\n");
    await chmod(executable, 0o700);
    const output = await runCli(executable, [], directory, undefined);
    assert.equal(output.code, 0);
    assert.equal(output.stdout, "ok");

    await writeFile(executable, "#!/bin/sh\nhead -c 1048577 /dev/zero\n");
    await assert.rejects(runCli(executable, [], directory, undefined), /exceeded 1 MiB/);

    await writeFile(executable, "#!/bin/sh\ntrap 'exit 0' TERM\nwhile :; do sleep 1; done\n");
    const controller = new AbortController();
    const running = runCli(executable, [], directory, undefined, controller.signal);
    setTimeout(() => controller.abort(), 40);
    await assert.rejects(running, /cancelled after submission/);
    await assert.rejects(runCli(executable, [], directory, undefined, undefined, 40), /timed out after submission/);
    controller.abort();
    await assert.rejects(runCli(executable, [], directory, undefined, controller.signal), (error) => error instanceof Error && "spawned" in error && error.spawned === false);

    const afterSpawn = new AbortController();
    const afterSpawnRun = runCli(executable, [], directory, undefined, afterSpawn.signal);
    setTimeout(() => afterSpawn.abort(), 40);
    await assert.rejects(afterSpawnRun, (error) => error instanceof Error && "spawned" in error && error.spawned === true);
  } finally {
    await rm(directory, { recursive: true, force: true });
  }
});
