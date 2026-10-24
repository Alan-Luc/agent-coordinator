import { createHash, randomUUID } from "node:crypto";
import { constants } from "node:fs";
import { lstat, mkdir, open, realpath, rename, unlink } from "node:fs/promises";
import { createServer, type Server, type Socket } from "node:net";
import { dirname, join, resolve } from "node:path";
import type { ExtensionAPI, ExtensionContext } from "@earendil-works/pi-coding-agent";
import { Value } from "typebox/value";
import { controlProtocol, controlRequestSchema, maxControlTextBytes, type AgentReport, type ControlReceipt, type ControlRequest, type ObservedPrompt, type SessionStatus } from "./control-protocol.ts";

const maxWireBytes = 512 * 1024;
const maxSummaryBytes = 1024;
const uuidPattern = /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;
const bootIdPattern = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/;
const bootIdScheme = "darwin-boot-session-uuid";

type JsonRecord = Record<string, unknown>;
interface ActiveSession {
  generation: string;
  sessionId: string;
  status: SessionStatus;
  socketPath: string;
  statusPath: string;
  requestsPath: string;
  directory: string;
  server: Server;
  socketIdentity?: { dev: number; ino: number };
  heartbeat: NodeJS.Timeout;
  writes: Promise<void>;
  context: ExtensionContext;
  stopping: boolean;
  cleanup?: Promise<void>;
  persistenceFailed: boolean;
}

export interface SessionController {
  report(summary: string, attention?: string | null): Promise<SessionStatus | undefined>;
  status(): SessionStatus | undefined;
}

function currentUid(): number | undefined { return process.getuid?.(); }
function record(value: unknown): value is JsonRecord {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}
function exactKeys(value: JsonRecord, keys: string[]): boolean {
  return Object.keys(value).every((key) => keys.includes(key)) && keys.every((key) => Object.hasOwn(value, key));
}
function validText(value: unknown, maxBytes: number): value is string {
  if (typeof value !== "string" || value.trim() === "" || Buffer.byteLength(value, "utf8") > maxBytes) return false;
  return !/[\uD800-\uDBFF](?![\uDC00-\uDFFF])|(?<![\uD800-\uDBFF])[\uDC00-\uDFFF]/u.test(value);
}

export function parseControlRequest(value: unknown): ControlRequest | undefined {
  if (!Value.Check(controlRequestSchema, value)) return undefined;
  if (!uuidPattern.test(value.job_id) || !value.session_id || !uuidPattern.test(value.generation) || !uuidPattern.test(value.request_id)) return undefined;
  if (value.action === "send" && !validText(value.text, maxControlTextBytes)) return undefined;
  return value;
}

async function privateDirectory(path: string): Promise<boolean> {
  try {
    const info = await lstat(path);
    const uid = currentUid();
    return uid !== undefined && info.isDirectory() && !info.isSymbolicLink() && info.uid === uid && (info.mode & 0o777) === 0o700;
  } catch { return false; }
}
function validProcessIdentity(value: unknown, pid?: number): value is JsonRecord {
  const uid = currentUid();
  return uid !== undefined && record(value) && exactKeys(value, ["boot_id", "pid", "uid", "start_sec", "start_usec"]) && typeof value.boot_id === "string" && bootIdPattern.test(value.boot_id) && typeof value.pid === "number" && Number.isInteger(value.pid) && (pid === undefined || value.pid === pid) && typeof value.uid === "number" && Number.isInteger(value.uid) && value.uid === uid && typeof value.start_sec === "number" && Number.isInteger(value.start_sec) && value.start_sec > 0 && typeof value.start_usec === "number" && Number.isInteger(value.start_usec) && value.start_usec >= 0 && value.start_usec < 1_000_000;
}
async function privateRecord(path: string): Promise<unknown> {
  const handle = await open(path, constants.O_RDONLY | constants.O_NOFOLLOW | constants.O_NONBLOCK);
  try {
    const info = await handle.stat();
    if (!info.isFile() || info.uid !== currentUid() || (info.mode & 0o777) !== 0o600 || info.nlink !== 1 || info.size > maxWireBytes) throw new Error("invalid private record");
    const text = await handle.readFile({ encoding: "utf8" });
    const value: unknown = JSON.parse(text);
    return value;
  } finally { await handle.close(); }
}
async function syncDirectory(path: string): Promise<void> {
  const handle = await open(path, constants.O_RDONLY);
  try { await handle.sync(); } finally { await handle.close(); }
}
async function atomicStatus(path: string, directory: string, status: SessionStatus): Promise<void> {
  const temp = join(directory, `.session-${randomUUID()}`);
  const handle = await open(temp, constants.O_WRONLY | constants.O_CREAT | constants.O_EXCL | constants.O_NOFOLLOW, 0o600);
  try {
    await handle.writeFile(`${JSON.stringify(status)}\n`);
    await handle.sync();
  } catch (error) {
    await handle.close();
    await unlink(temp).catch(() => undefined);
    throw error;
  }
  await handle.close();
  await rename(temp, path);
  await syncDirectory(directory);
}

