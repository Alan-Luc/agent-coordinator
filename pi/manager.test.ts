import assert from "node:assert/strict";
import { randomUUID } from "node:crypto";
import { fileURLToPath } from "node:url";
import { mkdtemp, realpath, rm, writeFile } from "node:fs/promises";
import { join } from "node:path";
import { tmpdir } from "node:os";
import test from "node:test";
import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";
import { Value } from "typebox/value";
import { communicationRequestSchema, communicationResultSchema, type CommunicationCaller } from "./communication-protocol.ts";
import type { SessionStatus } from "./control-protocol.ts";
import { registerManager } from "./manager.ts";

const sessionId = randomUUID();
const sessionFile = fileURLToPath(import.meta.url);
const managerId = randomUUID();
const jobId = randomUUID();
const managerHostJobId = randomUUID();
const jobFacts = { id: jobId, name: "worker", launch: "running", terminal: "attached", pi_readiness: "unverified", task_outcome: "unknown", clients: 1, retired: false, next_action: "inspect" };
const manager = { id: managerId, session_file: sessionFile, session_id: sessionId, enabled: true };
const processIdentity = { boot_id: "3e322019-1073-4f52-afd0-abed9a17ce70", pid: process.pid, uid: process.getuid?.() ?? 501, start_sec: 10, start_usec: 0 };

type Listener = (...args: unknown[]) => unknown;
interface TestRequest { action: string; caller?: CommunicationCaller; job_id?: string; task?: string; cursor?: number; message_ids?: string[]; }
function callerGeneration(request: TestRequest): string { assert.ok(request.caller); return request.caller.generation; }
function requestedJobId(request: TestRequest): string { assert.ok(request.job_id); return request.job_id; }
function harness(response: (request: TestRequest) => unknown, mode = "rpc", status?: () => SessionStatus | undefined, hostResponse?: { code: number; stdout: string }, requirePublication = false) {
  const events = new Map<string, Listener>();
  const commands = new Map<string, { handler: Listener }>();
  const requests: TestRequest[] = [];
  const messages: Parameters<ExtensionAPI["sendMessage"]>[] = [];
  const notifications: string[] = [];
  const widgets: string[][] = [];
  const confirmations: string[] = [];
  const currentStatus = status ?? (() => undefined);
  const protocol2Status = currentStatus();
  const workerTarget = { id: jobId, session_id: randomUUID(), generation: randomUUID(), pane: { ...processIdentity, pid: 123 } };
  const target = protocol2Status ? { id: protocol2Status.job_id, session_id: protocol2Status.session_id, generation: protocol2Status.generation, pane: { ...processIdentity, pid: protocol2Status.pid } } : workerTarget;
  const hostRequests: unknown[] = [];
  const endpointCalls: string[] = [];
  let publications = 0;
  let currentSessionId: string = sessionId;
  let currentSessionFile: string | undefined = sessionFile;
  let confirmation: boolean | (() => boolean) = true;
  let reverseTargetFields = false;
  const pi = {
    on: (event: string, fn: Listener) => { events.set(event, fn); return () => events.delete(event); },
    registerCommand: (name: string, value: { handler: Listener }) => { commands.set(name, value); },
    sendMessage: (...args: Parameters<ExtensionAPI["sendMessage"]>) => { messages.push(args); },
  };
  const cli = {
    findExecutable: async () => "/bin/coordinate",
    runCli: async (_exe: string, argv: string[], _cwd: string, input?: string) => {
      if (argv[0] === "_target") { if (requirePublication) assert.ok(publications > 0, "target lookup must await current native observation publication"); endpointCalls.push("_target"); const selected = argv[1] === target.id ? target : workerTarget; const wireTarget = reverseTargetFields ? { pane: { start_usec: selected.pane.start_usec, start_sec: selected.pane.start_sec, uid: selected.pane.uid, pid: selected.pane.pid, boot_id: selected.pane.boot_id }, generation: selected.generation, session_id: selected.session_id, id: selected.id } : selected; return { code: 0, stdout: JSON.stringify(JSON.parse(JSON.stringify(wireTarget)) as unknown) }; }
      if (argv[0] === "_features") { endpointCalls.push("_features"); return { code: 0, stdout: JSON.stringify({ features: { interactive_launch: 1, terminal_profile: "transparent-v1", manager_host: 1, pending_manager: 1 } }) }; }
      if (argv[0] === "_manager-host") { endpointCalls.push("_manager-host"); hostRequests.push(JSON.parse(input ?? "null") as unknown); return hostResponse ?? { code: 0, stdout: JSON.stringify({ role: { schema: 1, role: "manager-host" } }) }; }
      assert.deepEqual(argv, ["_communication", "--json"]);
      const parsed: unknown = JSON.parse(input ?? "null");
      assert.ok(Value.Check(communicationRequestSchema, parsed));
      const request: TestRequest = parsed;
      requests.push(request);
      const result = response(request);
      assert.ok(typeof result === "object" && result !== null);
      return { code: "error" in result ? 1 : 0, stdout: JSON.stringify(result) };
    },
  };
  const controller = { status: currentStatus, publishStatus: async () => { await new Promise<void>((resolveRefresh) => setImmediate(resolveRefresh)); publications++; return currentStatus(); }, report: async () => undefined };
  const adapter: ReturnType<typeof registerManager> = Reflect.apply(registerManager, undefined, [pi, controller, cli]);
  const context = {
    cwd: "/tmp",
    mode,
    hasUI: true,
    sessionManager: { getSessionId: () => currentSessionId, getSessionFile: () => currentSessionFile },
    ui: { confirm: async (_title: string, message: string) => { confirmations.push(message); return typeof confirmation === "function" ? confirmation() : confirmation; }, notify: (message: string) => { notifications.push(message); }, setStatus: () => {}, setWidget: (_key: string, lines: string[] | undefined) => { widgets.push(lines ?? []); } },
  };
  const calls = {
    start: (request: Parameters<typeof adapter.start>[0], signal: AbortSignal | undefined, ctx: typeof context): ReturnType<typeof adapter.start> => Reflect.apply(adapter.start, adapter, [request, signal, ctx]),
    execute: (request: Parameters<typeof adapter.execute>[0], signal: AbortSignal | undefined, ctx: typeof context): ReturnType<typeof adapter.execute> => Reflect.apply(adapter.execute, adapter, [request, signal, ctx]),
  };
  return { adapter: calls, context, events, commands, requests, messages, notifications, widgets, confirmations, target, hostRequests, endpointCalls, setConfirmation: (value: boolean | (() => boolean)) => { confirmation = value; }, reverseTargetFields: () => { reverseTargetFields = true; }, changeSession: (value: string) => { currentSessionId = value; }, setSessionFile: (value: string | undefined) => { currentSessionFile = value; } };
}

