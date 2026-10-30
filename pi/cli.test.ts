import assert from "node:assert/strict";
import { execFile as execFileCallback } from "node:child_process";
import { chmod, lstat, mkdir, mkdtemp, readFile, readdir, realpath, rm, writeFile } from "node:fs/promises";
import { delimiter, dirname, join, resolve } from "node:path";
import { promisify } from "node:util";
import { fileURLToPath } from "node:url";
import { tmpdir } from "node:os";
import test from "node:test";
import { DefaultResourceLoader, SettingsManager } from "@earendil-works/pi-coding-agent";

const execFile = promisify(execFileCallback);
const repositoryRoot = resolve(dirname(fileURLToPath(import.meta.url)), "..");
const taskHeader = "Initial task supplied by agent-coordinator:\n\n";
const jobIdPattern = /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;

interface JobFacts {
  id: string;
  name: string;
  launch: string;
  terminal: string;
  pi_readiness: string;
  task_outcome: string;
  clients: number;
  retired: boolean;
  next_action: string;
}

interface PiCapture {
  args: string[];
  body: string;
  cwd: string;
  env: Record<string, string>;
  task_path: string;
}

async function cliOutput(executable: string, env: NodeJS.ProcessEnv, ...args: string[]): Promise<string> {
  const result = await execFile(executable, args, { cwd: env.TEST_CALLER_CWD, env, timeout: 10_000, maxBuffer: 1 << 20 });
  return result.stdout;
}

async function cliJson(executable: string, env: NodeJS.ProcessEnv, ...args: string[]): Promise<unknown> {
  return parseJson(await cliOutput(executable, env, ...args));
}

function isMissingPath(error: unknown): boolean {
  return error instanceof Error && "code" in error && error.code === "ENOENT";
}

async function readSocketPath(state: string, id: string): Promise<string> {
  const manifest = record(parseJson(await readFile(join(state, "jobs", id, "manifest.json"), "utf8")));
  return stringField(manifest, "socket");
}

async function assertSocketAbsent(path: string): Promise<void> {
  try {
    await lstat(path);
  } catch (error) {
    if (isMissingPath(error)) return;
    throw error;
  }
  assert.fail("Forget left the task-owned tmux socket present");
}

async function readOwnedJobEntries(state: string): Promise<string[]> {
  try {
    return await readdir(join(state, "jobs"));
  } catch (error) {
    if (isMissingPath(error)) return [];
    throw error;
  }
}

