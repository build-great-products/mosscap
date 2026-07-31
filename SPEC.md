# Mosscap v1 Specification

**Status:** implementation-guiding v1 specification
**Product rule:** keep coordination durable; keep the agent human-owned.

Mosscap is a minimal, functional bridge from one trusted Slack conversation, local CLI requests, and schedules to one human-started interactive Pi session. A foreground gateway owns durable coordination in SQLite. A Pi extension claims work and inserts it as visible messages into the current persisted Pi session. Zellij makes that session easy for a human to observe, but is never part of correctness.

This document is normative for v1. [README.md](README.md) explains the product and [CORE_ETHOS.md](CORE_ETHOS.md) defines its constraints. Where those documents leave implementation choices open, the conservative defaults below apply.

## 1. Scope and conservative defaults

### 1.1 Required in v1

- Slack Socket Mode ingestion for one configured channel and explicitly trusted user IDs.
- Admission of direct bot mentions from trusted users in that channel, including mentions in an existing thread.
- SQLite-first event acknowledgement, source-event deduplication, revisioned tasks, schedules, delivery leases, consumer fencing, and an outbound-effect ledger.
- One active Pi leader, explicitly claimed through `/mosscap`; a takeover fences and disconnects the previous leader.
- Visible delivery into the human-owned Pi session, including recovery summaries and meaningful revision diffs with timestamps.
- Explicit task lifecycle, explicit outbound Slack actions, and inspectable ambiguous delivery/effect outcomes with no automatic retry.
- File transfer in both directions for a broad safe set of files, subject to explicit limits and validation.
- Bounded local retention and coalescing of missed recurring schedule runs.
- A foreground gateway, local CLI, Pi extension, and optional Zellij convenience commands.

### 1.2 Not required in v1

- Starting, supervising, selecting, resetting, or stopping Pi.
- Headless agents, subagents, implicit routing, multi-consumer work sharing, or autonomous model selection.
- A daemon installer, mandatory supervisor, browser UI, or Zellij integration for correctness.
- Backup, restore, database migration, downgrade, or cross-version compatibility guarantees. A v1 binary may refuse an unknown schema. Operators may copy files while the gateway is stopped, but that is not a supported restore contract.
- End-to-end exactly-once execution.
- Automatic retry of an ambiguous Slack effect or an ambiguous Pi delivery.
- A required tombstone/delete step after resolution. Deletion is optional administrative cleanup.

### 1.3 Conservative defaults resolving ambiguity

1. **Conversation:** exactly one Slack channel ID is configured. A thread in that channel remains part of the same trusted conversation.
2. **Admission:** a new task is admitted only from a non-bot user whose immutable Slack user ID is trusted and whose message text contains the configured bot user mention. Channel membership, display names, `@channel`, and messages merely replying to the bot are insufficient.
3. **Edits:** an edit that adds a direct bot mention from a trusted user in the configured channel admits the message as a new task. An edit to an already admitted Slack message creates a new revision of the same task. Deleted source messages are recorded as revisions but do not silently resolve or delete tasks.
4. **Meaningful edit:** normalized text changed, attachment/file identity changed, or the source was deleted. Changes only to Slack transport metadata are ignored. Whitespace normalization is limited to line endings and trailing spaces; content is otherwise preserved.
5. **Leadership:** `/mosscap` requests takeover by default after showing the current holder. Successful takeover increments a fencing generation and actively closes the previous extension connection. No concurrent leader grace period exists.
6. **Resolution:** `respond` records and attempts a response; a task becomes `responded` only after its Slack effect is confirmed (or immediately for a local task with `--no-slack`). Resolution is a separate explicit action unless `task respond --resolve` is used. A tombstone is never required.
7. **Retries:** transient connection establishment may reconnect with bounded backoff. Committed task delivery and submitted Slack effects are not automatically retried after an ambiguous outcome. An operator must inspect and issue an explicit retry/requeue command.
8. **Files:** regular files are accepted regardless of extension or MIME type except known dangerous local filesystem types; limits, hashes, and explicit destinations provide the boundary. Content is not interpreted as trusted.
9. **Retention:** keep open task history and unresolved effects. Prune old closed-task revisions, events, delivery attempts, cached files, and confirmed effects by age/size as specified below. Retention is housekeeping, not backup.

## 2. Architecture and authority

```text
Slack Socket Mode ─┐
CLI ───────────────┼──> foreground gateway ───> SQLite ledger
schedules ─────────┘             │                    │
                                 │ private protocol   │ source of truth
                                 ▼                    │
                           Pi extension               │
                                 │                    │
                                 ▼                    │
                       human-owned Pi session <───────┘
                           (usually in Zellij)
```

### 2.1 Gateway

The gateway is the only writer to the SQLite ledger and owns:

- Slack authentication, admission, event acknowledgement, and deduplication;
- task and revision transitions;
- schedule calculation and materialization;
- consumer generation, leases, and delivery reconciliation;
- outbound Slack effect creation and execution;
- retention; and
- a private, versioned local protocol for the CLI and Pi extension.

It runs in the foreground and exits non-zero on unsafe configuration, inaccessible state, unsupported schema, or inability to bind the local endpoint.

### 2.2 Pi extension

The extension owns only integration with the current Pi process/session:

- explicit opt-in and opt-out;
- leadership claim and heartbeat;
- rendering incoming work as distinct visible Pi messages;
- persisting delivery keys in Pi session data when the Pi extension API permits;
- reporting persisted, rejected, or uncertain insertion outcomes; and
- compact connection/binding/backlog status.

It must not start Pi, choose model settings, scrape terminal output, inject keystrokes, infer task state from model text, or execute Slack effects directly.

### 2.3 CLI

The CLI is a client of the gateway protocol. With the exception of `setup` and offline-safe config reads/writes when the gateway is stopped, it must not write SQLite directly. Every mutating command prints the resulting durable identifier and state. Human-readable output is default; `--json` emits stable machine-readable objects.

### 2.4 Zellij

Zellij commands are convenience wrappers around installed `zellij` commands. Zellij has no lease, delivery, task, or completion authority. Failure or absence of Zellij cannot affect gateway correctness.

## 3. Local layout and configuration

Defaults follow XDG locations:

```text
$XDG_CONFIG_HOME/mosscap/config.toml
$XDG_STATE_HOME/mosscap/mosscap.sqlite3
$XDG_STATE_HOME/mosscap/control.sock
$XDG_STATE_HOME/mosscap/files/
```

If XDG variables are unset, use `~/.config` and `~/.local/state`. Config and state directories must be owned by the effective user and mode `0700`; config, database, and credential-bearing files must be `0600`; the Unix socket must be under the state directory and mode `0600`. Symlinked roots, group/world-writable ancestors below the user's home, non-owner files, and socket paths in shared temporary directories are rejected.

Required configuration:

- Slack app token and bot token, preferably supplied by environment or a `0600` secrets file;
- workspace/team ID, bot user ID, one channel ID;
- at least one trusted immutable Slack user ID; and
- state and endpoint paths.

Optional configuration and v1 defaults:

| Setting | Default |
| --- | --- |
| delivery lease | 60 seconds |
| leader heartbeat / expiry | 10 / 30 seconds |
| Slack reconnect backoff | exponential, 1–60 seconds with jitter |
| max incoming or outgoing file | 100 MiB per file |
| max files per task/effect | 20 |
| file cache budget | 2 GiB |
| closed-history retention | 30 days |
| transport/event retention | 14 days after all references close |
| confirmed-effect retention | 30 days |
| schedule lookback | unlimited, but coalesced per schedule |
| SQLite busy timeout | 5 seconds |

Secrets are redacted by `config show` and all logs. `setup` initializes the current schema only in an empty/new state root; it does not import another installation.

## 4. Durable data model

Identifiers are opaque, URL-safe strings with type prefixes where shown. Times are UTC RFC 3339 with microsecond precision in output and integer microseconds in storage. Every mutable row has `created_at` and `updated_at`. Foreign keys and WAL mode are enabled.

### 4.1 Core records

#### `source_events`

- `event_id` (Slack event ID or derived stable envelope identity, unique)
- `team_id`, `channel_id`, `message_ts`, `thread_ts`, `user_id`
- `event_kind` (`message`, `message_changed`, `message_deleted`, `file_shared`)
- `received_at`, `slack_event_at`, `payload_json`
- `admission` (`admitted`, `duplicate`, `ignored_untrusted`, `ignored_channel`, `ignored_unmentioned`, `ignored_bot`, `malformed`)
- optional `task_id`, `revision_id`

The original payload is retained only within transport retention. Durable task revisions retain the normalized source needed after payload pruning.

#### `tasks`

- `task_id` (`tsk_…`)
- `origin` (`slack`, `local`, `schedule`)
- `state` (`open`, `working`, `responded`, `resolved`)
- title and current revision pointer
- optional source channel/message/thread identity or schedule ID
- `opened_at`, optional `working_at`, `responded_at`, `resolved_at`
- optional assignee/working note
- `deleted_at` only for optional tombstoning

Deleted tasks are hidden by default but retained until normal closed-history pruning. A deleted task cannot be delivered or mutated except by an explicit administrative undelete if implemented; undelete is not required in v1.

#### `task_revisions`

- `revision_id` (`rev_…`), `task_id`, monotonic `revision_number`
- `kind` (`created`, `source_edit`, `source_deleted`, `local_update`, `progress`, `response`, `reopened`, `resolved`)
- normalized title/instructions/body snapshot
- structured attachment/file references
- actor (`slack:<user_id>`, `local:<uid>`, `schedule:<id>`, `system`)
- source timestamp and committed timestamp
- optional `previous_revision_id`, textual unified diff, and change summary

