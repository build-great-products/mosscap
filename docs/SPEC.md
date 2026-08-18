# Mosscap v1 product contract

**Status:** normative contract for the current integrated v1 implementation

“Must”, “must not”, “should”, and “may” express requirements in descending order of strength. [`SYSTEM_MODEL.md`](SYSTEM_MODEL.md) explains the design; the [README](../README.md) and runbooks explain operation.

The former numbered phases were implementation milestones. They are not product modes and are not part of this contract.

## 1. Scope

Mosscap must provide:

- one foreground Go gateway and short-lived CLI clients;
- one Slack Socket Mode workspace/channel connection;
- admission of direct bot mentions from a live SQLite-backed trusted-user set;
- local and Slack-origin tasks with explicit state transitions;
- commit-before-ack Slack source deduplication;
- one explicitly opted-in persisted Pi TUI session at a time;
- visible task delivery that triggers model processing;
- explicit Slack reads, writes, reactions, and file operations;
- durable Slack intents and Pi delivery outcomes with honest uncertainty;
- temporary, bounded file transfer without permanent byte storage;
- validated, atomic configuration reload;
- a private versioned local protocol; and
- explicit inspection, retry/requeue, and metadata purge operations.

Mosscap does not need to start or supervise Pi, support more than one Slack destination, retain historical task text, archive file bytes, infer task state from conversation, retry uncertain external work automatically, provide a browser UI, or guarantee end-to-end exactly-once execution.

## 2. Authority and persistence

The gateway is the only normal SQLite writer. CLI commands other than setup and offline-safe configuration edits must use the gateway.

SQLite is authoritative for coordination state: tasks, source events, trust, deliveries, effects, files, and request outcomes. Pi JSONL is authoritative only for what that Pi session has persisted. Slack and Pi are external persistence domains and cannot share a transaction with SQLite.

Every gateway mutation must carry a client-generated request ID. A replay with the same operation and canonical input must return the original typed outcome without repeating the mutation. Reuse for different input must fail as a conflict. Stored request outcomes must not become a hidden archive of task text or credentials.

## 3. Configuration and local security

Mosscap uses fixed XDG-derived paths:

```text
$XDG_CONFIG_HOME/mosscap/config.toml
$XDG_STATE_HOME/mosscap/mosscap.sqlite3
$XDG_STATE_HOME/mosscap/control.sock
$XDG_STATE_HOME/mosscap/tmp/
```

Fallbacks are `~/.config` and `~/.local/state`. Configuration and state roots must be owner-only directories. Sensitive files must be owner-only regular files; unsafe ownership, modes, symlinks, hard links, or escaped state paths must fail closed.

`config.toml` contains Slack app and bot tokens, the channel ID, and all scalar limits/timeouts. Workspace/team and bot-user IDs are derived and verified through Slack authentication, not configured manually. Tokens must be set through the private config file, redacted from output, and excluded from SQLite, logs, protocol results, and Pi messages.

`config set` must update one known non-secret value atomically while preserving unrelated comments and formatting. `config secret set` must accept only known Slack token fields and avoid printing their values. Editing stages a candidate; it does not change the running snapshot.

On startup and `gateway reload`, the gateway must validate the complete candidate and verify Slack identity/channel access before activation. Activation is all-or-nothing. Failed reload must leave the prior runtime and connection usable.

The trusted-user set lives in SQLite, may be empty, and changes immediately after a successful `trust add` or `trust remove` commit. An empty set admits no Slack work.

## 4. Tasks and Slack admission

A task has a unique opaque ID, positive monotonic content version, current text, `open`/`working`/`done` state, timestamps, and a local or Slack source. Previous text is not retained.

Creation yields `open`. Explicit transitions may move between all three states; same-state transitions are idempotent. Entering `done` sets `done_at`; leaving it clears `done_at`. State-only changes do not increment content version. For Slack-origin tasks, entering `done` removes the current lifecycle reaction and reopening restores the reaction implied by durable delivery evidence; state transitions do not otherwise post or alter Slack content.

An initial Slack message is admitted only when all of these hold:

- it belongs to the authenticated workspace and configured channel;
- the sender is a non-bot user in the current trusted-user set; and
- its current text literally mentions the authenticated bot user.

For each identifiable envelope, the gateway must deduplicate and commit the admission result before acknowledging Slack. A commit failure must not be acknowledged. Mosscap makes no receipt claim while disconnected from Socket Mode.

An admitted message creates at most one task. A meaningful newer source edit replaces current text/attachment metadata and increments version. Stale edits must not roll content backward. Source deletion preserves task text and state while recording deletion metadata. Incoming attachments are metadata-only until explicitly retrieved.

Configured-channel events rejected as untrusted must prepare a deduplicated `no_entry` reaction before acknowledgement. Failure or ambiguity of that reaction must remain inspectable and must not admit the event.

