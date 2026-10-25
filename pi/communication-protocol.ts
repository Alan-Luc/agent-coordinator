import { Type } from "@earendil-works/pi-ai";
import type { Static } from "typebox";
import type { ExtensionContext } from "@earendil-works/pi-coding-agent";
import { controlReceiptSchema, jobFactsSchema, processIdentitySchema, sessionStatusSchema, sessionTargetSchema } from "./control-protocol.ts";

export const communicationProtocol = 1;
export const maxPublicationBytes = 4096;
const strict = { additionalProperties: false };
const id = Type.String({ pattern: "^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$" });
const cursor = Type.Optional(Type.Integer({ minimum: 0 }));
const ids = Type.Array(id, { minItems: 1, maxItems: 8, uniqueItems: true });
const kind = Type.Union([Type.Literal("question"), Type.Literal("blocker"), Type.Literal("result")]);
export const communicationToolRequests = [
  Type.Object({ action: Type.Literal("manage"), enabled: Type.Boolean() }, strict),
  Type.Object({ action: Type.Literal("adopt"), id }, strict),
  Type.Object({ action: Type.Literal("release"), id }, strict),
  Type.Object({ action: Type.Literal("publish"), message_id: id, kind, text: Type.String() }, strict),
  Type.Object({ action: Type.Literal("inbox"), cursor }, strict),
  Type.Object({ action: Type.Literal("ack"), message_ids: ids }, strict),
  Type.Object({ action: Type.Literal("escalate"), message_id: id, note: Type.String() }, strict),
  Type.Object({ action: Type.Literal("reply"), message_id: id, request_id: id, text: Type.String() }, strict),
  Type.Object({ action: Type.Literal("resolve"), message_id: id, note: Type.String() }, strict),
] as const;
export const communicationToolRequestSchema = Type.Union([...communicationToolRequests]);
export type CommunicationToolRequest = Static<typeof communicationToolRequestSchema>;
export const communicationCallerSchema = Type.Object({ session_file: Type.String(), session_id: Type.String(), generation: id, pid: Type.Integer({ minimum: 1 }), job_id: Type.Optional(id) }, strict);
export type CommunicationCaller = Static<typeof communicationCallerSchema>;
const lease = Type.Object({ generation: id, process: processIdentitySchema }, strict);
export const communicationManagerSchema = Type.Object({ id, session_file: Type.String(), session_id: Type.String(), enabled: Type.Boolean(), lease: Type.Optional(lease) }, strict);
export const communicationBindingSchema = Type.Object({ job_id: id, manager_id: id }, strict);
export const communicationDiagnosticSchema = Type.Object({ code: Type.String(), message: Type.String(), uncertain: Type.Boolean(), id: Type.Optional(Type.String()), name: Type.Optional(Type.String()) }, strict);
export const communicationErrorSchema = Type.Object({ error: communicationDiagnosticSchema }, strict);
const reply = Type.Object({ request_id: id, fingerprint: Type.String(), target: sessionTargetSchema, receipt: Type.Optional(controlReceiptSchema), failure: Type.Optional(communicationDiagnosticSchema) }, strict);
export const communicationMessageSchema = Type.Object({
  message_id: id, manager_id: id, source: sessionTargetSchema, kind, text: Type.String(), created_at: Type.Integer(),
  attempt_generation: Type.Optional(id), acknowledged_at: Type.Optional(Type.Integer()), escalated_at: Type.Optional(Type.Integer()), escalation_note: Type.Optional(Type.String()),
  resolved_at: Type.Optional(Type.Integer()), resolution_note: Type.Optional(Type.String()), replies: Type.Optional(Type.Array(reply)),
}, strict);
export type CommunicationMessage = Static<typeof communicationMessageSchema>;
export const communicationInboxSchema = Type.Object({ messages: Type.Array(communicationMessageSchema), next_cursor: cursor, pending: Type.Integer({ minimum: 0 }), escalated: Type.Integer({ minimum: 0 }), degraded: Type.Boolean(), workers: Type.Array(sessionStatusSchema) }, strict);
export type CommunicationInbox = Static<typeof communicationInboxSchema>;
export type CommunicationManager = Static<typeof communicationManagerSchema>;

const base = { protocol: Type.Literal(communicationProtocol), caller: communicationCallerSchema };
export const communicationRequestSchema = Type.Union([
  Type.Object({ protocol: Type.Literal(communicationProtocol), action: Type.Literal("capability") }, strict),
  Type.Object({ ...base, action: Type.Literal("enroll") }, strict),
  Type.Object({ ...base, action: Type.Literal("reconnect") }, strict),
  Type.Object({ ...base, action: Type.Literal("pause") }, strict),
  Type.Object({ ...base, action: Type.Literal("disconnect") }, strict),
  Type.Object({ ...base, action: Type.Literal("adopt"), job_id: id, target: sessionTargetSchema }, strict),
  Type.Object({ ...base, action: Type.Literal("release"), job_id: id }, strict),
  Type.Object({ ...base, action: Type.Literal("start"), name: Type.String(), cwd: Type.String(), task: Type.String() }, strict),
  Type.Object({ ...base, action: Type.Literal("publish"), message_id: id, kind, text: Type.String() }, strict),
  Type.Object({ ...base, action: Type.Literal("inbox"), cursor }, strict),
  Type.Object({ ...base, action: Type.Literal("attempt"), message_ids: ids }, strict),
  Type.Object({ ...base, action: Type.Literal("ack"), message_ids: ids }, strict),
  Type.Object({ ...base, action: Type.Literal("escalate"), message_id: id, note: Type.String() }, strict),
  Type.Object({ ...base, action: Type.Literal("resolve"), message_id: id, note: Type.String() }, strict),
  Type.Object({ ...base, action: Type.Literal("reply"), message_id: id, request_id: id, text: Type.String() }, strict),
]);
export type CommunicationRequest = Static<typeof communicationRequestSchema>;
export const communicationResultSchemas = [
  Type.Object({ capability: Type.Object({ protocol: Type.Literal(communicationProtocol) }, strict) }, strict),
  Type.Object({ manager: communicationManagerSchema }, strict),
  Type.Object({ binding: communicationBindingSchema }, strict),
  Type.Object({ released: Type.Literal(true) }, strict),
  Type.Object({ inbox: communicationInboxSchema }, strict),
  Type.Object({ messages: Type.Array(communicationMessageSchema) }, strict),
  Type.Object({ message: communicationMessageSchema }, strict),
  Type.Object({ receipt: controlReceiptSchema }, strict),
  Type.Object({ started: jobFactsSchema }, strict),
] as const;
export const communicationResultSchema = Type.Union([...communicationResultSchemas]);
export type CommunicationResult = Static<typeof communicationResultSchema>;
export type CommunicationError = Static<typeof communicationErrorSchema>;
export interface CommunicationAdapter {
  execute(request: CommunicationToolRequest, signal: AbortSignal | undefined, ctx: ExtensionContext): Promise<CommunicationResult | CommunicationError>;
  start(request: { name: string; cwd: string; task: string }, signal: AbortSignal | undefined, ctx: ExtensionContext): Promise<{ managed: false } | { managed: true; code: number; stdout: string }>;
}
export interface CommunicationCli {
  findExecutable(): Promise<string>;
  runCli(executable: string, args: string[], cwd: string, input: string | undefined, signal?: AbortSignal, timeoutMs?: number): Promise<{ code: number; stdout: string }>;
}