Revisions are immutable. Current task content is derived from the latest applicable revision and cached on `tasks` only transactionally.

#### `task_events`

An append-only transition/audit stream: event ID, task ID, prior/new state, actor, reason, command request ID, and committed timestamp. It provides lifecycle history independent of revision content.

#### `consumers`

- stable Pi session identity and connection identity
- current generation, state (`connected`, `disconnected`, `fenced`)
- claimed/heartbeat/disconnected timestamps
- Pi session display metadata, never credentials or transcript

A singleton `leader` row stores current consumer ID, generation, and lease expiry.

#### `deliveries`

- `delivery_id` (`dlv_…`), task ID, revision ID, consumer ID, generation
- stable `delivery_key = hash(task_id, revision_id, Pi session identity)`
- state and timestamps
- lease token/expiry, attempt count
- optional diagnostic and operator decision

The unique delivery key prevents duplicate insertion into the same persisted Pi session. A new explicitly opted-in Pi session can receive current open work again under new keys.

#### `effects`

- `effect_id` (`eff_…`), idempotency key (unique), optional task ID
- kind (`send`, `edit`, `delete`, `react`, `unreact`, `file_upload`, `agents_progress`)
- exact target and normalized request payload
- state, attempt count, submitted/confirmed timestamps
- Slack response identifiers and diagnostic
- optional `supersedes_effect_id`

#### `files`

- `file_id` (`fil_…`), direction (`from_slack`, `to_slack`, `local_task`)
- source metadata, original name, declared/detected MIME, size, SHA-256
- cache path, state (`metadata`, `available`, `missing`, `rejected`, `expired`)
- Slack identity/permalink where applicable and timestamps

#### `schedules` and `schedule_runs`

A schedule stores title, instructions, enabled state, one-off timestamp or five-field cron plus IANA timezone, next due time, and last evaluated time. A run stores scheduled interval boundaries, materialized task, coalesced count, and committed time. A uniqueness constraint on schedule and due interval prevents duplicate materialization.

### 4.2 Transactions and request deduplication

Each local protocol mutation carries a client-generated request ID. The gateway stores request/result pairs so a client reconnect can retrieve the original result without duplicating a mutation. The following are single SQLite transactions:

- event deduplication + admission + task/revision creation before Slack acknowledgement;
- any task state transition + audit event;
- schedule run claim + task materialization + next-run update;
- effect intent creation before network submission;
- leadership generation increment + old leader fencing;
- delivery lease transition and final acknowledgement.

SQLite is authoritative. Logs, Slack reactions, Pi JSONL, and terminal output are projections only.

## 5. Task lifecycle and revisions

### 5.1 State machine

```text
create/admit ──> open ──working──> working ──respond──> responded ──resolve──> resolved
                   │                    │                    │
                   ├────respond─────────┘                    │
                   └────────────────resolve──────────────────┘

resolved ──reopen──> open
responded ──reopen──> open
```

Rules:

- Creation always yields `open`.
- `working` is idempotent when already working; a changed note still creates a revision/event.
- `respond` creates an effect for Slack-origin work. State remains unchanged while the effect is `prepared` or `submitted`; confirmed response moves it to `responded`. Ambiguous/failed response does not.
- A local or scheduled task without a Slack target requires either `--no-slack` to record a response or an explicit target; `--no-slack` moves it to `responded` transactionally.
- `resolve` is explicit and valid from `open`, `working`, or `responded`; it records a reason. It does not require a response or delete/tombstone.
- `reopen` is explicit from `responded` or `resolved`, creates a revision, and returns to `open`.
- Source edits update content but never move lifecycle state. If an edit arrives after response/resolution, it remains visible as a new revision and marks the task as having an unread revision; it does not automatically reopen.
- Optional `delete` is valid only with `--force` for non-resolved work. It is administrative tombstoning, not normal completion.

### 5.2 Meaningful revisions and visible diffs

For every meaningful source edit, the gateway stores the full normalized revision plus a bounded unified diff against the previous revision. The Pi extension renders a separate visible message containing:

- task ID and revision number;
- Slack author and source timestamp;
- gateway receipt/commit timestamp;
- prior revision number;
- concise change summary;
- unified diff for textual changes; and
- added/removed/changed file metadata.

Diffs are capped at 200 KiB and 2,000 lines in a delivery. If capped, the visible message says so and gives `mosscap task show TASK --revision REV` for full content. Timestamp labels must distinguish Slack event time from gateway commit time. A delete revision visibly says the source was deleted and preserves prior durable content for inspection.

Edits received before initial Pi delivery are not collapsed invisibly: normal delivery may show only the latest full content, but it must include a revision timeline and meaningful diffs since admission. Once a revision has been delivered, each later meaningful revision receives its own delivery.

