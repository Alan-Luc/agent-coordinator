import assert from "node:assert/strict";
import { randomUUID } from "node:crypto";
import { fileURLToPath } from "node:url";
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
const jobFacts = { id: jobId, name: "worker", launch: "running", terminal: "attached", pi_readiness: "unverified", task_outcome: "unknown", clients: 1, retired: false, next_action: "inspect" };
const manager = { id: managerId, session_file: sessionFile, session_id: sessionId, enabled: true };
const processIdentity = { boot_id: "3e322019-1073-4f52-afd0-abed9a17ce70", pid: process.pid, uid: process.getuid?.() ?? 501, start_sec: 10, start_usec: 0 };

type Listener = (...args: unknown[]) => unknown;
interface TestRequest { action: string; caller?: CommunicationCaller; job_id?: string; task?: string; cursor?: number; message_ids?: string[]; }
function callerGeneration(request: TestRequest): string { assert.ok(request.caller); return request.caller.generation; }
function requestedJobId(request: TestRequest): string { assert.ok(request.job_id); return request.job_id; }
function harness(response: (request: TestRequest) => unknown, mode = "rpc", status?: () => SessionStatus | undefined) {
  const events = new Map<string, Listener>();
  const commands = new Map<string, { handler: Listener }>();
  const requests: TestRequest[] = [];
  const messages: Parameters<ExtensionAPI["sendMessage"]>[] = [];
  const notifications: string[] = [];
  const widgets: string[][] = [];
  const confirmations: string[] = [];
  const target = { id: jobId, session_id: randomUUID(), generation: randomUUID(), pane: { boot_id: "3e322019-1073-4f52-afd0-abed9a17ce70", pid: 123, uid: 501, start_sec: 10, start_usec: 0 } };
  let currentSessionId: string = sessionId;
  let currentSessionFile: string | undefined = sessionFile;
  let confirmation: boolean | (() => boolean) = true;
  const pi = {
    on: (event: string, fn: Listener) => { events.set(event, fn); return () => events.delete(event); },
    registerCommand: (name: string, value: { handler: Listener }) => { commands.set(name, value); },
    sendMessage: (...args: Parameters<ExtensionAPI["sendMessage"]>) => { messages.push(args); },
  };
  const cli = {
    findExecutable: async () => "/bin/agent-coordinator",
    runCli: async (_exe: string, argv: string[], _cwd: string, input?: string) => {
      if (argv[0] === "_target") return { code: 0, stdout: JSON.stringify(target) };
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
  const controller = { status: status ?? (() => undefined), report: async () => undefined };
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
  return { adapter: calls, context, events, commands, requests, messages, notifications, widgets, confirmations, target, setConfirmation: (value: boolean | (() => boolean)) => { confirmation = value; }, changeSession: (value: string) => { currentSessionId = value; }, setSessionFile: (value: string | undefined) => { currentSessionFile = value; } };
}

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
