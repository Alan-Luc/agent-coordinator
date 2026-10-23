import { spawn } from "node:child_process";
import { access, realpath } from "node:fs/promises";
import { delimiter, isAbsolute, resolve } from "node:path";
import { constants } from "node:fs";
import { Type } from "@earendil-works/pi-ai";
import { Value } from "typebox/value";
import type { Static } from "typebox";
import { defineTool, type ExtensionAPI } from "@earendil-works/pi-coding-agent";

const taskLimit = 1 << 20;
const outputLimit = 1 << 20;
const deadlineMs = 30_000;

const requestSchema = Type.Object({
  request: Type.Union([
    Type.Object({ action: Type.Literal("start"), name: Type.String(), cwd: Type.String(), task: Type.String() }, { additionalProperties: false }),
    Type.Object({ action: Type.Literal("list") }, { additionalProperties: false }),
    Type.Object({ action: Type.Literal("doctor"), name: Type.String() }, { additionalProperties: false }),
  ]),
}, { additionalProperties: false });

const jobSchema = Type.Object({
  id: Type.String(), name: Type.String(), launch: Type.String(), terminal: Type.String(),
  pi_readiness: Type.Literal("unverified"), task_outcome: Type.Literal("unknown"), clients: Type.Integer({ minimum: 0 }),
  retired: Type.Boolean(), next_action: Type.String(), exit_status: Type.Optional(Type.Integer()),
}, { additionalProperties: false });
const listSchema = Type.Object({ jobs: Type.Array(jobSchema) }, { additionalProperties: false });
const errorSchema = Type.Object({ error: Type.Object({ code: Type.String(), message: Type.String(), id: Type.Optional(Type.String()), name: Type.Optional(Type.String()), uncertain: Type.Boolean() }, { additionalProperties: false }) }, { additionalProperties: false });
const resultSchema = Type.Union([jobSchema, listSchema, errorSchema]);

type Request = Static<typeof requestSchema>["request"];
type Job = Static<typeof jobSchema>;
type Result = Job | Static<typeof listSchema> | Static<typeof errorSchema>;

export function cliArguments(request: Request, cwd: string): { argv: string[]; cwd: string; input?: string } {
  switch (request.action) {
    case "start":
      return { argv: ["start", request.name, "--cwd", resolve(cwd, request.cwd), "--task-file", "-", "--json"], cwd, input: request.task };
    case "list":
      return { argv: ["list", "--json"], cwd };
    case "doctor":
      return { argv: ["doctor", request.name, "--json"], cwd };
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

export function parseResult(stdout: string, action: Request["action"]): Result {
  let parsed: unknown;
  try { parsed = JSON.parse(stdout); } catch { throw new Error("Coordinator returned invalid JSON."); }
  if (!Value.Check(resultSchema, parsed)) throw new Error("Coordinator returned JSON that does not match its response contract.");
  if ("error" in parsed) return parsed;
  if (action === "start" || action === "doctor") {
    if ("jobs" in parsed) throw new Error("Coordinator returned the wrong response for this action.");
  } else if (!("jobs" in parsed)) throw new Error("Coordinator returned the wrong response for this action.");
  return parsed;
}

function asJson(value: Result): string { return JSON.stringify(value); }

export const coordinatorTool = defineTool({
  name: "coordinator",
  label: "Coordinator",
  description: "Start a named Pi session, list sessions, or inspect one session. A timed-out or uncertain start must be checked with list/doctor before retrying.",
  parameters: requestSchema,
  outputSchema: resultSchema,
  async execute(_toolCallId, params, signal, _onUpdate, ctx) {
    if (!Value.Check(requestSchema, params)) throw new Error("Invalid coordinator request.");
    const request: Request = params.request;
    if (request.action === "start" && Buffer.byteLength(request.task, "utf8") > taskLimit) {
      throw new Error("Task text exceeds the 1 MiB coordinator limit.");
    }
    let cliSpawned = false;
    try {
      const executable = await findExecutable();
      const invocation = cliArguments(request, ctx.cwd);
      const outcome = await runCli(executable, invocation.argv, invocation.cwd, invocation.input, signal);
      cliSpawned = true;
      const result = parseResult(outcome.stdout, request.action);
      if (outcome.code === 75) {
        const known = "error" in result ? result.error : undefined;
        const uncertain = { error: { code: "uncertain", message: "Launch may have been submitted; inspect list/doctor before retrying.", ...(known?.id ? { id: known.id } : {}), ...(request.action === "start" ? { name: request.name } : {}), uncertain: true } };
        return { content: [{ type: "text", text: asJson(uncertain) }], details: uncertain, structuredContent: uncertain, isError: true };
      }
      if (outcome.code !== 0 || "error" in result) {
        return { content: [{ type: "text", text: asJson(result) }], details: result, structuredContent: result, isError: true };
      }
      return { content: [{ type: "text", text: asJson(result) }], details: result, structuredContent: result };
    } catch (error) {
      const uncertain = request.action === "start" && (cliSpawned || (error instanceof CliRunnerError && error.spawned));
      const message = error instanceof Error ? error.message : "Coordinator CLI failed.";
      const result: Result = { error: { code: uncertain ? "uncertain" : "coordinator_error", message: uncertain && !message.includes("inspect list/doctor") ? `${message} Inspect list/doctor before retrying.` : message, ...(request.action === "start" ? { name: request.name } : {}), uncertain } };
      return { content: [{ type: "text", text: asJson(result) }], details: result, structuredContent: result, isError: true };
    }
  },
});

export function registerCoordinator(pi: Pick<ExtensionAPI, "registerTool">): void {
  pi.registerTool(coordinatorTool);
}

export default function (pi: ExtensionAPI): void {
  registerCoordinator(pi);
}
