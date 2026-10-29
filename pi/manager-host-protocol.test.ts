import assert from "node:assert/strict";
import test from "node:test";
import { Value } from "typebox/value";
import { awaitingConversationSchema, launchFeaturesSchema, managerHostRequestSchema, managerHostResultSchema } from "./manager-host-protocol.ts";

test("manager host endpoint contracts are strict and correlate exact targets", () => {
  const target = { id: "560ee6df-3ce6-4231-9656-c318a839659e", session_id: "native", generation: "1465f55e-ebc9-4df3-9a3f-3fc7c89ae94c", pane: { boot_id: "boot", pid: 123, uid: 501, start_sec: 10, start_usec: 0 } };
  assert.equal(Value.Check(launchFeaturesSchema, { features: { interactive_launch: 1, terminal_profile: "transparent-v1", manager_host: 1, pending_manager: 1 } }), true);
  assert.equal(Value.Check(launchFeaturesSchema, { features: { interactive_launch: 1, terminal_profile: "transparent-v1", manager_host: 1, pending_manager: 1, extra: true } }), false);
  assert.equal(Value.Check(managerHostRequestSchema, { target }), true);
  assert.equal(Value.Check(managerHostRequestSchema, { target, extra: true }), false);
  assert.equal(Value.Check(managerHostResultSchema, { role: { schema: 1, role: "manager-host" } }), true);
  assert.equal(Value.Check(managerHostResultSchema, { role: { schema: 1, role: "standalone" } }), false);
  assert.equal(Value.Check(awaitingConversationSchema, { awaiting_conversation: { job_id: target.id } }), true);
});
