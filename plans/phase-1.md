# Mosscap v1 Phase 1 implementation plan

**Status:** implementation-ready plan

**Normative source:** [`SPEC.md`](../SPEC.md), especially sections 1–6, 8, 11–18. If this plan and the specification differ, the specification wins.

## 1. Goal and release gate

Phase 1 adds one durable Slack vertical slice to the completed local gateway:

1. the foreground gateway validates one Slack workspace, bot, and channel and maintains a Socket Mode connection;
2. a trusted human's direct bot mention is normalized and committed as exactly one `open` task before Mosscap acknowledges its Slack envelope;
3. source redelivery, edits, deletion, stale events, and incoming file metadata preserve the source/task invariants;
4. bounded history and thread reads target only the configured channel;
5. send, edit, delete, react, and unreact commands commit one exact effect intent before Slack I/O;
6. conclusive outcomes become `confirmed` or `failed`, while possibly submitted outcomes become `ambiguous` and are never retried automatically; and
7. status and doctor expose Slack health separately from operator-action-required outcomes without leaking tokens or message bodies.

The release gate uses a deterministic fake Slack service. It must prove:

- two deliveries of one eligible envelope produce one source-event result and one task, and both envelopes are acknowledged only after the committed result exists;
- an injected commit failure prevents acknowledgement;
- a post-submit timeout creates one `ambiguous` effect and restart makes no second Slack API call; and
- explicit retry creates a linked effect rather than mutating or silently resubmitting the predecessor.

A documented manual smoke test against one real Slack development workspace completes the phase gate.

## 2. Scope boundaries

### 2.1 Included

- Real Slack Web API and Socket Mode adapters.
- Required Slack configuration validation and the existing reload verifier seam completed with real checks.
- Foreground connection lifecycle, bounded reconnect backoff, and connection diagnostics.
- Strict single-workspace/channel/trusted-user/direct-mention admission.
- Source-event deduplication and bounded admission audit metadata.
- Slack-origin task creation, source edits, source deletion, and incoming file metadata.
- Bounded Slack history and thread reads.
- Durable text send/edit/delete/react/unreact effects.
- Effect list, inspect, and explicit retry.
- Slack source metadata in task show/read DTOs.
- File metadata inspection where useful; no file-byte transfer.

### 2.2 Explicitly deferred

- Pi consumers, leadership, leases, delivery attempts, and the TypeScript extension (Phase 2).
- Incoming file download, outgoing upload, `slack file`, and `--file` execution (Phase 3).
- Permanent file bytes, purge, and automatic durable-record retention (Phase 3).
- Automatic replies, reactions, task transitions, or effect retries.
- More than one workspace or channel.

The CLI may expose future file flags in help only if invoking them returns typed `unsupported` without preparing an effect. Do not create placeholder successful records for unimplemented transfer behavior.

## 3. Dependencies and package boundaries

Add one direct dependency:

| Dependency | Version | Purpose |
| --- | --- | --- |
| `github.com/slack-go/slack` | `v0.27.0` | Web API types/client and `socketmode` transport |

Keep the SDK behind Mosscap-owned interfaces. Store, task, protocol, and app packages must not import Slack SDK types.

Add these package boundaries:

```text
internal/slackapi/       Mosscap DTOs, interfaces, error classification
internal/slackapi/slackgo/ real slack-go Web API and Socket Mode adapters
internal/slackapi/fake/  deterministic scripted fake used by tests
internal/slackingest/    envelope normalization and admission orchestration
internal/effect/         effect validation, preparation, execution, reconciliation
```

Names may be adjusted to avoid package stutter, but preserve the dependency direction:

```text
app -> client/config/protocol
server -> slackingest/effect/slackapi/store
slackingest/effect -> slackapi + store domain DTOs
slackapi/slackgo -> slack-go SDK
store -> no Slack SDK
```

Define separate interfaces for Web API calls and Socket Mode lifecycle. Tests inject both independently. No test outside a real opt-in smoke test may contact Slack.

The Web interface needs only the Phase 1 operations:

- auth/team/bot/channel/scope and trusted-user verification;
- permalink lookup;
- bounded history and replies;
- post/update/delete message; and
- add/remove reaction.

The Socket interface emits a Mosscap envelope with an explicit acknowledgement callback. The callback must be callable at most once and only by the ingestion coordinator after a committed admission/dedup result.

## 4. Configuration, startup, reload, and Slack lifecycle

### 4.1 Configuration validation

Keep Phase 0 local-only setup usable, but Phase 1 gateway Slack startup requires non-empty, lexically valid:

- `slack_app_token` (`xapp-…`);
- `slack_bot_token` (`xoxb-…`);
- workspace/team ID;
- bot user ID; and
- channel ID.

Never include supplied values in validation errors. `config show`, status, doctor, protocol results, logs, SQLite, and fake call diagnostics must remain token-free.

A conclusive Slack authentication, team mismatch, bot mismatch, inaccessible channel, or missing required scope is startup-fatal. A transient network outage after local validation is connection-degraded rather than unsafe configuration: the foreground gateway remains available locally and reconnects with configured exponential 1–60 second jittered backoff.

Required app setup for the operator documentation:

- Socket Mode enabled and an app-level token with `connections:write`;
- bot event subscriptions `app_mention` and `message.channels` for a public channel, plus `message.groups` when the configured channel is private;
- bot scopes `app_mentions:read`, `chat:write`, `reactions:read`, `reactions:write`, `users:read`, and `files:read`;
- `channels:read` and `channels:history` for a public configured channel;
- `groups:read` and `groups:history` for a private configured channel; and
- bot membership in the configured channel.

Confirm the exact scope/event behavior against the selected slack-go and Slack API version before freezing a checked-in example app manifest. If Slack requires an additional scope for permalink lookup or exposes a narrower supported set, update the manifest, doctor checks, and this plan together rather than silently broadening runtime authority.

### 4.2 Runtime manager

Add one gateway-owned Slack runtime manager. It owns:

- the active Web client and Socket Mode connection;
- connection/reconnect state and bounded diagnostics;
- last envelope received and last source result committed timestamps;
- backoff state driven by an injectable clock/random source; and
- shutdown ordering.

The manager must not own durable domain state. It passes normalized envelopes to ingestion and effects through interfaces whose durable decisions remain in SQLite.

### 4.3 Failure-atomic reload

Replace the Phase 0 no-op verifier with a prepared activation protocol:

1. parse and locally validate the staged TOML;
2. construct candidate Slack clients without exposing credentials;
3. perform auth/team/bot/channel/scope checks;
4. establish or prove readiness of the candidate Socket Mode connection;
5. if any conclusive or transient preparation step fails, abort the candidate and retain the old active snapshot and connection;
6. commit the digest-aware reload result;
7. atomically publish the new config/client/runtime snapshot; then
8. drain and close the old Socket Mode connection after already-received envelopes finish their commit/ack path.

Already-started Slack effects retain the Web client/config snapshot with which they began. New operations use the new snapshot. Do not close the old client underneath in-flight calls.

The successful result sets `slack_network_verification_deferred=false`. Remove the pre-Slack disclaimer only after real verification is wired and tested. Preserve the existing token-free digest deduplication behavior.

### 4.4 Shutdown and reconnect

On gateway shutdown:

1. stop accepting new Socket envelopes;
2. allow already-received envelopes a bounded chance to commit and acknowledge;
3. stop claiming prepared effects for execution;
4. allow submitted API calls the normal server shutdown grace period;
5. classify unresolved submitted effects as ambiguous before store close where possible; and
6. close Slack transports before closing SQLite.

Reconnect never implies receipt while offline. On reconnect, rely on source-event deduplication for Slack redelivery and make no stronger Socket Mode retention claim.

## 5. SQLite migration 0003

Add `internal/store/migrations/0003_slack.sql`, increment `SchemaVersion`, and retain exact checksum/history checks. Upgrade only an intact schema-v2 ledger in one transaction. Preserve all Phase 0/SQLite-trust data.