test("public slash command registers only coordinate and advertises its usage", async () => {
  const h = harness(() => ({}));
  assert.deepEqual([...h.commands.keys()], ["coordinate"]);
  await h.commands.get("coordinate")?.handler("invalid", h.context);
  assert.match(h.notifications[0], /Usage: \/coordinate /);
});

test("uninitialized session start is definitively unmanaged", async () => {
  const h = harness(() => ({ error: { code: "not_enrolled", message: "not enrolled", uncertain: false } }));
  assert.deepEqual(await h.adapter.start({ name: "worker", cwd: ".", task: "task" }, undefined, h.context), { managed: false });
});

test("a newly persisted conversation can start after unpersisted startup", async () => {
  const h = harness(request => request.action === "capability" ? { capability: { protocol: 1 } } : { error: { code: "manager_unknown", message: "not enrolled", uncertain: false } });
  h.setSessionFile(undefined);
  await h.events.get("session_start")?.({}, h.context);
  h.setSessionFile(sessionFile);
  assert.deepEqual(await h.adapter.start({ name: "worker", cwd: ".", task: "Inspect only" }, undefined, h.context), { managed: false });
  await h.events.get("session_shutdown")?.();
});

test("protocol2 manage-on reserves the exact host and enrolls only after genuine persistence", async () => {
  const directory = await mkdtemp(join(tmpdir(), "manager-pending-"));
  const nativeFile = join(directory, "native.jsonl");
  const generation = randomUUID();
  const status: SessionStatus = { protocol: 1, launch_protocol: "2", job_id: managerHostJobId, owner: "owner", session_id: sessionId, generation, pid: process.pid, updated_at: Date.now(), activity: "idle" };
  const h = harness(request => {
    if (request.action === "capability") return { capability: { protocol: 1 } };
    if (request.action === "enroll") return { manager: { id: managerId, session_file: request.caller?.session_file, session_id: request.caller?.session_id, enabled: true, lease: { generation: callerGeneration(request), process: processIdentity } } };
    if (request.action === "adopt") return { binding: { job_id: requestedJobId(request), manager_id: managerId } };
    return { error: { code: "unexpected", message: "unexpected", uncertain: false } };
  }, "tui", () => status, undefined, true);
  h.setSessionFile(nativeFile);
  try {
    await h.events.get("session_start")?.({}, h.context);
    h.reverseTargetFields();
    const pending = await h.adapter.execute({ action: "manage", enabled: true }, undefined, h.context);
    assert.deepEqual(pending, { awaiting_conversation: { job_id: managerHostJobId } });
    assert.deepEqual(h.endpointCalls.slice(0, 3), ["_target", "_features", "_target"]);
    assert.equal(h.endpointCalls.filter(action => action === "_manager-host").length, 1);
    assert.deepEqual(h.hostRequests, [{ target: h.target }]);
    const beforePersistence = await h.adapter.execute({ action: "adopt", id: jobId }, undefined, h.context);
    assert.deepEqual(beforePersistence, { awaiting_conversation: { job_id: managerHostJobId } });
    assert.equal(h.requests.some(request => request.action === "adopt"), false);

    await writeFile(nativeFile, `${JSON.stringify({ type: "session", version: 3, id: sessionId })}\n`);
    const adopted = await h.adapter.execute({ action: "adopt", id: jobId }, undefined, h.context);
    assert.deepEqual(adopted, { binding: { job_id: jobId, manager_id: managerId } });
    assert.deepEqual(h.requests.filter(request => request.action === "enroll" || request.action === "adopt").map(request => request.action), ["enroll", "adopt"]);
    assert.equal(h.requests.find(request => request.action === "enroll")?.caller?.session_file, await realpath(nativeFile));
  } finally {
    await h.events.get("session_shutdown")?.();
    await rm(directory, { recursive: true, force: true });
  }
});

test("unpersisted protocol2 startup remains unmanaged after deliberate persistence", async () => {
  const directory = await mkdtemp(join(tmpdir(), "manager-standalone-"));
  const nativeFile = join(directory, "native.jsonl");
  const status: SessionStatus = { protocol: 1, launch_protocol: "2", job_id: managerHostJobId, owner: "owner", session_id: sessionId, generation: randomUUID(), pid: process.pid, updated_at: Date.now(), activity: "idle" };
  const h = harness(request => request.action === "capability" ? { capability: { protocol: 1 } } : { error: { code: "manager_unknown", message: "not enrolled", uncertain: false } }, "tui", () => status);
  h.setSessionFile(nativeFile);
  try {
    await h.events.get("session_start")?.({}, h.context);
    await writeFile(nativeFile, `${JSON.stringify({ type: "session", version: 3, id: sessionId })}\n`);
    assert.deepEqual(await h.adapter.start({ name: "worker", cwd: ".", task: "Inspect only" }, undefined, h.context), { managed: false });
    assert.deepEqual(h.notifications, [], "a never-enrolled conversation must not warn about failed reconnect");
    assert.equal(h.requests.some(request => request.action === "enroll"), false);
  } finally {
    await h.events.get("session_shutdown")?.();
    await rm(directory, { recursive: true, force: true });
  }
});

test("slash manage-on uses the protocol2 consent path", async () => {
  const directory = await mkdtemp(join(tmpdir(), "manager-command-pending-"));
  const status: SessionStatus = { protocol: 1, launch_protocol: "2", job_id: managerHostJobId, owner: "owner", session_id: sessionId, generation: randomUUID(), pid: process.pid, updated_at: Date.now(), activity: "idle" };
  const h = harness(() => ({ error: { code: "unused", message: "unused", uncertain: false } }), "tui", () => status);
  h.setSessionFile(join(directory, "native.jsonl"));
  try {
    await h.events.get("session_start")?.({}, h.context);
    await h.commands.get("coordinate")?.handler("manage on", h.context);
    assert.equal(h.endpointCalls.filter(action => action === "_manager-host").length, 1);
    assert.ok(h.notifications.some(message => message.includes("awaits a persisted conversation")));
  } finally {
    await h.events.get("session_shutdown")?.();
    await rm(directory, { recursive: true, force: true });
  }
});

