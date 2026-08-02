# Phase 1 plan: Slack admission and operations MVP

**Status:** implementation-ready
**Authority:** `docs/SPEC.md` §§3, 4.2, 5.3–5.5, 6, 8, 11–14, and 17, interpreted against the current Phase 0 control plane and the unintegrated Slack substrate already in this tree.
**Release gate:** deterministic fake-Slack tests prove one eligible mention creates exactly one task and no ambiguous effect is automatically retried.

## 1. Outcome and boundaries

Phase 1 completes one production-shaped vertical slice:

1. `mosscap gateway run` validates one Slack app, bot, workspace, and channel, then owns one Socket Mode runtime;
2. a trusted human's direct bot mention is normalized and committed as one `open` Slack task before acknowledgement;
3. redelivery, source edits, source deletion, and incoming file **metadata** preserve one current task and monotonic version;
4. bounded history/thread reads and explicit text/reaction writes operate only in the configured channel;
5. every write intent is durable before network I/O, and uncertain submission becomes `ambiguous` without automatic retry; and
6. protocol, CLI, status, doctor, recovery, and secret-safe diagnostics expose enough information to operate that slice.

### In scope

- Complete Phase 1 Slack startup verification and failure-atomic runtime reload.
- SQLite-backed live trust as the admission authority, including an empty trust set.
- Socket Mode admission for initial messages, directly mentioned thread replies, edits, and deletions.
- Bounded source-event audit metadata and incoming Slack file metadata only.
- Configured-channel history and thread reads.
- Text `send`, `edit`, `delete`, `react`, and `unreact` effects.
- Effect list, inspect, and explicit retry.
- Slack source/deletion/file metadata in task/file read output.
- Phase 1 Slack status, doctor checks, structured diagnostics, reconnect, startup reconciliation, and shutdown.

### Explicitly out of scope

- Pi extension code, consumers, leadership, leasing, insertion, delivery, or delivery commands.
- Any incoming or outgoing file bytes, download, upload, temporary path, `file get`, `slack file`, or `slack send --file` behavior.
- Purge or retention deletion.
- More than one configured workspace or channel.
- Automatic Slack replies/reactions, implicit effects from task state, effect cancellation, or automatic retry of terminal effects.

`docs/SPEC.md` §5.5 requires a later visible delivery notice for a deleted source. This phase records deletion, increments the task content version, preserves text/state, and exposes `source_deleted_at`; constructing and inserting the Pi-visible notice remains Phase 2 because delivery is expressly excluded here.

## 2. Evidence from the current tree

The Phase 0 gateway already supplies the private XDG layout, strict TOML parsing and comment-preserving edits, local protocol, request deduplication, tasks, trust commands, status/doctor foundations, and restart-safe SQLite authority:

- `internal/config/config.go`: fixed paths, complete scalar file, redaction, setup, and atomic `config set`.
- `internal/server/server.go`: same-user private Unix server, dispatch, immutable active config pointer, reload seam, and orderly local shutdown.
- `internal/protocol/protocol.go`: version-1 framed JSON, strict envelopes/params, typed errors, frame bounds, and capability negotiation.
- `internal/store/store.go` and migrations 0001–0002: WAL/foreign keys/busy timeout, tasks, request records, and live trusted users.
- `internal/app/app.go`: strict CLI parsing, generated request IDs, JSON output, and stable Phase 0 exit mapping.

The current branch also contains an unintegrated Slack foundation that should be reused rather than rewritten:

