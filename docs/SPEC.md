# Mosscap v1 Specification

**Status:** implementation-guiding specification

**Normative language:** “must”, “must not”, “should”, and “may” are requirements in descending order of strength.

**Product rule:** keep coordination durable; keep the agent human-owned.

Mosscap connects one trusted Slack channel and local CLI requests to one human-started interactive Pi session. A foreground Go gateway owns durable coordination in SQLite. A TypeScript extension inserts eligible work as visible Pi messages and triggers model processing.

[README.md](../README.md) introduces the product. [SYSTEM_MODEL.md](SYSTEM_MODEL.md) explains the design. This document is authoritative when they differ.

## 1. Scope

### 1.1 Required in v1

- One Go `mosscap` executable containing a foreground gateway and short-lived CLI commands.
- Slack Socket Mode ingestion for exactly one configured workspace and channel.
- Admission of direct bot mentions from Slack user IDs in a live, SQLite-backed trust set.
- Complete file-backed scalar configuration with validated, failure-atomic gateway reload.
- Automatic creation of one generic task for every admitted Slack message.
- A fixed, durable `no_entry` reaction on every configured-channel event rejected as `ignored_untrusted`, deduplicated per Slack message.
- Local task creation and explicit task state management.
- SQLite-first Slack acknowledgement and source-event deduplication.
- One explicitly opted-in Pi delivery leader with generation fencing, leases, persisted delivery keys, and recovery.
- Visible task delivery that immediately triggers model processing.
- Explicit single-channel Slack history, thread, send, edit, delete, reaction, and file operations.
- Durable outbound Slack intents with conclusive or ambiguous outcomes.
- Lazy incoming file download and direct outgoing upload using temporary bytes.
- A private, versioned local protocol used by the CLI and extension.
- Explicit inspection and requeue/retry for exceptional delivery and Slack outcomes.
- An explicit root-level purge command for old finalized history.

### 1.2 Not required in v1

- Starting, supervising, selecting, resetting, or stopping Pi.
- Headless agents, subagents, agent pools, or automatic model selection.
- More than one Slack workspace or channel.
- Task priority, assignment, tags, dependencies, or due dates.
- Historical task text, immutable revisions, or reliably retained diffs.
- Permanent storage, deduplication, or archival of file bytes.
- Automatic task progress posts or reactions beyond the fixed untrusted-sender denial reaction.
- Automatic retry after a possibly submitted Pi delivery or Slack operation.
- Automatic retention or background deletion of durable metadata.
- Per-task deletion or effect cancellation.
- Browser UI, installed service, mandatory supervisor, backup, restore, import, export, or cross-version migration guarantees.
- End-to-end exactly-once execution.

## 2. Architecture and authority

```text
Slack ───────┐
             ├──> foreground gateway ───> SQLite ledger
CLI ─────────┘              │
                            │ private local protocol
                            ▼
                       Pi extension
                            │
                            ▼
                  human-owned Pi session
```

### 2.1 Gateway

The gateway is the only normal writer to the SQLite coordination ledger. It owns:

- Slack connection, validation, admission, acknowledgement, and deduplication;
- task creation, content replacement, versioning, and state transitions;
- delivery leadership, generations, leases, and outcomes;
- Slack effect preparation, execution, and reconciliation;
- durable file metadata and temporary transfer lifecycle;
- mutation request deduplication;
- purge execution;
- validated configuration reload and atomic activation; and
- the local protocol server.

It runs in the foreground. It must exit non-zero when configuration or state permissions are unsafe, the schema is unsupported, SQLite cannot be opened safely, or the local endpoint cannot be bound.

### 2.2 CLI

The CLI is a gateway client. Except for `setup` and direct, offline-safe edits to `config.toml`, it must not write state directly. Trust changes and all other SQLite mutations go through the gateway.

Every gateway mutation must use a client-generated request ID. Human-readable output is the default. `--json` must emit stable typed objects without decorative text. A reload's mutation input is the SHA-256 digest of its exact staged TOML bytes: SQLite stores that digest and its typed outcome, never TOML bytes or token values. Reusing a request ID with different staged bytes is a conflict.

### 2.3 Pi extension

The TypeScript extension owns only integration with its current Pi process. It must:

- require explicit runtime opt-in through `/mosscap`;
- claim, maintain, and release delivery leadership;
- insert work as visible custom messages in the persisted Pi session;
- immediately trigger model processing when idle;
- queue delivery as a follow-up when Pi is already processing;
- persist the delivery key in the same custom-message entry;
- confirm persisted, rejected, or uncertain insertion outcomes; and
- show compact connection, leader, and backlog status.

It must not connect to Slack, write SQLite, infer task state from conversation text, inject terminal keystrokes, scrape terminal output, or manage Pi's process, model, or thinking level.

### 2.4 SQLite

SQLite is authoritative for coordination state. Pi JSONL, Slack messages and reactions, logs, and terminal output are projections.

The gateway must enable foreign keys and WAL mode and use a bounded busy timeout. Transactions listed in this specification must be atomic.

## 3. Configuration, trust, and local state