## 6. Slack admission and acknowledgement

For each Socket Mode envelope, the gateway:

1. validates envelope shape and workspace identity;
2. begins a transaction and deduplicates the event identity;
3. checks configured channel, non-bot sender, explicit trusted user ID, and direct bot mention for initial admission;
4. normalizes message/thread/file metadata and creates or updates the task revision;
5. commits; then
6. acknowledges the Slack envelope.

Known duplicates are also acknowledged after the existing committed result is found. Malformed events are recorded when safely identifiable and acknowledged to prevent a poison loop. A database commit failure is not acknowledged; Slack may redeliver.

Thread identity is `thread_ts` when present, otherwise the admitted message's `message_ts`. An admitted initial message maps to exactly one task. Edits map by channel plus message timestamp. Replies from trusted users are not separate tasks unless they directly mention the bot; when they directly mention it, v1 conservatively creates a separate task linked to the same thread.

“Durable receipt” begins only after the gateway receives and commits the event. Socket Mode cannot guarantee receipt while the gateway is offline beyond Slack's own behavior.

## 7. Pi leadership and delivery

### 7.1 Claim and fencing

`/mosscap` connects to the private endpoint, authenticates as the same OS user, presents protocol/Pi session metadata and persisted delivery keys, then requests leadership. The gateway atomically increments the singleton generation and assigns it to that consumer. If a leader exists:

- the extension shows its non-sensitive identity and asks for confirmation unless `/mosscap --takeover` was explicit;
- on confirmation, the gateway marks the old consumer `fenced`, invalidates all its leases, sends a disconnect/fenced frame, and closes its connection;
- every subsequent delivery acknowledgement and mutation from an extension must carry the current generation; stale generations are rejected.

`/mosscap off` releases leadership if current and disconnects without closing tasks. A heartbeat lapse expires the leader, but a new extension must still opt in explicitly.

### 7.2 Delivery state machine

```text
pending -> leased -> inserting -> persisted
                    |      |          |
                    |      +-> uncertain
                    +--------> rejected
leased --expiry before insertion report--> pending
uncertain --operator requeue--> pending
rejected  --operator requeue--> pending
```

- `pending`: durable and eligible for current leader.
- `leased`: gateway issued a token for a specific generation; no insertion started report yet.
- `inserting`: extension reported it began Pi insertion.
- `persisted`: extension confirmed the visible message and delivery key were persisted in the Pi session.
- `uncertain`: connection/process failed after insertion began but before persistence could be confirmed. It is never automatically redelivered.
- `rejected`: Pi API rejected insertion or the extension refused malformed/oversized content. It is inspectable and not automatically retried.

A lease may automatically return to pending only if it expires while still `leased`, because the extension has not reported insertion started. Expiry from `inserting` becomes `uncertain`. An operator uses `delivery inspect` and `delivery requeue --reason ...` after checking the Pi session. Requeue creates a new attempt while retaining history.

### 7.3 Visible message contract

Each injected `mosscap.slack`, `mosscap.local`, or `mosscap.schedule` message contains, in human-readable and structured metadata:

- stable task/revision/delivery IDs and delivery key;
- origin, author/schedule, Slack thread link when available;
- source and committed timestamps;
- lifecycle state and current working note;
- full current instructions within message limits, with an explicit CLI retrieval command if truncated;
- revision timeline/diff as required above; and
- file names, sizes, hashes, and safe retrieval commands.

The message must be visible in ordinary Pi conversation history. Hidden context-only injection does not satisfy delivery.

### 7.4 Recovery handshake and summary

On every opt-in/reconnect, the extension sends its persisted delivery keys and last observed gateway generation. The gateway reconciles them, then inserts one visible recovery summary before normal backlog delivery. The summary includes:

- disconnect/reconnect time window when known;
- counts by task state and delivery state;
- current leader identity/generation;
- open tasks ordered by oldest unresolved activity;
- for each open task, latest revision number, state, last source and commit timestamps, and whether it changed since this Pi session last persisted it;
- all `uncertain`/`rejected` deliveries and `ambiguous`/`failed` effects requiring inspection; and
- coalesced schedule-run counts.

After the summary, the gateway delivers the latest revision of every open task not persisted in this Pi session. It also delivers a compact sequence of meaningful diffs from the session's last persisted revision to latest. Closed tasks are included only when they have an unresolved delivery/effect or changed after the session's last known revision.

## 8. Outbound Slack effects

### 8.1 Effect state machine

```text
prepared -> submitted -> confirmed
               |  |
               |  +-> ambiguous
               +----> failed
prepared -> cancelled
ambiguous/failed --explicit retry--> new prepared effect superseding prior
```

- `prepared`: intent and idempotency key committed before I/O.
- `submitted`: request transmission began.
- `confirmed`: Slack returned a conclusive success and identifiers are recorded.
- `failed`: conclusive rejection, such as invalid target or permission denial.
- `ambiguous`: timeout/disconnect/invalid response after submission may have reached Slack.
- `cancelled`: intent was cancelled before submission.

