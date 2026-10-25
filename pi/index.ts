import { spawn } from "node:child_process";
import { access, realpath } from "node:fs/promises";
import { delimiter, isAbsolute, resolve } from "node:path";
import { constants } from "node:fs";
import { Type } from "@earendil-works/pi-ai";
import { Value } from "typebox/value";
import type { Static } from "typebox";
import { defineTool, type ExtensionAPI, type ExtensionContext } from "@earendil-works/pi-coding-agent";
import { maxControlTextBytes, jobFactsSchema as jobSchema, sessionStatusSchema as sessionSchema, controlReceiptSchema as receiptSchema, sessionTargetSchema as targetSchema, stopAuthorizationSchema as authorizationSchema, type SessionTarget, type StopAuthorization } from "./control-protocol.ts";
import { registerSessionControl, type SessionController } from "./session-control.ts";
import { registerManager } from "./manager.ts";
import { communicationErrorSchema as errorSchema, communicationToolRequests, communicationResultSchemas, communicationToolRequestSchema, type CommunicationAdapter, type CommunicationResult, type CommunicationError } from "./communication-protocol.ts";

const taskLimit = 1 << 20;
const outputLimit = 1 << 20;
const deadlineMs = 30_000;

const requestSchema = Type.Object({
  request: Type.Union([
    Type.Object({ action: Type.Literal("start"), name: Type.String(), cwd: Type.String(), task: Type.String() }, { additionalProperties: false }),
    Type.Object({ action: Type.Literal("list") }, { additionalProperties: false }),
    Type.Object({ action: Type.Literal("doctor"), name: Type.String() }, { additionalProperties: false }),
    Type.Object({ action: Type.Literal("send"), id: Type.String(), text: Type.String(), request_id: Type.String() }, { additionalProperties: false }),
    Type.Object({ action: Type.Literal("report"), summary: Type.String(), attention: Type.Optional(Type.Union([Type.String(), Type.Null()])) }, { additionalProperties: false }),
    Type.Object({ action: Type.Literal("stop"), id: Type.String() }, { additionalProperties: false }),
    Type.Object({ action: Type.Literal("force_stop"), id: Type.String() }, { additionalProperties: false }),
    ...communicationToolRequests,
  ]),
}, { additionalProperties: false });

const listSchema = Type.Object({ jobs: Type.Array(jobSchema) }, { additionalProperties: false });
const reportedSchema = Type.Object({ reported: sessionSchema }, { additionalProperties: false });
const resultSchema = Type.Union([jobSchema, listSchema, errorSchema, receiptSchema, reportedSchema, ...communicationResultSchemas]);

type Request = Static<typeof requestSchema>["request"];
type ExistingRequest = Extract<Request, { action: "start" | "list" | "doctor" }>;
type Result = Static<typeof jobSchema> | Static<typeof listSchema> | Static<typeof errorSchema> | Static<typeof receiptSchema> | Static<typeof reportedSchema>;

export function cliArguments(request: ExistingRequest, cwd: string): { argv: string[]; cwd: string; input?: string } {
  switch (request.action) {
    case "start":
      return { argv: ["start", request.name, "--cwd", resolve(cwd, request.cwd), "--task-file", "-", "--json"], cwd, input: request.task };
    case "list":
      return { argv: ["list", "--session-info", "--json"], cwd };
    case "doctor":
      return { argv: ["doctor", request.name, "--session-info", "--json"], cwd };
  }
}

async function findExecutable(): Promise<string> {
  for (const directory of (process.env.PATH ?? "").split(delimiter)) {
    if (!directory) continue;
    const candidate = resolve(directory, "agent-coordinator");
    try {
      await access(candidate, constants.X_OK);
      const path = await realpath(candidate);
      if (isAbsolute(path)) return path;
    } catch { /* Continue through trusted PATH entries. */ }
  }
  throw new Error("agent-coordinator was not found on PATH; install the coordinator CLI first.");
}

interface CliResult { code: number; stdout: string; }

class CliRunnerError extends Error {
  spawned: boolean;

  constructor(message: string, spawned: boolean) {
    super(message);
    this.spawned = spawned;
  }
}

