import assert from "node:assert/strict";
import { createHash, randomBytes, randomUUID } from "node:crypto";
import { mkdir, mkdtemp, readFile, realpath, rm, writeFile } from "node:fs/promises";
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
    await listeners.get("session_start")?.({}, { sessionManager: { getSessionId: () => sessionId } });
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
    await listeners.get("session_start")?.({}, { sessionManager: { getSessionId: () => sessionId } });
    const replacement = { ...controller.status(), generation: randomUUID() };
    await writeFile(join(job, "session.json"), JSON.stringify(replacement), { mode: 0o600 });
    await listeners.get("session_shutdown")?.();
    assert.deepEqual(parseRecord(await readFile(join(job, "session.json"), "utf8")), replacement, "old generation cleanup cannot delete replacement status");
  } finally {
    await listeners.get("session_shutdown")?.();
    for (const [key, value] of previous) value === undefined ? delete process.env[key] : process.env[key] = value;
    await rm(root, { recursive: true, force: true });
    await rm(socketRoot, { recursive: true, force: true });
  }
});