### 3.1 Fixed XDG paths

Mosscap uses these XDG-derived locations:

```text
$XDG_CONFIG_HOME/mosscap/config.toml
$XDG_STATE_HOME/mosscap/mosscap.sqlite3
$XDG_STATE_HOME/mosscap/control.sock
$XDG_STATE_HOME/mosscap/tmp/
```

Fallbacks are `~/.config` and `~/.local/state` when the corresponding XDG variable is unset. These paths are not settings in `config.toml`; a process that needs different roots must start with different XDG roots.

Configuration and state roots must be owned by the effective user and mode `0700`. `config.toml` and the database must be regular files owned by the effective user and mode `0600`. The socket must be under the private state root and mode `0600`.

Mosscap must reject symlinked roots, non-owner files, group/world-writable paths below the user's home, and endpoints in shared temporary directories.

### 3.2 Complete scalar configuration

`config.toml` is the authority for every scalar setting. It contains every setting explicitly so the available configuration is discoverable, including:

- Slack app token and bot token;
- workspace/team ID;
- bot user ID;
- exactly one channel ID; and
- all limits, lease durations, timeouts, and reconnect behavior.

The generated file should document its settings with comments. `config set KEY VALUE` must validate the known key and typed value, update only that key atomically, preserve comments, ordering, and unrelated formatting, and retain mode `0600`. It may operate while the gateway is stopped or running. Editing the file does not by itself change the gateway's active configuration.

The complete generated configuration contains these initial values:

| Setting | Initial value |
| --- | --- |
| delivery lease | 60 seconds |
| leader heartbeat / expiry | 10 / 30 seconds |
| Slack reconnect backoff | exponential 1–60 seconds with jitter |
| SQLite busy timeout | 5 seconds |
| maximum file size | 100 MiB |
| maximum files per Slack operation | 20 |
| temporary download lifetime | 24 hours |
| local protocol maximum frame | 1 MiB |

`config show` displays the file-backed settings and must redact both Slack tokens. Logs, diagnostics, protocol results, and mutation records must never contain tokens.

Slack operation commands never accept a workspace or channel argument.

### 3.3 Live trust set

Trusted Slack user IDs are not stored in `config.toml`. The current trust set is stored in SQLite, and each entry is an immutable Slack user ID. The set itself is mutable and may be empty; an empty set admits no Slack messages.

`trust add` and `trust remove` are request-deduplicated gateway mutations. A successful mutation takes effect for admission immediately after its transaction commits. Mosscap does not maintain a separate trust-change audit log.

### 3.4 Reload, setup, and compatibility

`gateway reload` parses and validates the complete TOML file, authenticates the proposed Slack credentials, and verifies the proposed workspace identity and channel access before activation. It must activate the complete proposed configuration as one in-memory snapshot or leave the prior snapshot active unchanged. A failed reload reports actionable diagnostics without disrupting the existing Slack connection. Operations already started under an earlier snapshot retain that snapshot for their attempt; new work uses the newly activated snapshot.

> **Current implementation boundary:** the pre-Slack phase performs complete local parsing, semantic/path validation, and failure-atomic snapshot activation through the local protocol. It has an injectable verifier seam and explicitly reports Slack network verification as deferred. Credential authentication and workspace/channel access checks remain a Phase 1 gate and are not simulated.

`mosscap setup` creates or validates private paths, writes a complete documented `config.toml`, and initializes the current schema only in new or empty state. It does not start the gateway or Pi. Re-running setup must not silently overwrite an existing configuration or state.

On startup, the gateway validates and activates the current complete TOML configuration before accepting work. A v1 binary may refuse an unknown schema or protocol version. It must never silently reinterpret unknown task, delivery, or effect states.

## 4. Durable records

Identifiers are opaque URL-safe strings with type prefixes. Output timestamps are UTC RFC 3339 with microsecond precision; storage uses integer microseconds. Mutable rows have `created_at` and `updated_at`.

### 4.1 Tasks

A task contains:

- `task_id` (`tsk_…`), unique;
- `version`, a positive monotonically increasing integer;
- `state`: `open`, `working`, or `done`;
- current `text`;
- `source_kind`: `slack` or `local`;
- optional Slack source reference;
- `created_at`, `updated_at`, and optional `done_at`.

A Slack source reference contains:

- source event ID;
- team and channel ID, retained for integrity even though configured globally;
- author user ID;
- message timestamp;
- thread timestamp;
- permalink when available;
- latest accepted Slack event timestamp; and
- optional source deletion timestamp.

Tasks contain only current text. Previous text and task revisions are not retained.

`version` increments whenever current text or source attachment metadata meaningfully changes. State-only transitions do not increment content version.

### 4.2 Source events

A source-event record contains:

- unique Slack event or derived envelope identity;
- workspace, channel, message, thread, and user identity when available;
- event kind and Slack event timestamp;
- received and committed timestamps;
- admission result;
- optional task ID; and
- only the bounded normalized metadata needed for deduplication and audit.

Full Slack envelopes and message bodies must not be retained after normalization. The current task contains the admitted current text.

Admission results include at least:

```text
admitted
duplicate
ignored_workspace
ignored_channel
ignored_untrusted
ignored_unmentioned
ignored_bot
ignored_stale
malformed
```

### 4.3 Task state events

State transitions are append-only audit records containing task ID, prior and new state, actor, optional note, request ID, and committed timestamp. They do not store historical task text.

### 4.4 Consumers and leader

A consumer records Pi session ID, Pi session file identity, connection ID, generation, connection state, and timestamps. It may include non-sensitive display metadata but never transcript content or credentials.

A singleton leader record stores current consumer ID, generation, and lease expiry.

### 4.5 Deliveries

A delivery records:

- delivery ID (`dlv_…`);
- task ID and content version;
- consumer and Pi session identity;
- leader generation;
- stable key derived from task ID, version, and Pi session ID;
- state, attempt count, and timestamps;
- lease token and expiry while applicable; and
- bounded diagnostics and operator decisions.

The delivery does not need to retain old task text. Before insertion starts, the gateway supplies the task's current complete content for the leased version. If that version is no longer current, the stale lease is rejected and the current version becomes eligible.

### 4.6 Effects

An effect records:

- effect ID (`eff_…`);
- unique idempotency key;
- optional related task ID;
- operation kind;
- exact configured-channel target and normalized request metadata;
- payload hash, including file hash where applicable;
- state and attempt count;
- prepared, submitted, and completed timestamps;
- Slack response identifiers;
- bounded diagnostic; and
- optional predecessor effect ID for explicit retries.

Effect kinds are:

```text
send
edit
delete
react
unreact
file_upload
```

### 4.7 Files

A file record stores metadata only:

- file ID (`fil_…`);
- direction (`from_slack` or `to_slack`);
- Slack file identity and permalink when applicable;
- original name;
- declared MIME type;
- size and SHA-256 when known;
- related task or effect;
- transfer state and diagnostic; and
- timestamps.

A temporary path may be recorded only while the path is live. Temporary paths are not durable file identity.

### 4.8 Request deduplication

Every local gateway mutation carries a request ID. The gateway stores its typed result so reconnecting clients can retrieve the original outcome without repeating the mutation.

Request records have an expiry eligibility timestamp. Expiry does not delete them automatically; it only makes finalized records eligible for explicit purge.

### 4.9 Trusted users

A trusted-user row stores one immutable Slack user ID as current authorization state. Adding an existing ID and removing an absent ID are idempotent. Removal deletes the current row; no separate trust-change history is retained. The table may contain zero rows.

## 5. Task behavior

### 5.1 State machine

```text
open ─────> working ─────> done
  ▲             │             │
  └─────────────┴─────────────┘
```

Rules:

- Creation always yields `open`.
- `working` is idempotent when already working.
- `done` is idempotent when already done and sets `done_at` on the first transition.
- `open` is valid from `working` or `done`; from `done` it clears `done_at`.
- An optional transition note is audit metadata, not task text.
- Slack activity never changes task state.
- Task state never causes Slack activity.
- There is no per-task delete or tombstone operation.

### 5.2 Local tasks

`task create` creates a task with `source_kind=local`. Its complete text is supplied by `--text` or `--text-file`.

`task update` replaces current text and increments version only when normalized text changes.

### 5.3 Slack-origin tasks

Every admitted Slack message creates exactly one task. The complete normalized message text becomes task text. The source reference points to the originating message and thread.

An admitted thread reply is a separate task only when that reply itself directly mentions the bot.

### 5.4 Slack edits

A meaningful edit to an already admitted source message replaces the task's current text or attachment metadata and increments version. Meaningful comparison normalizes line endings and trailing spaces but otherwise preserves content.

Events older than the source's latest accepted Slack event timestamp must not roll current content backward. They are recorded as `ignored_stale`.

When the gateway still has both old and new text in memory and the active Pi consumer is connected, it may include a bounded best-effort unified diff in the live update. The diff is not written to SQLite. Failure or reconnect delivers the latest complete text without a diff.

An edit that first adds a direct bot mention is admitted as a new task if all other checks pass.

### 5.5 Slack deletion

Deleting an admitted source message must not erase, complete, or delete the task. The gateway records source deletion metadata, increments version, and delivers a visible notice containing the preserved current task text and deleted-source status.

## 6. Slack admission

For each Socket Mode envelope, the gateway must:

1. validate envelope shape and configured workspace;
2. begin a SQLite transaction;
3. deduplicate source identity;
4. evaluate configured channel, human sender, the current committed trusted-user set, and direct bot mention;
5. normalize current text, thread identity, and file metadata;
6. create or update the task and source-event result;
7. commit; then
8. acknowledge the Slack envelope.

Known duplicates are acknowledged only after finding their committed result. A database commit failure is not acknowledged. Safely identifiable malformed events are recorded and acknowledged to prevent poison redelivery loops.

Initial admission requires all of:

- configured workspace and channel;
- non-bot sender;
- a sender ID present in the current committed trusted-user set; and
- literal direct mention of the configured bot user.

Display name, channel membership, `@channel`, and merely replying in a bot thread do not establish admission.

