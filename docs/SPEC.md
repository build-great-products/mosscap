# Mosscap v1 product contract

**Status:** normative contract for the current integrated v1 implementation

“Must”, “must not”, “should”, and “may” express requirements in descending order of strength. [`SYSTEM_MODEL.md`](SYSTEM_MODEL.md) explains the design; the [README](../README.md) and runbooks explain operation.

The former numbered phases were implementation milestones. They are not product modes and are not part of this contract.

## 1. Scope

Mosscap must provide:

- one foreground Go gateway and short-lived CLI clients;
- one Slack Socket Mode workspace connection that admits work from every channel the bot is a member of;
- admission of direct bot mentions and tracked-thread replies from every human member of that workspace;
- local and Slack-origin tasks with explicit state transitions;
- commit-before-ack Slack source deduplication;
- a harness-neutral task event stream (`task watch`) with a durable resume cursor, which is the only way work reaches an agent;
- explicit Slack reads, writes, reactions, and file operations;
- Slack lifecycle reactions that project task state;
- durable Slack intents with honest uncertainty;
- temporary, bounded file transfer without permanent byte storage;
- validated, atomic configuration reload;
- a private versioned local protocol; and
- explicit inspection, retry, and metadata purge operations.

Mosscap does not need to start, supervise, or prompt an agent, support more than one Slack workspace or direct messages, retain historical task text, archive file bytes, infer task state from conversation, retry uncertain external work automatically, provide a browser UI, or guarantee end-to-end exactly-once execution.

## 2. Authority and persistence

The gateway is the only normal SQLite writer. CLI commands other than setup and offline-safe configuration edits must use the gateway.

SQLite is authoritative for coordination state: tasks, source events, effects, files, and request outcomes. Slack is an external persistence domain and cannot share a transaction with SQLite.

The gateway upgrades an intact older ledger atomically on open and refuses a newer or altered one. Upgrades are one-way. Schema version 8 drops the table of the removed Pi delivery feature without archiving it. Schema version 9 adds the nullable `parent_task_id` follow-up link to Slack task sources and thread lookup indexes; existing sources get no parent. Schema version 10 drops the `trusted_users` table of the removed trusted-user feature without archiving it.

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

`config.toml` contains Slack app and bot tokens and all scalar limits/timeouts. It configures no channels and no users. Workspace/team and bot-user IDs are derived and verified through Slack authentication, not configured manually. Tokens must be set through the private config file, redacted from output, and excluded from SQLite, logs, and protocol results.

Keys of removed features must be rejected with guidance to remove them: `delivery_lease`, `leader_heartbeat`, and `leader_expiry` (Pi delivery), `slack_channel_id` and `slack_channel_allowlist` (channel restrictions), and `trusted_user_ids` (trusted users). The gateway must refuse to start, and a reload must fail, while any of them is present, and the error must name every unknown or obsolete key found, not only the first.

`config set` must update one known non-secret value atomically while preserving unrelated comments and formatting. `config secret set` must accept only known Slack token fields and avoid printing their values. Editing stages a candidate; it does not change the running snapshot.

On startup and `gateway reload`, the gateway must validate the complete candidate and verify Slack identity and required scopes for both public and private channels before activation. No channel is verified up front. Activation is all-or-nothing. Failed reload must leave the prior runtime and connection usable.

## 4. Tasks and Slack admission

A task has a unique opaque ID, positive monotonic content version, current text, `open`/`working`/`done` state, timestamps, and a local or Slack source. Previous text is not retained.

Every task also carries a durable change sequence and the kind of its latest change (`created`, `edited`, or `state`). The ledger must assign each creation, content-version change, and state change the next value of one monotonic counter inside the writing transaction, so sequence order equals commit order. Sequence values are never reused, including after purge. No-op edits and same-state transitions must not advance the counter. Only the latest change of each task is retained; it is not an event history.

Creation yields `open`. Explicit transitions may move between all three states; same-state transitions are idempotent. Entering `done` sets `done_at`; leaving it clears `done_at`. State-only changes do not increment content version. For Slack-origin tasks, each committed state change replaces the lifecycle reaction (section 4a); state transitions do not otherwise post or alter Slack content.