export async function verifyInheritedJob(): Promise<{ id: string; owner: string; socketPath: string; directory: string; requestsPath: string } | undefined> {
  const id = process.env.AC_JOB;
  const owner = process.env.AC_OWNER;
  const state = process.env.AC_STATE_DIR;
  const uid = currentUid();
  if (!id || !uuidPattern.test(id) || !owner || !/^[0-9a-f]{16}$/.test(owner) || !state || uid === undefined) return undefined;
  try {
    const root = await realpath(state);
    if (root !== resolve(state) || !(await privateDirectory(root))) return undefined;
    const jobs = join(root, "jobs");
    const directory = join(jobs, id);
    if (!(await privateDirectory(jobs)) || !(await privateDirectory(directory))) return undefined;
    const ownerRecord = await privateRecord(join(root, "owner.json"));
    const manifest = await privateRecord(join(directory, "manifest.json"));
    const launch = await privateRecord(join(directory, "launch.json"));
    const manifestKeys = ["schema", "id", "name", "owner", "cwd", "pi", "tmux", "coordinator", "created", "boot_id", "boot_id_scheme", "protocol", "socket"];
    if (!record(ownerRecord) || !exactKeys(ownerRecord, ["schema", "token"]) || ownerRecord.schema !== 1 || ownerRecord.token !== owner || !record(manifest) || !exactKeys(manifest, manifestKeys) || manifest.schema !== 1 || manifest.id !== id || typeof manifest.name !== "string" || !/^[a-z0-9][a-z0-9-]{0,47}$/.test(manifest.name) || manifest.owner !== owner || manifest.protocol !== "1" || typeof manifest.cwd !== "string" || !manifest.cwd.startsWith("/") || typeof manifest.pi !== "string" || !manifest.pi.startsWith("/") || typeof manifest.tmux !== "string" || !manifest.tmux.startsWith("/") || typeof manifest.coordinator !== "string" || !manifest.coordinator.startsWith("/") || typeof manifest.created !== "string" || !Number.isFinite(Date.parse(manifest.created)) || manifest.boot_id_scheme !== bootIdScheme || typeof manifest.boot_id !== "string" || !bootIdPattern.test(manifest.boot_id) || typeof manifest.socket !== "string" || !record(launch) || Object.keys(launch).some((key) => !["schema", "phase", "server", "pane", "error", "retired"].includes(key)) || launch.schema !== 1 || launch.phase !== "exec-intent" || launch.retired === true || !validProcessIdentity(launch.pane, process.pid) || !validProcessIdentity(launch.server) || launch.pane.boot_id !== manifest.boot_id || launch.server.boot_id !== manifest.boot_id) return undefined;
    const socketPath = await realpath(dirname(manifest.socket)).then((path) => join(path, "control"));
    const socketRoot = join(await realpath("/tmp"), `ac-${uid}-${owner}`);
    const expectedDir = join(socketRoot, id);
    if (dirname(socketPath) !== expectedDir || manifest.socket !== join(expectedDir, "s")) return undefined;
    if (!(await privateDirectory(socketRoot))) return undefined;
    const endpointDir = await lstat(expectedDir);
    if (!endpointDir.isDirectory() || endpointDir.isSymbolicLink() || endpointDir.uid !== uid || (endpointDir.mode & 0o777) !== 0o700) return undefined;
    const requestsPath = join(directory, "control-requests");
    await mkdir(requestsPath, { mode: 0o700 }).catch((error: NodeJS.ErrnoException) => { if (error.code !== "EEXIST") throw error; });
    if (!(await privateDirectory(requestsPath))) return undefined;
    return { id, owner, socketPath, directory, requestsPath };
  } catch { return undefined; }
}

