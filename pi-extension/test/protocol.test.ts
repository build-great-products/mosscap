import test from "node:test";
import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import { join } from "node:path";
import { GatewayClient } from "../src/client.ts";

test("consumes the Go v1 delivery golden frames", async () => { const fixture = JSON.parse(await readFile(join(import.meta.dirname, "../../internal/protocol/testdata/delivery-v1-golden.json"), "utf8")) as { frame: string }[]; assert.equal(fixture.length, 4); for (const item of fixture) { const frame = JSON.parse(item.frame) as Record<string, unknown>; assert.equal(frame.protocol_version, 1); assert.ok(typeof frame.request_id === "string"); } });
test("client refuses a second in-flight request", async () => { const client = new GatewayClient({ endpoint: "/does-not-matter", connect: () => { throw new Error("not used"); } }); const pending = (client as unknown as { _pending?: unknown }); assert.equal(pending._pending, undefined); await client.close(); await assert.rejects(client.request("delivery.poll", {}), /closed/); });