export function runCli(executable: string, args: string[], cwd: string, input: string | undefined, signal?: AbortSignal, timeoutMs = deadlineMs): Promise<CliResult> {
  if (signal?.aborted) return Promise.reject(new CliRunnerError("Coordinator request was cancelled before launch.", false));
  return new Promise((resolvePromise, rejectPromise) => {
    const child = spawn(executable, args, { cwd, env: process.env, shell: false, stdio: ["pipe", "pipe", "pipe"] });
    let stdout = "";
    let outputBytes = 0;
    let failure: CliRunnerError | undefined;
    let spawned = false;
    let exited = false;
    let exitCode: number | null = null;
    let settled = false;
    let killTimer: NodeJS.Timeout | undefined;
    let reapTimer: NodeJS.Timeout | undefined;
    let timer: NodeJS.Timeout | undefined;

    const closeStreams = () => {
      child.stdin.destroy();
      child.stdout.destroy();
      child.stderr.destroy();
    };
    const finish = (cleanupError?: CliRunnerError) => {
      if (settled) return;
      settled = true;
      if (cleanupError) failure = cleanupError;
      clearTimeout(timer);
      clearTimeout(killTimer);
      clearTimeout(reapTimer);
      signal?.removeEventListener("abort", onAbort);
      if (failure) rejectPromise(failure);
      else resolvePromise({ code: exitCode ?? 1, stdout });
    };
    const terminate = (reason: CliRunnerError) => {
      if (failure) return;
      failure = reason;
      if (exited) {
        closeStreams();
        return;
      }
      child.kill("SIGTERM");
      killTimer = setTimeout(() => {
        if (exited) return;
        child.kill("SIGKILL");
        reapTimer = setTimeout(() => {
          if (!exited) finish(new CliRunnerError("Coordinator CLI could not be reaped after SIGKILL; cancellation cleanup failed.", spawned));
        }, 1000);
      }, 1000);
    };
    const onAbort = () => terminate(new CliRunnerError("Coordinator request cancelled after submission; inspect list/doctor before retrying.", spawned));
    signal?.addEventListener("abort", onAbort, { once: true });
    timer = setTimeout(() => terminate(new CliRunnerError("Coordinator request timed out after submission; inspect list/doctor before retrying.", spawned)), timeoutMs);
    child.on("spawn", () => {
      spawned = true;
      if (failure) failure.spawned = true;
    });
    child.on("error", () => terminate(new CliRunnerError("Could not start or continue coordinator CLI.", spawned)));
    child.on("exit", (code) => {
      exited = true;
      exitCode = code;
      clearTimeout(killTimer);
      clearTimeout(reapTimer);
      if (failure) closeStreams();
    });
    for (const stream of [child.stdout, child.stderr]) {
      stream.on("data", (chunk: Buffer) => {
        outputBytes += chunk.length;
        if (outputBytes > outputLimit) terminate(new CliRunnerError("Coordinator output exceeded 1 MiB; inspect list/doctor before retrying.", spawned));
        else if (stream === child.stdout) stdout += chunk.toString("utf8");
      });
    }
    child.stdin.on("error", (error: NodeJS.ErrnoException) => {
      const message = error.code === "EPIPE" ? "Coordinator CLI closed stdin before accepting the request; inspect list/doctor before retrying." : "Could not deliver request text to coordinator CLI.";
      terminate(new CliRunnerError(message, spawned));
    });
    child.on("close", () => {
      if (!exited && failure && spawned) finish(new CliRunnerError("Coordinator CLI closed without a reapable exit status; cancellation cleanup failed.", spawned));
      else finish();
    });
    child.stdin.end(input, "utf8");
  });
}

export function parseResult(stdout: string, action: ExistingRequest["action"]): Result {
  let parsed: unknown;
  try { parsed = JSON.parse(stdout); } catch { throw new Error("Coordinator returned invalid JSON."); }
  if (!Value.Check(resultSchema, parsed)) throw new Error("Coordinator returned JSON that does not match its response contract.");
  if ("error" in parsed) return parsed;
  if (action === "start" || action === "doctor") {
    if (!("launch" in parsed)) throw new Error("Coordinator returned the wrong response for this action.");
  } else if (!("jobs" in parsed)) throw new Error("Coordinator returned the wrong response for this action.");
  return parsed;
}

