import { createHash, randomUUID } from "node:crypto";
import { constants } from "node:fs";
import { lstat, mkdir, open, realpath, rename, unlink } from "node:fs/promises";
import { createServer, type Server, type Socket } from "node:net";
import { dirname, join, resolve } from "node:path";
import type { ExtensionAPI, ExtensionContext } from "@earendil-works/pi-coding-agent";
import { Value } from "typebox/value";
import { controlProtocol, controlRequestSchema, maxControlTextBytes, type AgentReport, type ControlReceipt, type ControlRequest, type ObservedPrompt, type ProcessIdentity, type SessionStatus } from "./control-protocol.ts";

const maxWireBytes = 512 * 1024;
const maxSummaryBytes = 1024;
const uuidPattern = /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;
const nativeSessionIdPattern = /^[A-Za-z0-9](?:[A-Za-z0-9._-]*[A-Za-z0-9])?$/;
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
  protocol: "1" | "2";
  pane: ProcessIdentity;
  nativeSession?: NativeSessionObservation;
}

export interface SessionController {
  publishStatus(): Promise<SessionStatus | undefined>;
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

async function pathMissing(path: string): Promise<boolean> {
  try { await lstat(path); return false; }
  catch (error) { return record(error) && error.code === "ENOENT"; }
}
interface SessionHeaderObservation { id: string; device: number; inode: number }
interface NativeSessionObservation {
  schema: 1;
  job_id: string;
  owner: string;
  session_id: string;
  generation: string;
  process: JsonRecord;
  updated_at: string;
  session_file: string;
  cwd: string;
  state: "unpersisted" | "persisted";
  file?: { device: number; inode: number };
  header_id?: string;
}
async function supportedSessionHeader(path: string): Promise<SessionHeaderObservation | undefined> {
  let before;
  try { before = await lstat(path); }
  catch (error) { if (record(error) && error.code === "ENOENT") return undefined; throw error; }
  if (before.isSymbolicLink()) throw new Error("native session path is a symbolic link");
  const handle = await open(path, constants.O_RDONLY | constants.O_NOFOLLOW | constants.O_NONBLOCK);
  try {
    const info = await handle.stat();
    if (info.dev !== before.dev || info.ino !== before.ino) throw new Error("native session changed while opening");
    if (!info.isFile() || info.uid !== currentUid() || (info.mode & 0o022) !== 0 || info.nlink !== 1) throw new Error("native session file is not integrity-safe");
    const buffer = Buffer.alloc(4096);
    const { bytesRead } = await handle.read(buffer, 0, buffer.length, 0);
    const end = buffer.subarray(0, bytesRead).indexOf(10);
    if (end < 0) throw new Error("native session header is incomplete");
    let header: unknown;
    try { header = JSON.parse(buffer.toString("utf8", 0, end)); } catch { throw new Error("native session header is invalid"); }
    if (!record(header) || header.type !== "session" || header.version !== 3 || typeof header.id !== "string" || !nativeSessionIdPattern.test(header.id)) throw new Error("native session header is unsupported");
    const after = await lstat(path);
    if (after.dev !== info.dev || after.ino !== info.ino || after.isSymbolicLink()) throw new Error("native session path changed while observed");
    return { id: header.id, device: info.dev, inode: info.ino };
  } finally { await handle.close(); }
}
async function observeNativeSession(current: ActiveSession): Promise<NativeSessionObservation> {
  const file = current.context.sessionManager.getSessionFile();
  if (!file) throw new Error("native session path unavailable");
  const parent = await realpath(dirname(file));
  const sessionFile = await realpath(file).catch((error: unknown) => {
    if (record(error) && error.code === "ENOENT") return join(parent, file.slice(file.lastIndexOf("/") + 1));
    throw error;
  });
  const sessionId = current.context.sessionManager.getSessionId();
  if (!sessionId || sessionId !== current.sessionId) throw new Error("native session identity changed");
  const native: NativeSessionObservation = { schema: 1, job_id: current.status.job_id, owner: current.status.owner, session_id: sessionId, generation: current.generation, process: current.pane, updated_at: new Date().toISOString(), session_file: sessionFile, cwd: current.context.cwd, state: "unpersisted" };
  const header = await supportedSessionHeader(sessionFile);
  if (header) {
    if (header.id !== sessionId) throw new Error("native session header changed");
    native.state = "persisted";
    native.file = { device: header.device, inode: header.inode };
    native.header_id = header.id;
  }
  return native;
}
function validNativeObservation(value: unknown): value is NativeSessionObservation {
  if (!record(value) || value.schema !== 1 || typeof value.job_id !== "string" || !uuidPattern.test(value.job_id) || typeof value.owner !== "string" || !/^[0-9a-f]{16}$/.test(value.owner) || typeof value.session_id !== "string" || !nativeSessionIdPattern.test(value.session_id) || typeof value.generation !== "string" || !uuidPattern.test(value.generation) || !validProcessIdentity(value.process) || typeof value.updated_at !== "string" || !Number.isFinite(Date.parse(value.updated_at)) || typeof value.session_file !== "string" || !value.session_file.startsWith("/") || typeof value.cwd !== "string" || !value.cwd.startsWith("/")) return false;
  const common = ["schema", "job_id", "owner", "session_id", "generation", "process", "updated_at", "session_file", "cwd", "state"];
  if (value.state === "unpersisted") return exactKeys(value, common);
  return value.state === "persisted" && exactKeys(value, [...common, "file", "header_id"]) && record(value.file) && exactKeys(value.file, ["device", "inode"]) && [value.file.device, value.file.inode].every((part) => typeof part === "number" && Number.isSafeInteger(part) && part >= 0) && value.header_id === value.session_id;
}
function sameHolderTuple(left: NativeSessionObservation, right: NativeSessionObservation): boolean {
  return left.session_file === right.session_file && left.session_id === right.session_id && left.process.boot_id === right.process.boot_id && left.process.pid === right.process.pid && left.process.uid === right.process.uid && left.process.start_sec === right.process.start_sec && left.process.start_usec === right.process.start_usec && left.file?.device === right.file?.device && left.file?.inode === right.file?.inode;
}
async function publishNativeSession(current: ActiveSession): Promise<void> {
  const observation = await observeNativeSession(current);
  const historyPath = join(current.directory, "native-session-history.json");
  let observations: NativeSessionObservation[] = [];
  if (!(await pathMissing(historyPath))) {
    const history = await privateRecord(historyPath);
    if (!record(history) || !exactKeys(history, ["schema", "observations"]) || history.schema !== 1 || !Array.isArray(history.observations) || history.observations.length > 128 || !history.observations.every(validNativeObservation)) throw new Error("native session history is invalid");
    observations = history.observations;
  }
  if (!observations.some((previous) => sameHolderTuple(previous, observation))) {
    if (observations.length >= 128) throw new Error("native session history limit reached");
    observations.push(observation);
  }
  const history = { schema: 1, observations };
  if (Buffer.byteLength(`${JSON.stringify(history)}\n`, "utf8") > 512 * 1024) throw new Error("native session history size limit reached");
  await atomicJson(historyPath, current.directory, history, "native-session-history");
  await atomicJson(join(current.directory, "native-session.json"), current.directory, observation, "native-session");
  current.nativeSession = observation;
}
function validManagedObservation(value: unknown): boolean {
  if (!record(value) || !exactKeys(value, ["Launcher", "Current", "Package", "BinLink", "Entry", "Version"]) || value.Version !== "1.4.0") return false;
  const validFile = (file: unknown): boolean => record(file) && exactKeys(file, ["Path", "Hash", "Size", "Mode", "UID", "Dev", "Ino"]) && typeof file.Path === "string" && file.Path.startsWith("/") && typeof file.Hash === "string" && /^[0-9a-f]{64}$/.test(file.Hash) && [file.Size, file.Mode, file.UID, file.Dev, file.Ino].every((field) => typeof field === "number" && Number.isSafeInteger(field) && field >= 0);
  const link = value.BinLink;
  return validFile(value.Launcher) && validFile(value.Current) && validFile(value.Package) && validFile(value.Entry) && record(link) && exactKeys(link, ["Path", "Target", "UID", "Dev", "Ino"]) && typeof link.Path === "string" && link.Path.startsWith("/") && typeof link.Target === "string" && [link.UID, link.Dev, link.Ino].every((field) => typeof field === "number" && Number.isSafeInteger(field) && field >= 0);
}
async function privateDirectory(path: string): Promise<boolean> {
  try {
    const info = await lstat(path);
    const uid = currentUid();
    return uid !== undefined && info.isDirectory() && !info.isSymbolicLink() && info.uid === uid && (info.mode & 0o777) === 0o700;
  } catch { return false; }
}
function validProcessIdentity(value: unknown, pid?: number): value is ProcessIdentity {
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
async function atomicJson(path: string, directory: string, value: unknown, prefix: string): Promise<void> {
  const temp = join(directory, `.${prefix}-${randomUUID()}`);
  const handle = await open(temp, constants.O_WRONLY | constants.O_CREAT | constants.O_EXCL | constants.O_NOFOLLOW, 0o600);
  try { await handle.writeFile(`${JSON.stringify(value)}\n`); await handle.sync(); }
  catch (error) { await handle.close(); await unlink(temp).catch(() => undefined); throw error; }
  await handle.close();
  await rename(temp, path);
  await syncDirectory(directory);
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

export async function verifyInheritedJob(): Promise<{ id: string; owner: string; socketPath: string; directory: string; requestsPath: string; protocol: "1" | "2"; pane: ProcessIdentity } | undefined> {
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
    if (!record(ownerRecord) || !exactKeys(ownerRecord, ["schema", "token"]) || ownerRecord.schema !== 1 || ownerRecord.token !== owner || !record(manifest) || !exactKeys(manifest, manifestKeys) || manifest.schema !== 1 || manifest.id !== id || typeof manifest.name !== "string" || !/^[a-z0-9][a-z0-9-]{0,47}$/.test(manifest.name) || manifest.owner !== owner || (manifest.protocol !== "1" && manifest.protocol !== "2") || typeof manifest.cwd !== "string" || !manifest.cwd.startsWith("/") || typeof manifest.pi !== "string" || !manifest.pi.startsWith("/") || typeof manifest.tmux !== "string" || !manifest.tmux.startsWith("/") || typeof manifest.coordinator !== "string" || !manifest.coordinator.startsWith("/") || typeof manifest.created !== "string" || !Number.isFinite(Date.parse(manifest.created)) || manifest.boot_id_scheme !== bootIdScheme || typeof manifest.boot_id !== "string" || !bootIdPattern.test(manifest.boot_id) || typeof manifest.socket !== "string" || !record(launch) || Object.keys(launch).some((key) => !["schema", "phase", "server", "pane", "error", "retired"].includes(key)) || launch.schema !== 1 || launch.phase !== "exec-intent" || launch.retired === true || !validProcessIdentity(launch.pane, process.pid) || !validProcessIdentity(launch.server) || launch.pane.boot_id !== manifest.boot_id || launch.server.boot_id !== manifest.boot_id) return undefined;
    if (manifest.protocol === "2") {
      const intent = await privateRecord(join(directory, "intent.json"));
      const roleRecord = await privateRecord(join(directory, "role.json"));
      if (!record(intent) || !exactKeys(intent, ["schema", "kind", "terminal_profile", "argv_sha256", "native_pi"]) || intent.schema !== 1 || intent.kind !== "interactive" || intent.terminal_profile !== "transparent-v1" || typeof intent.argv_sha256 !== "string" || !/^[0-9a-f]{64}$/.test(intent.argv_sha256) || !record(intent.native_pi) || !exactKeys(intent.native_pi, ["Executable", "Observation"]) || typeof intent.native_pi.Executable !== "string" || !intent.native_pi.Executable.startsWith("/") || !validManagedObservation(intent.native_pi.Observation) || !record(roleRecord) || !exactKeys(roleRecord, ["schema", "role"]) || roleRecord.schema !== 1 || (roleRecord.role !== "standalone" && roleRecord.role !== "manager-host") || !(await pathMissing(join(directory, "initial-task.txt"))) || !(await pathMissing(join(directory, "launch-argv.json")))) return undefined;
    } else if (!(await pathMissing(join(directory, "intent.json")))) return undefined;
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
    return { id, owner, socketPath, directory, requestsPath, protocol: manifest.protocol, pane: launch.pane };
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
      if (active === current && !current.persistenceFailed && !current.stopping) {
        if (current.protocol === "2") await publishNativeSession(current);
        if (!current.persistenceFailed && !current.stopping) await atomicStatus(current.statusPath, current.directory, current.status);
      }
    }).catch(async () => { current.persistenceFailed = true; current.stopping = true; await invalidateStatus(current); });
    return current.writes;
  };
  pi.on("session_start", async (_event, context) => {
    if (active) { const previous = active; await shutdown(previous); if (active === previous) active = undefined; }
    const inherited = await verifyInheritedJob();
    if (!inherited) return;
    const sessionId = context.sessionManager.getSessionId();
    if (!sessionId) return;
    const generation = randomUUID();
    const status: SessionStatus = { protocol: controlProtocol, job_id: inherited.id, owner: inherited.owner, session_id: sessionId, generation, pid: process.pid, updated_at: Date.now(), activity: "unknown", ...(inherited.protocol === "2" ? { launch_protocol: "2" as const } : {}) };
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
  pi.on("session_before_switch", async (event) => {
    if (active?.protocol === "2" && event.reason === "resume") return { cancel: true };
  });
  pi.on("session_before_fork", async (_event, context) => {
    const current = active;
    if (current?.protocol !== "2") return;
    const source = context.sessionManager.getSessionFile();
    const currentSource = current.context.sessionManager.getSessionFile();
    if (!source || !currentSource || resolve(source) !== resolve(currentSource)) return { cancel: true };
    try {
      const observed = current.nativeSession;
      const resolvedSource = await realpath(source);
      const sourceHeader = await supportedSessionHeader(source);
      if (!observed || observed.state !== "persisted" || observed.session_file !== resolvedSource || observed.session_id !== current.sessionId || sourceHeader?.id !== current.sessionId || sourceHeader.device !== observed.file?.device || sourceHeader.inode !== observed.file?.inode || current.context.sessionManager.getSessionId() !== current.sessionId) {
        if (active === current) { current.persistenceFailed = true; current.stopping = true; await invalidateStatus(current); }
        return { cancel: true };
      }
    } catch {
      if (active === current) { current.persistenceFailed = true; current.stopping = true; await invalidateStatus(current); }
      return { cancel: true };
    }
  });
  pi.on("session_shutdown", async () => { if (active) { active.stopping = true; const current = active; await shutdown(current); if (active === current) active = undefined; } });

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
    publishStatus: async () => {
      const current = active;
      if (!current || current.persistenceFailed || current.stopping) return undefined;
      await update(() => {});
      return active === current && !current.persistenceFailed && !current.stopping ? structuredClone(current.status) : undefined;
    },
    status: () => active && !active.persistenceFailed && !active.stopping ? structuredClone(active.status) : undefined,
    report: async (summary, attention) => {
      if (!active || active.persistenceFailed || active.stopping || !updateAgentReport(active.status, summary, attention)) return undefined;
      const current = active;
      await update(() => {});
      return active === current && !current.persistenceFailed && !current.stopping ? structuredClone(current.status) : undefined;
    },
  };
}

async function invalidateStatus(current: ActiveSession): Promise<void> {
  try {
    const before = await lstat(current.statusPath);
    const value = await privateRecord(current.statusPath);
    const after = await lstat(current.statusPath);
    if (record(value) && value.generation === current.generation && before.dev === after.dev && before.ino === after.ino) await unlink(current.statusPath);
  } catch { /* A replacement status or missing path is not ours to remove. */ }
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
