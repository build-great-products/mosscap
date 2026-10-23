# Mosscap system model

## Product rule

**Keep coordination durable; keep the agent human-owned.**

Mosscap is a durable inbox and Slack interface for an ordinary interactive Pi session. A person starts Pi, chooses its model and working directory, and decides when it stops. Running `/mosscap` opts that persisted TUI session into delivery; `/mosscap off` opts out without changing tasks or stopping Pi.

## Components and authority

```text
Slack ───────┐
             ├──> foreground gateway ───> SQLite ledger
CLI ─────────┘              │
                            │ private Unix-socket protocol
                            ▼
                       Pi extension
                            │
                            ▼
                  human-owned Pi session
```

### Go executable

The `mosscap` executable has two roles:

- The foreground **gateway** connects to Slack, owns SQLite, admits work, coordinates Pi delivery, executes recorded Slack operations, manages temporary transfers, and serves the local protocol.
- Short-lived **CLI** commands perform setup or ask the gateway to read and mutate state. Normal CLI operation never writes SQLite directly.

### SQLite ledger

SQLite is authoritative for tasks, Slack source deduplication, delivery attempts, Slack effect intents and outcomes, file metadata, trusted users, and mutation request deduplication. Logs, reactions, Pi conversation text, and terminal output are projections, not task state. Human terminal renderers escape control characters only at presentation time; stored and JSON-protocol text remains exact.

### Pi extension

The TypeScript extension integrates only with the Pi process in which it is loaded. It:

- requires explicit `/mosscap` opt-in in a persisted interactive TUI session;
- claims the gateway's single active delivery slot;
- reconciles delivery keys already present in that Pi session;
- inserts work as visible custom messages and triggers a model turn;
- records delivery keys in Pi session entries; and
- reports inserting, persisted, or rejected outcomes.

It does not connect to Slack, write SQLite, infer task completion from prose, add model-callable tools, or manage Pi's process. A second extension cannot silently replace the active one; the current session must opt out or disconnect first.

### Task watchers

`mosscap task watch` is a harness-neutral consumer for agents that cannot load the Pi extension, such as Claude Code running it under a background monitor. It is an ordinary CLI client that polls the `task.changes` read over the local protocol and prints one line per selected task change.

SQLite triggers stamp every task creation, content-version change, and state change with the next value of a durable counter in the same transaction, so the counter orders changes exactly as they committed and is never reused after purge. The watcher's cursor is that counter. Because only each task's latest change is kept, a watcher that falls behind sees coalesced current snapshots rather than a history.

A watcher owns no delivery slot and writes no delivery evidence: printing a line proves nothing about what an agent did with it. Agents make their handling visible through ordinary explicit commands (`task working`, `task reply --done`).

### Slack

Mosscap is configured for exactly one channel. Workspace and bot identity are derived from Slack authentication. Work is admitted only when a non-bot sender is in the live trusted-user set and directly mentions the bot in the configured channel.

Slack reads and writes are explicit. Each write records a durable intent before network submission. Slack activity and task state are independent, except for the explicit `task reply --done` composition, which marks done only after a confirmed reply.

### Local protocol

A private, versioned Unix-socket protocol connects the gateway, CLI, and extension. Request IDs deduplicate local mutations. Gateway-calling CLI leaves use a positive shared `--timeout` (10 seconds by default) beginning immediately before dial; it covers dial, hello, calls, and framed I/O, while caller cancellation and an earlier caller deadline take precedence. Body stdin is deliberately consumed before this budget. Owner-only filesystem permissions and peer credentials protect the endpoint from other OS users; they do not protect against another process running as the same user.

## Task and request flow

A task has one current text value, a monotonic content version, and one of three states:

```text
open → working → done
  ▲       │        │
  └───────┴────────┘
```

State transitions do not cause Slack activity. A meaningful edit to an admitted Slack source replaces current task text and increments its version; old text is not retained. Source deletion preserves the task text and state.

An eligible Slack envelope follows this path:

1. The gateway validates workspace, channel, sender, trust, and direct mention.
2. In SQLite it deduplicates the source and creates or updates the task.
3. Only after commit does it acknowledge the Slack envelope.
4. An opted-in extension polls eligible task versions, inserts one visible message, and triggers a turn; independently, any `task watch` client sees the change on its next poll.
5. The extension reports insertion start and then confirms that the delivery key is present in persisted Pi session entries. A watcher reports nothing back.
6. Pi or a human uses ordinary CLI commands for explicit replies and task transitions.

Tasks remain durable while Pi is absent or opted out. Slack Socket Mode does not guarantee indefinite retention while the gateway itself is offline.

`task context` keeps this authority boundary explicit: SQLite supplies current task, admitted source attachment, effect, and delivery evidence; a single bounded Slack thread page is live and timestamped separately. If that live read is unavailable, durable context still succeeds with an unavailable component status and warning. Live thread-reply attachments are neither admitted task attachments nor exposed by this view.

## Cross-system uncertainty

SQLite and Pi JSONL cannot share a transaction:

```text
pending → inserting → persisted
              ├────→ uncertain
              └────→ rejected
```

If insertion may have started but persistence cannot be confirmed, the attempt becomes `uncertain` and is not automatically repeated. Explicit requeue preserves the previous attempt and warns about duplicate insertion.

SQLite and Slack also cannot share a transaction:

```text
prepared → submitted → confirmed
                       ├────→ failed
                       └────→ ambiguous
```

A conclusive rejection is `failed`. A timeout, cancellation, or disconnect after submission is `ambiguous`; Mosscap never retries it automatically. The terminal post-submission commit uses a cancellation-detached context, so loss of the client cannot erase this evidence. Explicit retry creates a linked new effect and, after ambiguity, requires duplicate-risk confirmation.

## Files and retention

Mosscap stores durable file identity, metadata, hashes when known, and transfer outcomes—not durable file bytes. Incoming bytes are fetched only by `file get`. Outgoing bytes are validated and uploaded from regular files. Temporary bytes live under the private state root and may expire independently of durable metadata.

Durable metadata is not deleted automatically. `mosscap purge` requires an absolute cutoff and exactly one of dry-run or confirmation, and preserves live tasks and unresolved delivery/effect evidence.

## Security boundary

A trusted Slack user can trigger a fully capable local Pi while `/mosscap` is active. Workspace, channel, immutable user ID, bot exclusion, and direct mention are authorization checks, not a sandbox. Slack text, task text, filenames, and file bytes remain untrusted input.

Tokens and authorization headers must not enter SQLite, protocol results, logs, or Pi messages. Local paths and ordinary diagnostics are not redacted and may enter Pi JSONL and model context. Operators choose a safe working directory and model with that disclosure in mind.

## Non-goals

Mosscap does not start or supervise Pi, run headless agent pools, infer task state from conversation, retain historical task text, automatically retry uncertain external actions, permanently archive transferred bytes, support multiple Slack destinations, or claim end-to-end exactly-once execution.