function toolOutput(result: Result | CommunicationResult | CommunicationError, isError = false) {
  return { content: [{ type: "text" as const, text: JSON.stringify(result) }], details: result, structuredContent: result, ...(isError ? { isError: true } : {}) };
}
function errorResult(code: string, message: string, uncertain = false, id?: string): Static<typeof errorSchema> {
  return { error: { code, message, ...(id ? { id } : {}), uncertain } };
}
const uuid = /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;
function validLiteral(text: string): boolean {
  return Buffer.byteLength(text, "utf8") > 0 && Buffer.byteLength(text, "utf8") <= maxControlTextBytes && text.trim() !== "" && !/[\uD800-\uDBFF](?![\uDC00-\uDFFF])|(?<![\uD800-\uDBFF])[\uDC00-\uDFFF]/u.test(text);
}
function decode(stdout: string): unknown {
  try { return JSON.parse(stdout) as unknown; } catch { throw new Error("Coordinator returned invalid JSON."); }
}
async function resolveTarget(executable: string, id: string, cwd: string, signal?: AbortSignal): Promise<SessionTarget | Static<typeof errorSchema>> {
  const outcome = await runCli(executable, ["_target", id, "--json"], cwd, undefined, signal);
  const parsed = decode(outcome.stdout);
  if (outcome.code !== 0) {
    if (Value.Check(errorSchema, parsed)) return parsed;
    throw new Error("Coordinator returned an invalid target error.");
  }
  if (!Value.Check(targetSchema, parsed)) throw new Error("Coordinator returned an invalid session target.");
  return parsed;
}
function isErrorResult(value: SessionTarget | Static<typeof errorSchema>): value is Static<typeof errorSchema> { return "error" in value; }
function sameTarget(left: SessionTarget, right: SessionTarget): boolean {
  return left.id === right.id && left.session_id === right.session_id && left.generation === right.generation && left.pane.pid === right.pane.pid && left.pane.uid === right.pane.uid && left.pane.boot_id === right.pane.boot_id && left.pane.start_sec === right.pane.start_sec && left.pane.start_usec === right.pane.start_usec;
}
async function executeControl(request: Extract<Request, { action: "send" | "report" | "stop" | "force_stop" }>, signal: AbortSignal | undefined, ctx: ExtensionContext, controller: SessionController | undefined): Promise<ReturnType<typeof toolOutput>> {
  if (request.action === "report") {
    const reported = await controller?.report(request.summary, request.attention);
    if (!reported) return toolOutput(errorResult("session_unavailable", "verified calling session is unavailable"), true);
    const result: Result = { reported };
    return toolOutput(result);
  }
  if (!uuid.test(request.id)) return toolOutput(errorResult("invalid_id", "id must be a job UUID"), true);
  if (request.action === "send" && (!uuid.test(request.request_id) || !validLiteral(request.text))) return toolOutput(errorResult("invalid_request", "request id or literal text is invalid"), true);
  const stopping = request.action === "stop" || request.action === "force_stop";
  if (stopping && (!ctx.hasUI || signal?.aborted)) return toolOutput(errorResult("confirmation_unavailable", "stop requires an active UI and confirmation"), true);
  const executable = await findExecutable();
  const target = await resolveTarget(executable, request.id, ctx.cwd, signal);
  if (isErrorResult(target)) return toolOutput(target, true);
  if (target.id !== request.id || target.session_id.length === 0 || !uuid.test(target.generation)) return toolOutput(errorResult("invalid_target", "coordinator returned a mismatched session target"), true);
  if (request.action === "send") {
    let submitted = false;
    try {
      const outcome = await runCli(executable, ["send", target.id, "--session-id", target.session_id, "--generation", target.generation, "--request-id", request.request_id, "--json"], ctx.cwd, request.text, signal);
      submitted = true;
      const parsed = decode(outcome.stdout);
      if (outcome.code !== 0 && Value.Check(errorSchema, parsed)) return toolOutput(parsed, true);
      if (!Value.Check(receiptSchema, parsed) || parsed.id !== target.id || parsed.session_id !== target.session_id || parsed.generation !== target.generation || parsed.request_id !== request.request_id || parsed.operation !== "send") return toolOutput(errorResult("uncertain", "send result is invalid after submission", true, target.id), true);
      if (outcome.code === 75) return toolOutput(errorResult("uncertain", "send outcome uncertain; do not retry automatically", true, target.id), true);
      if (outcome.code !== 0) return toolOutput(Value.Check(errorSchema, parsed) ? parsed : errorResult("send_failed", "coordinator rejected send", false, target.id), true);
      return toolOutput(parsed);
    } catch (error) {
      const uncertain = submitted || (error instanceof CliRunnerError && error.spawned);
      return toolOutput(errorResult(uncertain ? "uncertain" : "coordinator_error", uncertain ? "send outcome uncertain; do not retry automatically" : "send was not submitted", uncertain, target.id), true);
    }
  }
  const operation = request.action;
  const initiatingSession = ctx.sessionManager.getSessionId();
  const title = operation === "force_stop" ? "Force-stop Pi session?" : "Stop Pi session?";
  const message = operation === "force_stop"
    ? `Force-stop job ${target.id} (${target.session_id}) with SIGKILL? Writes may be incomplete and detached children may survive. This cannot be undone.`
    : `Request graceful native shutdown of job ${target.id} (${target.session_id})?`;
  let confirmed = false;
  try { confirmed = await ctx.ui.confirm(title, message); } catch { return toolOutput(errorResult("confirmation_cancelled", "stop confirmation was cancelled"), true); }
  if (!confirmed || signal?.aborted || ctx.sessionManager.getSessionId() !== initiatingSession) return toolOutput(errorResult("confirmation_cancelled", "stop confirmation was denied or session changed"), true);
  let stopSubmitted = false;
  try {
    const prepared = await runCli(executable, ["_prepare-stop", target.id, "--session-id", target.session_id, "--generation", target.generation, ...(operation === "force_stop" ? ["--force"] : []), "--json"], ctx.cwd, undefined, signal);
    const preparedValue = decode(prepared.stdout);
    if (prepared.code !== 0 || !Value.Check(authorizationSchema, preparedValue)) return toolOutput(Value.Check(errorSchema, preparedValue) ? preparedValue : errorResult("stop_rejected", "stop authorization was rejected"), true);
    const authorization: StopAuthorization = preparedValue;
    if (authorization.operation !== operation || authorization.expires_at <= Date.now() || !sameTarget(authorization.target, target) || authorization.token.length < 32) return toolOutput(errorResult("stop_rejected", "stop authorization did not match the confirmed target"), true);
    if (signal?.aborted || ctx.sessionManager.getSessionId() !== initiatingSession) return toolOutput(errorResult("confirmation_cancelled", "initiating session changed before stop"), true);
    const confirmedStop = await runCli(executable, ["_confirmed-stop", "--json"], ctx.cwd, `${JSON.stringify(authorization)}\n`, signal);
    stopSubmitted = true;
    const parsed = decode(confirmedStop.stdout);
    if (confirmedStop.code === 75) return toolOutput(errorResult("uncertain", "stop outcome uncertain; do not retry automatically", true, target.id), true);
    if (confirmedStop.code !== 0 && Value.Check(errorSchema, parsed)) return toolOutput(parsed, true);
    if (!Value.Check(receiptSchema, parsed) || parsed.id !== target.id || parsed.session_id !== target.session_id || parsed.generation !== target.generation || parsed.operation !== operation) return toolOutput(errorResult("uncertain", "stop result is invalid after submission", true, target.id), true);
    if (confirmedStop.code !== 0) return toolOutput(Value.Check(errorSchema, parsed) ? parsed : errorResult("stop_failed", "coordinator rejected stop", false, target.id), true);
    return toolOutput(parsed);
  } catch (error) {
    const uncertain = stopSubmitted || (error instanceof CliRunnerError && error.spawned);
    return toolOutput(errorResult(uncertain ? "uncertain" : "coordinator_error", uncertain ? "stop outcome uncertain; do not retry automatically" : "stop was not submitted", uncertain, target.id), true);
  }
}

