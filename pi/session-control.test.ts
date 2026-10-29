import assert from "node:assert/strict";
import { createHash, randomBytes, randomUUID } from "node:crypto";
import { lstat, mkdir, mkdtemp, readFile, realpath, rename, rm, writeFile } from "node:fs/promises";
import { connect } from "node:net";
import { join } from "node:path";
import { tmpdir } from "node:os";
import test from "node:test";
import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";
import { registerSessionControl } from "./session-control.ts";

function record(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}
function parseRecord(text: string): Record<string, unknown> {
  const value: unknown = JSON.parse(text);
  assert.ok(record(value));
  return value;
}
function exchange(path: string, request: object): Promise<Record<string, unknown>> {
  return new Promise((resolve, reject) => {
    const socket = connect(path);
    let received = "";
    socket.setTimeout(4000, () => socket.destroy(new Error("control response timed out")));
    socket.setEncoding("utf8");
    socket.on("connect", () => socket.end(`${JSON.stringify(request)}\n`));
    socket.on("data", (chunk) => { received += chunk; });
    socket.on("end", () => {
      try { resolve(parseRecord(received)); } catch (error) { reject(error); }
    });
    socket.on("error", reject);
  });
}

test("half-closed control socket preserves literal text, replay fencing and generation-owned cleanup", async () => {
  const id = randomUUID();
  const sessionId = randomUUID();
  const requestId = randomUUID();
  const root = await mkdtemp(join(await realpath(tmpdir()), "session-control-socket-"));
  const uid = process.getuid?.();
  assert.ok(typeof uid === "number");
  const owner = randomBytes(8).toString("hex");
  const job = join(root, "jobs", id);
  const socketRoot = join(await realpath("/tmp"), `ac-${uid}-${owner}`);
  const socketDir = join(socketRoot, id);
  const endpoint = join(socketDir, "control");
  const previous = new Map(["AC_STATE_DIR", "AC_OWNER", "AC_JOB"].map((key) => [key, process.env[key]]));
  const listeners = new Map<string, (...args: unknown[]) => unknown>();
  const calls: unknown[][] = [];
  const bootId = "3e322019-1073-4f52-afd0-abed9a17ce70";
  const identity = { boot_id: bootId, pid: process.pid, uid, start_sec: 1, start_usec: 0 };
  const pi: Pick<ExtensionAPI, "on" | "sendUserMessage"> = {
    on(event, listener) {
      listeners.set(event, (...args) => Reflect.apply(listener, undefined, args));
      return () => { listeners.delete(event); };
    },
    sendUserMessage(...args) { calls.push(args); },
  };
  const controller = registerSessionControl(pi);
  try {
    await Promise.all([join(root, "jobs"), socketRoot].map((path) => mkdir(path, { mode: 0o700 })));
    await Promise.all([job, socketDir].map((path) => mkdir(path, { mode: 0o700 })));
    await writeFile(join(root, "owner.json"), JSON.stringify({ schema: 1, token: owner }), { mode: 0o600 });
    await writeFile(join(job, "manifest.json"), JSON.stringify({ schema: 1, id, name: "socket-test", owner, cwd: root, pi: "/bin/pi", tmux: "/bin/tmux", coordinator: "/bin/coordinator", created: new Date().toISOString(), boot_id: bootId, boot_id_scheme: "darwin-boot-session-uuid", protocol: "1", socket: join(socketDir, "s") }), { mode: 0o600 });
    await writeFile(join(job, "launch.json"), JSON.stringify({ schema: 1, phase: "exec-intent", server: { ...identity, pid: process.pid + 1 }, pane: identity }), { mode: 0o600 });
    process.env.AC_STATE_DIR = root;
    process.env.AC_OWNER = owner;
    process.env.AC_JOB = id;
    await listeners.get("session_start")?.({}, { cwd: root, sessionManager: { getSessionId: () => sessionId, getSessionFile: () => join(root, `${sessionId}.jsonl`) } });
    const status = controller.status();
    assert.ok(status);
    const request = { protocol: 1, job_id: id, session_id: sessionId, generation: status.generation, request_id: requestId, action: "send", text: "literal 世界" };
    const response = await exchange(endpoint, request);
    const claim = parseRecord(await readFile(join(job, "control-requests", `${requestId}.json`), "utf8"));
    assert.ok(claim.receipt, "reply must follow durable receipt publication");
    assert.deepEqual(response.receipt, claim.receipt);
    assert.deepEqual(calls, [["literal 世界", { deliverAs: "steer", expandPromptTemplates: false }]]);
    assert.deepEqual(await exchange(endpoint, request), response, "completed replay returns original receipt");
    const conflict = await exchange(endpoint, { ...request, text: "different" });
    assert.ok(record(conflict.error));
    assert.equal(conflict.error.code, "request_conflict");
    const stale = await exchange(endpoint, { ...request, request_id: randomUUID(), generation: randomUUID() });
    assert.ok(record(stale.error));
    assert.equal(stale.error.code, "stale_target");
    const ambiguous = { ...request, request_id: randomUUID(), text: "ambiguous" };
    const fingerprint = createHash("sha256").update(JSON.stringify([id, sessionId, status.generation, "send", "ambiguous"])).digest("hex");
    await writeFile(join(job, "control-requests", `${ambiguous.request_id}.json`), JSON.stringify({ fingerprint, receipt: null }), { mode: 0o600 });
    const uncertain = await exchange(endpoint, ambiguous);
    assert.ok(record(uncertain.error));
    assert.equal(uncertain.error.uncertain, true);
    assert.equal(calls.length, 1, "replays, conflicts, crash claims and stale runtimes must have no new effect");

    const controlText = "\u0001".repeat(64 * 1024);
    const bounded = await exchange(endpoint, { ...request, request_id: randomUUID(), text: controlText });
    assert.ok(record(bounded.receipt), "every accepted 64 KiB UTF-8 payload must fit the JSON wire bound");
    assert.equal(bounded.receipt.result, "api_invoked");
    assert.equal(calls.length, 2);
    assert.equal(calls[1]?.[0], controlText);
    const oversized = await exchange(endpoint, { ...request, request_id: randomUUID(), text: "x".repeat(600 * 1024) });
    assert.ok(record(oversized.error));
    assert.equal(oversized.error.code, "invalid_request");
    assert.equal(calls.length, 2, "oversized wire input must not dispatch");

    await listeners.get("session_shutdown")?.();
    assert.equal(controller.status(), undefined);
    await assert.rejects(readFile(endpoint), { code: "ENOENT" });
    await assert.rejects(readFile(join(job, "session.json")), { code: "ENOENT" });
    await listeners.get("session_start")?.({}, { cwd: root, sessionManager: { getSessionId: () => sessionId, getSessionFile: () => join(root, `${sessionId}.jsonl`) } });
    const replacement = { ...controller.status(), generation: randomUUID() };
    await writeFile(join(job, "session.json"), JSON.stringify(replacement), { mode: 0o600 });
    await listeners.get("session_shutdown")?.();
    assert.deepEqual(parseRecord(await readFile(join(job, "session.json"), "utf8")), replacement, "old generation cleanup cannot delete replacement status");
    const fileObservation = { Path: "/managed/file", Hash: "a".repeat(64), Size: 1, Mode: 0, UID: uid, Dev: 1, Ino: 1 };
    const observation = { Launcher: fileObservation, Current: fileObservation, Package: fileObservation, BinLink: { Path: "/managed/link", Target: "target", UID: uid, Dev: 1, Ino: 1 }, Entry: fileObservation, Version: "1.4.0" };
    await writeFile(join(job, "intent.json"), JSON.stringify({ schema: 1, kind: "interactive", terminal_profile: "transparent-v1", argv_sha256: "a".repeat(64), native_pi: { Executable: "/bin/pi", Observation: observation } }), { mode: 0o600 });
    await writeFile(join(job, "role.json"), JSON.stringify({ schema: 1, role: "standalone" }), { mode: 0o600 });
    await writeFile(join(job, "manifest.json"), JSON.stringify({ schema: 1, id, name: "socket-test", owner, cwd: root, pi: "/bin/pi", tmux: "/bin/tmux", coordinator: "/bin/coordinator", created: new Date().toISOString(), boot_id: bootId, boot_id_scheme: "darwin-boot-session-uuid", protocol: "2", socket: join(socketDir, "s") }), { mode: 0o600 });
    const nativeFile = join(root, `${sessionId}.jsonl`);
    await writeFile(nativeFile, `${JSON.stringify({ type: "session", version: 3, id: sessionId })}\n`, { mode: 0o644 });
    await listeners.get("session_start")?.({}, { cwd: root, sessionManager: { getSessionId: () => sessionId, getSessionFile: () => nativeFile } });
    assert.equal(controller.status()?.launch_protocol, "2");
    const nativeObservation = parseRecord(await readFile(join(job, "native-session.json"), "utf8"));
    assert.deepEqual(Object.keys(nativeObservation).sort(), ["cwd", "file", "generation", "header_id", "job_id", "owner", "process", "schema", "session_file", "session_id", "state", "updated_at"].sort());
    assert.equal(nativeObservation.state, "persisted");
    assert.equal(nativeObservation.header_id, sessionId);
    const historyPath = join(job, "native-session-history.json");
    const initialHistory = parseRecord(await readFile(historyPath, "utf8"));
    assert.equal(initialHistory.schema, 1);
    assert.ok(Array.isArray(initialHistory.observations));
    assert.equal(initialHistory.observations.length, 1, "first persisted holder is retained in history");
    assert.deepEqual(await listeners.get("session_before_switch")?.({ reason: "resume", targetSessionFile: nativeFile }), { cancel: true });
    const afterResume = await exchange(endpoint, { protocol: 1, job_id: id, session_id: sessionId, generation: controller.status()?.generation, request_id: randomUUID(), action: "send", text: "still live" });
    assert.equal(record(afterResume.receipt) && afterResume.receipt.result, "api_invoked", "canceled resume must leave the unchanged runtime controllable");
    assert.equal(calls.length, 3, "canceled resume must dispatch exactly once");
    assert.equal(await listeners.get("session_before_switch")?.({ reason: "new" }), undefined);
    assert.equal(await listeners.get("session_before_fork")?.({ entryId: "entry", position: "before" }, { sessionManager: { getSessionFile: () => nativeFile } }), undefined);
    const sameIdOtherFile = join(root, "other.jsonl");
    await writeFile(sameIdOtherFile, `${JSON.stringify({ type: "session", version: 3, id: sessionId })}\n`, { mode: 0o644 });
    assert.deepEqual(await listeners.get("session_before_fork")?.({ entryId: "entry", position: "before" }, { sessionManager: { getSessionFile: () => sameIdOtherFile } }), { cancel: true }, "same ID in another file is not the current holder");
    const afterOtherFork = await exchange(endpoint, { protocol: 1, job_id: id, session_id: sessionId, generation: controller.status()?.generation, request_id: randomUUID(), action: "send", text: "unrelated fork" });
    assert.equal(record(afterOtherFork.receipt) && afterOtherFork.receipt.result, "api_invoked", "unrelated fork cancellation must preserve the current runtime");
    await writeFile(nativeFile, "malformed header\\n", { mode: 0o644 });
    assert.deepEqual(await listeners.get("session_before_fork")?.({ entryId: "entry", position: "before" }, { sessionManager: { getSessionFile: () => nativeFile } }), { cancel: true }, "malformed current header must cancel without throwing");
    await writeFile(nativeFile, `${JSON.stringify({ type: "session", version: 3, id: sessionId })}\n`, { mode: 0o644 });
    assert.deepEqual(await listeners.get("session_before_fork")?.({ entryId: "entry", position: "before" }, { sessionManager: { getSessionFile: () => join(root, "missing.jsonl") } }), { cancel: true });
    await writeFile(nativeFile, "not-json\\n", { mode: 0o644 });
    assert.equal(controller.status(), undefined, "invalid current publication must hide cached actionable status");
    await listeners.get("agent_start")?.();
    assert.equal(await controller.publishStatus(), undefined, "fenced publication must not republish status");
    assert.equal(await controller.report("later"), undefined, "fenced report must remain unavailable");
    await new Promise((resolveWait) => setTimeout(resolveWait, 30));
    await assert.rejects(readFile(join(job, "session.json")), { code: "ENOENT" }, "invalid existing session header must invalidate actionable status");
    await listeners.get("session_shutdown")?.();

    const sessionB = "saved.Custom-session_2026";
    const nativeFileB = join(root, `${sessionB}.jsonl`);
    await writeFile(nativeFileB, `${JSON.stringify({ type: "session", version: 3, id: sessionB })}\n`, { mode: 0o644 });
    const contextB = { cwd: root, sessionManager: { getSessionId: () => sessionB, getSessionFile: () => nativeFileB } };
    await listeners.get("session_start")?.({}, contextB);
    assert.equal(controller.status()?.session_id, sessionB, "native session IDs must not be restricted to coordinator UUIDs");
    const historyB = parseRecord(await readFile(historyPath, "utf8"));
    assert.ok(Array.isArray(historyB.observations));
    assert.equal(historyB.observations.length, 2);
    assert.deepEqual(historyB.observations.map((item) => { assert.ok(record(item)); return item.session_id; }), [sessionId, sessionB]);
    await listeners.get("session_shutdown")?.();
    await listeners.get("session_start")?.({}, contextB);
    const historyReload = parseRecord(await readFile(historyPath, "utf8"));
    assert.ok(Array.isArray(historyReload.observations));
    assert.equal(historyReload.observations.length, 2, "generation-only reload must not duplicate the holder tuple");
    await listeners.get("session_shutdown")?.();
    const corruptHistory = "corrupt history\n";
    await writeFile(historyPath, corruptHistory, { mode: 0o600 });
    await listeners.get("session_start")?.({}, contextB);
    await new Promise((resolveWait) => setTimeout(resolveWait, 30));
    assert.equal(await readFile(historyPath, "utf8"), corruptHistory, "corrupt history must not be overwritten or dropped");
    await assert.rejects(readFile(join(job, "session.json")), { code: "ENOENT" }, "corrupt history must fence status publication");
    await listeners.get("session_shutdown")?.();
    assert.ok(Array.isArray(historyReload.observations));
    const historyTemplate = historyReload.observations[0];
    assert.ok(record(historyTemplate));
    const fullObservations = Array.from({ length: 128 }, (_, index) => {
      const tupleId = randomUUID();
      return { ...historyTemplate, session_id: tupleId, header_id: tupleId, session_file: join(root, `holder-${index}.jsonl`), file: { device: index + 1, inode: index + 1 } };
    });
    const fullHistoryText = `${JSON.stringify({ schema: 1, observations: fullObservations })}\n`;
    await writeFile(historyPath, fullHistoryText, { mode: 0o600 });
    const sessionC = randomUUID();
    const nativeFileC = join(root, `${sessionC}.jsonl`);
    await writeFile(nativeFileC, `${JSON.stringify({ type: "session", version: 3, id: sessionC })}\n`, { mode: 0o644 });
    await listeners.get("session_start")?.({}, { cwd: root, sessionManager: { getSessionId: () => sessionC, getSessionFile: () => nativeFileC } });
    await new Promise((resolveWait) => setTimeout(resolveWait, 30));
    assert.equal(await readFile(historyPath, "utf8"), fullHistoryText, "history capacity failure must preserve all existing tuples");
    await assert.rejects(readFile(join(job, "session.json")), { code: "ENOENT" }, "history capacity failure must fence status publication");
    await listeners.get("session_shutdown")?.();
    await writeFile(historyPath, JSON.stringify({ schema: 1, observations: [] }), { mode: 0o600 });
    await listeners.get("session_start")?.({}, { cwd: root, sessionManager: { getSessionId: () => sessionC, getSessionFile: () => nativeFileC } });
    const observedSource = await lstat(nativeFileC);
    const replacementFile = join(root, "replacement.jsonl");
    await writeFile(replacementFile, `${JSON.stringify({ type: "session", version: 3, id: sessionC })}\n`, { mode: 0o644 });
    const replacementSource = await lstat(replacementFile);
    await rename(replacementFile, nativeFileC);
    const replacedSource = await lstat(nativeFileC);
    assert.notEqual(`${replacedSource.dev}:${replacedSource.ino}`, `${observedSource.dev}:${observedSource.ino}`, "fixture must replace the path with a new inode");
    assert.deepEqual(await listeners.get("session_before_fork")?.({ entryId: "entry", position: "before" }, { sessionManager: { getSessionFile: () => nativeFileC } }), { cancel: true }, "same path and header ID with a replacement inode is not the published holder");
    await listeners.get("session_shutdown")?.();
  } finally {
    await listeners.get("session_shutdown")?.();
    for (const [key, value] of previous) value === undefined ? delete process.env[key] : process.env[key] = value;
    await rm(root, { recursive: true, force: true });
    await rm(socketRoot, { recursive: true, force: true });
  }
});