test("manage-off cancels memory-only consent without clearing sticky host reservation", async () => {
  const directory = await mkdtemp(join(tmpdir(), "manager-cancel-pending-"));
  const status: SessionStatus = { protocol: 1, launch_protocol: "2", job_id: managerHostJobId, owner: "owner", session_id: sessionId, generation: randomUUID(), pid: process.pid, updated_at: Date.now(), activity: "idle" };
  const h = harness(() => ({ error: { code: "unused", message: "unused", uncertain: false } }), "tui", () => status);
  h.setSessionFile(join(directory, "native.jsonl"));
  try {
    await h.events.get("session_start")?.({}, h.context);
    assert.deepEqual(await h.adapter.execute({ action: "manage", enabled: true }, undefined, h.context), { awaiting_conversation: { job_id: managerHostJobId } });
    const cancelled = await h.adapter.execute({ action: "manage", enabled: false }, undefined, h.context);
    assert.ok("error" in cancelled && cancelled.error.code === "pending_cancelled");
    assert.equal(h.endpointCalls.filter(action => action === "_manager-host").length, 1);
    assert.equal(h.requests.some(request => request.action === "enroll" || request.action === "pause"), false);
  } finally {
    await h.events.get("session_shutdown")?.();
    await rm(directory, { recursive: true, force: true });
  }
});

test("changed protocol2 controller generation cancels pending consent before dispatch", async () => {
  const directory = await mkdtemp(join(tmpdir(), "manager-stale-pending-"));
  let status: SessionStatus = { protocol: 1, launch_protocol: "2", job_id: managerHostJobId, owner: "owner", session_id: sessionId, generation: randomUUID(), pid: process.pid, updated_at: Date.now(), activity: "idle" };
  const h = harness(() => ({ error: { code: "unused", message: "unused", uncertain: false } }), "tui", () => status);
  h.setSessionFile(join(directory, "native.jsonl"));
  try {
    await h.events.get("session_start")?.({}, h.context);
    await h.adapter.execute({ action: "manage", enabled: true }, undefined, h.context);
    status = { ...status, generation: randomUUID() };
    const result = await h.adapter.execute({ action: "adopt", id: jobId }, undefined, h.context);
    assert.ok("error" in result && result.error.code === "pending_consent_stale");
    assert.equal(h.requests.some(request => request.action === "enroll" || request.action === "adopt"), false);
  } finally {
    await h.events.get("session_shutdown")?.();
    await rm(directory, { recursive: true, force: true });
  }
});

test("same PID with changed process birth fences pending consent", async () => {
  const directory = await mkdtemp(join(tmpdir(), "manager-birth-changed-"));
  const status: SessionStatus = { protocol: 1, launch_protocol: "2", job_id: managerHostJobId, owner: "owner", session_id: sessionId, generation: randomUUID(), pid: process.pid, updated_at: Date.now(), activity: "idle" };
  const h = harness(() => ({ error: { code: "unused", message: "unused", uncertain: false } }), "tui", () => status);
  h.setSessionFile(join(directory, "native.jsonl"));
  try {
    await h.events.get("session_start")?.({}, h.context);
    await h.adapter.execute({ action: "manage", enabled: true }, undefined, h.context);
    h.target.pane.start_usec++;
    const result = await h.adapter.execute({ action: "adopt", id: jobId }, undefined, h.context);
    assert.ok("error" in result && result.error.code === "pending_consent_stale");
    assert.equal(h.requests.some(request => request.action === "enroll" || request.action === "adopt"), false);
  } finally {
    await h.events.get("session_shutdown")?.();
    await rm(directory, { recursive: true, force: true });
  }
});

test("mismatched persisted session header remains fenced after the header is restored", async () => {
  const directory = await mkdtemp(join(tmpdir(), "manager-header-mismatch-"));
  const nativeFile = join(directory, "native.jsonl");
  const status: SessionStatus = { protocol: 1, launch_protocol: "2", job_id: managerHostJobId, owner: "owner", session_id: sessionId, generation: randomUUID(), pid: process.pid, updated_at: Date.now(), activity: "idle" };
  const h = harness(() => ({ error: { code: "unused", message: "unused", uncertain: false } }), "tui", () => status);
  h.setSessionFile(nativeFile);
  try {
    await h.events.get("session_start")?.({}, h.context);
    await h.adapter.execute({ action: "manage", enabled: true }, undefined, h.context);
    await writeFile(nativeFile, `${JSON.stringify({ type: "session", version: 3, id: randomUUID() })}\n`);
    const result = await h.adapter.execute({ action: "adopt", id: jobId }, undefined, h.context);
    assert.ok("error" in result && result.error.code === "pending_consent_stale");
    await writeFile(nativeFile, `${JSON.stringify({ type: "session", version: 3, id: sessionId })}\n`);
    const start = await h.adapter.start({ name: "worker", cwd: ".", task: "Inspect only" }, undefined, h.context);
    assert.equal(start.managed, true);
    if (start.managed) assert.notEqual(start.code, 0);
    assert.equal(h.requests.some(request => request.action === "enroll" || request.action === "adopt" || request.action === "start"), false);
  } finally {
    await h.events.get("session_shutdown")?.();
    await rm(directory, { recursive: true, force: true });
  }
});

test("changed native session id invalidates pending consent", async () => {
  const directory = await mkdtemp(join(tmpdir(), "manager-session-changed-"));
  const status: SessionStatus = { protocol: 1, launch_protocol: "2", job_id: managerHostJobId, owner: "owner", session_id: sessionId, generation: randomUUID(), pid: process.pid, updated_at: Date.now(), activity: "idle" };
  const h = harness(() => ({ error: { code: "unused", message: "unused", uncertain: false } }), "tui", () => status);
  h.setSessionFile(join(directory, "native.jsonl"));
  try {
    await h.events.get("session_start")?.({}, h.context);
    await h.adapter.execute({ action: "manage", enabled: true }, undefined, h.context);
    h.changeSession(randomUUID());
    const result = await h.adapter.execute({ action: "adopt", id: jobId }, undefined, h.context);
    assert.ok("error" in result && result.error.code === "pending_consent_stale");
    assert.equal(h.requests.some(request => request.action === "enroll" || request.action === "adopt"), false);
  } finally {
    await h.events.get("session_shutdown")?.();
    await rm(directory, { recursive: true, force: true });
  }
});

