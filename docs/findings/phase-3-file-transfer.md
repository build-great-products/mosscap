# Phase 3 file-transfer trial findings

**Status: PARTIALLY EXECUTED.** The manual trial confirmed one real Slack outgoing upload and passed the restart/no-replay check. The incoming path has not been exercised.

## Safe record fields

- Date/time: not retained in this durable finding
- Revision under test: not retained in this durable finding
- Automated-gate result: not recorded by this partial manual trial
- Disposable environment label (no path): not recorded
- Gateway/status/doctor result: sufficient for the confirmed outgoing upload; detailed diagnostics not retained
- Incoming `fil_…` ID and retrieval result: UNEXECUTED — no fresh trusted direct mention with an attachment was available
- Output-placement result: UNEXECUTED — depends on incoming retrieval
- Pi session acceptance result: UNEXECUTED — depends on incoming admission and output placement
- Outgoing `fil_…`/effect ID and result: PASS — one real Slack outgoing upload confirmed (effect ID intentionally not retained)
- Restart/no-replay result: PASS — restart completed without replaying the confirmed outgoing effect
- Credential-redaction result: PASS — no credentials or tokens retained here
- Durable private-transfer-URL/file-byte exclusion result: PASS — no private Slack transfer URLs or raw file bytes retained here
- Overall pass/fail: PARTIAL PASS — outgoing upload and restart/no-replay passed; incoming coverage remains unexecuted

Never record credentials or tokens. Do not copy authenticated/private Slack transfer URLs or raw file payload bytes into this durable finding; record only whether their transient lifecycle passed. Local filenames, paths, hashes, ordinary diagnostics, non-sensitive IDs/states/counts, and pass/fail outcomes do not require redaction. Exact paths may also appear in configured model context.

## Deviations, failures, and follow-up

- Incoming admission, retrieval, output placement, and Pi session acceptance are blocked pending a future fresh trusted direct mention containing one harmless attachment.
- Do not infer incoming-path coverage from this outgoing-only partial trial.