- `internal/slackapi/**`: SDK-free bounded DTOs, validators, one-shot acknowledgement, failure classification, verification, and real adapters.
- `internal/slackapi/slackgo/socket.go`: a deliberately narrow, Mosscap-owned Socket Mode read/ack path. Retain this instead of replacing it with a high-level event loop that could acknowledge before SQLite commit.
- `internal/slackapi/slackgo/web.go`: one-call reads/writes with slack-go retries disabled and secret-safe classified errors.
- `internal/slackapi/fake/**`: deterministic FIFO Web/Socket fakes, barriers, reconnect sessions, and acknowledgement recorder.
- `internal/store/slack_source.go`: transactional admission, current trust lookup, source deduplication, edits/deletion/stale ordering, and metadata-only file reconciliation.
- `internal/store/slack_effect.go`: effect preparation/request replay, submission claim, terminal outcome, startup reconciliation, inspection/listing, and linked retry.

There is no production composition yet: no package receives Socket envelopes and calls `CommitSlackSourceEvent`, no service executes an effect through `slackapi.Web`, no active Slack runtime is owned by the server, and no Slack/effect protocol or CLI command is public.

### Migration-number reconciliation

A separate inventory described source/events/effects/files as migration 0002. That does **not** match this checkout. In the current tree:

- `0001_phase0.sql` is tasks/request state;
- `0002_trusted_users.sql` is trust only; and
- `0003_slack.sql` is source events, task sources, effects, files metadata, and indexes.

Terra work must use the checked-in schema-v3 history. It must not renumber or edit an applied migration. No Phase 1 migration is planned: schema v3 already represents every durable admission/effect/file-metadata fact needed by this MVP. If implementation uncovers a true schema defect, stop that increment and propose an append-only `0004_*.sql` plan amendment with upgrade/fingerprint tests; do not opportunistically change 0003.

Baseline evidence collected while planning:

```text
go version: go1.26.5 linux/amd64
go test ./...: pass
git diff --check: pass
slack-go: v0.27.0
gorilla/websocket: v1.5.3
```

## 3. Frozen Phase 1 contracts

These decisions resolve details left open by the selected SPEC sections and keep Terra increments independent.

### 3.1 Runtime snapshot

Define an immutable runtime snapshot containing:

- redacted-safe team, bot-user, and channel IDs;
- the complete active `config.Config` value;
- one verified `slackapi.Web`;
- one `slackapi.Socket` and currently opened stream generation; and
- a generation/ref-count boundary used only for draining, not as Pi leadership.

An operation acquires one snapshot before validation/preflight and retains it through its attempt. Reload prepares and verifies a complete candidate, opens it to Socket readiness, commits the reload decision, atomically publishes the candidate, then drains/closes the old stream after already-received admission and already-started operations finish. Candidate failure closes only the candidate and leaves the old snapshot/connection active.

Production `gateway run` requires non-empty, lexically valid `xapp-…`, `xoxb-…`, team `T…`, bot-user `U…`, and channel `C…|G…` values. `setup` may still generate empty credential/identity placeholders without contacting Slack. Direct server tests may inject a disabled/fake runtime; the production app must never silently choose disabled mode.

No new TOML key is required. The existing reconnect 1–60 second bounds, jitter flag, maximum file count, frame bound, and other §3 scalar values remain authoritative. Slack call contexts are bounded by the server request/runtime lifecycle; no hidden retry is added.

### 3.2 Admission coordinator

The coordinator depends on a narrow store interface:

```text
CommitSlackSourceEvent(ctx, policy, normalizedEvent) -> canonicalResult
```

For each `slackapi.Envelope` it:

1. captures the receipt-time runtime policy;
2. converts only bounded normalized fields to `store.SlackSourceEvent`;
3. calls the store once;
4. on store error, records a safe diagnostic and does **not** acknowledge;
5. on committed admission, ignore, malformed, stale, edit, deletion, or duplicate result, calls the one-shot acknowledgement exactly once; and
6. updates only runtime-safe received/committed timestamps and result codes.

It does not retain raw envelopes, write message text to logs, call Pi, or perform a Web read. Event identity is the adapter's Events API ID or its bounded derived identity. Thread identity remains `thread_ts`, falling back to `message_ts`.