The SQL may be normalized to project conventions, but must represent the following records and constraints.

### 5.1 Slack task sources

One admitted source message maps to at most one task:

```text
slack_task_sources
  task_id                 PK/FK tasks, source_kind must be slack in repository logic
  initial_source_event_id UNIQUE
  team_id
  channel_id
  author_user_id
  message_ts              UNIQUE with team_id/channel_id
  thread_ts
  permalink nullable
  latest_event_at_us
  source_deleted_at_us nullable
  created_at_us
  updated_at_us
```

Validate Slack timestamp strings lexically and convert event ordering timestamps to integer microseconds before entering the store. Preserve message/thread timestamp strings as opaque Slack identifiers.

### 5.2 Source events

```text
slack_source_events
  source_event_id         PRIMARY KEY
  envelope_id nullable
  event_kind
  team_id nullable
  channel_id nullable
  message_ts nullable
  thread_ts nullable
  user_id nullable
  slack_event_at_us nullable
  received_at_us
  committed_at_us
  admission_result
  task_id nullable FK tasks
  diagnostic_code nullable
```

`admission_result` is constrained to the SPEC values plus explicit update lifecycle results:

```text
admitted, updated, deleted, duplicate,
ignored_workspace, ignored_channel, ignored_untrusted,
ignored_unmentioned, ignored_bot, ignored_stale,
ignored_untracked, malformed
```

Do not persist raw envelopes, full message bodies, prior task text, diffs, display names, or unbounded Slack errors. `diagnostic_code` is a small allowlisted enum/string, not arbitrary SDK text.

A duplicate delivery returns the already committed result. It need not insert a second source-event row merely to say `duplicate`; the stored canonical result plus a typed duplicate response satisfies inspection, provided duplicate counts/diagnostics remain available without raw payload retention.

### 5.3 Incoming file metadata

Create Phase 1 metadata-only records:

```text
files
  file_id                  PRIMARY KEY, fil_…
  direction                from_slack|to_slack
  slack_file_id nullable
  permalink nullable
  original_name
  declared_mime nullable
  size_bytes nullable
  sha256 nullable
  related_task_id nullable FK tasks
  related_effect_id nullable
  transfer_state
  diagnostic_code nullable
  temporary_path nullable
  created_at_us
  updated_at_us
```

For Phase 1, only `from_slack` metadata with a non-live transfer state is created. Add uniqueness preventing one Slack file identity from being attached twice to the same admitted source. Use a join table if one file may relate to multiple tasks; do not encode lists as JSON.

Filenames, MIME labels, permalinks, and file IDs are bounded untrusted metadata. No bytes are downloaded in Phase 1.

### 5.4 Effects

```text
slack_effects
  effect_id                PRIMARY KEY, eff_…
  idempotency_key          UNIQUE
  predecessor_effect_id nullable FK slack_effects
  related_task_id nullable FK tasks
  kind                     send|edit|delete|react|unreact|file_upload
  team_id
  channel_id
  target_message_ts nullable
  thread_ts nullable
  text nullable
  emoji nullable
  operator_reason nullable
  payload_hash             64 lowercase hex
  state                    prepared|submitted|confirmed|failed|ambiguous
  attempt_count
  prepared_at_us
  submitted_at_us nullable
  completed_at_us nullable
  response_message_ts nullable
  response_file_id nullable
  diagnostic_code nullable
  created_at_us
  updated_at_us
```

Repository validation enforces operation-specific required/forbidden columns. Phase 1 never prepares `file_upload` through public commands, but reserving the constrained enum avoids reinterpreting the state later.

Effect text is the exact current intent, not task history. Bound it to Slack/API and local protocol limits. Diagnostics are sanitized bounded codes/messages and never contain tokens or full external payloads.

Indexes must support:

- source lookup by team/channel/message;
- source events by committed time/result;
- effects by state/update time;
- predecessor traversal; and
- exceptional status counts.

## 6. Slack event normalization and admission

### 6.1 Adapter output