Thread identity is `thread_ts` when present, otherwise the source message timestamp.

Durable receipt begins only after the gateway receives and commits the envelope. Mosscap makes no claim that Socket Mode retains events indefinitely while the gateway is offline.

## 7. Pi leadership and delivery

### 7.1 Opt-in

The extension must not connect for work until the user runs `/mosscap` in that Pi process. Opt-in is process-runtime state and is not automatically restored merely because a session file was previously opted in.

The extension must refuse delivery when Pi reports an in-memory or otherwise non-persisted session.

### 7.2 Claim and takeover

The extension presents protocol version, extension version, Pi session ID, session-file identity, and persisted Mosscap delivery keys.

If no valid leader exists, the gateway atomically increments the generation and grants leadership.

If another leader exists, `/mosscap` shows its non-sensitive identity and asks for confirmation. `/mosscap --takeover` is explicit confirmation. On takeover the gateway atomically:

- increments generation;
- marks the old consumer fenced;
- invalidates its outstanding leases;
- grants the new consumer leadership; and
- sends a fenced notification before closing the old connection when possible.

Every extension acknowledgement carries generation and lease token. Stale values are rejected.

Takeover fences gateway delivery authority only. It must warn that the old Pi process and already-running tools are not stopped.

`/mosscap off` releases leadership when current and disconnects without changing tasks.

### 7.3 Delivery states

```text
pending → leased → inserting → persisted
              │         ├────→ uncertain
              │         └────→ rejected
              └──────────────→ pending  (expiry before insertion start)
```

- `pending`: current task version is eligible for the leader.
- `leased`: gateway issued a generation-bound lease; insertion has not started.
- `inserting`: extension reported insertion start.
- `persisted`: extension confirmed the visible custom message and delivery key are in Pi JSONL.
- `uncertain`: insertion may have occurred but persistence cannot be confirmed.
- `rejected`: Pi rejected insertion or the extension rejected invalid content.

A lease may return automatically to pending only when it expires in `leased`. Expiry, disconnect, or process failure from `inserting` becomes `uncertain`.

`uncertain` and `rejected` are never automatically requeued. Explicit requeue creates a new attempt while preserving prior diagnostics.

### 7.4 Visible message

Each work message presents concise, actionable visible text:

- task ID, current state, and complete current text within Pi message limits;
- local or Slack origin, plus Slack author and thread when applicable;
- the exact Slack reply command when applicable;
- the task completion and detail commands; and
- attachment metadata and retrieval commands only when attachments exist.

Version, delivery ID, delivery key, and Pi session ID remain in the custom message's structured details rather than cluttering the visible instructions. User-provided text is visibly delimited from Mosscap control text. Tokens and raw Slack envelopes must never appear.

The extension must use a custom message with `display: true` so it is visible and participates in model context. Hidden context injection does not satisfy delivery. The delivery key must be stored in that same custom message's structured details.

When idle, the extension submits the custom message with immediate turn triggering. When Pi is already processing, it queues the message as a follow-up that starts after the current agent run settles. It must not interrupt tool calls in progress.

A `message_end` observation and matching persisted session entry are required before reporting `persisted`. Merely calling Pi's fire-and-forget send API is insufficient.

### 7.5 Session-wide deduplication

On startup of an opted-in connection, the extension scans all Mosscap custom-message entries in the persisted session file, including entries outside the active branch, and reports their delivery keys.

Keys are session-wide. Moving to another conversation branch does not make a task version eligible for duplicate insertion into the same Pi session.

### 7.6 Recovery

After claim or reconnect, the gateway reconciles reported keys before issuing new leases. The extension first displays a recovery message containing:

- current leader generation;
- task counts by state;
- current `open` and `working` tasks ordered by oldest update;
- tasks whose current version has not been persisted in this Pi session;
- uncertain or rejected deliveries; and
- ambiguous or failed effects.

The recovery message triggers one model turn. The gateway then makes the latest content version of each eligible non-done task available for normal delivery. It does not reconstruct historical text or diffs.

A newly opted-in Pi session may receive current non-done work again under keys derived from its new session ID.

## 8. Slack reads and effects

### 8.1 Single-channel rule

Every Slack command implicitly operates on the configured channel. No command accepts a channel or workspace selector. Message and thread targets are validated as belonging to that channel before mutation.

### 8.2 Read operations

`slack history` and `slack thread` perform bounded reads and do not create effects. They return message timestamps, thread identity, author IDs, text, reactions, and file metadata needed for later explicit operations.

Read results are not persisted as task state or long-term cache.

### 8.3 Effect states

```text
prepared → submitted → confirmed
                       ├────→ failed
                       └────→ ambiguous
```

- `prepared`: exact intent and idempotency key committed before network I/O.
- `submitted`: transmission may have begun.
- `confirmed`: Slack returned conclusive success and identifiers were stored.
- `failed`: Slack conclusively rejected the operation.
- `ambiguous`: timeout, disconnect, or malformed response after submission means Slack may have applied it.