The existing store remains the sole authorization/transaction authority. In the same transaction it checks current committed trust, workspace, channel, non-bot sender, and literal `<@BOT_USER_ID>`; creates at most one source/task; normalizes current text/files; and commits before returning.

### 3.3 Source update behavior

- A directly mentioned initial message creates one `open`, version-1 Slack task.
- A thread reply creates a separate task only if that reply independently passes every admission gate.
- An edit of an admitted source compares normalized line endings/trailing spaces, current attachment metadata, permalink, and deletion flag. Meaningful change replaces current values and increments version once; no-op edit advances source ordering without incrementing content version.
- An edit not already attached to a source may create a new task if it first adds a direct mention and all initial gates pass.
- `event_ts <= latest_event_at` records `ignored_stale` and cannot roll current content backward.
- Deletion preserves task text/state/files, sets deletion metadata, and increments version once. Repeated/older deletion does not increment again.
- File identity/name/MIME/size/permalink are untrusted metadata. No URL with credentials, hash of unreceived bytes, temporary path, or bytes are accepted from admission.

### 3.4 Reads and target validation

- `slack history` uses the configured channel, a default limit of 50, and a maximum of the existing `slackapi.MaxReadLimit` (100).
- `slack thread` validates its timestamp and reads one page of at most 100 from the configured channel.
- Read results contain timestamp/thread, immutable author ID, text, reactions, and file metadata; they are returned to the requesting client but never persisted or logged as bodies.
- No request DTO or CLI flag contains team/workspace/channel selectors.
- Before preparing a send-to-thread, edit, delete, react, or unreact effect, the service performs a configured-channel permalink/target lookup using the same acquired runtime snapshot. A missing or inaccessible target produces a typed read/external failure and no effect. A plain channel send needs no message preflight.
- `--task` resolves only an audit relation. It never supplies a timestamp or authorizes a destination.

### 3.5 Effect execution and recovery

Use one service path for all five public effect kinds:

1. normalize input and validate target/task relation;
2. perform required configured-channel target preflight;
3. call `PrepareSlackEffect` with the local request ID;
4. if the canonical effect is already terminal, return it without Web I/O;
5. atomically claim `prepared -> submitted` before Web I/O;
6. make exactly one Web call through the acquired runtime snapshot;
7. commit `confirmed`, `failed`, or `ambiguous`; and
8. return the current durable effect, not merely the original `prepared` mutation result.

Classification is conservative:

- conclusive Slack rejection -> `failed` and CLI exit 7 (`external_failed`);
- conclusive success -> `confirmed`;
- timeout, disconnect, cancellation, malformed success, or unknown outcome after claim -> `ambiguous` and CLI exit 8;
- failure before claim leaves `prepared`, eligible for later execution;
- a terminal or submitted effect is never moved back to `prepared`.

At startup, before accepting effect commands:

1. classify every unresolved `submitted` effect as `ambiguous`;
2. execute safely unsubmitted `prepared` effects in bounded batches through the current verified snapshot; and
3. never call Slack for reconciled ambiguous effects.

`effect retry` requires a reason and creates a new prepared effect linked to the unchanged failed/ambiguous predecessor. Ambiguous retry additionally requires `--confirm-duplicate-risk`. The new prepared effect enters the same executor; retry does not mutate or resubmit its predecessor. There is no cancellation command.

### 3.6 Protocol and CLI

Keep protocol version 1 because the common envelope, enum interpretation, and framing remain compatible. Additive commands are gated by advertised capabilities; clients return unsupported exit 9 when a required capability is absent.

Capabilities:

```text
slack_runtime
slack_reads
slack_effects
slack_source_metadata
```

Commands and strict DTOs:

```text
slack.history
slack.thread
slack.send
slack.edit
slack.delete
slack.react
slack.unreact
effect.list
effect.inspect
effect.retry
file.show                 # metadata only
task.get                  # additive Slack source/deletion/files fields
status.get / doctor.run   # additive Phase 1 fields/checks
```