Translate SDK types at the boundary into a small normalized envelope containing only fields needed in memory:

- envelope ID and Events API event ID;
- event kind and event timestamp;
- team/channel/user/bot/subtype identity;
- message timestamp and thread timestamp;
- current text;
- current bounded file metadata;
- edit/deletion identity; and
- the one-shot acknowledgement function.

Reject oversized envelope/text/metadata before unbounded allocation where the SDK allows. Cap file count and each metadata field even though bytes are not downloaded.

### 6.2 Stable event identity

Use Slack's outer Events API `event_id` when present. For safely identifiable events without one, derive a documented SHA-256 identity from immutable envelope/event fields. Never derive identity from mutable display text alone.

The source message identity is `(team_id, channel_id, message_ts)`. Thread identity is `thread_ts` when present, otherwise `message_ts`.

### 6.3 Initial mention transaction

For every supported message/app-mention envelope:

1. validate the envelope shape and derive stable source identity outside the transaction without acknowledging;
2. begin one SQLite write transaction;
3. look up the source-event identity;
4. if committed, return its canonical result;
5. evaluate configured workspace and channel from the config snapshot attached to receipt;
6. reject bot/system/subtype senders;
7. query the current committed `trusted_users` table in the same transaction;
8. require a literal `<@BOT_USER_ID>` token in the current Slack text;
9. normalize text using existing task normalization and normalize bounded file metadata;
10. create one `slack`, `open`, version-1 task, source row, file metadata, and admitted source event atomically; or create only the bounded ignored/malformed source event;
11. commit; then
12. invoke the one-shot Slack acknowledgement.

A commit failure, busy timeout, cancellation before commit, or corrupt/full database must not acknowledge. Return control to Socket Mode so Slack may redeliver. A known committed duplicate is acknowledged only after the lookup confirms it.

Display names, channel membership, thread participation, `@channel`, and replies in bot threads never substitute for immutable user trust plus direct mention.

### 6.4 Replies

A thread reply creates a separate task only when that reply independently passes every admission check, including its own direct mention. Its source message timestamp remains the reply timestamp; its thread timestamp points to the root.

### 6.5 Edits

For message-change events:

- deduplicate the edit event identity first;
- find an existing source by team/channel/message timestamp;
- reject an event whose ordering timestamp is not newer than `latest_event_at_us` as `ignored_stale`;
- if no source exists, admit the edited current message as a new task only when all initial checks now pass;
- for an existing admitted source, preserve authorization identity and update current text/file metadata when meaningfully changed, even if the edit removes the mention;
- increment task version exactly once for a meaningful text, attachment, permalink, or deleted-source metadata change;
- retain no old text or durable diff; and
- update source/latest-event and source-event rows in the same transaction.

Line-ending and trailing-space normalization follows existing task rules. A connected Pi diff remains Phase 2 work.

### 6.6 Deletions

For deletion events:

- deduplicate and stale-check as above;
- if no admitted source exists, store only the bounded inspectable event result;
- otherwise set source deletion time, advance latest-event ordering, increment task version once, preserve task text/state/files, and commit a visible deleted-source flag for later delivery;
- repeated/older deletions are duplicates or stale and do not increment again.

Deletion never completes, erases, or tombstones a task.

## 7. Slack reads

Implement bounded, non-mutating gateway operations:

```text
slack.history
slack.thread
```

Every call uses the active configured workspace/channel. No request accepts workspace/channel selectors. Validate thread timestamps lexically and enforce configured maximum/default limits.

Return stable Mosscap DTOs containing only message timestamp, thread identity, immutable author ID, text, reactions, and file metadata needed for explicit later commands. Do not persist read responses or log text.

Classify Slack failures as:

- typed conclusive external failures for invalid targets/access;
- retriable/unavailable for calls known not to mutate external state; and
- sanitized errors with Slack request IDs only when safe.

## 8. Durable Slack effects

### 8.1 Command validation

Implement these Phase 1 intents:

```text
send(text, optional thread, optional task relationship)
edit(message_ts, text, optional task relationship)
delete(message_ts, required reason, optional task relationship)
react(message_ts, emoji, optional task relationship)
unreact(message_ts, emoji, optional task relationship)
```

Normalize text and emoji representation before hashing. Validate optional task existence for audit relation only. It never derives a target or grants authority.

Before preparing edit/delete/react/unreact, verify through a bounded Slack read that the target belongs to the configured channel. A source-task convenience lookup may supply a timestamp only when the CLI explicitly chooses that source; it does not bypass target validation.

### 8.2 Prepare transaction

Every local mutation has a request ID. In one transaction:

1. canonicalize the exact intent and active team/channel target;
2. compute payload hash and idempotency key;
3. replay an existing matching mutation request without creating another effect;
4. reject request-ID reuse with different input;
5. insert one `prepared` effect with attempt count 0; and
6. store a compact token-free mutation result identifying the effect.

Commit before any Slack API call. A disconnect after commit can replay the request ID and recover the same effect.

### 8.3 Submission and outcome

Use a single bounded effect executor owned by the gateway initially; concurrency may be increased only with per-effect claims and deterministic tests.

For an eligible prepared effect:

1. atomically transition `prepared -> submitted`, increment attempt count, and commit before API invocation;
2. attach the effect/idempotency key where Slack supports a client message ID;
3. make exactly one API call;
4. classify the response:
   - conclusive success -> `confirmed` with Slack identifiers;
   - conclusive API rejection before/after transmission -> `failed` with bounded diagnostic;
   - timeout, cancellation, disconnect, malformed response, or any uncertainty after submission -> `ambiguous`;
5. commit the outcome before returning it to the caller.

Never transition `submitted` back to `prepared`. On gateway startup, classify every unresolved `submitted` effect as `ambiguous` before starting execution. A safely never-submitted `prepared` effect remains eligible and may execute.

Task state and Slack activity remain independent in both directions.

### 8.4 Explicit retry

`effect retry` requires a reason and creates a new `prepared` effect linked by `predecessor_effect_id`. It never changes the predecessor.

- `ambiguous` predecessor: require `--confirm-duplicate-risk`;
- `failed` predecessor: confirmation flag not required;
- `confirmed`, `prepared`, or `submitted`: reject retry;
- preserve the exact predecessor operation/target/payload unless the operator issues a new Slack command;
- record only the new effect's compact request-dedup result; the reason is bounded operator metadata, not task text.

There is no cancellation operation.

## 9. Protocol and CLI

Retain protocol version 1 for additive commands and require advertised capabilities. Add capabilities such as `slack_reads`, `slack_effects`, and `slack_admission_status`; an older gateway that omits one is unsupported for that command.

Add strict parameter/result DTOs for:

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
file.show                 # metadata only, if implemented in this phase
source.list/inspect       # optional operator-facing names; keep internal otherwise
```

Public CLI commands match SPEC section 11:

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
```

Mutation human/JSON output includes request ID, effect ID, effect state, target, and action-required ambiguity without decorative Slack text. Read output may include message text by explicit user request but logs and diagnostics may not.

Expand task DTOs for Slack source metadata without changing local-task semantics. Prefix lookup remains unambiguous and typed for `tsk_`, `eff_`, and `fil_` identifiers.

## 10. Status, doctor, and logging

### 10.1 Status

Replace Phase 0's Slack/effect `not_implemented` fields with typed fields for:

- Slack state: `disabled` only when running a deliberately test-only injected runtime; production Phase 1 is `connecting|connected|reconnecting|failed_auth`;
- bounded reconnect attempt/backoff and last safe diagnostic code;
- last envelope received and last source result committed;
- configured team/channel/bot IDs (never tokens);
- trusted-user count;
- source-event admission counts;
- effect counts by state; and
- action-required ambiguous/failed effect counts separate from process health.

A healthy gateway with an ambiguous effect remains process-healthy and operator-action-required.

### 10.2 Doctor

Keep doctor non-destructive. Add checks for:

- app/bot token authentication;
- configured team and bot identity;
- configured channel existence, bot access/membership;
- exact required app subscriptions/scopes where Slack exposes enough information, otherwise provide a documented manual check;
- every currently trusted immutable user ID exists in the workspace;
- Socket Mode compatibility and slack-go adapter version; and
- effect/source schema integrity.

Doctor never acknowledges an event, sends/edits/deletes/reacts, downloads a file, or starts the long-lived Socket Mode runtime. Network-unavailable checks are `warn`/`skipped` as appropriate; conclusive auth/access mismatch is `fail`.

### 10.3 Logs

Structured logs may include envelope/event/task/effect IDs, event kind, result enums, durations, and Slack request IDs. They must exclude:

- tokens and authorization headers;
- message/task/effect text;
- raw envelopes and Web API bodies;
- filenames and permalinks unless explicitly bounded and proven necessary; and
- arbitrary Slack error strings.

## 11. Failure classification

Create one adapter-level classifier covered by table tests. Do not scatter SDK string matching through domain code.

| Point | Classification |
| --- | --- |
| Socket envelope DB commit fails | no ack; report degraded storage |
| Known committed source duplicate | ack after lookup |
| Transient Socket disconnect | reconnect with bounded jitter |
| Read-only Web API timeout | unavailable/retriable; no durable effect |
| Effect conclusively rejected | failed |
| Effect call cannot have started | prepared may remain eligible only when provable |
| Effect call may have started | ambiguous |
| Process starts with submitted effect | ambiguous before execution |
| Config candidate auth/access failure | reload fails; old runtime remains active |
| Database full/corrupt | stop admitting/acking and fail closed |

When uncertain, choose the state that preserves evidence and avoids duplicate external action.

## 12. Test plan

### 12.1 Adapter tests

Use `httptest.Server` where testing slack-go translation is necessary and the scripted fake for domain tests. Cover:

- SDK event-to-Mosscap normalization for app mentions, ordinary messages, replies, changes, deletions, bot/system messages, malformed events, and files;
- one-shot acknowledgement behavior;
- auth/team/bot/channel verification and redaction;
- error classification for conclusive rejection, pre-submit failure, post-submit timeout, disconnect, malformed response, and cancellation; and
- reconnect backoff with injected clock and deterministic jitter.

### 12.2 Store tests

Use real temporary-file SQLite databases. Cover:

- exact schema-v2 to v3 migration preserving tasks, trust, and mutation outcomes;
- unknown/newer/gapped/checksum-altered schema rejection;
- one source message to one task and source-event dedup under concurrency;
- ignored/malformed event records without task creation;
- atomic admitted task/source/files/event commit;
- stale edit prevention and exactly-one version increment;
- deletion preserving text/state;
- no old text/raw envelope/token in any SQLite table;
- effect prepare plus request result atomicity;
- prepared/submitted/outcome legal transitions;
- startup submitted-to-ambiguous reconciliation;
- retry predecessor graph and duplicate-risk guard; and
- busy/commit-failure injection proving no ack/success before commit.

### 12.3 Gateway and black-box tests

Run the real server/client against fake Slack:

- startup conclusive auth mismatch fails safely without exposing credentials;
- transient Socket disconnect reconnects while local task/trust CLI remains available;
- direct trusted mention creates one task and commit precedes ack;
- untrusted/wrong workspace/wrong channel/unmentioned/bot events create no task and remain inspectable;
- thread reply requires its own mention;
- redelivery is acknowledged without duplicate task;
- transaction failure is not acknowledged;
- edit-first-adds-mention, stale edit, meaningful edit, no-op edit, and deletion behavior;
- incoming file metadata is durable and no bytes are fetched;
- every Slack command uses exactly the configured channel;
- history/thread reads are bounded and not persisted;
- pre-submit crash leaves prepared eligible;
- post-submit timeout creates ambiguous and restart makes zero duplicate calls;
- explicit retry creates one linked effect and enforces confirmation;
- reload candidate failure leaves old connection/config active;
- successful reload swaps runtime only after candidate readiness; and
- token/message canaries never occur in logs, status, doctor, source-event metadata, diagnostics, or mutation records.