There is no automatic retry after `submitted`. Reconnect logic may resume polling/receiving but cannot repeat a consequential API call. `effect inspect` prints the target, payload hash, timestamps, Slack identifiers, and a concrete inspection command. `effect retry --reason ...` always creates a new linked effect and requires `--confirm-duplicate-risk` for an ambiguous predecessor.

Where Slack supports a client idempotency token for an operation, Mosscap supplies the effect key, but still records ambiguous outcomes honestly.

### 8.2 Agents API progress and fallback

If configured and available, Mosscap may project task progress through Slack's Agents API as `agents_progress` effects. This is optional and never authoritative. Capability is detected at startup/doctor and failures do not block task transitions or ordinary replies.

Fallback behavior is deterministic:

1. If Agents API progress is available, send/update the progress projection as an explicit recorded effect.
2. Otherwise, use a configured status reaction for coarse states when supported.
3. Otherwise, record progress only in the task ledger; `task update` output states that no Slack projection was made.

Mosscap must not post repetitive fallback thread messages automatically. A user can explicitly send one. Progress effects use the same no-automatic-retry and ambiguity rules as all Slack effects.

## 9. File passing

### 9.1 Incoming Slack files

For admitted tasks, the gateway records file metadata in the admission transaction. It downloads bytes using authenticated Slack APIs only after commit, to a random temporary name under the private file root, while enforcing the configured byte limit. It computes SHA-256 while streaming, `fsync`s, and atomically renames to a content-addressed path. Redirects are restricted to HTTPS and an allowlist appropriate to Slack file hosts; credentials are never forwarded elsewhere.

A file too large, unavailable, or disallowed remains a visible metadata reference with state and reason; it does not prevent text task admission. Download failures that are conclusively transient may be retried only before any bytes are exposed as available; an operator can explicitly retry later.

### 9.2 Local and outgoing files

`task create/update --file PATH` and `slack file --file PATH` accept regular files only. The CLI/gateway reject directories, devices, FIFOs, sockets, symlinks, paths outside operator-readable access, sparse files whose logical size exceeds the limit, and files that change identity/size while being copied. Files are copied into private storage and hashed before a task/effect references them; the original path is informational only.

A wide set of file types is supported, including text, source, images, audio, video, archives, PDFs, office documents, and opaque binary data. Extension and MIME are metadata, not a trust decision. Executable bits are stripped in cache, extracted archives are never automatic, and downloaded content is never executed or rendered as trusted HTML by Mosscap.

`file get FILE_ID --output PATH` uses exclusive creation by default; `--force` is required to overwrite a regular file. Stdout streaming requires explicit `--stdout` and never mixes with status output. Outgoing upload confirmation records Slack's file ID and permalink.

## 10. Schedules

A one-off schedule uses an RFC 3339 timestamp with explicit offset. A recurring schedule uses a five-field cron expression and an IANA timezone. Cron evaluation follows wall time in that timezone:

- nonexistent DST local times are skipped;
- repeated local times run once, at the first occurrence;
- due instants are stored as UTC.

The gateway evaluates schedules on startup and at least once per minute. For a recurring schedule with one or more missed due instants, it creates exactly one task/run containing earliest missed due, latest missed due, and count. It then advances `next_due_at` beyond now in the same transaction. A one-off missed while down creates one ordinary task on recovery.

Runs are never merged across distinct schedules. A schedule disabled before recovery does not materialize missed runs. `remove` disables future runs and tombstones schedule configuration but never deletes already materialized tasks.

## 11. CLI and Pi commands

All commands support `--help`; read commands support `--json`; identifiers may be abbreviated only when unambiguous. Mutations support `--request-id`. Destructive or duplicate-risk commands require the flags shown.