Public CLI:

```text
mosscap slack history [--limit N] [--json]
mosscap slack thread THREAD_TS [--json]
mosscap slack send [--thread THREAD_TS] (--text TEXT|--text-file PATH) [--task TASK] [--request-id ID] [--json]
mosscap slack edit MESSAGE_TS (--text TEXT|--text-file PATH) [--task TASK] [--request-id ID] [--json]
mosscap slack delete MESSAGE_TS --reason TEXT [--task TASK] [--request-id ID] [--json]
mosscap slack react MESSAGE_TS EMOJI [--task TASK] [--request-id ID] [--json]
mosscap slack unreact MESSAGE_TS EMOJI [--task TASK] [--request-id ID] [--json]
mosscap effect list [--state prepared|submitted|confirmed|ambiguous|failed] [--json]
mosscap effect inspect EFFECT [--json]
mosscap effect retry EFFECT --reason TEXT [--confirm-duplicate-risk] [--request-id ID] [--json]
mosscap file show FILE [--json]
```

Do not accept `--workspace`, `--team`, `--channel`, `--file`, output paths, or file-byte commands in Phase 1. Every command supports help. Identifiers use existing typed unambiguous-prefix resolution. Mutation output includes request ID plus durable effect ID/state/kind/target; explicit inspect may show the bounded exact intent, but errors/status/logs may not.

Stable exits after this phase:

| Code | Category |
| --- | --- |
| 0 | success |
| 2 | usage |
| 3 | not found |
| 4 | conflict/stale selector or request |
| 5 | unsafe configuration/permissions |
| 6 | gateway unavailable |
| 7 | conclusive external operation failure |
| 8 | ambiguous external outcome |
| 9 | unsupported schema/protocol/capability |

### 3.7 Status, doctor, and logs

Add secret-safe status fields for:

- Slack state: `connecting`, `connected`, `reconnecting`, or injected-test `disabled`;
- configured team/bot/channel IDs;
- bounded reconnect attempt/backoff and last diagnostic code;
- last envelope received and last source result committed timestamps;
- source-event result counts;
- effect counts by state; and
- `action_required` counts for failed/ambiguous effects, separate from process health.

Preserve existing database/endpoint/schema/protocol/task/storage fields. Excluded Pi-delivery and temporary-byte fields remain absent or explicitly unsupported; do not fabricate implementation.

`doctor` stays non-destructive. It verifies private paths/config/database/protocol plus Slack auth, configured workspace/bot/channel access, required scopes, and every currently trusted immutable user ID. It does not send/edit/delete/react, acknowledge an event, download a file, open a long-lived competing runtime, or take Pi leadership. Network unavailability is reported distinctly from conclusive auth/access failure.

Structured logs may include request, source-event, task, effect IDs, timestamps, state/result enums, durations, and allowlisted diagnostics. They must exclude tokens/authorization headers, raw envelopes, full task/message/effect text, filenames unless explicitly required and bounded, Web response bodies, and file bytes.

## 4. Security and failure invariants

These are release-blocking:

1. **Commit before ack:** an identifiable envelope is acknowledged only after its admission/dedup result commits; DB failure leaves it unacknowledged.
2. **One source, one task:** one configured-workspace/channel/message source maps to at most one task across redelivery/restart.
3. **Current truth only:** task text is current-only and version-monotonic; stale events cannot roll it backward.
4. **Strict authority:** admission requires configured workspace/channel, current committed immutable-user trust, human sender, and literal direct bot mention. Empty trust admits nothing.
5. **Bounded retention:** no raw envelope, historical body/diff, token, authorization header, or file byte enters SQLite/log/status/protocol diagnostics.
6. **Single-channel operations:** callers cannot select workspace/channel; target preflight and write use the acquired configured-channel snapshot.
7. **Intent before write I/O:** exact effect intent and request replay result commit before submission claim and Slack mutation.
8. **Honest uncertainty:** possibly submitted but unconfirmed work is ambiguous and is never automatically retried.
9. **Explicit retry evidence:** retry creates a linked effect; ambiguous retry requires duplicate-risk confirmation.
10. **Task/effect independence:** Slack activity never changes task state, and task state never causes Slack activity.
11. **Failure-atomic reload:** candidate auth/access/readiness failure cannot replace or disrupt the prior active snapshot.
12. **Fail closed storage:** full/corrupt/incompatible SQLite stops new admission/ack and reports safe diagnostics; it is not auto-repaired.
13. **Bounded reconnect:** disconnect uses configured exponential jitter, deduplicates any redelivery, and makes no offline receipt claim.
14. **Safe startup:** unresolved submitted effects become ambiguous before execution; only prepared effects may be automatically submitted.
15. **Shutdown order:** stop Socket intake and close/drain Slack transports before closing SQLite or unlinking the private endpoint.

## 5. Reviewable Terra increments

One integration owner must coordinate the contracts above before implementation. File scopes below are exclusive: no two increments edit the same listed path, and no agent may make opportunistic edits outside its allocation.

### T0 — strengthen the deterministic fake boundary

**Commit:** `test: add Phase 1 fake Slack fault gates`
**Exclusive scope:** `internal/slackapi/fake/**` only.

- Add a per-Web-step reached/release barrier or observer without retaining secret/body diagnostics.
- Preserve strict FIFO expected calls and exact request capture.
- Add explicit pre-call/post-call timeout and disconnect scripts.
- Retain Socket reconnect sessions and one-shot ack recorder.
- Prove fakes themselves are race-safe and deterministic.

**Gate:** `go test -race ./internal/slackapi/fake`.

### T1 — coordinate admission and acknowledgement

**Commit:** `feat: coordinate committed Slack admission`
**Exclusive scope:** new `internal/slackingest/**` only.
**Depends on:** T0.

- Implement the envelope-to-store adapter and commit-before-ack sequence in §3.2.
- Handle all four existing normalized kinds: initial/app mention, message, change, deletion.
- Treat safely identifiable malformed envelopes as committable/acknowledgeable; leave unidentifiable input without ack.
- Expose safe last-received/last-committed/result observations to runtime status.
- Keep store and acknowledgement behind narrow interfaces for injected commit failure.

**Gate:** focused tests cover eligible mention, every admission rejection, empty/live trust behavior, thread reply, redelivery, malformed event, commit failure/no-ack, stale/no-op/meaningful edit, edit-first-mention, deletion preservation, and metadata-only files.

### T2 — implement reads and the durable effect executor

**Commit:** `feat: execute configured-channel Slack operations`
**Exclusive scope:** new `internal/slackops/**` only.
**Depends on:** T0.

- Implement bounded history/thread reads.
- Implement target preflight and all five Web mutation mappings.
- Drive existing prepare/claim/outcome/reconcile/retry store primitives.
- Serialize/claim safely so concurrent same-request calls cannot duplicate Web I/O.
- Recover submitted/prepared effects as specified; process prepared batches until empty.
- Return domain results/errors suitable for protocol mapping without SDK types.

**Gate:** table tests prove configured-channel calls and normalized fields for all operations; barriers prove `submitted` is durable before the fake observes a write; request replay makes no second call; pre-submit timeout stays prepared; post-submit timeout becomes ambiguous; startup makes no call for ambiguous effects; explicit retry preserves predecessor and confirmation rule.

### T3 — manage verified runtime snapshots

**Commit:** `feat: manage verified Slack runtime snapshots`
**Exclusive scope:** new `internal/slackruntime/**`, new `internal/config/slack_phase1.go`, and new `internal/config/slack_phase1_test.go`.
**Depends on:** T1, T2.

