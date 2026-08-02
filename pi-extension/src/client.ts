import { Socket } from "node:net";
import { randomUUID } from "node:crypto";
import { isUtf8 } from "node:buffer";
import { deliveryCapability, maxFrameBytes, protocolVersion, type OpenResult, type PollResult, type Request, type Response, type Observation } from "./types.ts";

export class ClientError extends Error { readonly code: string; constructor(message: string, code = "protocol_error") { super(message); this.code = code; } }
export type ClientOptions = { endpoint: string; timeoutMs?: number; maxFrameBytes?: number; connect?: (endpoint: string) => Socket };

/** A deliberately serial, strict v1 Unix-socket protocol client. */
export class GatewayClient {
  #socket: Socket | undefined;
  #buffer = Buffer.alloc(0);
  #pending: { id: string; resolve: (r: Response) => void; reject: (e: Error) => void; timer: ReturnType<typeof setTimeout> } | undefined;
  #closed = false;
  readonly endpoint: string;
  readonly timeoutMs: number;
  readonly maxFrameBytes: number;
  #connect: (endpoint: string) => Socket;
  constructor(options: ClientOptions) { this.endpoint = options.endpoint; this.timeoutMs = options.timeoutMs ?? 10_000; this.maxFrameBytes = options.maxFrameBytes ?? maxFrameBytes; this.#connect = options.connect ?? (p => new Socket().connect(p)); }
  async open(): Promise<void> {
    if (this.#socket && !this.#socket.destroyed) return;
    if (this.#closed) throw new ClientError("client is closed");
    const socket = this.#connect(this.endpoint); this.#socket = socket;
    socket.on("data", data => this.#read(data)); socket.on("error", error => this.#fail(error)); socket.on("close", () => this.#fail(new ClientError("gateway connection closed", "connection_closed")));
    await new Promise<void>((resolve, reject) => { socket.once("connect", resolve); socket.once("error", reject); });
    const hello = await this.request("hello", { supported_versions: [protocolVersion], capabilities: [deliveryCapability] });
    const result = strictObject(hello.result, "hello result"); const capabilities = result.capabilities;
    if (!Array.isArray(capabilities) || !capabilities.every(v => typeof v === "string") || !capabilities.includes(deliveryCapability)) throw new ClientError("gateway lacks pi_delivery_mvp", "capability_absent");
  }
  async request(command: string, params: Record<string, unknown>): Promise<Response> {
    if (this.#pending) throw new ClientError("only one request may be in flight", "request_in_flight");
    await this.open();
    const id = `req_${randomUUID().replaceAll("-", "")}`;
    const request: Request = { protocol_version: 1, request_id: id, client_kind: "extension", client_version: "0.83.0", command, params };
    const payload = Buffer.from(JSON.stringify(request));
    if (payload.length === 0 || payload.length > this.maxFrameBytes) throw new ClientError("request frame out of bounds");
    return await new Promise<Response>((resolve, reject) => {
      const timer = setTimeout(() => { this.#pending = undefined; reject(new ClientError("gateway request timed out", "deadline_exceeded")); this.#socket?.destroy(); }, this.timeoutMs);
      this.#pending = { id, resolve, reject, timer };
      const header = Buffer.allocUnsafe(4); header.writeUInt32BE(payload.length); this.#socket?.write(Buffer.concat([header, payload]));
    });
  }
  async deliveryOpen(sessionId: string, observed: Observation[]): Promise<OpenResult> { return this.#result<OpenResult>(await this.request("delivery.open", { pi_session_id: sessionId, observed })); }
  async poll(): Promise<PollResult> { const value = this.#result<Record<string, unknown>>(await this.request("delivery.poll", {})); const work = value.work; const base = { eligible_backlog_count: numberField(value, "eligible_backlog_count"), uncertain_count: numberField(value, "uncertain_count"), rejected_count: numberField(value, "rejected_count") }; if (work === undefined || work === null) return { ...base, work: null }; const w = strictObject(work, "delivery work"); const sourceKind = w.source_kind; const slack = w.slack_source === undefined ? undefined : strictObject(w.slack_source, "Slack source"); return { ...base, work: { delivery_id: stringField(w, "delivery_id"), delivery_key: stringField(w, "delivery_key"), task_id: stringField(w, "task_id"), task_version: numberField(w, "task_version"), task_state: taskState(w.task_state), task_text: stringField(w, "task_text"), source: sourceKind === "local" ? { kind: "local" } : { kind: sourceKind === "slack" ? "slack" : invalidSource(), slack_author_id: slack && stringField(slack, "author_user_id"), slack_message_ts: slack && stringField(slack, "message_ts"), slack_thread_ts: slack && stringField(slack, "thread_ts") } } }; }
  async inserting(deliveryId: string, deliveryKey: string): Promise<void> { await this.#ok(await this.request("delivery.inserting", { delivery_id: deliveryId, delivery_key: deliveryKey })); }
  async persisted(deliveryId: string, deliveryKey: string): Promise<void> { await this.#ok(await this.request("delivery.persisted", { delivery_id: deliveryId, delivery_key: deliveryKey })); }
  async rejected(deliveryId: string, deliveryKey: string, diagnosticCode: string): Promise<void> { await this.#ok(await this.request("delivery.rejected", { delivery_id: deliveryId, delivery_key: deliveryKey, diagnostic_code: diagnosticCode })); }
  async close(): Promise<void> { if (this.#closed) return; try { if (this.#socket && !this.#socket.destroyed) await this.request("delivery.close", {}); } catch { /* socket loss has server cleanup */ } finally { this.#closed = true; this.#socket?.destroy(); } }
  #result<T>(response: Response): T { this.#ok(response); return response.result as T; }
  #ok(response: Response): void { if (response.error) throw new ClientError(response.error.message, response.error.code); }
  #read(chunk: Buffer): void { this.#buffer = Buffer.concat([this.#buffer, chunk]); while (this.#buffer.length >= 4) { const size = this.#buffer.readUInt32BE(); if (size === 0 || size > this.maxFrameBytes) return this.#fail(new ClientError("invalid response frame", "invalid_frame")); if (this.#buffer.length < size + 4) return; const body = this.#buffer.subarray(4, size + 4); this.#buffer = this.#buffer.subarray(size + 4); if (!isUtf8(body)) return this.#fail(new ClientError("response is not UTF-8", "invalid_utf8")); let response: Response; try { response = parseResponse(JSON.parse(body.toString("utf8"))); } catch (error) { return this.#fail(error instanceof Error ? error : new ClientError("invalid response JSON")); } const pending = this.#pending; if (!pending || response.request_id !== pending.id) return this.#fail(new ClientError("uncorrelated response", "response_correlation")); clearTimeout(pending.timer); this.#pending = undefined; pending.resolve(response); } }
  #fail(error: Error): void { const pending = this.#pending; if (pending) { clearTimeout(pending.timer); this.#pending = undefined; pending.reject(error); } }
}
function strictObject(value: unknown, name: string): Record<string, unknown> { if (value === null || typeof value !== "object" || Array.isArray(value)) throw new ClientError(`invalid ${name}`); return value as Record<string, unknown>; }
function stringField(value: Record<string, unknown>, name: string): string { if (typeof value[name] !== "string") throw new ClientError(`invalid ${name}`); return value[name]; }
function numberField(value: Record<string, unknown>, name: string): number { if (!Number.isInteger(value[name]) || (value[name] as number) < 0) throw new ClientError(`invalid ${name}`); return value[name] as number; }
function taskState(value: unknown): "open" | "working" { if (value !== "open" && value !== "working") throw new ClientError("invalid task state"); return value; }
function invalidSource(): never { throw new ClientError("invalid source kind"); }
function parseResponse(value: unknown): Response { const o = strictObject(value, "response"); const allowed = new Set(["protocol_version", "request_id", "server_version", "result_type", "result", "error"]); if (Object.keys(o).some(k => !allowed.has(k)) || o.protocol_version !== 1 || typeof o.request_id !== "string" || typeof o.server_version !== "string" || (("result" in o) === ("error" in o))) throw new ClientError("invalid response envelope"); if ("error" in o && (o.error === null || typeof o.error !== "object")) throw new ClientError("invalid error response"); return o as Response; }
