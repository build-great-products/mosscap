export const protocolVersion = 1;
export const deliveryCapability = "pi_delivery_mvp";
export const maxFrameBytes = 65_536;
export const maxObservations = 256;
export const maxTaskTextBytes = 40_000;
export const maxAttachedFiles = 20;
export const maxFileNameBytes = 1_024;
export const maxMimeBytes = 255;

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
/** Incoming Slack attachment metadata only; it never carries bytes, URLs, hashes, or paths. */
export type AttachedFile = { file_id: string; original_name: string; declared_mime: string | null; declared_size_bytes: number | null; transfer_state: "metadata_only" | "validated" | "unavailable" | "oversized" | "content_changed" };
export type Work = { delivery_id: string; delivery_key: string; task_id: string; task_version: number; task_state: "open" | "working"; task_text: string; source: Source; attached_files?: readonly AttachedFile[] };
export type Counts = { eligible_backlog_count: number; uncertain_count: number; rejected_count: number };
export type PollResult = Counts & { work: Work | null };
export type OpenResult = Counts & { pi_session_id: string };

export const byteLength = (value: string): number => Buffer.byteLength(value, "utf8");
export function isDeliveryID(value: unknown): value is string { return typeof value === "string" && /^dlv_[A-Za-z0-9_-]{22}$/.test(value); }
/** Matches Go identity.Generator.NewTaskID: tsk_ plus 16 raw-base64url bytes. */
export function isTaskID(value: unknown): value is string { return typeof value === "string" && /^tsk_[A-Za-z0-9_-]{22}$/.test(value); }
export function isFileID(value: unknown): value is string { return typeof value === "string" && /^fil_[A-Za-z0-9_-]{22}$/.test(value); }
export function isDeliveryKey(value: unknown): value is string { return typeof value === "string" && /^sha256:[0-9a-f]{64}$/.test(value); }
export function isDetails(value: unknown, sessionId?: string): value is DeliveryDetails {
  if (value === null || typeof value !== "object") return false;
  const v = value as Record<string, unknown>;
  return v.schemaVersion === 1 && isDeliveryID(v.deliveryId) && isDeliveryKey(v.deliveryKey) && isTaskID(v.taskId) && Number.isInteger(v.taskVersion) && (v.taskVersion as number) > 0 && typeof v.sessionId === "string" && (!sessionId || v.sessionId === sessionId);
}
export function isAttachedFile(value: unknown): value is AttachedFile {
  if (value === null || typeof value !== "object" || Array.isArray(value)) return false;
  const file = value as Record<string, unknown>;
  const allowed = new Set(["file_id", "original_name", "declared_mime", "declared_size_bytes", "transfer_state"]);
  return Object.keys(file).every(key => allowed.has(key)) && isFileID(file.file_id) && typeof file.original_name === "string" && byteLength(file.original_name) <= maxFileNameBytes && (file.declared_mime === null || (typeof file.declared_mime === "string" && byteLength(file.declared_mime) <= maxMimeBytes)) && (file.declared_size_bytes === null || (Number.isInteger(file.declared_size_bytes) && (file.declared_size_bytes as number) >= 0)) && ["metadata_only", "validated", "unavailable", "oversized", "content_changed"].includes(file.transfer_state as string);
}
export function isWork(value: unknown): value is Work {
  if (value === null || typeof value !== "object") return false;
  const w = value as Record<string, unknown>, s = w.source as Record<string, unknown> | undefined;
  return isDeliveryID(w.delivery_id) && isDeliveryKey(w.delivery_key) && isTaskID(w.task_id) && Number.isInteger(w.task_version) && (w.task_version as number) > 0 && (w.task_state === "open" || w.task_state === "working") && typeof w.task_text === "string" && byteLength(w.task_text) <= maxTaskTextBytes && s !== undefined && (s.kind === "local" || s.kind === "slack") && (w.attached_files === undefined || (Array.isArray(w.attached_files) && w.attached_files.length <= maxAttachedFiles && w.attached_files.every(isAttachedFile)));
}
export function isCounts(value: unknown): value is Counts { const v = value as Record<string, unknown>; return !!v && ["eligible_backlog_count", "uncertain_count", "rejected_count"].every(k => Number.isInteger(v[k]) && (v[k] as number) >= 0); }