- Add strict non-empty production Slack config validation without weakening setup.
- Add injectable real/fake factories; the real factory constructs the existing narrow slack-go Web/Socket adapters.
- Verify auth/team/bot/channel/scopes and Socket readiness before candidate activation.
- Run synchronous envelope handling, bounded jittered reconnect, state observations, and drain/close.
- Acquire immutable snapshots for operations; reload publishes only a complete ready candidate.
- Never place token values in returned errors or status.

**Gate:** deterministic tests cover missing/invalid config, identity/access/scope mismatch, startup readiness, reconnect backoff/jitter reset, no offline receipt claim, candidate failure retaining old snapshot, successful swap, old-attempt retention, and close-before-return.

### T4 — add Phase 1 store read models

**Commit:** `feat: expose Slack operational read models`
**Exclusive scope:** new `internal/store/phase1_queries.go` and `internal/store/phase1_queries_test.go` only.
**Can run parallel with:** T1–T3 after DTO freeze.

- Add one task read model joining optional Slack source/deletion/files while preserving local-task shape.
- Add effect counts by state and action-required counts without payload text.
- Reuse existing effect/file/source selectors and stable ordering.
- Do not alter schema or migrations.

**Gate:** real-SQLite tests prove local/Slack DTO compatibility, deleted-source/file metadata, unambiguous prefixes, count accuracy, and absence of text from diagnostics/counts.

### T5 — add additive protocol-v1 contracts

**Commit:** `feat: add Phase 1 protocol capabilities`
**Exclusive scope:** `internal/protocol/**` only.
**Depends on:** frozen T1–T4 domain DTOs; implementation may proceed in parallel once interfaces are committed.

- Add capabilities, commands, strict params, typed result DTOs, and source/file/effect/status fields.
- Validate limits, timestamps, states, emoji, required reason, mutually exclusive text source after CLI resolves file content, and duplicate-risk confirmation.
- Preserve strict unknown-field rejection, typed enums, request IDs, and frame preallocation bound.
- Keep version 1 and make capability absence explicitly unsupported.

**Gate:** encode/decode round trips for every command/result, unknown field/enum rejection, oversized frame rejection, capability negotiation, and token/body canaries absent from error DTOs.

### T6 — compose the production gateway

**Commit:** `feat: run Phase 1 Slack services in the gateway`
**Exclusive scope:** `internal/server/**`, `internal/status/**`, and `internal/doctor/**` only.
**Depends on:** T1–T5.

- Add server options for an injected disabled/fake runtime while requiring production app injection.
- Startup: open/verify SQLite, reconcile submitted effects, prepare verified Slack snapshot, recover prepared effects, bind endpoint, then activate Socket intake.
- Dispatch Slack/effect/file-metadata commands and joined task reads.
- Refactor reload orchestration so slow Slack verification/readiness occurs before the short durable reload transaction; publish only after its successful decision and drain the old snapshot afterward.
- Add Phase 1 status/doctor behavior and typed error mapping.
- On storage failure, stop admission/ack; on shutdown, stop Socket and operation intake before store close.

**Gate:** server tests with fake runtime prove startup/reload ordering, dispatch/capabilities, effect recovery, ack ordering, status health versus action-required separation, doctor non-mutation, and secret-safe failures.

### T7 — expose the CLI and production runtime

**Commit:** `feat: add Phase 1 Slack CLI`
**Exclusive scope:** `internal/app/**` only.
**Depends on:** T5, T6.

- Add exact commands/flags in §3.6 and root/branch/leaf help.
- Construct and inject the real Slack runtime only for production `gateway run`.
- Reuse private endpoint/capability handshake/request-ID/text-file patterns.
- Add exit 7 and exit 8 mappings; preserve exits 0–6/9.
- Reject all workspace/channel selectors and file-byte flags/commands.
- Render explicit read text only on requested read output; render mutation identity/state without leaking effect text to errors.