An initial Slack message is admitted only when all of these hold:

- it belongs to the authenticated workspace and to a `C...` or `G...` channel (any channel Slack delivers events for, which is every channel the bot is a member of);
- its sender is a human: it has no bot ID, its sender is not the authenticated bot user, and its subtype is empty, `thread_broadcast`, or `file_share`;
- its sender is a member of the authenticated workspace (see below); and
- its current text literally mentions the authenticated bot user.

There is no user or channel allowlist: every member of the workspace may send work.

**External senders.** A sender is not a member when the event names a sender workspace that differs from the authenticated team: the first non-empty of the event's `user_team`, `user_profile.team`, and `team` fields (for an edit, those of the changed message). This is how Slack Connect external users appear in shared channels. Such a message is `ignored_workspace`, whether it mentions the bot or replies in a tracked thread, and gets no reaction. The check uses only the delivered payload; the gateway must not call `users.info` per message. A message whose event names no sender workspace is treated as a member's. A malformed sender workspace ID makes the event `malformed`.

Every task records its channel. The same message timestamp in two channels is two distinct sources.

A new message (not an edit) is also admitted without a mention as a **follow-up** when all of these hold:

- it is a reply (its thread timestamp differs from its message timestamp) in an admissible channel;
- its thread is tracked: an admitted task's source is in that thread, or a confirmed `send` or `file_upload` effect posted the thread's root or a reply in it;
- its sender is a human member of the workspace, as for a mention (any member, not only the thread's original requester); and
- it has text or attached files.

A follow-up is a new task, never a change to an earlier one. Its source records the thread root as `thread_ts` and `parent_task_id`: the task admitted from the root message, else the related task of a confirmed root `send`, else the earliest task in the thread, else the related task of the earliest confirmed post in the thread, else none. `parent_task_id` is provenance only: it is not a foreign key and may name a purged task. Any admitted reply, mentioned or not, gets the same link. A message that neither mentions the bot nor is a follow-up is `ignored_unmentioned` and gets no reaction. Messages from the bot, or with a subtype other than `thread_broadcast` or `file_share`, are `ignored_bot`. An attachment-only follow-up has the task text `(attachment without message text)`. Admission, deduplication, commit-before-ack, edits, and deletion are otherwise exactly as for mentioned messages.

For each identifiable envelope, the gateway must deduplicate and commit the admission result before acknowledging Slack. A commit failure must not be acknowledged. Mosscap makes no receipt claim while disconnected from Socket Mode.

An admitted message creates at most one task. A meaningful newer source edit replaces current text/attachment metadata and increments version. Stale edits must not roll content backward. Source deletion preserves task text and state while recording deletion metadata. Incoming attachments are metadata-only until explicitly retrieved.

An edit of an admitted source whose sender is not the source's author is ignored and recorded with the result value `ignored_untrusted`; the name is retained from the removed trusted-user feature because the ledger fixes the set of result values. The result `ignored_channel` is no longer produced; it remains only on events recorded by releases that had a channel allowlist, so their redelivery replays it. No admission outcome other than a new task adds a reaction.

## 4a. Lifecycle reactions

A Slack-origin task's source message carries one lifecycle reaction that projects its current state:

| State | Reaction |
| --- | --- |
| `open` | `hourglass_flowing_sand` |
| `working` | `eyes` |
| `done` | none |

Admission prepares the `open` reaction before acknowledgement and submits it afterwards. Each later committed state change prepares, in order, an add of the new state's reaction and then a removal of the prior state's reaction, each under a durable intent keyed by that transition's state event, so a reopened task gets fresh intents. The removal is submitted only after the add is confirmed; a transition into `done` has no add and only removes. Content edits and source deletion do not change the reaction. The replacement is explicitly not atomic. A failed or ambiguous submission remains inspectable and is never automatically retried. Adding a reaction that is already present, or removing one that is absent, is a confirmed outcome because the requested end state holds.

On startup the gateway re-prepares any missing lifecycle intent for the latest transition of each `open` or `working` Slack task before effect recovery runs, so a crash between a transition commit and its projection converges. `done` tasks are not reconciled at startup.

## 5. Task watch

`task watch` is a read-only, long-running client of the `task_changes_v1` capability. It must poll the gateway over the private local protocol and write each selected task change to stdout as exactly one complete line, flushed per line, until interrupted. Stdout must contain only events. Reconnect and other diagnostics go to stderr.

Each event is the task's current snapshot at its latest change sequence. Changes committed between polls (or while no watcher runs) are coalesced: a watcher observes the newest version and state, not every intermediate value.

The default line is:

```text
cursor=SEQ change=created|edited|state task=TASK_ID version=N state=open|working|done source=local|slack channel=CHANNEL_ID thread=THREAD_TS parent=TASK_ID text=ESCAPED
```

`channel` and `thread` are the Slack channel and reply thread root of a Slack task and are empty for a local task. `parent` is the source's `parent_task_id`, empty when absent. `text` is the last field and is never truncated. It escapes backslash as `\\`, newline as `\n`, carriage return as `\r`, tab as `\t`, and every other control character and U+2028/U+2029 as `\uXXXX`; other printable Unicode is retained. With `--json`, each line is one `TaskChangeDTO` object (`change_seq`, `change`, and a `task` with the `TaskDTO` shape including `slack_source` for Slack tasks), and the cursor is `change_seq`.

By default a live event is emitted when a task is created, when its content version changes, or when it enters `open`. `--state-changes` also emits transitions to `working` and `done`. Filtered changes still advance the cursor.

The cursor is the change sequence. `--since CURSOR` resumes after that value. `--cursor-file PATH` reads the cursor at startup if the file exists and atomically replaces it after each processed change, only after that change's line was written; a crash can therefore repeat at most the last line and never skips one. Consumers that need exactly-once handling must deduplicate on `task`, `version`, and `state`. A cursor newer than the ledger head fails with `cursor_ahead` (exit 4). Without a cursor, the watcher first emits every currently `open` task, then streams live changes; `--no-backlog` starts at the current head instead.

When the gateway is unavailable, the watcher must retry with capped exponential backoff, report each failure and the eventual reconnection on stderr, and resume from its in-memory cursor. Typed refusals (unsupported capability, invalid or ahead cursor) end the command with their normal exit category. `--timeout` bounds each dial/handshake and each poll separately. A `--json` fatal error is the shared runtime error object on stdout, which callers distinguish from events by its `code` field.

Watching must not change task state or add Slack reactions. A watcher should claim work explicitly, for example with `task working` (which moves the lifecycle reaction to `eyes`), and complete it with `task reply --done` or `task done`. Several watchers may observe the same tasks; coordination between them is the caller's responsibility.

## 6. Slack operations and effects

Every Slack command targets the active workspace and exactly one channel. The channel is the explicit `--channel`, else the related `--task`'s source channel; otherwise the command fails with `channel_required`. A target that is not a `C`/`G` ID, or a durable effect or source in a workspace other than the active one, fails with `invalid_channel` before any intent is recorded. No operation accepts an alternate workspace. Slack enforces bot membership. Targeted writes preflight the target message in that channel. Each effect records its channel, and recovery and retry use the effect's own channel, which must still be in the active workspace. Clients must see `slack_channels_v1` before sending a channel.

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

Purge requires an absolute RFC 3339 cutoff and exactly one of `--dry-run` or `--confirm`. Dry-run must not mutate state. Confirmed purge may remove eligible finalized history but must preserve open/working tasks, unresolved effects, live request outcomes, configuration, and every record required by retained state.

`gateway status` must distinguish process health from outcomes requiring operator action and report bounded task, Slack, effect, database, and temporary-file state. `doctor` must be non-destructive: it must not send Slack messages, acknowledge events, or transfer bytes.

Logs and diagnostics must exclude credentials, authorization headers, raw Slack envelopes, and full message bodies. Local paths and ordinary diagnostics are not redacted and may appear in agent context.

## 9. Commands and compatibility

The current command families are:

```text
mosscap setup
mosscap config show|set|secret set
mosscap gateway run|reload|status
mosscap doctor
mosscap task create|list|show|context|watch|update|open|working|done|reply
mosscap slack history|thread|send|file|edit|delete|react|unreact   (each accepts --channel; thread and writes accept --task)
mosscap effect list|inspect|retry
mosscap file show|get
mosscap purge
```

SQLite-backed `task list` and `effect list` accept bounded `--limit`/`--cursor` pages (effects also accept composable `--task`). Tasks are oldest-created-first; effects newest-created-first. Resource cursors are full IDs and a purged anchor returns typed `cursor_expired` conflict guidance to restart without `--cursor`. `slack history` and `slack thread` likewise accept `--limit` and `--cursor`; both default to and cap at 50 messages, preserve Slack's native order, and pass the opaque Slack cursor unchanged.

The private Unix-socket protocol uses bounded 32-bit-length-prefixed JSON frames, a versioned handshake, typed command/results, and capability negotiation. The only client kind is `cli`. Unknown incompatible envelopes or enum values must fail safe. Slow clients must be disconnected without discarding durable work. Same-user authentication is not protection from compromise of that OS account.

Every CLI leaf that contacts the gateway accepts a positive shared `--timeout` duration, defaulting to 10 seconds. For `task watch` it applies to each connection attempt and each poll (section 5); for every other leaf its deadline begins immediately before gateway contact and covers dial, hello, calls, and frame I/O; an earlier application/caller deadline wins. Explicit body stdin and local validation occur before this deadline. Cancellation before a Slack external boundary leaves work unsubmitted; after a boundary, the gateway durably records a confirmed, failed, or ambiguous outcome using a non-cancelled commit context. Timeout, cancellation, or disconnect after submission is ambiguous and is never retried automatically.

Cobra help is the complete command-discovery interface. Running the root or a command branch without a leaf prints help and exits successfully; every leaf documents its syntax, flags, examples, and applicable irreversible or duplicate-risk warnings. Help performs no configuration, gateway, or mutation work. Unknown commands and flags print usage for the relevant command and exit 2. Only unambiguous close command or flag misspellings may be suggested; positional values are never rewritten or suggested. Shell-completion and machine-readable help commands are not exposed.

`task context TASK [--thread-limit N]` is a read-only bounded task workflow view. It includes the current durable task/source attachment metadata, latest reply effect, and at most ten newest non-confirmed related effects and their total. For Slack tasks it makes one best-effort live thread-page read (default 20, maximum 100); `thread.status` is `available`, `unavailable`, or `not_applicable`. An unavailable live thread is a successful partial result with a warning; missing tasks remain not-found. It never downloads files or returns live-reply file metadata, file bytes, private URLs, effect body text, or operator reasons.

`task create`, `task update`, `task reply`, `slack send`, and `slack edit` require exactly one text source: `--text`, `--text-file`, or `--stdin`. `--text` is exact literal text and must not decode escapes: a supplied `\\n` is a backslash followed by `n`. `--text-file` and explicit stdin preserve exact multiline UTF-8 bodies; stdin reads its complete bounded body (including trailing newlines) before gateway interaction. Empty, NUL-containing, malformed UTF-8, and over-limit bodies fail locally. This body stdin is independent from `config secret set --stdin` credential input.

For every syntactically valid leaf invocation other than `task watch` whose parsed `--json` flag is true, success and runtime failure write exactly one JSON object followed by one newline to stdout and nothing to stderr. Successful objects retain each command's existing DTO shape. Runtime failures use a shared error object with `code`, `category`, `message`, `retriable`, and optional safe `details`, `progress`, `request_id`, and guidance; doctor gateway failures additionally contain only the completed diagnostic report in `partial_result`. Parse and usage failures remain text on stderr with exit 2.

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
3. Task state and Slack content remain independent except for the explicit confirmed-reply composition; lifecycle reactions only project state.
4. SQLite is the sole coordination authority.
5. Slack intent commits before submission; possibly submitted work becomes ambiguous and is never automatically retried.
6. Durable records contain file metadata and outcomes, not permanent transfer bytes.
7. Purge preserves live work and unresolved evidence.
8. Mosscap never starts or owns an agent.
9. Watching tasks never changes task or Slack state.
10. Tokens never enter SQLite, logs, or protocol output.
11. Configuration reload either activates one complete verified snapshot or leaves the previous snapshot active.

The automated Go suite (`just check`) is the repeatable release gate. Future work belongs in issues or scoped proposals, not numbered implementation milestones in this specification.