An admitted Slack task version prepares an `hourglass_flowing_sand` queued reaction before acknowledgement. Once that exact latest version is durably recorded as `persisted` by Pi, Mosscap prepares ordered effects to add `eyes` and then remove the queued reaction; entering `done` removes whichever current lifecycle marker applies, and each later reopening restores the appropriate marker with a distinct durable intent. A meaningful edit invalidates an older persisted version by adding its new queued marker before removing `eyes`. These are Slack projections, not task-state transactions: duplicate delivery/restart reuses an intent, and a failed or ambiguous submission is inspectable and is never automatically retried. The remove/add replacement is explicitly not atomic.

## 5. Pi delivery

The extension must remain inactive until a human runs `/mosscap` in a persisted interactive TUI session. `/mosscap off` must disconnect without changing tasks or stopping Pi. Runtime opt-in must not be silently restored by a prior session file.

The gateway grants one active extension connection/session slot. A second session must be refused rather than silently replacing the first. Disconnect releases the slot after reconciling any in-progress insertion.

Before polling work, the extension must scan persisted Mosscap custom-message entries and reconcile observed delivery IDs/keys for its Pi session. A task delivery must:

- be a visible custom message with clearly delimited user-provided text;
- include its delivery key in structured details;
- trigger a model turn immediately when idle or as a follow-up when busy; and
- be reported persisted only after the matching message is observed in Pi session entries.

Delivery states are:

```text
pending → inserting → persisted
              ├────→ uncertain
              └────→ rejected
```

A disconnect after insertion starts must become `uncertain`, never silently pending. Uncertain and rejected attempts must not be automatically repeated. `delivery requeue` creates a new attempt, preserves predecessor evidence, requires a reason, and warns that duplicate insertion is possible.

The extension must not connect to Slack, write SQLite, add hidden model-callable Mosscap tools, infer task state from prose, or manage the Pi process/model.

## 6. Slack operations and effects

Every Slack command implicitly targets the configured channel. No operation may accept an alternate workspace or channel selector. Targeted writes must verify the target belongs to that channel.

Reads are bounded and do not create effects or change task state. Writes commit an exact effect intent before their single network submission.

Effect states are:

```text
prepared → submitted → confirmed
                       ├────→ failed
                       └────→ ambiguous
```

Conclusive rejection is `failed`. Timeout, disconnect, cancellation, or malformed completion after submission is `ambiguous`. Submitted work without a conclusive result must not be retried automatically, including after restart.

`effect retry` creates a linked successor rather than rewriting its predecessor. Retrying an ambiguous effect requires explicit duplicate-risk confirmation. Task relationships are audit metadata only.

`task reply` derives its destination from a durable Slack task source. With `--done`, it must confirm the reply before applying the local completion. Interrupted replay with the same request ID must not post twice, and failures/ambiguity must leave the task incomplete.

## 7. Files

File records contain identity, direction, untrusted name/MIME metadata, size/hash when known, relationships, transfer state, diagnostics, and timestamps. File bytes and authenticated private transfer URLs must not be durable metadata.

`file get` must fetch an admitted incoming file only on explicit request, enforce configured limits, use a random private temporary file, and compute/validate its identity. Explicit output placement must be atomic and exclusive by default. `--force` may replace only a regular file using race-safe behavior.

Outgoing uploads must accept regular files only and reject symlinks, devices, FIFOs, sockets, unsafe sparse files, oversized files, and files that change during validation/upload. A prepared upload must record size and SHA-256 before submission; changed bytes must conclusively fail rather than upload under the old intent.

Temporary files must use generated owner-only paths under the state root. Expired temporary bytes may be removed automatically. Durable file metadata requires explicit purge.

## 8. Purge and diagnostics

Purge requires an absolute RFC 3339 cutoff and exactly one of `--dry-run` or `--confirm`. Dry-run must not mutate state. Confirmed purge may remove eligible finalized history but must preserve open/working tasks, current delivery ownership, unresolved deliveries/effects, live request outcomes, trust, configuration, and every record required by retained state.

`gateway status` must distinguish process health from outcomes requiring operator action and report bounded task, Slack, delivery, effect, database, and temporary-file state. `doctor` must be non-destructive: it must not send Slack messages, acknowledge events, transfer bytes, or claim Pi ownership.

Logs and diagnostics must exclude credentials, authorization headers, raw Slack envelopes, and full message bodies. Local paths and ordinary diagnostics are not redacted and may appear in opted-in Pi/model context.

## 9. Commands and compatibility

The current command families are:

```text
mosscap setup
mosscap config show|set|secret set
mosscap gateway run|reload|status
mosscap doctor
mosscap trust list|add|remove
mosscap task create|list|show|update|open|working|done|reply
mosscap slack history|thread|send|file|edit|delete|react|unreact
mosscap effect list|inspect|retry
mosscap file show|get
mosscap delivery list|inspect|requeue
mosscap purge
```