function errorResponse(code: string, message: string, uncertain = false): string {
  return JSON.stringify({ error: { code, message, uncertain } }) + "\n";
}
function requestFingerprint(request: ControlRequest): string {
  return createHash("sha256").update(JSON.stringify([request.job_id, request.session_id, request.generation, request.action, "text" in request ? request.text : null])).digest("hex");
}
function validReceipt(value: unknown): value is ControlReceipt {
  return record(value) && typeof value.id === "string" && typeof value.session_id === "string" && typeof value.generation === "string" && typeof value.request_id === "string" && (value.operation === "send" || value.operation === "stop") && (value.result === "api_invoked" || value.result === "exit_observed" || value.result === "uncertain");
}

async function durableRequest(active: ActiveSession, request: ControlRequest, sendUserMessage: ExtensionAPI["sendUserMessage"]): Promise<{ receipt?: ControlReceipt; error?: string; stop?: boolean }> {
  if (request.job_id !== active.status.job_id || request.session_id !== active.sessionId || request.generation !== active.generation) return { error: "stale_target" };
  const fingerprint = requestFingerprint(request);
  const claimPath = join(active.requestsPath, `${request.request_id}.json`);
  const receipt: ControlReceipt = { id: request.job_id, session_id: request.session_id, generation: request.generation, request_id: request.request_id, operation: request.action, result: "api_invoked" };
  let handle;
  try {
    handle = await open(claimPath, constants.O_WRONLY | constants.O_CREAT | constants.O_EXCL | constants.O_NOFOLLOW, 0o600);
  } catch (error) {
    if (!record(error) || error.code !== "EEXIST") return { error: "claim_failed" };
    try {
      const previous = await privateRecord(claimPath);
      if (!record(previous) || previous.fingerprint !== fingerprint) return { error: "request_conflict" };
      if (validReceipt(previous.receipt) && previous.receipt.id === request.job_id && previous.receipt.session_id === request.session_id && previous.receipt.generation === request.generation && previous.receipt.request_id === request.request_id && previous.receipt.operation === request.action) return { receipt: previous.receipt };
      return { error: "uncertain", };
    } catch { return { error: "uncertain" }; }
  }
  const claim = { fingerprint, receipt: null };
  try {
    await handle.writeFile(`${JSON.stringify(claim)}\n`);
    await handle.sync();
  } catch {
    await handle.close();
    return { error: "claim_failed" };
  }
  await handle.close();
  await syncDirectory(active.requestsPath);
  try {
    if ("text" in request) sendUserMessage(request.text, { deliverAs: "steer", expandPromptTemplates: false });
    else active.stopping = true;
  } catch { return { error: "uncertain" }; }
  if (request.action === "send") await writeRequestReceipt(active, claimPath, fingerprint, receipt);
  return { receipt, stop: request.action === "stop" };
}

async function writeRequestReceipt(active: ActiveSession, claimPath: string, fingerprint: string, receipt: ControlReceipt): Promise<void> {
  const temp = `${claimPath}.${randomUUID()}`;
  const output = await open(temp, constants.O_WRONLY | constants.O_CREAT | constants.O_EXCL | constants.O_NOFOLLOW, 0o600);
  try {
    await output.writeFile(`${JSON.stringify({ fingerprint, receipt })}\n`);
    await output.sync();
  } catch (error) {
    await output.close();
    await unlink(temp).catch(() => undefined);
    throw error;
  }
  await output.close();
  await rename(temp, claimPath);
  await syncDirectory(active.requestsPath);
}

