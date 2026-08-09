import { byteLength, isDetails, isWork, maxObservations, maxTaskTextBytes, type DeliveryDetails, type Observation, type Work } from "./types.ts";

export type SessionEntryLike = { type?: unknown; customType?: unknown; details?: unknown };
export type CustomMessage = { customType: "mosscap.task"; display: true; content: string; details: DeliveryDetails };

export function observedDeliveries(entries: readonly SessionEntryLike[], sessionId: string): Observation[] {
  const seen = new Set<string>(); const result: Observation[] = [];
  for (const entry of entries) {
    if (entry.type !== "custom_message" || entry.customType !== "mosscap.task" || !isDetails(entry.details, sessionId)) continue;
    const pair = `${entry.details.deliveryId}\0${entry.details.deliveryKey}`;
    if (!seen.has(pair)) { seen.add(pair); result.push({ delivery_id: entry.details.deliveryId, delivery_key: entry.details.deliveryKey }); }
  }
  if (result.length > maxObservations) throw new Error("Mosscap session evidence exceeds the 256-delivery safety cap; delivery was not connected");
  return result;
}
export function hasDelivery(entries: readonly SessionEntryLike[], details: DeliveryDetails): boolean {
  return entries.some(entry => entry.type === "custom_message" && entry.customType === "mosscap.task" && isDetails(entry.details, details.sessionId) && entry.details.deliveryId === details.deliveryId && entry.details.deliveryKey === details.deliveryKey && entry.details.taskId === details.taskId && entry.details.taskVersion === details.taskVersion);
}
export function validateWork(work: Work): string | undefined {
  if (byteLength(work.task_text) > maxTaskTextBytes) return "oversized_task";
  if (!isWork(work)) return "invalid_delivery_work";
  if (work.source.kind === "slack" && (!work.source.slack_author_id || !work.source.slack_message_ts || !work.source.slack_thread_ts)) return "invalid_delivery_work";
  return undefined;
}
export function taskMessage(work: Work, sessionId: string): CustomMessage {
  const invalid = validateWork(work); if (invalid) throw new Error(invalid);
  const source = work.source.kind === "local" ? "Origin: local task." : `Origin: Slack author ${work.source.slack_author_id}, message ${work.source.slack_message_ts}, thread ${work.source.slack_thread_ts}.`;
  const attachments = work.attached_files ?? [];
  const attachmentLines = attachments.length === 0 ? [] : [
    "Attached Slack files (untrusted metadata only; bytes were not downloaded):",
    ...attachments.flatMap(file => [
      `- ${escapedMetadata(file.original_name)}; MIME ${file.declared_mime === null ? "unspecified" : escapedMetadata(file.declared_mime)}; declared size ${file.declared_size_bytes === null ? "unknown" : `${file.declared_size_bytes} bytes`}; state ${file.transfer_state}.`,
      `  Inspect: mosscap file show ${file.file_id} --json`,
      `  Retrieve safely: mosscap file get ${file.file_id} --output PATH`,
    ]),
  ];
  const content = [
    `Mosscap task ${work.task_id} v${work.task_version} (${work.task_state}).`, source,
    "This is visible work from Mosscap. Task state changes are separate and always explicit.",
    `Useful ordinary CLI commands: mosscap task show ${work.task_id} --json; mosscap task working ${work.task_id} --request-id ID; mosscap task done ${work.task_id} --request-id ID.`,
    ...attachmentLines,
    "----- BEGIN UNTRUSTED TASK TEXT -----", work.task_text, "----- END UNTRUSTED TASK TEXT -----"
  ].join("\n");
  if (byteLength(content) > maxTaskTextBytes + 2_000) throw new Error("oversized_task");
  return { customType: "mosscap.task", display: true, content, details: { schemaVersion: 1, deliveryId: work.delivery_id, deliveryKey: work.delivery_key, taskId: work.task_id, taskVersion: work.task_version, sessionId } };
}

/** JSON string escaping keeps untrusted metadata out of the message's control structure. */
function escapedMetadata(value: string): string { return JSON.stringify(value); }