```text
mosscap setup
mosscap config show
mosscap config set KEY VALUE
mosscap gateway run
mosscap gateway status [--json]
mosscap doctor [--json]

mosscap trust list
mosscap trust add SLACK_USER_ID
mosscap trust remove SLACK_USER_ID

mosscap task create --title TEXT --instructions TEXT [--file PATH ...]
mosscap task list [--state open|working|responded|resolved] [--all]
mosscap task show TASK [--revision REV] [--history]
mosscap task update TASK [--title TEXT] [--instructions TEXT] [--note TEXT] [--file PATH ...]
mosscap task working TASK [--note TEXT]
mosscap task respond TASK (--text TEXT|--text-file PATH) [--file PATH ...] [--channel ID --thread TS] [--no-slack] [--resolve]
mosscap task resolve TASK --reason TEXT
mosscap task reopen TASK --reason TEXT
mosscap task delete TASK --reason TEXT [--force]

mosscap delivery list [--state pending|leased|inserting|persisted|uncertain|rejected]
mosscap delivery inspect DELIVERY
mosscap delivery requeue DELIVERY --reason TEXT

mosscap effect list [--state prepared|submitted|confirmed|ambiguous|failed|cancelled]
mosscap effect inspect EFFECT
mosscap effect retry EFFECT --reason TEXT [--confirm-duplicate-risk]
mosscap effect cancel EFFECT --reason TEXT

mosscap slack history [--limit N]
mosscap slack thread THREAD_TS
mosscap slack send --channel ID [--thread TS] (--text TEXT|--text-file PATH) [--file PATH ...]
mosscap slack edit MESSAGE_TS (--text TEXT|--text-file PATH)
mosscap slack delete MESSAGE_TS --reason TEXT
mosscap slack react MESSAGE_TS EMOJI
mosscap slack unreact MESSAGE_TS EMOJI
mosscap slack file --channel ID [--thread TS] --file PATH [--comment TEXT]

mosscap file show FILE_ID
mosscap file get FILE_ID (--output PATH|--stdout) [--force]
mosscap file retry FILE_ID --reason TEXT

mosscap schedule add --title TEXT --at RFC3339 --instructions TEXT
mosscap schedule add --title TEXT --cron CRON --timezone IANA --instructions TEXT
mosscap schedule list
mosscap schedule show SCHEDULE
mosscap schedule enable SCHEDULE
mosscap schedule disable SCHEDULE
mosscap schedule remove SCHEDULE --reason TEXT

mosscap zellij list
mosscap zellij attach SESSION
mosscap zellij watch SESSION
```

Pi extension commands:

```text
/mosscap                 # inspect holder, confirm if needed, claim/take over, recover
/mosscap --takeover      # explicit takeover without interactive holder confirmation
/mosscap status          # connection, generation, backlog, exceptional states
/mosscap off             # release leadership and disconnect
```

The extension may expose task/effect CLI shortcuts, but v1 correctness and acceptance do not depend on them.

Exit codes are stable categories: `0` success, `2` usage, `3` not found, `4` conflict/stale generation, `5` unsafe configuration/permissions, `6` gateway unavailable, `7` external operation failed, `8` ambiguous outcome, and `9` unsupported schema/protocol.

## 12. Status, diagnostics, and logs

`gateway status` reports endpoint/database identity, schema/protocol version, Slack state, last received/committed event, current leader/generation/heartbeat, task counts, delivery counts, effect counts, schedule next wake, and retention usage. It is unhealthy if the database is unwritable, endpoint is unavailable, scheduler is stalled, Slack reconnect exceeded its bounded status threshold, or unresolved exceptional states exist; ambiguous outcomes are explicitly listed rather than hidden in a generic unhealthy bit.

`doctor` is non-destructive and checks configuration, redaction, ownership/modes, SQLite integrity/read-write transaction, local endpoint, Slack auth/channel/bot identity, trusted IDs, required scopes, optional Agents API capability, Pi extension/protocol compatibility, Zellij availability (advisory), and storage limits. It never sends a Slack message or takes leadership.

Structured logs include request/event/task/effect/delivery IDs and timestamps but exclude tokens, full Slack payloads, message bodies, and file bytes by default. `--verbose` may include metadata, not secrets or content.

## 13. Failure and recovery

| Failure | Required behavior |
| --- | --- |
| Gateway exits | SQLite transactions preserve committed work; clients reconnect after restart. |
| Slack disconnects | Bounded reconnect with jitter; event deduplication handles redelivery; no claim of receipt while offline. |
| Database full/corrupt | Stop admitting/acknowledging new work, fail closed, report exact diagnostic; do not auto-repair. |
| Pi/SSH/Zellij exits before insertion | Unstarted lease returns to pending after expiry. |
| Pi exits during insertion | Delivery becomes uncertain; inspect, then explicitly requeue if appropriate. |
| Extension reconnects | Reconcile persisted keys, present recovery summary/latest revisions, then resume leases. |
| New Pi takes over | Increment fence, disconnect old leader, reject stale acknowledgements. |
| Slack effect times out after submit | Mark ambiguous; never automatic retry. |
| Incoming file fails | Keep task and metadata visible; file has explicit failed/missing state and retry command. |
| Clock moves | Use monotonic time for leases/heartbeats and UTC instants for durable timestamps; scheduler recalculates without duplicate run rows. |
| Process killed during retention | Batched transactions and referential checks leave live records intact; retry housekeeping later. |

Startup performs schema compatibility and SQLite quick checks, expires dead leadership, changes `inserting` deliveries owned by dead connections to `uncertain`, releases only never-started expired leases, evaluates schedules, and resumes Slack. It does not resubmit effects left `submitted`; these become or remain `ambiguous` pending inspection.

## 14. Retention and disk bounds

Retention runs in small batches and never removes:

- non-resolved tasks or their revisions/files;
- the latest revision and lifecycle audit needed to understand a retained task;
- unresolved (`prepared`, `submitted`, `ambiguous`, `failed`) effects;
- `uncertain` or `rejected` delivery history;
- active schedules and run uniqueness data needed to prevent duplication; or
- files referenced by any retained live record.

After the configured ages, Mosscap may prune raw Slack payloads, superseded revisions of resolved tasks, confirmed/cancelled effects, persisted delivery attempts, disabled schedule details, and unreferenced cached files. Before deleting file bytes it marks them expired transactionally; missing bytes remain honestly represented.

The file cache uses least-recently-used eviction among eligible unreferenced/closed items to remain under 2 GiB. If live referenced content alone exceeds the budget, Mosscap does not delete it: it rejects further file caching with a visible `storage_limit` result while continuing text admission. SQLite has no hard byte cap because live durable work cannot be safely discarded; age pruning and status warnings at configurable thresholds provide a simple bounded-growth policy. The default warns at 512 MiB and blocks optional raw payload/file metadata expansion at 1 GiB while preserving minimal task admission.

No compaction, copy, export, import, backup, restore, or migration workflow is guaranteed in v1.

## 15. Security boundaries

- A trusted Slack user is authorized to send instructions and files to a fully capable local Pi session. Trust must therefore use immutable user IDs and be explicit.
- The configured channel, user allowlist, workspace, and direct mention are admission checks, not a sandbox.
- Slack message content, filenames, MIME labels, file bytes, schedule text, and task text are untrusted data. They are clearly delimited in visible messages and never interpreted as Mosscap protocol commands.
- The Unix socket authenticates same-user local clients through filesystem permissions and peer credentials where supported. Same-user processes are trusted; the socket is not protection from a compromised account.
- Pi and its tools retain whatever local authority the human gave them. Mosscap adds no OS sandbox and must not imply one.
- Zellij watch/attach access is outside Mosscap's security boundary.
- Tokens are least-scope, redacted, never included in Pi messages, and not persisted in SQLite payloads.
- Outbound targets are shown before/with effects. Slack-origin text cannot silently invoke CLI verbs; an agent or human must explicitly execute them.
- File cache paths are generated, not derived directly from filenames. Reads/writes defend against traversal, symlink races, and replacement races.

## 16. Local integration protocol

Use a versioned framed protocol over the private Unix socket. JSON messages with a 32-bit length prefix are sufficient for v1; files transfer through gateway-managed IDs rather than inline frames. Every request includes protocol version, request ID, client kind/version, and claimed session identity. Every response includes request ID, server version, durable result IDs, and a typed error.

Required protocol capabilities:

- health/status and capability negotiation;
- CLI request/result deduplication;
- leadership claim/release, generation fencing, heartbeat, and server-initiated disconnect;
- backlog/recovery summary query;
- lease, insertion-started, persisted, rejected, and uncertain delivery messages;
- task/effect/file/schedule operations; and
- streaming notifications for newly eligible revisions and state changes.

Protocol mismatch fails explicitly with supported versions. Unknown fields are ignored only within a declared compatible protocol version; unknown state enum values fail safe. The gateway applies backpressure with bounded per-connection queues and disconnects a slow client without losing durable work.

Pi integration must use supported extension APIs for visible custom/user messages and persisted extension state. If the installed Pi version cannot confirm visible persistence, `doctor` marks it incompatible rather than degrading to key injection or claiming delivery.

## 17. Invariants

1. An eligible Slack event is acknowledged only after its admission/deduplication result is committed.
2. One admitted source message maps to at most one task; revisions are unique and monotonic per task.
3. SQLite is the sole task/effect authority; Pi, Slack projections, logs, and Zellij are not.
4. At most one unexpired leader generation is valid. Stale generations cannot acknowledge or mutate delivery state.
5. A delivery is `persisted` only after the extension confirms both visible insertion and persisted delivery key.
6. An insertion that might have occurred but cannot be confirmed is `uncertain`, never silently pending.
7. An outbound effect intent is durable before submission; a possibly submitted unconfirmed effect is `ambiguous`, never automatically retried.
8. Resolving a task is explicit and never requires deletion/tombstoning.
9. Meaningful source edits remain revisioned and visibly deliverable with source and commit timestamps.
10. Schedule recovery creates at most one coalesced run per schedule evaluation gap.
11. Retention never deletes state required to understand open work or inspect unresolved ambiguity.
12. Files are content-hashed, size-bounded regular files in private storage; user-provided names never choose cache paths.
13. No component other than the human starts or owns Pi.

## 18. Phased implementation

### Phase 0: skeleton and storage

- CLI executable, config validation, private XDG layout, SQLite schema, request IDs, typed errors, logging, `setup`, `status`, and `doctor` foundations.
- Task/revision/event CRUD and lifecycle commands.
- Tests: unsafe permissions, transaction rollback, request deduplication, lifecycle and schema constraints.