Run all existing Phase 0 tests unchanged except for intentional status/schema expectation updates.

### 12.4 Manual Slack smoke test

Document and perform against a disposable development channel:

1. install the exact manifest/scopes and configure real IDs/tokens;
2. run setup, gateway, doctor, and status;
3. add one trusted user live;
4. send one direct mention, one unmentioned message, one thread reply with mention, and one message with file metadata;
5. edit and delete the admitted source;
6. verify tasks/source metadata and no automatic task-state changes;
7. run history/thread/send/edit/react/unreact/delete commands on test messages;
8. force one disconnect/reconnect and verify source deduplication; and
9. rotate a token or channel through `config set` plus `gateway reload` and verify failure-atomic behavior.

Record timestamps/IDs only after redaction; never commit tokens or raw message bodies.

## 13. Implementation sequence

Each step must build and pass its focused tests before the next begins. Prefer these reviewable commits:

1. **`build: add Slack interfaces and deterministic fake`**
   - pin slack-go;
   - add Mosscap-owned DTOs/interfaces, classifier, fake clock/jitter hooks, and adapter tests;
   - no gateway network behavior yet.

2. **`feat: add durable Slack source and effect schema`**
   - migration 0003, compatibility checks, DTOs, IDs, constraints, repositories, and store tests;
   - no SDK types below adapter boundary.

3. **`feat: verify Slack configuration and manage Socket Mode`**
   - real adapter, startup checks, runtime manager, reconnect state, real reload preparation/swap, status/doctor foundations;
   - remove `slack_network_verification_deferred` only when genuine.

4. **`feat: admit Slack mentions as durable tasks`**
   - normalization, transaction orchestration, commit-before-ack, trust query, dedup, replies, ignored/malformed results;
   - deterministic concurrency/failure tests.

5. **`feat: reconcile Slack source edits and deletion`**
   - stale ordering, current-text/file replacement, source-deleted status, version rules, task/source DTO expansion.

6. **`feat: add bounded Slack reads`**
   - history/thread protocol, CLI, channel enforcement, stable DTOs, non-persistence tests.

7. **`feat: execute durable Slack effects`**
   - prepare/submit/outcome engine, startup reconciliation, send/edit/delete/react/unreact CLI and protocol, target validation;
   - crash-boundary and ambiguity tests.

8. **`feat: inspect and explicitly retry Slack effects`**
   - list/inspect/retry, predecessor graph, duplicate-risk confirmation, operator-action status.

9. **`test: prove the Phase 1 Slack reliability gate`**
   - built-binary fake-Slack E2E, full fault matrix, race/vet/build/vulnerability checks, operator docs, real manual smoke checklist.

Do not combine admission and outbound effects into one implementation change; their acknowledgement and ambiguity boundaries require separate review.

## 14. Completion checklist

Phase 1 is complete only when:

- all configured Slack identity/credential/access checks are real and secret-safe;
- Socket reconnect behavior is bounded and makes no offline receipt claim;
- every eligible direct mention commits one task before acknowledgement;
- every ignored, malformed, stale, duplicate, edited, and deleted source follows the specified durable result without historical text;
- trust changes take effect from SQLite without config reload;
- every Slack write commits one exact effect before I/O;
- no submitted uncertain effect is automatically retried;
- explicit retry preserves its predecessor and duplicate-risk confirmation;
- all commands are single-channel and task-state independent;
- incoming files remain metadata-only and no outgoing bytes are accepted yet;
- status separates process health from action-required effects;
- doctor and logs remain non-destructive and token/message safe;
- all existing Phase 0 gates still pass after schema migration; and
- automated fake-Slack tests plus the documented manual smoke test satisfy the Phase 1 release gate.

Required final validation:

```text
gofmt -w ...
go vet ./...
go test ./...
go test -race ./...
go build ./cmd/mosscap
git diff --check
govulncheck ./...
```
