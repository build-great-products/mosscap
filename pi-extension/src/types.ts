export const protocolVersion = 1;
export const deliveryCapability = "pi_delivery_mvp";
export const maxFrameBytes = 65_536;
export const maxObservations = 256;
export const maxTaskTextBytes = 40_000;

export type DeliveryDetails = {
  schemaVersion: 1;
  deliveryId: string;
  deliveryKey: string;
  taskId: string;
  taskVersion: number;
  sessionId: string;
};
export type Observation = { delivery_id: string; delivery_key: string };
export type Request = { protocol_version: 1; request_id: string; client_kind: "extension"; client_version: string; command: string; params: Record<string, unknown> };
export type ProtocolError = { code: string; category: string; message: string; retriable: boolean; details?: Record<string, string> };
export type Response = { protocol_version: 1; request_id: string; server_version: string; result_type?: string; result?: unknown; error?: ProtocolError };
export type Source = { kind: "local" | "slack"; slack_author_id?: string; slack_message_ts?: string; slack_thread_ts?: string };
export type Work = { delivery_id: string; delivery_key: string; task_id: string; task_version: number; task_state: "open" | "working"; task_text: string; source: Source };
export type Counts = { eligible_backlog_count: number; uncertain_count: number; rejected_count: number };
export type PollResult = Counts & { work: Work | null };
export type OpenResult = Counts & { pi_session_id: string };

export const byteLength = (value: string): number => Buffer.byteLength(value, "utf8");
export function isDeliveryID(value: unknown): value is string { return typeof value === "string" && /^dlv_[A-Za-z0-9_-]{22}$/.test(value); }
export function isDeliveryKey(value: unknown): value is string { return typeof value === "string" && /^sha256:[0-9a-f]{64}$/.test(value); }
export function isDetails(value: unknown, sessionId?: string): value is DeliveryDetails {
  if (value === null || typeof value !== "object") return false;
  const v = value as Record<string, unknown>;
  return v.schemaVersion === 1 && isDeliveryID(v.deliveryId) && isDeliveryKey(v.deliveryKey) && typeof v.taskId === "string" && /^tsk_[a-z0-9]+$/.test(v.taskId) && Number.isInteger(v.taskVersion) && (v.taskVersion as number) > 0 && typeof v.sessionId === "string" && (!sessionId || v.sessionId === sessionId);
}
export function isWork(value: unknown): value is Work {
  if (value === null || typeof value !== "object") return false;
  const w = value as Record<string, unknown>, s = w.source as Record<string, unknown> | undefined;
  return isDeliveryID(w.delivery_id) && isDeliveryKey(w.delivery_key) && typeof w.task_id === "string" && /^tsk_[a-z0-9]+$/.test(w.task_id) && Number.isInteger(w.task_version) && (w.task_version as number) > 0 && (w.task_state === "open" || w.task_state === "working") && typeof w.task_text === "string" && byteLength(w.task_text) <= maxTaskTextBytes && s !== undefined && (s.kind === "local" || s.kind === "slack");
}
export function isCounts(value: unknown): value is Counts { const v = value as Record<string, unknown>; return !!v && ["eligible_backlog_count", "uncertain_count", "rejected_count"].every(k => Number.isInteger(v[k]) && (v[k] as number) >= 0); }
