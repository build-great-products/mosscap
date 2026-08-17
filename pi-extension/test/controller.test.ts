import test from "node:test";
import assert from "node:assert/strict";
import { Controller, type PiAdapter } from "../src/controller.ts";
import type { GatewayClient } from "../src/client.ts";
import { taskMessage } from "../src/messages.ts";
import type { Work } from "../src/types.ts";

const work: Work = {
  delivery_id: "dlv_AbCdEfGhIjKlMnOpQrStUv",
  delivery_key: "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
  task_id: "AbCdEfGhIjKlMnOpQrStUv",
  task_selector: "AbCdEfGh",
  task_version: 1,
  task_state: "open",
  task_text: "task",
  source: { kind: "local" },
};
const counts = { eligible_backlog_count: 0, uncertain_count: 0, rejected_count: 0 };
const flush = (): Promise<void> => new Promise(resolve => setImmediate(resolve));

function adapter(overrides: Partial<PiAdapter> = {}): PiAdapter {
  return {
    sessionId: () => "session-1",
    entries: () => [],
    isIdle: () => true,
    send: () => {},
    status: () => {},
    notify: () => {},
    ...overrides,
  };
}

function gateway(overrides: Record<string, unknown> = {}): GatewayClient {
  return {
    deliveryOpen: async () => ({ pi_session_id: "session-1", ...counts }),
    poll: async () => ({ work: null, ...counts }),
    inserting: async () => {},
    persisted: async () => {},
    rejected: async () => {},
    close: async () => {},
    ...overrides,
  } as unknown as GatewayClient;
}

test("controller uses exact Pi delivery options for busy and idle sessions", async () => {
  for (const [idle, expected] of [[false, { triggerTurn: true, deliverAs: "followUp" }], [true, { triggerTurn: true }]] as const) {
    let sentOptions: unknown;
    const controller = new Controller(adapter({
      isIdle: () => idle,
      send: (_message, options) => { sentOptions = options; },
    }), { client: () => gateway({ poll: async () => ({ work, ...counts }) }), pollMs: 100_000 });
    await controller.start();
    await flush();
    assert.deepEqual(sentOptions, expected);
    await controller.stop();
  }
});

test("observed-delivery cap fails closed before creating or opening a gateway client", async () => {
  let factories = 0;
  const notifications: string[] = [];
  const entries = Array.from({ length: 257 }, (_, i) => ({
    type: "custom_message",
    customType: "mosscap.task",
    details: taskMessage({ ...work, delivery_id: `dlv_${String(i).padStart(22, "0")}` }, "session-1").details,
  }));
  const controller = new Controller(adapter({ entries: () => entries, notify: text => notifications.push(text) }), {
    client: () => { factories++; return gateway(); },
  });

  await assert.rejects(controller.start(), /256-delivery safety cap/);
  assert.equal(factories, 0);
  assert.equal(controller.enabled, false);
  assert.match(notifications.at(-1) || "", /256-delivery safety cap/);
});

test("missing post-start evidence closes for server-side uncertainty without automatic redelivery", async () => {
  const later: (() => void)[] = [];
  let sends = 0;
  let resumeSleep: (() => void) | undefined;
  let firstClosed = 0;
  let secondClosed = 0;
  let polls = 0;
  const first = gateway({
    poll: async () => ({ work: polls++ === 0 ? work : null, ...counts }),
    close: async () => { firstClosed++; },
  });
  const second = gateway({ close: async () => { secondClosed++; } });
  let clients = 0;
  const controller = new Controller(adapter({ send: () => { sends++; } }), {
    client: () => clients++ === 0 ? first : second,
    later: fn => later.push(fn),
    pollMs: 100_000,
    sleep: () => new Promise(resolve => { resumeSleep = resolve; }),
  });

  await controller.start();
  await flush();
  const details = taskMessage(work, "session-1").details;
  await controller.messageStart({ role: "custom", customType: "mosscap.task", details });
  controller.messageEnd({ role: "custom", customType: "mosscap.task", details });
  later.shift()?.();
  await flush();
  assert.equal(firstClosed, 1, "missing durable entry releases the insertion as uncertain");
  assert.equal(sends, 1);

  resumeSleep?.();
  await flush();
  assert.equal(clients, 2, "the controller reconnects after releasing uncertainty");
  assert.equal(sends, 1, "the controller never resends the unproven insertion itself");
  await controller.stop();
  assert.equal(secondClosed, 1);
});

test("reconnect submits persisted evidence for a base62 task ID without duplicate delivery", async () => {
  const later: (() => void)[] = [];
  const entries = [{ type: "custom_message", customType: "mosscap.task", details: taskMessage(work, "session-1").details }];
  let sends = 0;
  let firstPolls = 0;
  let resumeSleep: (() => void) | undefined;
  const first = gateway({
    poll: async () => firstPolls++ === 0 ? { work, ...counts } : Promise.reject(new Error("connection lost")),
  });
  const opened: unknown[][] = [];
  const second = gateway({
    deliveryOpen: async (_session: string, observed: unknown[]) => { opened.push(observed); return { pi_session_id: "session-1", ...counts }; },
  });
  let clients = 0;
  const controller = new Controller(adapter({ entries: () => entries, send: () => { sends++; } }), {
    client: () => clients++ === 0 ? first : second,
    later: fn => later.push(fn),
    pollMs: 100_000,
    reconnectMs: 100_000,
    sleep: () => new Promise(resolve => { resumeSleep = resolve; }),
  });

  await controller.start();
  await flush();
  const details = taskMessage(work, "session-1").details;
  await controller.messageStart({ role: "custom", customType: "mosscap.task", details });
  controller.messageEnd({ role: "custom", customType: "mosscap.task", details });
  later.shift()?.();
  await flush();
  resumeSleep?.();
  await flush();
  resumeSleep?.();
  await flush();

  assert.deepEqual(opened, [[{ delivery_id: work.delivery_id, delivery_key: work.delivery_key }]]);
  assert.equal(sends, 1);
  await controller.stop();
});

test("stop is idempotent and prevents future polling", async () => {
  let polls = 0;
  const controller = new Controller(adapter(), {
    client: () => gateway({ poll: async () => { polls++; return { work: null, ...counts }; } }),
    pollMs: 100_000,
  });
  await controller.start();
  await controller.stop();
  await controller.stop();
  assert.equal(controller.enabled, false);
  assert.ok(polls <= 1);
});