**Gate:** table-driven parser/help/human/JSON tests cover every command, required/mutually exclusive flags, generated and supplied request IDs, capability absence, external-failed/ambiguous exits, and forbidden selectors/file flags.

### T8 — prove the Phase 1 vertical slice and document operation

**Commit:** `test: prove the Phase 1 Slack MVP gate`
**Exclusive scope:** new `internal/e2e/phase1_test.go`, new `docs/operations/phase-1-slack.md`, and Phase 1-only README edits.
**Depends on:** T0–T7.

- Run real server/client/CLI paths against deterministic fake Web/Socket adapters and a real temporary SQLite ledger.
- Document Slack app setup, least scopes/subscriptions, private credential placement, startup/status/doctor, trust, source inspection, reads/writes, ambiguous inspection/retry, reconnect, and shutdown.
- Include an optional disposable-workspace smoke checklist. It is exploratory adapter/UX evidence, not a substitute for or blocker beyond the deterministic release gate; never commit tokens or message bodies.

**Gate:** §7 plus full regression/race/vet/build validation.

## 6. Dependency graph and integration rule

```text
T0 fake gates ─┬─> T1 admission ─┐
               └─> T2 operations ├─> T3 runtime ─┐
T4 store queries ────────────────────────────────┤
T1–T4 frozen DTOs ─> T5 protocol ───────────────┤
                                                 v
                                          T6 server integration
                                                 |
                                                 v
                                            T7 CLI/app
                                                 |
                                                 v
                                            T8 E2E/docs
```

T1, T2, and T4 may run in parallel after the interface freeze. T5 may begin when their exported DTOs are committed. Only T6 edits central server routing/lifecycle. Only T7 edits app command routing. Each increment must hand off changed files, focused validation, discovered SPEC ambiguity, and deliberately deferred work.

## 7. Deterministic fake-Slack release gate

The Phase 1 release test must execute these observable scenarios, not merely unit-test helpers.

### Configuration/runtime

1. Production start rejects missing/invalid Slack tokens or IDs without echoing values.
2. Auth/team/bot/channel/scope mismatch fails before endpoint/runtime activation.
3. Failed reload leaves prior config, Web client, and Socket stream usable.
4. Successful reload routes new work to the new snapshot while a started old operation finishes on the old snapshot.
5. Disconnect reconnects with deterministic bounded jitter; no event is claimed while offline.

### Admission/source lifecycle

6. One trusted direct mention in the configured workspace/channel creates one open Slack task; fake ack observes the committed task/source/result.
7. Two deliveries of the same event and alternate duplicate source delivery create one task; each ack follows a canonical committed lookup.
8. Injected begin/write/commit failure creates no success result and makes zero ack calls.
9. Empty trust, wrong workspace/channel, untrusted sender, bot sender, and unmentioned human create no task and record the expected bounded result.
10. A safely identifiable malformed event records `malformed` then acks; an unidentifiable frame is not acked.
11. A thread reply requires its own direct mention and uses the root thread identity.
12. Stale/no-op/meaningful edits prove no rollback and exact version behavior; an edit that first adds mention may admit.
13. Deletion preserves text/state/files, records deletion time, increments once, and is visible through `task show`.
14. Incoming file metadata is bounded and inspectable; no test observes a download, upload, temporary path, or file bytes.

### Reads/effects

15. History/thread use only the configured channel, enforce 50/default and 100/max bounds, return one bounded page, and leave SQLite unchanged.
16. Target preflight rejects a message/thread outside the configured channel before effect preparation.
17. A table proves send/edit/delete/react/unreact call the correct fake method exactly once with the configured channel and normalized input.
18. At the fake write barrier, SQLite already contains `submitted` with the exact intent/hash and request record.
19. Conclusive success/rejection becomes `confirmed`/`failed`; CLI exits 0/7.
20. A pre-submit timeout makes no Web call and leaves prepared work eligible.
21. A post-submit timeout/disconnect/malformed response becomes ambiguous; CLI exits 8.
22. Restart converts leftover submitted to ambiguous and performs zero duplicate Web calls.
23. Restart executes safely prepared work once; concurrent request replay cannot create or submit a second effect.
24. Explicit retry creates a linked effect, preserves the predecessor, and enforces ambiguous duplicate-risk confirmation.
25. Slack reads/writes/retries do not change task state; task transitions do not create effects.