test("session replacement during confirmation refuses sticky host promotion", async () => {
  const directory = await mkdtemp(join(tmpdir(), "manager-consent-stale-"));
  let status: SessionStatus = { protocol: 1, launch_protocol: "2", job_id: managerHostJobId, owner: "owner", session_id: sessionId, generation: randomUUID(), pid: process.pid, updated_at: Date.now(), activity: "idle" };
  const h = harness(() => ({ error: { code: "unused", message: "unused", uncertain: false } }), "tui", () => status);
  h.setSessionFile(join(directory, "native.jsonl"));
  h.setConfirmation(() => { status = { ...status, generation: randomUUID() }; return true; });
  try {
    await h.events.get("session_start")?.({}, h.context);
    const result = await h.adapter.execute({ action: "manage", enabled: true }, undefined, h.context);
    assert.ok("error" in result && result.error.code === "confirmation_cancelled");
    assert.equal(h.endpointCalls.includes("_manager-host"), false);
  } finally {
    await h.events.get("session_shutdown")?.();
    await rm(directory, { recursive: true, force: true });
  }
});

test("uncertain persisted enrollment is not reissued from old consent", async () => {
  const directory = await mkdtemp(join(tmpdir(), "manager-enroll-uncertain-"));
  const nativeFile = join(directory, "native.jsonl");
  const status: SessionStatus = { protocol: 1, launch_protocol: "2", job_id: managerHostJobId, owner: "owner", session_id: sessionId, generation: randomUUID(), pid: process.pid, updated_at: Date.now(), activity: "idle" };
  const h = harness(request => {
    if (request.action === "capability") return { capability: { protocol: 1 } };
    if (request.action === "enroll") throw new Error("response lost after enrollment");
    if (request.action === "pause") return { manager: { id: managerId, session_file: request.caller?.session_file, session_id: request.caller?.session_id, enabled: false, lease: { generation: callerGeneration(request), process: processIdentity } } };
    if (request.action === "start") return { started: jobFacts };
    return { error: { code: "unused", message: "unused", uncertain: false } };
  }, "tui", () => status);
  h.setSessionFile(nativeFile);
  try {
    await h.events.get("session_start")?.({}, h.context);
    await h.adapter.execute({ action: "manage", enabled: true }, undefined, h.context);
    await writeFile(nativeFile, `${JSON.stringify({ type: "session", version: 3, id: sessionId })}\n`);
    const adopt = await h.adapter.execute({ action: "adopt", id: jobId }, undefined, h.context);
    assert.ok("error" in adopt && adopt.error.uncertain);
    const enrollmentCount = h.requests.filter(request => request.action === "enroll").length;
    const next = await h.adapter.execute({ action: "adopt", id: jobId }, undefined, h.context);
    assert.ok("error" in next && next.error.uncertain);
    const start = await h.adapter.start({ name: "worker", cwd: ".", task: "Inspect only" }, undefined, h.context);
    assert.equal(start.managed, true);
    if (start.managed) assert.equal(start.code, 75);
    assert.equal(h.requests.filter(request => request.action === "enroll").length, enrollmentCount);
    const manageOff = await h.adapter.execute({ action: "manage", enabled: false }, undefined, h.context);
    assert.ok("manager" in manageOff && manageOff.manager.enabled === false);
    assert.deepEqual(h.requests.filter(request => request.action === "pause").map(request => request.action), ["pause"]);
    assert.equal(h.requests.some(request => request.action === "adopt" || request.action === "start"), false);
  } finally {
    await h.events.get("session_shutdown")?.();
    await rm(directory, { recursive: true, force: true });
  }
});

test("confirmed manage-off clears uncertain enrollment only on definitive manager_unknown", async () => {
  const directory = await mkdtemp(join(tmpdir(), "manager-enroll-unknown-pause-"));
  const nativeFile = join(directory, "native.jsonl");
  const status: SessionStatus = { protocol: 1, launch_protocol: "2", job_id: managerHostJobId, owner: "owner", session_id: sessionId, generation: randomUUID(), pid: process.pid, updated_at: Date.now(), activity: "idle" };
  const h = harness(request => request.action === "capability" ? { capability: { protocol: 1 } }
    : request.action === "enroll" ? (() => { throw new Error("lost enrollment response"); })()
    : request.action === "pause" ? { error: { code: "manager_unknown", message: "manager is definitively unknown", uncertain: false } }
    : { error: { code: "unused", message: "unused", uncertain: false } }, "tui", () => status);
  h.setSessionFile(nativeFile);
  try {
    await h.events.get("session_start")?.({}, h.context);
    await h.adapter.execute({ action: "manage", enabled: true }, undefined, h.context);
    await writeFile(nativeFile, `${JSON.stringify({ type: "session", version: 3, id: sessionId })}\n`);
    const enrollment = await h.adapter.execute({ action: "adopt", id: jobId }, undefined, h.context);
    assert.ok("error" in enrollment && enrollment.error.uncertain);
    const paused = await h.adapter.execute({ action: "manage", enabled: false }, undefined, h.context);
    assert.ok("error" in paused && paused.error.code === "manager_unknown" && !paused.error.uncertain);
    assert.equal(h.requests.filter(request => request.action === "pause").length, 1);
    const start = await h.adapter.start({ name: "worker", cwd: ".", task: "Inspect only" }, undefined, h.context);
    assert.equal(start.managed, true);
    if (start.managed) assert.equal(start.code, 75);
    assert.equal(h.requests.filter(request => request.action === "enroll").length, 1);
  } finally {
    await h.events.get("session_shutdown")?.();
    await rm(directory, { recursive: true, force: true });
  }
});

