import assert from "node:assert/strict";
import { mkdtemp, rm, writeFile, chmod, readFile } from "node:fs/promises";
import { delimiter, join, resolve } from "node:path";
import { tmpdir } from "node:os";
import test from "node:test";
import { Value } from "typebox/value";
import { cliArguments, coordinatorTool, parseResult, registerCoordinator, runCli } from "./index.ts";

function register() {
  let registrations = 0;
  let registeredCoordinator = false;
  registerCoordinator({ registerTool(tool) { registrations += 1; registeredCoordinator = tool === coordinatorTool; } });
  assert.ok(registeredCoordinator, "registerCoordinator should pass the coordinator tool to the host");
  return { tool: coordinatorTool, registrations };
}

test("extension registers one strict coordinator request tool without spawning", () => {
  const { tool, registrations } = register();
  assert.equal(registrations, 1);
  assert.equal(tool?.name, "coordinator");
  assert.ok(tool);
  assert.equal(Value.Check(tool.parameters, { request: { action: "list" } }), true);
  assert.equal(Value.Check(tool.parameters, { request: { action: "list", extra: true } }), false);
  assert.equal(Value.Check(tool.parameters, { action: "list" }), false);
  assert.equal(Value.Check(tool.parameters, { request: { action: "run" } }), false);
});

test("fixed action vectors keep malicious task text exclusively on stdin", () => {
  const task = "\uFEFFUnicode @mention\n</file>; touch /tmp/should-not-run";
  assert.deepEqual(cliArguments({ action: "start", name: "job-a", cwd: "work dir", task }, "/caller/project"), {
    argv: ["start", "job-a", "--cwd", resolve("/caller/project", "work dir"), "--task-file", "-", "--json"],
    cwd: "/caller/project", input: task,
  });
  assert.deepEqual(cliArguments({ action: "list" }, "/caller"), { argv: ["list", "--json"], cwd: "/caller" });
  assert.deepEqual(cliArguments({ action: "doctor", name: "job-a" }, "/caller"), { argv: ["doctor", "job-a", "--json"], cwd: "/caller" });
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
  const executable = join(directory, "agent-coordinator");
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
      content: [{ type: "text", text: JSON.stringify({ error: { code: "coordinator_error", message: "Coordinator request was cancelled before launch.", name: "job", uncertain: false } }) }],
      details: { error: { code: "coordinator_error", message: "Coordinator request was cancelled before launch.", name: "job", uncertain: false } },
      structuredContent: { error: { code: "coordinator_error", message: "Coordinator request was cancelled before launch.", name: "job", uncertain: false } },
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
    const uncertainAbort = { error: { code: "uncertain", message: "Coordinator request cancelled after submission; inspect list/doctor before retrying.", name: "job", uncertain: true } };
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
      content: [{ type: "text", text: JSON.stringify({ error: { code: "coordinator_error", message: "agent-coordinator was not found on PATH; install the coordinator CLI first.", name: "job", uncertain: false } }) }],
      details: { error: { code: "coordinator_error", message: "agent-coordinator was not found on PATH; install the coordinator CLI first.", name: "job", uncertain: false } },
      structuredContent: { error: { code: "coordinator_error", message: "agent-coordinator was not found on PATH; install the coordinator CLI first.", name: "job", uncertain: false } },
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