A `prepared` effect may be submitted by the gateway. On startup, a safely unsubmitted prepared effect remains eligible. A `submitted` effect without conclusive result becomes `ambiguous`; it is never resubmitted automatically.

Where Slack accepts a client idempotency token, Mosscap supplies the effect key without weakening ambiguous-outcome handling.

### 8.4 Explicit retry

`effect retry` creates a new prepared effect linked to its predecessor. It never mutates the predecessor into a new attempt.

Retry of an ambiguous predecessor requires `--confirm-duplicate-risk`. Retry of a file upload also requires the current local file to match the predecessor's recorded size and SHA-256; otherwise the operator must issue a new upload command.

There is no effect cancellation operation.

### 8.5 Independence from tasks

Effects may optionally reference a task for audit, but:

- replying or reacting does not change task state;
- marking a task done does not create an effect; and
- task source identity supplies a convenient thread target, not implicit permission to act.

## 9. File transfer

### 9.1 General rules

Mosscap stores durable metadata and outcomes, not durable file bytes. It accepts broad file types and treats extension and MIME type as untrusted metadata.

All transfers enforce configured size and count limits. Mosscap must never execute downloaded content, extract archives automatically, or render bytes as trusted HTML.

### 9.2 Incoming Slack files

Admission stores Slack file identity and metadata but does not download bytes.

`file get FILE_ID --output PATH` causes the gateway to fetch the file using authenticated Slack APIs into a random private temporary file while enforcing the byte limit and computing SHA-256. After complete validation, the CLI places it at the requested path atomically.

Output creation is exclusive by default. `--force` may replace only an existing regular file using race-safe replacement.

`file get FILE_ID` without `--output` returns a private temporary path and its expiry time. The gateway may remove temporary files after expiry or on restart. Callers must move anything they need to retain.

Redirects are allowed only over HTTPS to Slack-approved file hosts. Slack credentials must never be forwarded to another host.

Unavailable or oversized files remain visible metadata and produce a typed error without affecting the task.

### 9.3 Outgoing files

`slack send --file` and `slack file` accept regular files only. They reject directories, devices, FIFOs, sockets, symlinks, sparse files whose logical size exceeds the limit, unreadable paths, and files that change identity or size during validation and upload.

Before submission, the gateway records path metadata, size, and SHA-256 in the prepared effect. It uploads the validated bytes without retaining a permanent copy.

If the process restarts before a prepared upload is submitted, the gateway must revalidate size and hash before using the path. A mismatch conclusively fails that effect rather than uploading different bytes.

Confirmed upload records Slack file ID and permalink. Ambiguous upload follows normal effect rules.

### 9.4 Temporary files

Temporary files use generated names under the private state root, mode `0600`, and atomic creation. User-provided filenames never determine temporary paths.

Expired temporary bytes are the only data Mosscap may delete automatically. Their durable metadata remains until explicit purge.

## 10. Purge

Mosscap performs no automatic retention of durable metadata.

The root command is:

```text
mosscap purge --before RFC3339 --dry-run
mosscap purge --before RFC3339 --confirm
```

Exactly one of `--dry-run` or `--confirm` is required. The cutoff is an absolute UTC-capable RFC 3339 timestamp.

Purge may remove finalized records older than the cutoff:

- `done` tasks;
- state events and source records owned only by purged tasks;
- persisted delivery history owned only by purged tasks;
- confirmed effects owned only by purged tasks;
- old confirmed standalone effects;
- old ignored or duplicate source-event records;
- expired request-deduplication results;
- old disconnected or fenced consumer records;
- unreferenced file metadata and expired temporary files; and
- unused SQLite pages through safe maintenance.

Purge must preserve:

- `open` or `working` tasks;
- connected consumers and current leadership;
- pending, leased, or inserting deliveries;
- uncertain or rejected deliveries;
- prepared, submitted, failed, or ambiguous effects;
- live request-deduplication records;
- any record referenced by retained state; and
- configuration, trust, and credentials.

If a done task references an unresolved delivery or effect, the task and required graph are skipped. Dry-run and confirmed output must group eligible, skipped, and removed counts by record type. Confirmed purge must be transactional in bounded batches and leave referential integrity valid after interruption.

## 11. CLI and extension commands

All commands support `--help`. Read commands support `--json`. Identifiers may be abbreviated only when unambiguous. Mutations accept `--request-id`.