function observePrompt(kind: string, title?: string): ObservedPrompt {
  return { kind, ...(title ? { title: Array.from(title).slice(0, 256).join("") } : {}) };
}

export function registerSessionControl(pi: Pick<ExtensionAPI, "on" | "sendUserMessage">): SessionController {
  let active: ActiveSession | undefined;
  const update = (change: (status: SessionStatus) => void): Promise<void> => {
    if (!active) return Promise.resolve();
    change(active.status);
    const current = active;
    current.status.updated_at = Date.now();
    current.writes = current.writes.then(async () => {
      if (active === current) await atomicStatus(current.statusPath, current.directory, current.status);
    }).catch(() => { current.persistenceFailed = true; });
    return current.writes;
  };
  pi.on("session_start", async (_event, context) => {
    if (active) { const previous = active; await shutdown(previous); if (active === previous) active = undefined; }
    const inherited = await verifyInheritedJob();
    if (!inherited) return;
    const sessionId = context.sessionManager.getSessionId();
    if (!sessionId) return;
    const generation = randomUUID();
    const status: SessionStatus = { protocol: controlProtocol, job_id: inherited.id, owner: inherited.owner, session_id: sessionId, generation, pid: process.pid, updated_at: Date.now(), activity: "unknown" };
    const server = createServer({ allowHalfOpen: true }, (socket) => serveConnection(socket));
    const current: ActiveSession = { ...inherited, generation, sessionId, status, statusPath: join(inherited.directory, "session.json"), server, heartbeat: setInterval(() => { void update(() => {}); }, 2000), writes: Promise.resolve(), context, stopping: false, persistenceFailed: false };
    active = current;
    server.on("error", () => { if (active === current) { current.stopping = true; void shutdown(current).then(() => { if (active === current) active = undefined; }).catch(() => { current.persistenceFailed = true; if (active === current) active = undefined; }); } });
    try {
      await new Promise<void>((resolveListen, reject) => { server.once("error", reject); server.listen(current.socketPath, resolveListen); });
      const socketInfo = await lstat(current.socketPath);
      current.socketIdentity = { dev: socketInfo.dev, ino: socketInfo.ino };
      await update(() => {});
    } catch {
      await shutdown(current);
      if (active === current) active = undefined;
      return;
    }
  });
  pi.on("agent_start", () => { void update((status) => { updateObservedActivity(status, "busy"); }); });
  pi.on("agent_settled", () => { void update((status) => { updateObservedActivity(status, "idle"); }); });
  pi.on("ui_prompt_start", (event) => { void update((status) => { updateObservedPrompt(status, observePrompt(event.kind, event.title)); }); });
  pi.on("ui_prompt_end", () => { void update((status) => { updateObservedPrompt(status, undefined); }); });
  pi.on("session_shutdown", async () => { if (active) { const current = active; await shutdown(current); if (active === current) active = undefined; } });

  function serveConnection(socket: Socket): void {
    let bytes = Buffer.alloc(0);
    let oversized = false;
    socket.on("data", (chunk) => {
      if (oversized) return;
      if (bytes.length + chunk.length > maxWireBytes) {
        oversized = true;
        bytes = Buffer.alloc(0);
        return;
      }
      bytes = Buffer.concat([bytes, chunk]);
    });
    socket.on("end", () => {
      if (oversized) { socket.end(errorResponse("invalid_request", "request too large")); return; }
      const newline = bytes.indexOf(10);
      if (newline !== bytes.length - 1 || newline < 0 || bytes.indexOf(10, newline + 1) >= 0) { socket.end(errorResponse("invalid_request", "invalid request framing")); return; }
      try { void handleRequest(socket, new TextDecoder("utf-8", { fatal: true }).decode(bytes.subarray(0, newline))); }
      catch { socket.end(errorResponse("invalid_request", "invalid request encoding")); }
    });
    socket.on("error", () => undefined);
    const deadline = setTimeout(() => socket.destroy(), 3000);
    socket.once("close", () => clearTimeout(deadline));
  }
  async function handleRequest(socket: Socket, wire: string): Promise<void> {
    let parsed: unknown;
    try { parsed = JSON.parse(wire) as unknown; } catch { socket.end(errorResponse("invalid_request", "invalid request")); return; }
    const request = parseControlRequest(parsed);
    const current = active;
    if (!request || !current || current.stopping || current.persistenceFailed) { socket.end(errorResponse("unavailable", "session control unavailable")); return; }
    let result: Awaited<ReturnType<typeof durableRequest>>;
    try { result = await durableRequest(current, request, pi.sendUserMessage); }
    catch { socket.end(errorResponse("uncertain", "request outcome uncertain", true)); return; }
    if (result.receipt) {
      const receipt = result.receipt;
      if (result.stop) socket.end(JSON.stringify({ receipt }) + "\n", () => setImmediate(() => {
        try {
          current.context.shutdown();
          void writeRequestReceipt(current, join(current.requestsPath, `${request.request_id}.json`), requestFingerprint(request), receipt).catch(() => { current.persistenceFailed = true; });
        } catch { current.persistenceFailed = true; }
      }));
      else socket.end(JSON.stringify({ receipt }) + "\n");
    } else if (result.error === "uncertain") socket.end(errorResponse("uncertain", "request outcome uncertain", true));
    else socket.end(errorResponse(result.error ?? "control_failed", result.error === "stale_target" ? "target changed" : "control request rejected"));
  }
  return {
    status: () => active ? structuredClone(active.status) : undefined,
    report: async (summary, attention) => {
      if (!active || !updateAgentReport(active.status, summary, attention)) return undefined;
      const current = active;
      current.writes = current.writes.then(() => atomicStatus(current.statusPath, current.directory, current.status)).catch(() => { current.persistenceFailed = true; });
      await current.writes;
      return current.persistenceFailed ? undefined : structuredClone(current.status);
    },
  };
}