Pi provides `/mosscap`, `/mosscap status`, and `/mosscap off`.

SQLite-backed `task list`, `effect list`, `delivery list`, and `trust list` accept bounded `--limit`/`--cursor` pages (effects and deliveries also accept composable `--task`). Tasks are oldest-created-first; effects and deliveries newest-created-first; trust is lexical. Resource cursors are full IDs and a purged anchor returns typed `cursor_expired` conflict guidance to restart without `--cursor`; trust cursors remain valid after removal. `slack history` and `slack thread` likewise accept `--limit` and `--cursor`; both default to and cap at 50 messages, preserve Slack's native order, and pass the opaque Slack cursor unchanged.

The private Unix-socket protocol uses bounded 32-bit-length-prefixed JSON frames, a versioned handshake, typed command/results, and capability negotiation. Unknown incompatible envelopes or enum values must fail safe. Slow clients must be disconnected without discarding durable work. Same-user authentication is not protection from compromise of that OS account.

Every CLI leaf that contacts the gateway accepts a positive shared `--timeout` duration, defaulting to 10 seconds. Its deadline begins immediately before gateway contact and covers dial, hello, calls, and frame I/O; an earlier application/caller deadline wins. Explicit body stdin and local validation occur before this deadline. Cancellation before a Slack/Pi external boundary leaves work unsubmitted; after a boundary, the gateway durably records a confirmed, failed, or ambiguous outcome using a non-cancelled commit context. Timeout, cancellation, or disconnect after submission is ambiguous and is never retried automatically.

Cobra help is the complete command-discovery interface. Running the root or a command branch without a leaf prints help and exits successfully; every leaf documents its syntax, flags, examples, and applicable irreversible or duplicate-risk warnings. Help performs no configuration, gateway, or mutation work. Unknown commands and flags print usage for the relevant command and exit 2. Only unambiguous close command or flag misspellings may be suggested; positional values are never rewritten or suggested. Shell-completion and machine-readable help commands are not exposed.

`task create`, `task update`, `task reply`, `slack send`, and `slack edit` require exactly one text source: `--text`, `--text-file`, or `--stdin`. Explicit stdin reads its complete bounded UTF-8 body (including trailing newlines) before gateway interaction; empty, NUL-containing, malformed UTF-8, and over-limit bodies fail locally. This body stdin is independent from `config secret set --stdin` credential input.

For every syntactically valid leaf invocation whose parsed `--json` flag is true, success and runtime failure write exactly one JSON object followed by one newline to stdout and nothing to stderr. Successful objects retain each command's existing DTO shape. Runtime failures use a shared error object with `code`, `category`, `message`, `retriable`, and optional safe `details`, `progress`, `request_id`, and guidance; doctor gateway failures additionally contain only the completed diagnostic report in `partial_result`. Parse and usage failures remain text on stderr with exit 2.

Without `--json`, each command uses a compact purpose-specific human view: task and Slack reads retain complete multiline text in cards, lists use rows, and mutations display resource state and request IDs. Presentation escapes terminal control characters while retaining printable Unicode; it never changes stored text or JSON output. Color is only enabled for a TTY and is disabled by `NO_COLOR` or `TERM=dumb`.

Stable process exit categories are:

| Code | Meaning |
| --- | --- |
| 0 | success |
| 2 | usage error |
| 3 | not found |
| 4 | conflict or ownership error |
| 5 | unsafe configuration or permissions |
| 6 | gateway unavailable |
| 7 | conclusive external failure |
| 8 | ambiguous outcome |
| 9 | unsupported schema or protocol |

## 10. Release invariants

1. An identifiable Slack envelope is acknowledged only after its admission/deduplication result commits.
2. One admitted Slack source maps to at most one task.
3. Task state and Slack activity remain independent except for the explicit confirmed-reply composition.
4. SQLite is the sole coordination authority.
5. Only one Pi extension connection owns delivery at a time.
6. Persisted delivery requires a visible matching Pi session entry.
7. Possibly inserted work becomes uncertain and is never automatically repeated.
8. Slack intent commits before submission; possibly submitted work becomes ambiguous and is never automatically retried.
9. Durable records contain file metadata and outcomes, not permanent transfer bytes.
10. Purge preserves live work and unresolved evidence.
11. Mosscap never starts or owns Pi.
12. Tokens never enter SQLite, logs, protocol output, or Pi messages.
13. Configuration reload either activates one complete verified snapshot or leaves the previous snapshot active.

The automated Go and extension suites are the repeatable release gate. Future work belongs in issues or scoped proposals, not numbered implementation milestones in this specification.
