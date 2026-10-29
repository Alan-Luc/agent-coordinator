import assert from "node:assert/strict";
import test from "node:test";
import { Value } from "typebox/value";
import { chmod, mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import { delimiter, join } from "node:path";
import { tmpdir } from "node:os";
import { coordinatorTool, registerCoordinator } from "./index.ts";
import { communicationRequestSchema, communicationResultSchema, type CommunicationAdapter, type CommunicationToolRequest, type CommunicationRequest, type CommunicationResult } from "./communication-protocol.ts";
import { awaitingConversationSchema } from "./manager-host-protocol.ts";

test("coordinator exposes attributed communication without model-selected caller identity", () => {
  const request = { request: { action: "publish", message_id: "560ee6df-3ce6-4231-9656-c318a839659e", kind: "question", text: "Which local fixture should I use?" } };
  assert.equal(Value.Check(coordinatorTool.parameters, request), true);
  assert.equal(Value.Check(coordinatorTool.parameters, { request: { ...request.request, manager_id: "spoof" } }), false);
  assert.equal(Value.Check(coordinatorTool.parameters, { request: { ...request.request, caller: { pid: 1 } } }), false);
});

test("acknowledgement, reply and explicit resolution are separate requests", () => {
  const message_id = "560ee6df-3ce6-4231-9656-c318a839659e";
  const requests: CommunicationToolRequest[] = [
    { action: "manage", enabled: true },
    { action: "adopt", id: message_id },
    { action: "release", id: message_id },
    { action: "inbox" },
    { action: "ack", message_ids: [message_id] },
    { action: "escalate", message_id, note: "Alan must choose" },
    { action: "reply", message_id, request_id: "1465f55e-ebc9-4df3-9a3f-3fc7c89ae94c", text: "Use the private fixture" },
    { action: "resolve", message_id, note: "Answered explicitly" },
  ];
  for (const request of requests) assert.equal(Value.Check(coordinatorTool.parameters, { request }), true, request.action);
});

test("awaiting-conversation is a strict public management result", () => {
  assert.equal(Value.Check(awaitingConversationSchema, { awaiting_conversation: { job_id: "560ee6df-3ce6-4231-9656-c318a839659e" } }), true);
  assert.equal(Value.Check(awaitingConversationSchema, { awaiting_conversation: { job_id: "bad" } }), false);
  assert.equal(Value.Check(awaitingConversationSchema, { awaiting_conversation: { job_id: "560ee6df-3ce6-4231-9656-c318a839659e", manager_id: "extra" } }), false);
});

test("authoritative wire schemas retain their discriminated TypeScript unions", () => {
  const caller = { session_file: "/private/session.jsonl", session_id: "native", generation: "560ee6df-3ce6-4231-9656-c318a839659e", pid: 123 };
  const requests: CommunicationRequest[] = [
    { protocol: 1, action: "capability" },
    { protocol: 1, action: "disconnect", caller },
    { protocol: 1, action: "resolve", caller, message_id: caller.generation, note: "Explicit resolution" },
  ];
  const results: CommunicationResult[] = [
    { capability: { protocol: 1 } }, { released: true }, { messages: [] },
    { inbox: { messages: [], pending: 0, escalated: 0, degraded: false, workers: [] } },
    { awaiting_conversation: { job_id: "560ee6df-3ce6-4231-9656-c318a839659e" } },
  ];
  for (const request of requests) assert.equal(Value.Check(communicationRequestSchema, request), true);
  for (const result of results) assert.equal(Value.Check(communicationResultSchema, result), true);
});

test("an uncertain associated start never falls back to an unassociated launch", async () => {
  const directory = await mkdtemp(join(tmpdir(), "coordinator-associated-start-"));
  const executable = join(directory, "agent-coordinator");
  const marker = join(directory, "unassociated-launch");
  const previousPath = process.env.PATH;
  try {
    await writeFile(executable, `#!/usr/bin/env node\nrequire('node:fs').writeFileSync(${JSON.stringify(marker)},'unexpected launch');process.exit(1);\n`);
    await chmod(executable, 0o700);
    process.env.PATH = `${directory}${delimiter}${previousPath ?? ""}`;
    const adapter: CommunicationAdapter = {
      execute: async () => ({ error: { code: "unused", message: "unused", uncertain: false } }),
      start: async () => ({ managed: true, code: 75, stdout: JSON.stringify({ error: { code: "uncertain", message: "binding persisted, launch response lost", uncertain: true } }) }),
    };
    let invoke: (...args: unknown[]) => unknown = () => { throw new Error("tool not registered"); };
    registerCoordinator({ registerTool(tool) { invoke = (...args) => Reflect.apply(tool.execute, undefined, args); } }, undefined, adapter);
    const result = await invoke("call", { request: { action: "start", name: "owned-job", cwd: ".", task: "Inspect only" } }, undefined, undefined, { cwd: directory });
    assert.ok(typeof result === "object" && result !== null && "isError" in result && result.isError === true);
    await assert.rejects(readFile(marker), { code: "ENOENT" });
  } finally {
    if (previousPath === undefined) delete process.env.PATH; else process.env.PATH = previousPath;
    await rm(directory, { recursive: true, force: true });
  }
});