function parseJson(text: string): unknown {
  const parsed: unknown = JSON.parse(text);
  return parsed;
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

function record(value: unknown): Record<string, unknown> {
  assert.ok(isRecord(value));
  return value;
}

function stringField(source: Record<string, unknown>, key: string): string {
  const value = source[key];
  assert.ok(typeof value === "string", `field ${key} should be text`);
  return value;
}

function jobFacts(value: unknown): JobFacts {
  const facts = record(value);
  const id = stringField(facts, "id");
  assert.match(id, jobIdPattern);
  const clients = facts.clients;
  const retired = facts.retired;
  assert.ok(typeof clients === "number");
  assert.ok(typeof retired === "boolean");
  return {
    id,
    name: stringField(facts, "name"),
    launch: stringField(facts, "launch"),
    terminal: stringField(facts, "terminal"),
    pi_readiness: stringField(facts, "pi_readiness"),
    task_outcome: stringField(facts, "task_outcome"),
    clients,
    retired,
    next_action: stringField(facts, "next_action"),
  };
}

async function waitForCapture(path: string): Promise<PiCapture> {
  const deadline = Date.now() + 5_000;
  while (Date.now() < deadline) {
    let content: string;
    try {
      content = await readFile(path, "utf8");
    } catch (error) {
      if (!(error instanceof Error) || !("code" in error) || error.code !== "ENOENT") throw error;
      await new Promise((resolvePromise) => setTimeout(resolvePromise, 25));
      continue;
    }
    return piCapture(parseJson(content));
  }
  assert.fail("fake Pi did not write its private capture");
}

function piCapture(value: unknown): PiCapture {
  const capture = record(value);
  const rawArgs = capture.args;
  const environment = record(capture.env);
  assert.ok(Array.isArray(rawArgs));
  const args = rawArgs.map((argument: unknown) => {
    assert.ok(typeof argument === "string");
    return argument;
  });
  const env: Record<string, string> = {};
  for (const [key, entry] of Object.entries(environment)) {
    assert.ok(typeof entry === "string");
    env[key] = entry;
  }
  return {
    args,
    body: stringField(capture, "body"),
    cwd: stringField(capture, "cwd"),
    env,
    task_path: stringField(capture, "task_path"),
  };
}

function toolResult(value: unknown): { isError?: boolean; structuredContent: unknown } {
  const result = record(value);
  return { isError: typeof result.isError === "boolean" ? result.isError : undefined, structuredContent: result.structuredContent };
}

function jobList(value: unknown): JobFacts[] {
  const jobs = record(value).jobs;
  assert.ok(Array.isArray(jobs));
  return jobs.map(jobFacts);
}

test("Pi package loader registers coordinator and real CLI actions run fake Pi from caller cwd", { timeout: 110_000 }, async () => {
  const root = await mkdtemp(join(tmpdir(), "coordinator-pi-loader-"));
  await chmod(root, 0o700);
  const bin = join(root, "bin");
  const agentDir = join(root, "pi-agent");
  const caller = join(root, "caller-project");
  const requestedCwd = join(caller, "work space");
  const state = join(root, "state");
  const capturePath = join(root, "capture.json");
  const releasePath = join(root, "release");
  const spawnMarker = join(root, "coordinator-spawned");
  const cliBinary = join(bin, "coordinate-real");
  const fakePiBinary = join(bin, "pi");
  const cliPath = join(bin, "coordinate");
  let jobId: string | undefined;
  let jobName = "loader-acceptance";
  let socketPath: string | undefined;
  let manifest: string | undefined;
  let cleaned = false;
  let cleanupError: unknown;
  const previous = new Map<string, string | undefined>();
  const setEnv = (key: string, value: string) => {
    if (!previous.has(key)) previous.set(key, process.env[key]);
    process.env[key] = value;
  };

  try {
    await Promise.all([bin, agentDir, caller, requestedCwd, state].map((path) => mkdir(path, { recursive: true, mode: 0o700 })));
    await Promise.all([
      execFile("go", ["build", "-o", cliBinary, "./cmd/coordinate"], { cwd: repositoryRoot, timeout: 60_000, maxBuffer: 8 << 20 }),
      execFile("go", ["build", "-o", fakePiBinary, "./internal/testpi"], { cwd: repositoryRoot, timeout: 60_000, maxBuffer: 8 << 20 }),
    ]);
    await chmod(cliBinary, 0o700);
    await chmod(fakePiBinary, 0o700);
    await writeFile(cliPath, `#!/bin/sh\nprintf started >> "$TEST_CLI_SPAWN_MARKER"\nexec "${cliBinary}" "$@"\n`, { mode: 0o700 });
    await chmod(cliPath, 0o700);

    const systemPath = process.env.PATH ?? "/usr/bin:/bin";
    const settingsManager = SettingsManager.inMemory({ packages: [repositoryRoot], enableInstallTelemetry: false });
    const resourceLoader = new DefaultResourceLoader({
      cwd: caller,
      agentDir,
      settingsManager,
      noSkills: true,
      noPromptTemplates: true,
      noThemes: true,
      noContextFiles: true,
    });
    setEnv("PATH", `${bin}${delimiter}${systemPath}`);
    setEnv("TEST_CLI_SPAWN_MARKER", spawnMarker);
    await resourceLoader.reload();

    const loaded = resourceLoader.getExtensions();
    assert.deepEqual(loaded.errors, [], "the actual Pi package loader should load the repository package cleanly");
    assert.deepEqual(loaded.warnings, [], "host-provided Pi dependencies should not be bundled as runtime dependencies");
    const extension = loaded.extensions.find(({ resolvedPath }) => resolvedPath === join(repositoryRoot, "pi", "index.ts"));
    assert.ok(extension, "package.json pi.extensions entry should resolve through Pi's package loader");
    const tool = extension.tools.get("coordinate")?.definition;
    assert.ok(tool, "the loaded extension should register its coordinator tool");
    assert.equal(await lstat(spawnMarker).then(() => true, () => false), false, "loading the package must not spawn the CLI");

    const task = "\uFEFFliteral malicious task\n世界 @mention -option $(touch SHOULD_NOT_EXIST) </file>\n";
    const env: NodeJS.ProcessEnv = {
      ...process.env,
      PI_CODING_AGENT_DIR: agentDir,
      PATH: `${bin}${delimiter}${systemPath}`,
      AC_STATE_DIR: state,
      TEST_CALLER_CWD: caller,
      TEST_CLI_SPAWN_MARKER: spawnMarker,
      TESTPI_CAPTURE: capturePath,
      TESTPI_RELEASE: releasePath,
      TESTPI_STOP_AFTER_CAPTURE: "0",
      TESTPI_EXIT: "",
      TERM: "xterm-256color",
    };
    for (const key of ["PI_CODING_AGENT_DIR", "AC_STATE_DIR", "TESTPI_CAPTURE", "TESTPI_RELEASE", "TESTPI_STOP_AFTER_CAPTURE", "TESTPI_EXIT", "TERM"]) {
      const value = env[key];
      assert.ok(value !== undefined);
      setEnv(key, value);
    }
    const context = { cwd: caller };
    const invoke = (request: unknown): Promise<unknown> => Reflect.apply(tool.execute, undefined, ["call", { request }, undefined, undefined, context]);

    const startedResult = toolResult(await invoke({ action: "start", name: jobName, cwd: "work space", task }));
    const startContent = startedResult.structuredContent;
    const startRecord = record(startContent);
    if (typeof startRecord.id === "string" && jobIdPattern.test(startRecord.id)) jobId = startRecord.id;
    else if (typeof startRecord.error === "object" && startRecord.error !== null && !Array.isArray(startRecord.error)) {
      const errorId = record(startRecord.error).id;
      if (typeof errorId === "string" && jobIdPattern.test(errorId)) jobId = errorId;
    }
    if (jobId) {
      manifest = await readFile(join(state, "jobs", jobId, "manifest.json"), "utf8");
      socketPath = stringField(record(parseJson(manifest)), "socket");
    }
    assert.equal(startedResult.isError, undefined, JSON.stringify(startContent));
    const started = jobFacts(startContent);
    assert.equal(started.name, jobName);
    jobId = started.id;
    assert.equal(started.launch, "submitted");
    assert.equal(started.terminal, "live");
    assert.equal(started.pi_readiness, "unverified");
    assert.equal(started.task_outcome, "unknown");

    assert.ok(manifest !== undefined);
    const launch = record(parseJson(await readFile(join(state, "jobs", jobId, "launch.json"), "utf8")));
    assert.ok(launch.server && launch.pane, "launch record should contain process identity for owned-resource cleanup");
    const capture = await waitForCapture(capturePath);
    assert.ok(capture.body === taskHeader + task, "fake Pi received altered task text");
    assert.equal(capture.cwd, await realpath(requestedCwd));
    assert.equal(capture.env.AC_STATE_DIR, await realpath(state));
    assert.equal(capture.env.AC_JOB, jobId);
    assert.ok(capture.args.includes("--session-id"));
    assert.ok(capture.args.includes(`@${capture.task_path}`));
    assert.equal(capture.args.some((argument) => argument.includes(task)), false, "task text must not appear in Pi argv");
    assert.equal(await lstat(join(requestedCwd, "SHOULD_NOT_EXIST")).then(() => true, () => false), false);
    assert.equal(manifest.includes(task), false, "task text must not be copied into the routine manifest");

    const listedResult = toolResult(await invoke({ action: "list" }));
    assert.equal(listedResult.isError, undefined);
    const listed = jobList(listedResult.structuredContent);
    assert.ok(listed.some(({ id, launch: phase }) => id === jobId && phase === "submitted"));
    const doctorResult = toolResult(await invoke({ action: "doctor", name: jobName }));
    assert.equal(doctorResult.isError, undefined);
    const doctor = jobFacts(doctorResult.structuredContent);
    assert.equal(doctor.id, jobId);
    assert.equal(doctor.launch, "submitted");
    assert.equal(doctor.terminal, "live");
    assert.ok((await readFile(spawnMarker, "utf8")).split("started").length - 1 >= 3, "start, list and doctor should each invoke the CLI");

    await writeFile(releasePath, "", { mode: 0o600 });
    let ended: JobFacts | undefined;
    const deadline = Date.now() + 8_000;
    while (Date.now() < deadline) {
      try {
        ended = jobFacts(await cliJson(cliPath, env, "doctor", jobName, "--json"));
        if (ended.launch === "exited") break;
      } catch { /* The release-to-exit transition is asynchronous. */ }
      await new Promise((resolvePromise) => setTimeout(resolvePromise, 50));
    }
    assert.equal(ended?.launch, "exited", "fresh doctor evidence must establish that the fake Pi pane ended");
    assert.equal(ended?.terminal, "exited");
    await cliOutput(cliPath, env, "forget", "--id", jobId);
    assert.ok(socketPath !== undefined);
    await assertSocketAbsent(socketPath);
    cleaned = true;
    const retired = jobList(await cliJson(cliPath, env, "list", "--json"));
    assert.ok(retired.some(({ id, retired: isRetired }) => id === jobId && isRetired));
  } finally {
    if (!jobId && !cleaned) {
      try {
        const env: NodeJS.ProcessEnv = { ...process.env, PATH: `${bin}${delimiter}${process.env.PATH ?? "/usr/bin:/bin"}`, AC_STATE_DIR: state, TEST_CALLER_CWD: caller };
        const current = jobList(await cliJson(cliPath, env, "list", "--json")).find(({ name }) => name === jobName);
        if (current) {
          jobId = current.id;
          jobName = current.name;
          socketPath = await readSocketPath(state, jobId);
        }
      } catch (error) {
        if (jobId) cleanupError = error;
      }
      if (!jobId && !cleanupError) {
        try {
          const entries = await readOwnedJobEntries(state);
          if (entries.length === 0) cleaned = true;
          else cleanupError = new Error("published job records exist but no owned job identity was established");
        } catch (error) {
          cleanupError = error;
        }
      }
    }
    if (jobId && !socketPath && !cleanupError) {
      try {
        socketPath = await readSocketPath(state, jobId);
      } catch (error) {
        cleanupError = error;
      }
    }
    if (jobId && !cleaned && !cleanupError) {
      try {
        await writeFile(releasePath, "", { mode: 0o600 });
        const env: NodeJS.ProcessEnv = {
          ...process.env,
          PATH: `${bin}${delimiter}${process.env.PATH ?? "/usr/bin:/bin"}`,
          PI_CODING_AGENT_DIR: agentDir,
          AC_STATE_DIR: state,
          TEST_CALLER_CWD: caller,
          TEST_CLI_SPAWN_MARKER: spawnMarker,
          TESTPI_CAPTURE: capturePath,
          TESTPI_RELEASE: releasePath,
          TESTPI_STOP_AFTER_CAPTURE: "0",
          TESTPI_EXIT: "",
          TERM: "xterm-256color",
        };
        let facts: JobFacts | undefined;
        const deadline = Date.now() + 8_000;
        while (Date.now() < deadline) {
          facts = undefined;
          try {
            const current = jobList(await cliJson(cliPath, env, "list", "--json")).find(({ id }) => id === jobId);
            if (!current) break;
            jobName = current.name;
            facts = jobFacts(await cliJson(cliPath, env, "doctor", jobName, "--json"));
            if (facts.launch === "exited") break;
          } catch {
            break;
          }
          await new Promise((resolvePromise) => setTimeout(resolvePromise, 50));
        }
        if (facts) {
          await cliOutput(cliPath, env, "forget", "--id", jobId);
          assert.ok(socketPath !== undefined);
          await assertSocketAbsent(socketPath);
          cleaned = true;
        } else {
          throw new Error(`cannot inspect owned job ${jobId}; preserve temp state at ${root}`);
        }
      } catch (error) {
        cleanupError = error;
      }
    }
    for (const [key, value] of previous) {
      if (value === undefined) delete process.env[key];
      else process.env[key] = value;
    }
    if (cleanupError) {
      assert.fail(`owned runtime cleanup failed; preserve ${root}: ${String(cleanupError)}`);
    }
    await rm(root, { recursive: true, force: true });
  }
});
