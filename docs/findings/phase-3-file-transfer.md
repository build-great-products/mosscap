# Phase 3 file-transfer trial findings

**Status: PASSED.** The authorized manual trial confirmed real outgoing upload, incoming retrieval/output placement, persisted Pi attachment delivery, and restart/no-replay behavior.

## Safe record fields

- Date/time: not retained in this durable finding
- Revision under test: not retained in this durable finding
- Automated-gate result: PASS — Go, race, vet, build, extension test/typecheck, and diff gates passed
- Disposable environment label (no path): isolated disposable environment removed after review
- Gateway/status/doctor result: PASS — connected/healthy gateway and doctor checks before/after operations
- Incoming `fil_…` ID and retrieval result: PASS — one admitted attachment retrieved only by explicit `file get`; temporary mode/hash/size checks passed
- Output-placement result: PASS — safe output placement matched hash/size, used mode `0600`, left no staging residue, and non-forced repeat conflicted
- Pi session acceptance result: PASS — real Pi 0.83 persisted-TUI opt-in showed one metadata-only attachment delivery; reconnect deduplicated; explicit opt-off passed
- Outgoing `fil_…`/effect ID and result: PASS — one real Slack outgoing upload confirmed (effect ID intentionally not retained)
- Restart/no-replay result: PASS — restart completed without replaying the confirmed outgoing effect
- Credential-redaction result: PASS — no credentials or tokens retained here
- Durable private-transfer-URL/file-byte exclusion result: PASS — no private Slack transfer URLs or raw file bytes retained here
- Overall pass/fail: PASS

Never record credentials or tokens. Do not copy authenticated/private Slack transfer URLs or raw file payload bytes into this durable finding; record only whether their transient lifecycle passed. Local filenames, paths, hashes, ordinary diagnostics, non-sensitive IDs/states/counts, and pass/fail outcomes do not require redaction. Exact paths may also appear in configured model context.

## Deviations, failures, and follow-up

- Early attempts used the wrong workspace user identity for isolated trust and were not admitted. The final fresh direct mention used the derived isolated-workspace human author and completed the incoming acceptance.
- No outstanding Phase 3 file-transfer acceptance deviation.