```text
mosscap setup
mosscap config show
mosscap config set KEY VALUE
mosscap gateway run
mosscap gateway reload
mosscap gateway status [--json]
mosscap doctor [--json]

mosscap trust list
mosscap trust add SLACK_USER_ID
mosscap trust remove SLACK_USER_ID

mosscap task create (--text TEXT|--text-file PATH)
mosscap task list [--state open|working|done] [--all] [--json]
mosscap task show TASK [--json]
mosscap task update TASK (--text TEXT|--text-file PATH)
mosscap task open TASK [--note TEXT]
mosscap task working TASK [--note TEXT]
mosscap task done TASK [--note TEXT]

mosscap delivery list [--state pending|leased|inserting|persisted|uncertain|rejected]
mosscap delivery inspect DELIVERY
mosscap delivery requeue DELIVERY --reason TEXT

mosscap effect list [--state prepared|submitted|confirmed|ambiguous|failed]
mosscap effect inspect EFFECT
mosscap effect retry EFFECT --reason TEXT [--confirm-duplicate-risk]

mosscap slack history [--limit N]
mosscap slack thread THREAD_TS
mosscap slack send [--thread THREAD_TS] (--text TEXT|--text-file PATH) [--file PATH ...] [--task TASK]
mosscap slack edit MESSAGE_TS (--text TEXT|--text-file PATH) [--task TASK]
mosscap slack delete MESSAGE_TS --reason TEXT [--task TASK]
mosscap slack react MESSAGE_TS EMOJI [--task TASK]
mosscap slack unreact MESSAGE_TS EMOJI [--task TASK]
mosscap slack file [--thread THREAD_TS] --file PATH [--comment TEXT] [--task TASK]

mosscap file show FILE_ID
mosscap file get FILE_ID [--output PATH] [--force]

mosscap purge --before RFC3339 (--dry-run|--confirm)
```

`--task` adds an audit relationship only. It does not derive a destination, change task state, or grant permission.

Pi extension commands:

```text
/mosscap
/mosscap --takeover
/mosscap status
/mosscap off
```

Stable exit categories are:

| Code | Meaning |
| --- | --- |
| 0 | success |
| 2 | usage error |
| 3 | not found |
| 4 | conflict or stale generation |
| 5 | unsafe configuration or permissions |
| 6 | gateway unavailable |
| 7 | conclusive external operation failure |
| 8 | ambiguous outcome |
| 9 | unsupported schema or protocol |

## 12. Local protocol

The gateway exposes a versioned 32-bit-length-prefixed JSON protocol over the private Unix socket. File bytes are never placed in JSON frames; clients exchange file IDs and private temporary transfer references.

Every request includes protocol version, request ID, client kind, and client version. Every response includes request ID, server version, and either a typed result or typed error.

Required capabilities:

- handshake and capability negotiation;
- status and health;
- mutation request deduplication;
- task, Slack, effect, delivery, file, trust, configuration reload, and purge operations;
- leader claim, release, heartbeat, generation fencing, and server disconnect;
- delivery lease, insertion-start, persisted, rejected, and uncertain reports; and
- bounded server notifications for newly eligible task versions and fencing.

Unknown fields may be ignored only within a declared compatible version. Unknown enum values fail safe. Frames larger than the configured maximum are rejected before allocation of an unbounded payload.

The current additive trust and reload commands retain protocol version 1 rather than bumping it: their envelopes use the existing command/result mechanism and the hello capability list is the compatibility boundary. A v1 client must inspect server capabilities before sending an additive command; a gateway that omits `trusted_users` or `config_reload` is unsupported for that command (exit 9), not treated as a transport failure. A future incompatible envelope, enum, or handshake change requires a protocol bump.

Per-connection outbound queues are bounded. A slow client is disconnected without discarding durable work.

The server authenticates same-user clients through private filesystem permissions and peer credentials where supported. The same-user boundary is not protection from a compromised account.

## 13. Status, diagnostics, and logs

`gateway status` reports:

- database and endpoint identity;
- schema and protocol versions;
- Slack connection and bounded reconnect state;
- last received and committed source event;
- current delivery leader, generation, and heartbeat age;
- task counts by state;
- delivery and effect counts by state;
- temporary-file count and bytes;
- SQLite file size; and
- exceptional outcomes requiring inspection.

Action-required outcomes such as ambiguous effects are reported separately from process health so a supervisor does not restart a healthy gateway repeatedly.

`doctor` is non-destructive. It checks configuration, redaction, path ownership and modes, SQLite integrity and a rolled-back write test, local endpoint compatibility, Slack authentication and configured-channel access, bot identity, trusted IDs, required scopes, Pi extension compatibility, and storage availability. It never sends a Slack message, downloads a file, or takes leadership.

Structured logs include request, source-event, task, effect, and delivery IDs with timestamps. They exclude tokens, full message bodies, and raw Slack payloads. Transfer components do not deliberately supply raw file bytes or private transfer URLs to the logger. Verbose mode may include complete local paths and ordinary diagnostics but never credentials.

### 13.1 Observability disclosure boundary

Mosscap does not redact local filesystem paths or ordinary operational diagnostics. Owner-only CLI and protocol responses, status, doctor, gateway logs, and opted-in Pi messages may render complete configuration, state, database, socket, temporary, source, and destination paths together with the underlying local diagnostic. A compact summary may omit detail for presentation, but path privacy is not a product requirement and no path-redaction acceptance gate applies.

Pi-visible diagnostics participate in model context and may persist in Pi JSONL. An operator who opts into Mosscap therefore accepts that these values, including directory, user, and project names, may be disclosed to the configured model provider.

Slack tokens, authorization headers, and equivalent credentials remain redacted and must never enter SQLite, logs, protocol results, or Pi messages. This is the only output-redaction boundary. Secret-bearing adapter inputs should not be embedded into errors in the first place.