function shutdown(current: ActiveSession): Promise<void> {
  if (current.cleanup) return current.cleanup;
  current.stopping = true;
  clearInterval(current.heartbeat);
  current.cleanup = (async () => {
    await new Promise<void>((resolveClose) => {
      if (!current.server.listening) { resolveClose(); return; }
      current.server.close(() => resolveClose());
    });
    await current.writes;
    if (current.socketIdentity) {
      try {
        const info = await lstat(current.socketPath);
        if (info.dev === current.socketIdentity.dev && info.ino === current.socketIdentity.ino) await unlink(current.socketPath);
      } catch { /* The endpoint was already removed. */ }
    }
    try {
      const before = await lstat(current.statusPath);
      const previous = await privateRecord(current.statusPath);
      const after = await lstat(current.statusPath);
      if (record(previous) && previous.generation === current.generation && before.dev === after.dev && before.ino === after.ino) await unlink(current.statusPath);
    } catch { /* No status from this generation remains. */ }
  })();
  return current.cleanup;
}

export function updateObservedActivity(status: SessionStatus, activity: SessionStatus["activity"]): void {
  status.activity = activity;
  status.updated_at = Date.now();
}
export function updateObservedPrompt(status: SessionStatus, prompt: ObservedPrompt | undefined): void {
  if (prompt) status.prompt = prompt;
  else delete status.prompt;
  status.updated_at = Date.now();
}

export function updateAgentReport(status: SessionStatus | undefined, summary: string, attention: string | null | undefined): AgentReport | undefined {
  if (!status || !validText(summary, maxSummaryBytes) || (attention !== undefined && attention !== null && !validText(attention, maxSummaryBytes))) return undefined;
  const report = { summary, ...(attention === undefined ? status.report?.attention !== undefined ? { attention: status.report.attention } : {} : attention === null ? {} : { attention }) };
  status.report = report;
  status.updated_at = Date.now();
  return report;
}
