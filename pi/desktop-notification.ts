import { execFile } from "node:child_process";

export function notifyWorkerPrompt(jobId: string, kind: string, signal: AbortSignal, platform: NodeJS.Platform = process.platform): Promise<boolean> {
  if (signal.aborted || (platform !== "darwin" && platform !== "linux")) return Promise.resolve(false);
  const message = `Worker ${jobId} is waiting in its native ${kind} dialog. Enter that worker session to respond.`;
  const executable = platform === "darwin" ? "terminal-notifier" : "notify-send";
  const argv = platform === "darwin" ? ["-title", "Pi coordinator", "-message", message] : ["--", "Pi coordinator", message];
  return new Promise((resolve) => {
    try {
      // Best effort only: never delay polling or expose dialog titles on the desktop.
      execFile(executable, argv, { signal, timeout: 2_000, killSignal: "SIGKILL", maxBuffer: 4_096 }, (error) => { resolve(!error); });
    } catch { resolve(false); }
  });
}
