# Phase 3 file-transfer manual trial

**Status: UNEXECUTED.** This is a bounded operator-acceptance runbook, not a record of a completed trial or a security certification.

## Safety and prerequisites

Use a disposable Slack workspace/channel, disposable credentials held only in the owner-only local configuration, harmless fixtures, a private disposable XDG state root, and a safe output directory. Reinstall the app with `files:read` and `files:write`, and run the full automated gate for the revision under test first.

Do not record or paste credentials or tokens. Raw Slack messages remain in the intended task/read flow, while authenticated/private transfer URLs and file payload bytes remain transient transfer data rather than durable findings. Local filenames, paths, and ordinary diagnostics are not redacted in CLI/log/Pi output or the findings; Pi-visible values can enter model context.

## Bounded procedure

1. Start the foreground gateway. Verify connected status and the file-transfer doctor checks before any Slack file operation.
2. From a trusted user, send one direct mention with one harmless small attachment. Confirm Pi displays only `fil_…` metadata and retrieval guidance—never bytes, a private URL, or a temporary path.
3. Run `mosscap file show FILE --json`, then one `mosscap file get FILE` without output. Verify the returned path is private, owner-only, and reflected in status counts. The complete path may be recorded.
4. Run one `mosscap file get FILE --output SAFE_DESTINATION`. Independently compare the fixture and output without copying file payload bytes into the findings. Verify a non-forced repeat conflicts; do not test destructive paths.
5. Upload one harmless fixture with `mosscap slack file --file SAFE_FIXTURE --request-id UNIQUE_ID --json`. Verify a durable outgoing `fil_…`/effect result and that task state does not change.
6. Restart the gateway once. Confirm completed file operations are not replayed. Do not deliberately induce ambiguous Slack completion, unsafe redirects, races, or crashes.
7. Inspect status, doctor, file/effect output, Pi-visible diagnostics, and logs. Confirm credential values are redacted and no authenticated/private transfer URL or file payload bytes were deliberately persisted. Local paths and ordinary diagnostics may appear in full. Revoke disposable configuration and remove trial output/state.

## Stop conditions

Stop and mark the trial failed if bytes download without explicit `file get`; credentials are exposed; an authenticated/private transfer URL or raw file payload is deliberately persisted in durable records; a link target is changed; a completed or ambiguous operation is replayed; a file operation changes task state; or expiry/doctor reports an unsafe temporary condition without an action-required result. A local path or ordinary diagnostic appearing unredacted is not a failure.

## Record

Use [`../findings/phase-3-file-transfer.md`](../findings/phase-3-file-transfer.md). It must remain **UNEXECUTED** until an operator performs every applicable bounded step.