test("uncertain pause keeps pending enrollment fenced", async () => {
  const directory = await mkdtemp(join(tmpdir(), "manager-enroll-uncertain-pause-"));
  const nativeFile = join(directory, "native.jsonl");
  const status: SessionStatus = { protocol: 1, launch_protocol: "2", job_id: managerHostJobId, owner: "owner", session_id: sessionId, generation: randomUUID(), pid: process.pid, updated_at: Date.now(), activity: "idle" };
  const h = harness(request => request.action === "capability" ? { capability: { protocol: 1 } }
    : request.action === "enroll" || request.action === "pause" ? (() => { throw new Error("response lost"); })()
    : { error: { code: "unused", message: "unused", uncertain: false } }, "tui", () => status);
  h.setSessionFile(nativeFile);
  try {
    await h.events.get("session_start")?.({}, h.context);
    await h.adapter.execute({ action: "manage", enabled: true }, undefined, h.context);
    await writeFile(nativeFile, `${JSON.stringify({ type: "session", version: 3, id: sessionId })}\n`);
    const enrollment = await h.adapter.execute({ action: "adopt", id: jobId }, undefined, h.context);
    assert.ok("error" in enrollment && enrollment.error.uncertain);
    const paused = await h.adapter.execute({ action: "manage", enabled: false }, undefined, h.context);
    assert.ok("error" in paused && paused.error.uncertain);
    const retry = await h.adapter.execute({ action: "adopt", id: jobId }, undefined, h.context);
    assert.ok("error" in retry && retry.error.uncertain);
    assert.equal(h.requests.filter(request => request.action === "enroll").length, 1);
    assert.equal(h.requests.filter(request => request.action === "pause").length, 1);
  } finally {
    await h.events.get("session_shutdown")?.();
    await rm(directory, { recursive: true, force: true });
  }
});

test("protocol2 manager consent rejects denied confirmation and unavailable UI", async () => {
  const directory = await mkdtemp(join(tmpdir(), "manager-host-failure-"));
  const status: SessionStatus = { protocol: 1, launch_protocol: "2", job_id: managerHostJobId, owner: "owner", session_id: sessionId, generation: randomUUID(), pid: process.pid, updated_at: Date.now(), activity: "idle" };
  const h = harness(() => ({ error: { code: "unused", message: "unused", uncertain: false } }), "tui", () => status);
  h.setSessionFile(join(directory, "native.jsonl"));
  h.setConfirmation(false);
  try {
    await h.events.get("session_start")?.({}, h.context);
    const denied = await h.adapter.execute({ action: "manage", enabled: true }, undefined, h.context);
    assert.ok("error" in denied && denied.error.code === "confirmation_cancelled");
    assert.equal(h.endpointCalls.includes("_manager-host"), false);
    const noUi = await h.adapter.execute({ action: "manage", enabled: true }, undefined, { ...h.context, hasUI: false });
    assert.ok("error" in noUi && noUi.error.code === "confirmation_unavailable");
  } finally {
    await h.events.get("session_shutdown")?.();
    await rm(directory, { recursive: true, force: true });
  }
});

test("uncertain manager-host promotion never enrolls", async () => {
  const directory = await mkdtemp(join(tmpdir(), "manager-host-uncertain-"));
  const status: SessionStatus = { protocol: 1, launch_protocol: "2", job_id: managerHostJobId, owner: "owner", session_id: sessionId, generation: randomUUID(), pid: process.pid, updated_at: Date.now(), activity: "idle" };
  const h = harness(() => ({ error: { code: "unused", message: "unused", uncertain: false } }), "tui", () => status, { code: 1, stdout: JSON.stringify({ error: { code: "uncertain", message: "host reservation response lost", uncertain: true } }) });
  h.setSessionFile(join(directory, "native.jsonl"));
  try {
    await h.events.get("session_start")?.({}, h.context);
    const result = await h.adapter.execute({ action: "manage", enabled: true }, undefined, h.context);
    assert.ok("error" in result && result.error.uncertain);
    assert.equal(h.requests.some(request => request.action === "enroll"), false);
    assert.deepEqual(h.endpointCalls.slice(-1), ["_manager-host"]);
  } finally {
    await h.events.get("session_shutdown")?.();
    await rm(directory, { recursive: true, force: true });
  }
});

test("manager_unknown permits public starts but uncertain reconnect does not", async () => {
  const h = harness((request) => request.action === "capability" ? { capability: { protocol: 1 } } : { error: { code: "manager_unknown", message: "not enrolled", uncertain: false } });
  await h.events.get("session_start")?.({}, h.context);
  assert.deepEqual(await h.adapter.start({ name: "worker", cwd: ".", task: "task" }, undefined, h.context), { managed: false });
  assert.deepEqual(h.requests.map((request) => request.action), ["capability", "reconnect"]);
});

test("failed reconnect refuses start instead of falling back to unassociated public start", async () => {
  const h = harness((request) => request.action === "capability" ? { capability: { protocol: 1 } } : request.action === "reconnect"
    ? { error: { code: "lease_owned", message: "lease unknown", uncertain: true } }
    : { started: jobFacts });
  await h.events.get("session_start")?.({}, h.context);
  const result = await h.adapter.start({ name: "worker", cwd: ".", task: "task" }, undefined, h.context);
  assert.equal(result.managed, true);
  if (result.managed) {
    assert.equal(result.code, 75);
    assert.match(result.stdout, /lease unknown/);
  }
  assert.deepEqual(h.requests.map((request) => request.action), ["capability", "reconnect"]);
});

test("lost enrollment response invalidates an earlier unmanaged classification", async () => {
  const h = harness((request) => {
    if (request.action === "capability") return { capability: { protocol: 1 } };
    if (request.action === "reconnect") return { error: { code: "manager_unknown", message: "not enrolled", uncertain: false } };
    if (request.action === "enroll") throw new Error("response lost after enrollment persisted");
    throw new Error(`unexpected ${request.action}`);
  }, "tui");
  await h.events.get("session_start")?.({}, h.context);
  const enrollment = await h.adapter.execute({ action: "manage", enabled: true }, undefined, h.context);
  assert.ok("error" in enrollment && enrollment.error.uncertain);
  const start = await h.adapter.start({ name: "worker", cwd: ".", task: "Inspect only" }, undefined, h.context);
  assert.equal(start.managed, true, "uncertain enrollment must not launch publicly");
  if (start.managed) assert.equal(start.code, 75);
  assert.equal(h.requests.filter(request => request.action === "start").length, 0);
  await h.events.get("session_shutdown")?.();
});

