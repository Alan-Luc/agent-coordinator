import { Type } from "@earendil-works/pi-ai";
import type { Static } from "typebox";

export const controlProtocol = 1;
export const maxControlTextBytes = 64 * 1024;

export const processIdentitySchema = Type.Object({ boot_id: Type.String(), pid: Type.Integer(), uid: Type.Integer(), start_sec: Type.Integer(), start_usec: Type.Integer() }, { additionalProperties: false });
export const sessionTargetSchema = Type.Object({ id: Type.String(), session_id: Type.String(), generation: Type.String(), pane: processIdentitySchema }, { additionalProperties: false });
export const agentReportSchema = Type.Object({ summary: Type.String(), attention: Type.Optional(Type.String()) }, { additionalProperties: false });
export const observedPromptSchema = Type.Object({ kind: Type.String(), title: Type.Optional(Type.String()) }, { additionalProperties: false });
export const sessionStatusSchema = Type.Object({
  protocol: Type.Literal(controlProtocol), job_id: Type.String(), owner: Type.String(), session_id: Type.String(), generation: Type.String(), pid: Type.Integer(), updated_at: Type.Integer(),
  activity: Type.Union([Type.Literal("unknown"), Type.Literal("idle"), Type.Literal("busy")]), launch_protocol: Type.Optional(Type.Literal("2")), prompt: Type.Optional(observedPromptSchema), report: Type.Optional(agentReportSchema),
}, { additionalProperties: false });
export const jobFactsSchema = Type.Object({
  id: Type.String(), name: Type.String(), launch: Type.String(), terminal: Type.String(),
  pi_readiness: Type.Literal("unverified"), task_outcome: Type.Literal("unknown"), clients: Type.Integer({ minimum: 0 }),
  retired: Type.Boolean(), next_action: Type.String(), exit_status: Type.Optional(Type.Integer()), session: Type.Optional(sessionStatusSchema),
}, { additionalProperties: false });
export const controlRequestSchema = Type.Union([
  Type.Object({ protocol: Type.Literal(controlProtocol), job_id: Type.String(), session_id: Type.String(), generation: Type.String(), request_id: Type.String(), action: Type.Literal("send"), text: Type.String() }, { additionalProperties: false }),
  Type.Object({ protocol: Type.Literal(controlProtocol), job_id: Type.String(), session_id: Type.String(), generation: Type.String(), request_id: Type.String(), action: Type.Literal("stop") }, { additionalProperties: false }),
]);
export const controlReceiptSchema = Type.Object({ id: Type.String(), session_id: Type.String(), generation: Type.String(), request_id: Type.String(), operation: Type.Union([Type.Literal("send"), Type.Literal("stop"), Type.Literal("force_stop")]), result: Type.Union([Type.Literal("api_invoked"), Type.Literal("exit_observed"), Type.Literal("signal_sent"), Type.Literal("uncertain")]) }, { additionalProperties: false });
export const stopAuthorizationSchema = Type.Object({ token: Type.String(), target: sessionTargetSchema, operation: Type.Union([Type.Literal("stop"), Type.Literal("force_stop")]), expires_at: Type.Integer() }, { additionalProperties: false });

export type ProcessIdentity = Static<typeof processIdentitySchema>;
export type SessionTarget = Static<typeof sessionTargetSchema>;
export type AgentReport = Static<typeof agentReportSchema>;
export type ObservedPrompt = Static<typeof observedPromptSchema>;
export type SessionStatus = Static<typeof sessionStatusSchema>;
export type ControlRequest = Static<typeof controlRequestSchema>;
export type ControlReceipt = Static<typeof controlReceiptSchema>;
export type StopAuthorization = Static<typeof stopAuthorizationSchema>;
