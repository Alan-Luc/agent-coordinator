import assert from "node:assert/strict";
import { mkdtemp, writeFile, chmod, readFile, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";
import { notifyWorkerPrompt } from "./desktop-notification.ts";

// Real child processes with owned executables; not visible desktop delivery.
test("desktop notifier submits literal private-safe argv and contains backend failures", async (t) => {
  const directory = await mkdtemp(join(tmpdir(), "coordinator-notify-"));
  const originalPath = process.env.PATH;
  process.env.PATH = directory;
  t.after(async () => {
    process.env.PATH = originalPath;
    await rm(directory, { recursive: true, force: true });
  });
  const record = join(directory, "argv.json");
  const jobId = "52b201e8-650c-4617-9672-7dac392ef546";
  const signal = new AbortController().signal;
  const message = `Worker ${jobId} is waiting in its native confirm dialog. Enter that worker session to respond.`;
  async function backend(name: string, body: string) {
    const path = join(directory, name);
    await writeFile(path, `#!${process.execPath}\n${body}\n`);
    await chmod(path, 0o700);
  }
  for (const [platform, executable, expected] of [
    ["darwin", "terminal-notifier", ["-title", "Pi coordinator", "-message", message]],
    ["linux", "notify-send", ["--", "Pi coordinator", message]],
  ] as const) {
    await backend(executable, `require('node:fs').writeFileSync(${JSON.stringify(record)}, JSON.stringify(process.argv.slice(2)));`);
    assert.equal(await notifyWorkerPrompt(jobId, "confirm", signal, platform), true);
    assert.deepEqual(JSON.parse(await readFile(record, "utf8")), expected);
    await rm(record);
  }
  assert.equal(await notifyWorkerPrompt(jobId, "confirm", signal, "win32"), false);
  await assert.rejects(readFile(record), { code: "ENOENT" });
  await rm(join(directory, "terminal-notifier"));
  assert.equal(await notifyWorkerPrompt(jobId, "confirm", signal, "darwin"), false);
  await backend("notify-send", "process.exit(7);");
  assert.equal(await notifyWorkerPrompt(jobId, "confirm", signal, "linux"), false);
  await backend("notify-send", "setInterval(() => {}, 1000);");
  const started = Date.now();
  assert.equal(await notifyWorkerPrompt(jobId, "confirm", signal, "linux"), false);
  assert.ok(Date.now() - started < 4000, "hung backend was not bounded");
  const abort = new AbortController();
  const pending = notifyWorkerPrompt(jobId, "confirm", abort.signal, "linux");
  abort.abort();
  assert.equal(await pending, false);
  assert.equal(await notifyWorkerPrompt(jobId, "confirm", abort.signal, "linux"), false);
});