for (const enabled of [false, true]) test(`definite enrollment refusal preserves prior management state (enabled=${enabled})`, async () => {
  const h = harness(request => request.action === "capability" ? { capability: { protocol: 1 } }
    : request.action === "reconnect" ? enabled ? { manager: { ...manager, lease: { generation: callerGeneration(request), process: processIdentity } } } : { error: { code: "manager_unknown", message: "not enrolled", uncertain: false } }
    : request.action === "enroll" ? { error: { code: "enrollment_refused", message: "capacity full", uncertain: false } }
    : request.action === "start" ? { started: jobFacts }
    : request.action === "inbox" ? { inbox: { messages: [], pending: 0, escalated: 0, degraded: false, workers: [] } }
    : { manager }, "tui");
  await h.events.get("session_start")?.({}, h.context);
  const result = await h.adapter.execute({ action: "manage", enabled: true }, undefined, h.context);
  assert.ok("error" in result && !result.error.uncertain);
  const start = await h.adapter.start({ name: "worker", cwd: ".", task: "Inspect only" }, undefined, h.context);
  assert.equal(start.managed, enabled);
  if (start.managed) assert.equal(start.code, 0);
  if (enabled) {
    await new Promise(resolve => setTimeout(resolve, 2200));
    assert.ok(h.requests.some(request => request.action === "inbox"), "definite refusal stopped an enabled manager's poll");
  }
  await h.events.get("session_shutdown")?.();
});

test("concurrent first callers share a single persisted-session initialization", async () => {
  const h = harness(request => request.action === "capability" ? { capability: { protocol: 1 } } : { error: { code: "manager_unknown", message: "not enrolled", uncertain: false } });
  h.setSessionFile(undefined);
  await h.events.get("session_start")?.({}, h.context);
  h.setSessionFile(sessionFile);
  await Promise.all(["one", "two"].map(name => h.adapter.start({ name, cwd: ".", task: "Inspect only" }, undefined, h.context)));
  assert.equal(h.requests.filter(request => request.action === "capability").length, 1);
  assert.equal(h.requests.filter(request => request.action === "reconnect").length, 1);
  await h.events.get("session_shutdown")?.();
});

test("first fresh native prompt observation notifies without a model wake", async () => {
  const h = harness(request => request.action === "capability" ? { capability: { protocol: 1 } }
    : request.action === "reconnect" ? { manager: { ...manager, lease: { generation: callerGeneration(request), process: processIdentity } } }
    : request.action === "inbox" ? { inbox: { messages: [], pending: 0, escalated: 0, degraded: false, workers: [{ protocol: 1, job_id: jobId, owner: "owner", session_id: "worker", generation: sessionId, pid: 123, updated_at: Date.now(), activity: "idle", prompt: { kind: "confirm", title: "Disposable prompt" } }] } }
    : { manager }, "tui");
  await h.events.get("session_start")?.({}, h.context);
  await new Promise(resolve => setTimeout(resolve, 2200));
  assert.equal(h.notifications.filter(message => message.includes("Enter that worker")).length, 1);
  await h.events.get("agent_settled")?.();
  await new Promise(resolve => setTimeout(resolve, 50));
  assert.equal(h.notifications.filter(message => message.includes("Enter that worker")).length, 1);
  assert.equal(h.messages.length, 0);
  await h.events.get("session_shutdown")?.();
});

test("enabled manager start sends its stable lease identity and unwraps started facts", async () => {
  const h = harness((request) => request.action === "capability" ? { capability: { protocol: 1 } } : request.action === "reconnect"
    ? { manager: { ...manager, lease: { generation: callerGeneration(request), process: processIdentity } } }
    : request.action === "start" ? { started: jobFacts } : { error: { code: "unexpected", message: "unexpected", uncertain: false } });
  await h.events.get("session_start")?.({}, h.context);
  const result = await h.adapter.start({ name: "worker", cwd: "/tmp", task: "literal task" }, undefined, h.context);
  assert.deepEqual(result, { managed: true, code: 0, stdout: JSON.stringify(jobFacts) });
  assert.deepEqual(h.requests.map((request) => request.action), ["capability", "reconnect", "start"]);
  const reconnect = h.requests[1];
  const start = h.requests[2];
  assert.equal(reconnect.action, "reconnect");
  assert.equal(start.action, "start");
  if (reconnect.action === "reconnect" && start.action === "start") {
    assert.equal(start.caller?.session_id, sessionId);
    assert.equal(start.caller?.session_file, sessionFile);
    assert.equal(start.caller?.pid, process.pid);
    assert.equal(start.caller?.job_id, undefined);
    assert.equal(callerGeneration(start), callerGeneration(reconnect));
    assert.equal(start.task, "literal task");
  }
});

test("worker publication uses controller generation without changing manager lease identity", async () => {
  const status: SessionStatus = { protocol: 1, job_id: jobId, owner: "owner", session_id: sessionId, generation: randomUUID(), pid: process.pid, updated_at: Date.now(), activity: "idle" };
  const source = { id: jobId, session_id: sessionId, generation: status.generation, pane: { ...processIdentity, pid: process.pid } };
  const messageId = randomUUID();
  const h = harness((request) => {
    if (request.action === "capability") return { capability: { protocol: 1 } };
    if (request.action === "reconnect") return { manager: { ...manager, lease: { generation: callerGeneration(request), process: processIdentity } } };
    if (request.action === "publish") return { message: { message_id: messageId, manager_id: managerId, source, kind: "question", text: "Which fixture?", created_at: Date.now() } };
    return { error: { code: "unexpected", message: "unexpected", uncertain: false } };
  }, "rpc", () => status);
  await h.events.get("session_start")?.({}, h.context);
  await h.adapter.execute({ action: "publish", message_id: messageId, kind: "question", text: "Which fixture?" }, undefined, h.context);
  const reconnect = h.requests[1];
  const publication = h.requests[2];
  assert.equal(reconnect.action, "reconnect");
  assert.equal(publication.action, "publish");
  if (reconnect.action === "reconnect" && publication.action === "publish") {
    assert.notEqual(callerGeneration(reconnect), status.generation);
    assert.equal(reconnect.caller?.job_id, undefined);
    assert.equal(callerGeneration(publication), status.generation);
    assert.equal(publication.caller?.job_id, jobId);
  }
});