File bytes and authenticated or capability-bearing private Slack download/upload URLs have separate lifecycle constraints: bytes flow only through explicit transfer and temporary files, and private transfer URLs remain transient adapter data rather than durable records. Those constraints do not require a generic diagnostic scrubber. An ordinary configured-channel permalink remains permitted source metadata under §7.4, and Pi may later read an explicitly retrieved file through its ordinary tool authority.

This policy does not protect paths or diagnostics from a process that already controls the same effective UID.

## 14. Failure and recovery

| Failure | Required behavior |
| --- | --- |
| Gateway exits | Committed SQLite state remains; clients reconnect after restart. |
| Slack disconnects | Reconnect with bounded jitter; deduplicate redelivery; make no claim of receipt while offline. |
| Database full or corrupt | Stop admitting and acknowledging new work, fail closed, and report exact diagnostics; do not auto-repair. |
| Pi exits before insertion start | Expired lease returns to pending. |
| Pi exits after insertion start | Delivery becomes uncertain; never redeliver automatically. |
| Extension reconnects | Reconcile session-wide persisted keys before recovery and new leases. |
| New Pi takes over | Increment generation, fence old delivery connection, and reject stale acknowledgements. |
| Slack operation times out after submit | Mark ambiguous; never retry automatically. |
| Lazy file download fails | Keep metadata and task intact; return a typed retriable or conclusive error. |
| Outgoing file changes | Fail the effect; never upload bytes that differ from the prepared hash. |
| Clock moves | Use monotonic time for live leases and UTC wall time for durable timestamps. |
| Purge is interrupted | Retain referential integrity; safely rerun later. |

Startup must check schema compatibility and SQLite integrity, expire dead leadership, convert inserting deliveries owned by dead connections to uncertain, release only never-started expired leases, classify unresolved submitted effects as ambiguous, remove expired temporary bytes, bind the local endpoint, and connect to Slack.

## 15. Security boundaries

- While `/mosscap` is active, a trusted Slack user's admitted mention automatically triggers model processing in a fully capable local Pi session.
- Trust is based only on configured workspace, configured channel, membership in the live allowlist of immutable Slack user IDs, non-bot sender, and direct bot mention.
- These checks are authorization gates, not a sandbox.
- Slack text, task text, filenames, MIME labels, and file bytes are untrusted data and must be visibly delimited from Mosscap control metadata.
- Slack-origin text cannot directly invoke local protocol commands. Pi may choose to act on it with the authority the human gave Pi; that is the intended trust boundary.
- The local socket trusts same-user clients and does not defend against compromise of that OS account.
- Owner-only operator and opted-in agent surfaces do not redact local paths or ordinary diagnostics; these values can reach the configured model provider and persisted Pi session.
- Tokens use least Slack scope, are redacted, never enter SQLite source payloads, and never enter Pi messages. Private Slack transfer URLs and file bytes follow transient transfer lifecycles rather than being deliberately persisted as durable metadata.
- All Slack writes are explicit CLI operations with visible targets and durable intents.
- File handling defends against traversal, symlink races, replacement races, oversized data, unsafe redirects, and non-regular local inputs.
- Purge is irreversible, requires an explicit cutoff and confirmation, and reports what it removed.

## 16. Invariants

1. An identifiable Slack envelope is acknowledged only after its admission or deduplication result commits.
2. One admitted Slack source message maps to at most one task.
3. Every task stores one current text value and a monotonic content version; stale Slack edits cannot roll it backward.
4. Task state changes and Slack operations are independent and explicit.
5. SQLite is the sole coordination authority.
6. At most one unexpired delivery-leader generation is valid; stale generations cannot acknowledge leases.
7. A delivery is persisted only after visible insertion and the delivery key are observed in Pi's persisted session.
8. A delivery that may have been inserted but cannot be confirmed is uncertain, never silently pending.
9. A Slack effect intent commits before submission.
10. A possibly submitted unconfirmed Slack effect is ambiguous and is never retried automatically.
11. File bytes are temporary; durable records contain identity, metadata, hashes, and outcomes only.
12. Automatic deletion is limited to expired temporary bytes; durable metadata requires explicit purge.
13. Purge never removes live tasks or unresolved delivery/effect evidence.
14. No Mosscap component starts or owns Pi.
15. Slack admission uses the current committed trusted-user set; an empty set admits no Slack messages.
16. Configuration reload either activates one complete validated snapshot or leaves the prior snapshot active.

## 17. Implementation sequence

### Phase 0: storage and local tasks

- Go executable and internal gateway/client separation.
- Private XDG layout, configuration, SQLite schema, typed errors, and request IDs.
- Foreground local protocol server.
- Task create, list, show, update, and state commands.
- Status and doctor foundations.

**Gate:** local tasks and request-deduplicated mutations survive gateway restart.

### Phase 1: Slack admission and operations

- Complete scalar TOML configuration, comment-preserving edits, and validated gateway reload (local snapshot foundation implemented before Slack; network verification completes here).
- SQLite-backed live trust commands (implemented before Slack) and empty-set admission behavior (completed with Slack admission here).
- Socket Mode connection and strict single-channel admission.
- Commit-before-ack deduplication, source edits, deletions, and file metadata.
- Single-channel reads and durable Slack effects.
- Deterministic fake Slack tests for redelivery, transaction failure, stale edits, and pre/post-submit timeout.