**Gate:** local tasks survive gateway restart and all invariants that do not require Slack/Pi pass.

### Phase 1: Slack admission and effects

- Socket Mode connection, strict admission, commit-before-ack, deduplication, edits/deletes, source thread identity.
- Explicit Slack effects and ambiguous-state handling; history/thread reads.
- Tests with a deterministic fake Slack server for redelivery, commit failure, timeout-before/after submit, and edit ordering.

**Gate:** an eligible mention creates exactly one durable task; no ambiguous effect is automatically retried.

### Phase 2: Pi extension, leadership, and delivery

- Versioned local protocol, `/mosscap`, takeover fencing/disconnect, heartbeats, leases, visible messages, persisted keys, exceptional delivery states.
- Recovery summary, latest revision delivery, and visible timestamped diffs.
- Tests with two fake extensions and a Pi extension integration harness.

**Gate:** two consumers cannot act concurrently; kill/reconnect scenarios produce pending versus uncertain exactly as specified.

### Phase 3: schedules and files

- One-off/cron scheduling with timezone/DST rules and coalesced recovery.
- Incoming download, local ingest, outgoing upload, hashes, safe retrieval, cache budget and retention.
- Tests for downtime gaps, DST boundaries, oversized/changing/symlink files, interrupted download, and cache pressure.

**Gate:** missed runs coalesce without loss of context; broad regular-file passing works without traversal or unbounded cache growth.

### Phase 4: operational polish

- Complete status/doctor, optional Agents API progress with fallback, Zellij wrappers, retention batches, compatibility diagnostics, packaging and operator documentation.
- End-to-end fault matrix and manual usability pass in a real interactive Pi/Zellij session.

**Gate:** all acceptance criteria below pass and no omitted v1 feature has become an implicit dependency.

## 19. Acceptance criteria

### Admission and tasks

1. A trusted user's direct bot mention in the configured channel is committed before Socket Mode acknowledgement and appears once after duplicate delivery.
2. Wrong-channel, untrusted, bot-authored, and unmentioned messages do not create tasks and have inspectable admission reasons.
3. A meaningful Slack edit creates a monotonic immutable revision; Pi shows author, source/commit timestamps, and a diff. Metadata-only edits do not.
4. `open -> working -> responded -> resolved`, direct resolve, reopen, and optional delete obey the state machine. A resolved task requires no tombstone.

### Human ownership and leadership

5. Mosscap never starts Pi. Running `/mosscap` in a human-started Pi session is required to receive work.
6. A second explicit takeover increments generation, closes/fences the first connection, and rejects a delayed acknowledgement from it.
7. Zellij can be absent without affecting task delivery correctness; attach/watch never mutate state.

### Delivery and recovery

8. Work appears as a distinct visible Pi message with stable IDs and persists a delivery key.
9. A crash before insertion-start returns the lease to pending; a crash after insertion-start produces `uncertain` and no automatic retry.
10. Reconnect/new opt-in presents a visible recovery summary and latest open revisions/diffs before ordinary new backlog.
11. Explicit inspection and requeue retain prior delivery attempt history.

### Slack effects and progress

12. Every send/edit/delete/react/unreact/file/progress request has a durable intent before network submission and a conclusive or ambiguous outcome afterward.
13. A simulated post-submit timeout creates `ambiguous`; restart performs no duplicate API call. Explicit retry records a linked new effect and duplicate-risk confirmation.
14. Agents API progress is used only when supported; otherwise reaction or ledger-only fallback works without blocking task progress.

### Schedules, files, and retention

15. One-off schedules survive restart. Multiple missed recurring instants create one task with correct count/range, including tested DST behavior.
16. Text, image, PDF, archive, and opaque binary files pass within limits with correct SHA-256; oversized, symlinked, changing, or unavailable files fail visibly without losing the task.
17. Cache pressure evicts only eligible data; live/open task files and unresolved inspection records remain. Raw/history pruning leaves retained tasks understandable.

### Operations and security

18. Unsafe state ownership/modes, unsupported schema/protocol, and incompatible Pi persistence APIs fail closed with actionable diagnostics.
19. `status` exposes Slack, database, leader, backlog, schedule, storage, delivery, and effect states; `doctor` is non-destructive and redacts secrets.
20. Kill/restart tests for gateway, extension, Slack connection, and Zellij satisfy the failure table without hidden terminal inference or claims of exactly-once execution.

## 20. Definition of v1 complete

V1 is complete when all acceptance criteria pass in automated tests where feasible and the remaining Pi/Zellij/Slack interactions pass a documented manual test. The release documentation must state the security boundary, offline receipt limitation, ambiguity inspection procedure, retention behavior, and absence of backup/restore/migration guarantees. Any implementation shortcut that makes terminal output, Slack reactions, or a transcript authoritative is a release blocker.