function createCoordinatorTool(controller?: SessionController, manager?: CommunicationAdapter) {
  return defineTool({
  name: "coordinator",
  label: "Coordinator",
  description: "Start, list, and inspect sessions; steer one session, report this session's status, or explicitly confirm a session stop. Receipts do not imply delivery or task success; uncertain mutations must not be retried automatically.",
  parameters: requestSchema,
  outputSchema: resultSchema,
  async execute(_toolCallId, params, signal, _onUpdate, ctx) {
    if (!Value.Check(requestSchema, params)) throw new Error("Invalid coordinator request.");
    const request: Request = params.request;
    if (request.action === "start" && Buffer.byteLength(request.task, "utf8") > taskLimit) return toolOutput(errorResult("invalid_task", "Task text exceeds the 1 MiB coordinator limit."), true);
    if (request.action === "send" || request.action === "report" || request.action === "stop" || request.action === "force_stop") return executeControl(request, signal, ctx, controller);
    if (Value.Check(communicationToolRequestSchema, request)) {
      const result = manager ? await manager.execute(request, signal, ctx) : errorResult("session_unavailable", "communication requires an active verified adapter");
      return toolOutput(result, "error" in result);
    }
    let cliSpawned = false;
    try {
      const executable = await findExecutable();
      const invocation = cliArguments(request, ctx.cwd);
      const managedStart = request.action === "start" && manager ? await manager.start(request, signal, ctx) : { managed: false as const };
      let outcome = managedStart.managed ? { code: managedStart.code, stdout: managedStart.stdout } : await runCli(executable, invocation.argv, invocation.cwd, invocation.input, signal);
      cliSpawned = true;
      if ((request.action === "list" || request.action === "doctor") && outcome.code === 2) {
        let legacyFallback = false;
        try {
          const error = decode(outcome.stdout);
          legacyFallback = Value.Check(errorSchema, error) && error.error.code === "invalid_usage";
        } catch { /* Malformed responses never trigger a compatibility retry. */ }
        if (legacyFallback) {
          const legacyArgs = invocation.argv.filter((argument) => argument !== "--session-info");
          outcome = await runCli(executable, legacyArgs, invocation.cwd, undefined, signal);
        }
      }
      const result = parseResult(outcome.stdout, request.action);
      if (outcome.code === 75) {
        const known = "error" in result ? result.error : undefined;
        const uncertain = errorResult("uncertain", "Launch may have been submitted; inspect list/doctor before retrying.", true, known?.id);
        return toolOutput({ error: { code: uncertain.error.code, message: uncertain.error.message, ...(uncertain.error.id ? { id: uncertain.error.id } : {}), ...(request.action === "start" ? { name: request.name } : {}), uncertain: true } }, true);
      }
      if (outcome.code !== 0 || "error" in result) return toolOutput(result, true);
      return toolOutput(result);
    } catch (error) {
      const uncertain = request.action === "start" && (cliSpawned || (error instanceof CliRunnerError && error.spawned));
      const message = error instanceof Error ? error.message : "Coordinator CLI failed.";
      const result: Result = { error: { code: uncertain ? "uncertain" : "coordinator_error", message: uncertain && !message.includes("inspect list/doctor") ? `${message} Inspect list/doctor before retrying.` : message, ...(request.action === "start" ? { name: request.name } : {}), uncertain } };
      return toolOutput(result, true);
    }
  },
  });
}

export const coordinatorTool = createCoordinatorTool();

export function registerCoordinator(pi: Pick<ExtensionAPI, "registerTool">, controller?: SessionController, manager?: CommunicationAdapter): void {
  pi.registerTool(controller || manager ? createCoordinatorTool(controller, manager) : coordinatorTool);
}

export default function (pi: ExtensionAPI): void {
  const controller = registerSessionControl(pi);
  const manager = registerManager(pi, controller, { findExecutable, runCli });
  registerCoordinator(pi, controller, manager);
}