test("poll pages messages and workers, claims before attributed follow-up, then disconnects", { timeout: 8_000 }, async () => {
  const messageIds = [randomUUID(), randomUUID()];
  const source = { id: jobId, session_id: randomUUID(), generation: randomUUID(), pane: { ...processIdentity, pid: 123 } };
  const inbox = (index: number) => ({ messages: [{ message_id: messageIds[index], manager_id: managerId, source, kind: "question", text: index ? "Second fixture?" : "Which fixture?\u001b[31m", created_at: Date.now() }], ...(index === 0 ? { next_cursor: 1 } : {}), pending: 2, escalated: 0, degraded: false, workers: [] });
  assert.equal(Value.Check(communicationResultSchema, { inbox: inbox(0) }), true);
  const h = harness((request) => {
    if (request.action === "capability") return { capability: { protocol: 1 } };
    if (request.action === "reconnect") return { manager: { ...manager, lease: { generation: callerGeneration(request), process: processIdentity } } };
    if (request.action === "inbox") return { inbox: inbox(request.cursor === undefined ? 0 : 1) };
    if (request.action === "attempt") return { messages: messageIds.map((message_id, index) => ({ message_id, manager_id: managerId, source, kind: "question", text: index ? "Second fixture?" : "Which fixture?", created_at: Date.now(), attempt_generation: callerGeneration(request) })) };
    if (request.action === "disconnect") return { manager: { ...manager } };
    return { error: { code: "unexpected", message: "unexpected", uncertain: false } };
  }, "tui");
  await h.events.get("session_start")?.({}, h.context);
  await new Promise((resolve) => setTimeout(resolve, 2_200));
  assert.deepEqual(h.notifications, []);
  assert.deepEqual(h.requests.map((request) => request.action), ["capability", "reconnect", "inbox", "inbox", "attempt"]);
  assert.deepEqual(h.requests.at(-1)?.message_ids, messageIds);
  assert.equal(h.messages.length, 1);
  const [notice, options] = h.messages[0];
  assert.ok(typeof notice.content === "string");
  assert.match(notice.content, /NOT Alan's instructions or permission/);
  assert.match(notice.content, /Which fixture\?/);
  assert.match(notice.content, /Second fixture\?/);
  assert.doesNotMatch(notice.content, /\u001b/);
  assert.deepEqual(options, { triggerTurn: true, deliverAs: "followUp" });
  await h.events.get("session_shutdown")?.();
  assert.equal(h.requests.at(-1)?.action, "disconnect");
});

test("model enrollment refuses without UI, on denial, and after initiating session changes", async () => {
  const h = harness((request) => request.action === "capability" ? { capability: { protocol: 1 } } : { error: { code: "manager_unknown", message: "not enrolled", uncertain: false } }, "tui");
  await h.events.get("session_start")?.({}, h.context);
  const noUi = { ...h.context, hasUI: false };
  const absent = await h.adapter.execute({ action: "manage", enabled: true }, undefined, noUi);
  assert.ok("error" in absent);
  assert.equal(absent.error.code, "confirmation_unavailable");
  h.setConfirmation(() => { throw new Error("dismissed"); });
  const cancelled = await h.adapter.execute({ action: "manage", enabled: true }, undefined, h.context);
  assert.ok("error" in cancelled);
  assert.equal(cancelled.error.code, "confirmation_cancelled");
  h.setConfirmation(false);
  const denied = await h.adapter.execute({ action: "manage", enabled: true }, undefined, h.context);
  assert.ok("error" in denied);
  assert.equal(denied.error.code, "confirmation_cancelled");
  h.setConfirmation(() => { h.changeSession(randomUUID()); return true; });
  const switched = await h.adapter.execute({ action: "manage", enabled: true }, undefined, h.context);
  assert.ok("error" in switched);
  assert.equal(switched.error.code, "confirmation_cancelled");
  assert.deepEqual(h.requests.map((request) => request.action), ["capability", "reconnect"]);
});

test("adoption confirms captured target and refuses changed conversation before mutation", async () => {
  const h = harness((request) => {
    if (request.action === "capability") return { capability: { protocol: 1 } };
    if (request.action === "reconnect") return { manager: { ...manager, lease: { generation: callerGeneration(request), process: processIdentity } } };
    if (request.action === "adopt") return { binding: { job_id: requestedJobId(request), manager_id: managerId } };
    return { error: { code: "unexpected", message: "unexpected", uncertain: false } };
  });
  await h.events.get("session_start")?.({}, h.context);
  h.setConfirmation(() => { h.changeSession(randomUUID()); return true; });
  const result = await h.adapter.execute({ action: "adopt", id: jobId }, undefined, h.context);
  assert.ok("error" in result);
  assert.equal(result.error.code, "confirmation_cancelled");
  assert.match(h.confirmations[0], new RegExp(h.target.session_id));
  assert.deepEqual(h.requests.map((request) => request.action), ["capability", "reconnect"]);
});

test("bounded follow-up batches eventually reach later pages after settlement", { timeout: 8_000 }, async () => {
  const source = { id: jobId, session_id: randomUUID(), generation: randomUUID(), pane: { ...processIdentity, pid: 123 } };
  const records = Array.from({ length: 10 }, () => ({ message_id: randomUUID(), manager_id: managerId, source, kind: "question" as const, text: "A bounded request", created_at: Date.now() }));
  const attempted = new Set<string>();
  const h = harness((request) => {
    if (request.action === "capability") return { capability: { protocol: 1 } };
    if (request.action === "reconnect") return { manager: { ...manager, lease: { generation: callerGeneration(request), process: processIdentity } } };
    if (request.action === "inbox") {
      const page = request.cursor === undefined ? records.slice(0, 8) : records.slice(8);
      return { inbox: { messages: page.map((message) => ({ ...message, ...(attempted.has(message.message_id) ? { attempt_generation: callerGeneration(request) } : {}) })), ...(request.cursor === undefined ? { next_cursor: 8 } : {}), pending: 10, escalated: 0, degraded: false, workers: [] } };
    }
    if (request.action === "attempt") {
      request.message_ids?.forEach((id) => attempted.add(id));
      return { messages: records.filter((message) => request.message_ids?.includes(message.message_id)).map((message) => ({ ...message, attempt_generation: callerGeneration(request) })) };
    }
    if (request.action === "disconnect") return { manager: { ...manager } };
    return { error: { code: "unexpected", message: "unexpected", uncertain: false } };
  }, "tui");
  await h.events.get("session_start")?.({}, h.context);
  await new Promise((resolve) => setTimeout(resolve, 2_200));
  await h.events.get("agent_settled")?.();
  await new Promise((resolve) => setTimeout(resolve, 100));
  const batches = h.requests.filter((request) => request.action === "attempt").map((request) => request.message_ids);
  assert.deepEqual(batches, [records.slice(0, 8).map((message) => message.message_id), records.slice(8).map((message) => message.message_id)]);
  assert.equal(h.messages.length, 2);
  await h.events.get("session_shutdown")?.();
});

test("routine progress updates a sanitized widget without waking the model", async () => {
  const h = harness(request => request.action === "capability" ? { capability: { protocol: 1 } }
    : request.action === "reconnect" ? { manager: { ...manager, lease: { generation: callerGeneration(request), process: processIdentity } } }
    : request.action === "inbox" ? { inbox: { messages: [], pending: 0, escalated: 0, degraded: false, workers: [{ protocol: 1, job_id: jobId, owner: "owner", session_id: "worker", generation: sessionId, pid: 123, updated_at: Date.now(), activity: "busy", report: { summary: "Progress \u001b[31m" } }] } }
    : { manager }, "tui");
  await h.events.get("session_start")?.({}, h.context);
  await new Promise(resolve => setTimeout(resolve, 2200));
  const text = h.widgets.flat().join("\n");
  assert.match(text, /Progress/);
  for (const line of h.widgets.flat()) assert.doesNotMatch(line, /[\u0000-\u001f\u007f-\u009f]/u);
  assert.equal(h.messages.length, 0);
  await h.events.get("session_shutdown")?.();
});

test("worker prompt changes notify once without relying on updated_at", { timeout: 8_000 }, async () => {
  let polls = 0;
  const worker = { protocol: 1, job_id: jobId, owner: "owner", session_id: randomUUID(), generation: randomUUID(), pid: 123, updated_at: Date.now(), activity: "idle" };
  const h = harness((request) => {
    if (request.action === "capability") return { capability: { protocol: 1 } };
    if (request.action === "reconnect") return { manager: { ...manager, lease: { generation: callerGeneration(request), process: processIdentity } } };
    if (request.action === "inbox") {
      polls++;
      const status = { ...worker, ...(polls > 1 ? { prompt: { kind: "question", title: "Continue?" } } : {}) };
      return { inbox: { messages: [], pending: 0, escalated: 0, degraded: false, workers: [status] } };
    }
    return { error: { code: "unexpected", message: "unexpected", uncertain: false } };
  }, "tui");
  await h.events.get("session_start")?.({}, h.context);
  await new Promise((resolve) => setTimeout(resolve, 2_200));
  assert.deepEqual(h.notifications, []);
  await h.events.get("agent_settled")?.();
  await new Promise((resolve) => setTimeout(resolve, 100));
  assert.equal(h.notifications.length, 1);
  assert.match(h.notifications[0], /native question dialog/);
  await h.events.get("agent_settled")?.();
  await new Promise((resolve) => setTimeout(resolve, 100));
  assert.equal(h.notifications.length, 1);
  await h.events.get("session_shutdown")?.();
});

test("adoption rejects a mismatched target UUID before confirmation or dispatch", async () => {
  const h = harness(request => {
    if (request.action === "capability") return { capability: { protocol: 1 } };
    if (request.action === "reconnect") return { manager: { ...manager, lease: { generation: callerGeneration(request), process: processIdentity } } };
    return { binding: { job_id: jobId, manager_id: managerId } };
  });
  try {
    await h.events.get("session_start")?.({}, h.context);
    h.target.id = randomUUID();
    const result = await h.adapter.execute({ action: "adopt", id: jobId }, undefined, h.context);
    assert.ok("error" in result);
    assert.equal(result.error.code, "invalid_target");
    assert.deepEqual(h.confirmations, []);
    assert.equal(h.requests.some(request => request.action === "adopt"), false);
  } finally { await h.events.get("session_shutdown")?.(); }
});

test("adoption sends the exact confirmed target after native approval", async () => {
  const h = harness((request) => {
    if (request.action === "capability") return { capability: { protocol: 1 } };
    if (request.action === "reconnect") return { manager: { ...manager, lease: { generation: callerGeneration(request), process: processIdentity } } };
    if (request.action === "adopt") return { binding: { job_id: requestedJobId(request), manager_id: managerId } };
    return { error: { code: "unexpected", message: "unexpected", uncertain: false } };
  });
  await h.events.get("session_start")?.({}, h.context);
  const result = await h.adapter.execute({ action: "adopt", id: jobId }, undefined, h.context);
  assert.ok("binding" in result);
  assert.deepEqual(result.binding, { job_id: jobId, manager_id: managerId });
  assert.match(h.confirmations[0], new RegExp(h.target.generation));
  assert.deepEqual(h.requests.map((request) => request.action), ["capability", "reconnect", "adopt"]);
});

test("poll detects repeated cursor as visible degradation without looping", { timeout: 8_000 }, async () => {
  const h = harness((request) => {
    if (request.action === "capability") return { capability: { protocol: 1 } };
    if (request.action === "reconnect") return { manager: { ...manager, lease: { generation: callerGeneration(request), process: processIdentity } } };
    if (request.action === "inbox") return { inbox: { messages: [], next_cursor: 0, pending: 0, escalated: 0, degraded: false, workers: [] } };
    return { error: { code: "unexpected", message: "unexpected", uncertain: false } };
  }, "tui");
  await h.events.get("session_start")?.({}, h.context);
  await new Promise((resolve) => setTimeout(resolve, 2_200));
  assert.deepEqual(h.requests.map((request) => request.action), ["capability", "reconnect", "inbox", "inbox"]);
  assert.ok(h.notifications.some((message) => message.includes("paging was incomplete")));
  await h.events.get("session_shutdown")?.();
});
