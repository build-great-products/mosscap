import { GatewayClient } from "../src/client.ts";

const endpoint = process.argv[2];
if (!endpoint) throw new Error("endpoint required");
const session = "node-e2e-session";
const client = new GatewayClient({ endpoint, timeoutMs: 3_000 });
const open = await client.deliveryOpen(session, []);
const poll = await client.poll();
if (!poll.work) throw new Error("expected delivery work");
await client.inserting(poll.work.delivery_id, poll.work.delivery_key);
await client.persisted(poll.work.delivery_id, poll.work.delivery_key);
await client.close();
const second = new GatewayClient({ endpoint, timeoutMs: 3_000 });
await second.deliveryOpen(session, [{ delivery_id: poll.work.delivery_id, delivery_key: poll.work.delivery_key }]);
const replay = await second.poll();
await second.close();
if (replay.work !== null) throw new Error("persisted delivery replayed");
process.stdout.write(JSON.stringify({ open, deliveryId: poll.work.delivery_id, taskId: poll.work.task_id }));
