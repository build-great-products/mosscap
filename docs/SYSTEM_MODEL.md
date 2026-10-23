# Mosscap system model

## Product rule

**Keep coordination durable; keep the agent separate.**

Mosscap is a durable inbox and Slack interface for a long-running agent session, such as Claude Code. The agent runs on its own: a person or a supervisor starts it, chooses its model and working directory, and decides when it stops. The agent receives work by following `mosscap task watch` and acts through ordinary CLI commands. Mosscap never starts, prompts, or supervises it.

## Components and authority

```text
Slack ───────┐
             ├──> foreground gateway ───> SQLite ledger
CLI ─────────┘              │
                            │ private Unix-socket protocol
                            ▼
                     mosscap task watch
                            │
                            ▼
                      agent session
```

### Go executable

The `mosscap` executable has two roles:

- The foreground **gateway** connects to Slack, owns SQLite, admits work, executes recorded Slack operations, manages temporary transfers, and serves the local protocol.
- Short-lived **CLI** commands perform setup or ask the gateway to read and mutate state. Normal CLI operation never writes SQLite directly. `task watch` is the one long-running CLI command.

### SQLite ledger

SQLite is authoritative for tasks, Slack source deduplication, Slack effect intents and outcomes, file metadata, trusted users, and mutation request deduplication. Logs, reactions, agent conversation text, and terminal output are projections, not task state. Human terminal renderers escape control characters only at presentation time; stored and JSON-protocol text remains exact.

### Task watchers

`mosscap task watch` is how work reaches an agent, for example Claude Code running it under a background monitor. It is an ordinary CLI client that polls the `task.changes` read over the local protocol and prints one line per selected task change.

SQLite triggers stamp every task creation, content-version change, and state change with the next value of a durable counter in the same transaction, so the counter orders changes exactly as they committed and is never reused after purge. The watcher's cursor is that counter. Because only each task's latest change is kept, a watcher that falls behind sees coalesced current snapshots rather than a history.

A watcher writes nothing: printing a line proves nothing about what an agent did with it. Agents make their handling visible through ordinary explicit commands (`task working`, `task reply --done`).

### Slack

Mosscap is configured for exactly one channel. Workspace and bot identity are derived from Slack authentication. Work is admitted only when a non-bot sender is in the live trusted-user set and directly mentions the bot in the configured channel.

Slack reads and writes are explicit. Each write records a durable intent before network submission. Slack content and task state are independent, except for the explicit `task reply --done` composition, which marks done only after a confirmed reply.

Lifecycle reactions are the one automatic Slack write. They project task state onto the source message: `hourglass_flowing_sand` while `open`, `eyes` while `working`, nothing once `done`. Each transition adds the new marker before removing the old one, through the same durable effect path as any other write.

### Local protocol

A private, versioned Unix-socket protocol connects the gateway and CLI. Request IDs deduplicate local mutations. Gateway-calling CLI leaves use a positive shared `--timeout` (10 seconds by default) beginning immediately before dial; it covers dial, hello, calls, and framed I/O, while caller cancellation and an earlier caller deadline take precedence. Body stdin is deliberately consumed before this budget. Owner-only filesystem permissions and peer credentials protect the endpoint from other OS users; they do not protect against another process running as the same user.

## Task and request flow

A task has one current text value, a monotonic content version, and one of three states:

```text
open → working → done
  ▲       │        │
  └───────┴────────┘
```

State transitions do not post Slack content; they only move the lifecycle reaction. A meaningful edit to an admitted Slack source replaces current task text and increments its version; old text is not retained. Source deletion preserves the task text and state.

An eligible Slack envelope follows this path:

1. The gateway validates workspace, channel, sender, trust, and direct mention.
2. In SQLite it deduplicates the source and creates or updates the task, and prepares the queued reaction for a new task.
3. Only after commit does it acknowledge the Slack envelope, then submits the reaction.
4. Every `task watch` client sees the change on its next poll.
5. The agent claims the task with `task working`, reads `task context`, and replies with `task reply --done`.

Tasks remain durable while no watcher runs. Slack Socket Mode does not guarantee indefinite retention while the gateway itself is offline.

`task context` keeps this authority boundary explicit: SQLite supplies current task, admitted source attachment, and effect evidence; a single bounded Slack thread page is live and timestamped separately. If that live read is unavailable, durable context still succeeds with an unavailable component status and warning. Live thread-reply attachments are neither admitted task attachments nor exposed by this view.

## Cross-system uncertainty

SQLite and Slack cannot share a transaction:

```text
prepared → submitted → confirmed
                       ├────→ failed
                       └────→ ambiguous
```

A conclusive rejection is `failed`. A timeout, cancellation, or disconnect after submission is `ambiguous`; Mosscap never retries it automatically. The terminal post-submission commit uses a cancellation-detached context, so loss of the client cannot erase this evidence. Explicit retry creates a linked new effect and, after ambiguity, requires duplicate-risk confirmation.

## Files and retention

Mosscap stores durable file identity, metadata, hashes when known, and transfer outcomes, not durable file bytes. Incoming bytes are fetched only by `file get`. Outgoing bytes are validated and uploaded from regular files. Temporary bytes live under the private state root and may expire independently of durable metadata.

Durable metadata is not deleted automatically. `mosscap purge` requires an absolute cutoff and exactly one of dry-run or confirmation, and preserves live tasks and unresolved effect evidence.

## Security boundary

A trusted Slack user can direct a fully capable local agent. Workspace, channel, immutable user ID, bot exclusion, and direct mention are authorization checks, not a sandbox. Slack text, task text, filenames, and file bytes remain untrusted input.

Tokens and authorization headers must not enter SQLite, protocol results, or logs. Local paths and ordinary diagnostics are not redacted and may enter agent context. Operators choose a safe working directory and model with that disclosure in mind.

## Non-goals

Mosscap does not start, prompt, or supervise an agent, run headless agent pools, infer task state from conversation, retain historical task text, automatically retry uncertain external actions, permanently archive transferred bytes, support multiple Slack destinations, or claim end-to-end exactly-once execution.