**Gate:** an eligible mention creates exactly one task and no ambiguous effect is automatically retried.

### Phase 2: Pi delivery

- TypeScript extension and compatibility checks.
- Explicit opt-in, takeover, heartbeat, leases, visible custom messages, immediate turns, and persisted keys.
- Recovery and exceptional delivery inspection/requeue.
- Tests with two fake extension clients plus a Pi integration harness.

**Gate:** only the current generation receives delivery authority, and crash timing produces pending versus uncertain as specified.

### Phase 3: temporary files and purge

- Lazy Slack download, direct outgoing upload, validation, hashes, and temporary expiry.
- Root purge dry-run, confirmation, bounded batches, and referential checks.
- Tests for oversized, changing, sparse, symlinked, interrupted, and unavailable files.

**Gate:** bidirectional transfer works without permanent byte storage, and purge cannot remove live or unresolved state.

### Phase 4: operational completion

- Complete diagnostics, structured logging, packaging, and operator documentation.
- End-to-end fault matrix and manual use in a real interactive Pi session.

## 18. Acceptance criteria

### Admission and tasks

1. A trusted human's direct bot mention in the configured channel commits before acknowledgement and creates exactly one `open` task after duplicate delivery.
2. Wrong-workspace, wrong-channel, untrusted, bot-authored, and unmentioned messages create no tasks and have inspectable admission results.
3. Local task creation produces the same task shape without Slack source metadata.
4. `open`, `working`, and `done` transitions obey the state machine and never post to Slack.
5. A meaningful current Slack edit replaces task text, increments version, and cannot be rolled back by a later-arriving older event.
6. No historical task text is available after replacement; a connected Pi may receive a best-effort live diff.
7. Source deletion preserves task text and state while making deletion visible.

### Pi delivery

8. Mosscap never starts Pi; explicit `/mosscap` in a persisted human-started session is required.
9. Delivered work is a distinct visible custom message containing a key in the same session entry and triggers a model turn immediately or as a follow-up.
10. A crash before insertion start returns the lease to pending; a crash after insertion start yields uncertain with no automatic retry.
11. A takeover increments generation, fences the previous delivery connection, and rejects its delayed acknowledgement without claiming to stop its Pi process.
12. Reconnect reconciles all session-wide keys and presents current non-done work and exceptional outcomes.

### Slack operations

13. No Slack command accepts a channel or workspace selector; all targets are validated against the configured channel.
14. Send, edit, delete, react, unreact, and upload commit an exact durable intent before network submission.
15. A simulated post-submit timeout becomes ambiguous and restart performs no duplicate API call.
16. Explicit retry creates a linked effect and requires duplicate-risk confirmation after ambiguity.
17. Slack activity never changes task state, even when an effect references a task.

### Files and purge

18. Incoming files are metadata-only until requested and can be downloaded to an explicit destination or expiring temporary path.
19. Outgoing text, image, PDF, archive, and opaque binary files upload within limits without retaining a permanent copy.
20. Oversized, symlinked, sparse, changing, unavailable, or unsafe-redirect files fail visibly without losing their task or effect evidence.
21. Purge dry-run groups eligible and skipped records without mutation.
22. Confirmed purge removes eligible old finalized history but preserves live tasks and every unresolved delivery or effect.

### Operations and security

23. Unsafe path ownership or modes, unsupported schema/protocol, and incompatible Pi persistence APIs fail closed with actionable diagnostics.
24. Status distinguishes gateway health from operator-action-required ambiguity and reports SQLite and temporary-byte usage.
25. Doctor is non-destructive and secret-safe.
26. Gateway, Slack, extension, and file-transfer fault tests satisfy the recovery table without terminal inference or exactly-once claims.
27. Setup writes a complete, documented, owner-only TOML file; targeted CLI edits preserve its comments and formatting, and all output redacts tokens.
28. A reload with valid Slack configuration atomically activates the complete snapshot; an invalid or inaccessible proposal leaves the prior configuration and connection active.
29. Trust add/remove operations survive restart, apply immediately after commit, and an empty trust set admits no Slack messages.

## 19. Definition of v1 complete

V1 is complete when all acceptance criteria pass in automated tests where feasible and remaining Slack/Pi interactions pass a documented manual test. Release documentation must state:

- the trusted-user and same-user security boundaries;
- that opted-in Slack mentions automatically trigger Pi;
- that local paths and ordinary diagnostics are not redacted and may enter Pi/model context, while credentials remain redacted and private transfer URLs/file bytes are not deliberately persisted as durable records;
- the Socket Mode offline receipt limitation;
- uncertain-delivery and ambiguous-effect inspection procedures;
- temporary file behavior;
- irreversible purge behavior; and
- the absence of backup, restore, and migration guarantees.

Any implementation that makes terminal output, Slack reactions, or Pi conversation claims authoritative task state is a release blocker.