### Protocol/operations/security

26. Every Phase 1 CLI path traverses protocol v1 capability negotiation; absent capability exits 9.
27. Unknown fields/enums and oversized frames fail safely; all identifiers abbreviate only when unambiguous.
28. Status separates healthy process state from failed/ambiguous action required.
29. Doctor performs no Slack mutation, acknowledgement, file transfer, or Pi action.
30. Token, authorization-header, raw-envelope, and body canaries do not appear in SQLite audit/diagnostics, logs, status, doctor, protocol errors, or mutation results.
31. Gateway shutdown closes/drains Socket before SQLite and preserves all committed state across restart.

The gate passes only if all scenarios pass repeatedly under `go test -race ./...` and there is no automatic call for an ambiguous predecessor.

## 8. Validation and release procedure

Each Terra increment runs its focused package tests and `git diff --check`. The integration owner then runs from a clean tree:

```sh
gofmt -w <changed Go files>
go test ./...
go test -race ./...
go vet ./...
go build ./cmd/mosscap
go mod tidy -diff
git diff --check
```

Release review additionally verifies:

- schema remains version 3 and migrations 0001–0003 are byte-identical;
- protocol remains version 1 and every additive command has a capability gate;
- no public workspace/channel selector or file-byte/Pi/purge command was added;
- no Slack SDK type crossed into store/protocol/app domain contracts;
- all Phase 0 restart/request-dedup/local-task/trust tests still pass;
- the deterministic scenarios in §7 pass; and
- documentation contains no credentials or private message bodies.

**Phase 1 is releasable when:** an eligible mention produces exactly one committed task before acknowledgement, all explicit single-channel operations have durable outcomes, and restart never automatically retries an ambiguous effect.

## 9. Known risks and stop conditions

| Risk | Mitigation / stop condition |
| --- | --- |
| High-level Socket library behavior could ack early. | Keep the existing narrow `slackgo.Socket` ack callback; reject any replacement that hides ack timing. |
| Reload verification may hold a SQLite transaction or disrupt old work. | Prepare/verify/open candidate before the short durable decision; integration tests barrier both old and candidate paths. |
| Request replay races with effect execution. | Store claim is the only submission authority; service handles transition loss by reading current durable state, never calling Web speculatively. |
| Adapter error text may contain credentials or message bodies. | Persist/map allowlisted `slackapi` diagnostics only; canary tests inspect all durable and rendered surfaces. |
| Source edits/deletions arrive in different Slack shapes. | Keep normalization in the adapter, domain event kinds closed, and test captured bounded fixtures without retaining raw payloads. |
| File metadata work expands into transfer. | Interfaces contain metadata only; any byte/path/hash/download/upload request is a Phase 3 blocker, not a Phase 1 enhancement. |
| Existing migration inventory is inconsistent. | Current git tree is authoritative: schema v3/0003 Slack. Any required schema change stops for an append-only migration review. |
| Central files attract parallel conflicts. | T6 alone owns server/status/doctor; T7 alone owns app; all package contracts freeze first. |
| Pi-visible deletion notice is requested before delivery exists. | Expose durable deletion/current text now; defer notice insertion to Phase 2 and do not create a placeholder delivery. |
| Real Slack behavior differs from deterministic adapters. | Use optional disposable-workspace smoke evidence to amend a later plan; never weaken release invariants or introduce credentials into tests. |

If a release-blocking invariant cannot be met within an increment's exclusive scope, stop, commit a clearly named WIP branch, report exact evidence, and amend this plan before broadening scope.
