import { homedir } from "node:os";
import { join } from "node:path";
import type { ExtensionAPI, ExtensionCommandContext, ExtensionContext, MessageEndEvent, MessageStartEvent } from "@earendil-works/pi-coding-agent";
import { GatewayClient } from "./client.ts";
import { Controller, type PiAdapter } from "./controller.ts";
import type { SessionEntryLike } from "./messages.ts";

const statusKey = "mosscap";
function endpoint(): string { return join(process.env.XDG_STATE_HOME || join(homedir(), ".local", "state"), "mosscap", "control.sock"); }
function adapter(ctx: ExtensionContext, pi: ExtensionAPI): PiAdapter {
  return {
    sessionId: () => ctx.sessionManager.getSessionId(), entries: () => ctx.sessionManager.getEntries() as SessionEntryLike[], isIdle: () => ctx.isIdle(),
    send: (message, options) => Promise.resolve(pi.sendMessage(message, options)), status: text => ctx.ui.setStatus(statusKey, text), notify: (text, kind) => ctx.ui.notify(text, kind === "error" ? "error" : "info")
  };
}
function persisted(ctx: ExtensionCommandContext): boolean { return (ctx.sessionManager as unknown as { isPersisted(): boolean }).isPersisted(); }
export default function mosscap(pi: ExtensionAPI): void {
  let controller: Controller | undefined;
  const stop = async (): Promise<void> => { await controller?.stop(); controller = undefined; };
  pi.registerCommand("mosscap", { description: "Explicitly connect this persisted Pi TUI session to Mosscap", handler: async (args, ctx) => {
    const command = args.trim();
    if (command === "off") { await stop(); ctx.ui.notify("Mosscap delivery is off.", "info"); return; }
    if (command === "status") { ctx.ui.notify(controller?.status || "Mosscap delivery is off.", "info"); return; }
    if (command !== "") { ctx.ui.notify("Usage: /mosscap [status|off]", "error"); return; }
    if (ctx.mode !== "tui") { ctx.ui.notify("Mosscap delivery is supported only in the interactive TUI.", "error"); return; }
    if (!persisted(ctx)) { ctx.ui.notify("Mosscap delivery requires a persisted Pi session.", "error"); return; }
    if (controller?.enabled) { ctx.ui.notify(controller.status, "info"); return; }
    controller = new Controller(adapter(ctx, pi), { client: () => new GatewayClient({ endpoint: endpoint() }) });
    try { await controller.start(); } catch { controller = undefined; }
  }});
  pi.on("message_start", async (event: MessageStartEvent) => { await controller?.messageStart(event.message); });
  pi.on("message_end", (event: MessageEndEvent) => { controller?.messageEnd(event.message); });
  pi.on("session_shutdown", async () => { await stop(); });
}
