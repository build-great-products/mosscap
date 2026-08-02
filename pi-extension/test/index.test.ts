import test from "node:test";
import assert from "node:assert/strict";
import type { ExtensionAPI, ExtensionCommandContext } from "@earendil-works/pi-coding-agent";
import { installMosscap } from "../src/index.ts";

function commandContext(mode: "tui" | "json", persisted: boolean, notices: string[]): ExtensionCommandContext {
  return {
    mode,
    ui: { notify: (text: string) => { notices.push(text); } },
    sessionManager: { isPersisted: () => persisted },
  } as unknown as ExtensionCommandContext;
}

test("extension creates a controller only after explicit persisted-TUI opt-in and off/shutdown are idempotent", async () => {
  let command: ((args: string, ctx: ExtensionCommandContext) => Promise<void>) | undefined;
  const events = new Map<string, (() => Promise<void>)[]>();
  const pi = {
    registerCommand: (_name: string, options: { handler: (args: string, ctx: ExtensionCommandContext) => Promise<void> }) => { command = options.handler; },
    on: (event: string, handler: () => Promise<void>) => { events.set(event, [...(events.get(event) || []), handler]); },
  } as unknown as ExtensionAPI;
  let factories = 0;
  let starts = 0;
  let stops = 0;
  const notices: string[] = [];
  installMosscap(pi, () => {
    factories++;
    let enabled = false;
    return {
      get enabled(): boolean { return enabled; },
      get status(): string { return "connected"; },
      start: async () => { starts++; enabled = true; },
      stop: async () => { stops++; enabled = false; },
      messageStart: async () => {},
      messageEnd: () => {},
    };
  });
  assert.ok(command);

  await command("", commandContext("json", true, notices));
  await command("", commandContext("tui", false, notices));
  await command("status", commandContext("tui", true, notices));
  await command("off", commandContext("tui", true, notices));
  assert.equal(factories, 0, "non-opt-in command paths create no controller/client resource");
  assert.equal(starts, 0);
  assert.match(notices[0] || "", /interactive TUI/);
  assert.match(notices[1] || "", /persisted Pi session/);

  await command("", commandContext("tui", true, notices));
  await command("", commandContext("tui", true, notices));
  assert.equal(factories, 1);
  assert.equal(starts, 1);

  for (const shutdown of events.get("session_shutdown") || []) await shutdown();
  await command("off", commandContext("tui", true, notices));
  for (const shutdown of events.get("session_shutdown") || []) await shutdown();
  assert.equal(stops, 1, "off and shutdown share a single idempotent stop path");
});
