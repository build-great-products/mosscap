# Mosscap System Model

## Product rule

**Keep coordination durable; keep the agent human-owned.**

Mosscap is a durable inbox and Slack interface for an ordinary interactive Pi session. It owns coordination around Pi, but never owns Pi itself.

A person starts Pi, chooses its model and thinking level, selects its working directory, and decides when it stops. Running `/mosscap` opts that process into receiving work. The extension immediately submits delivered work to the model, but normal Pi controls remain available for steering and interruption.

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

- The **gateway** is a foreground, long-running process. It connects to Slack, owns the SQLite ledger, admits work, manages delivery leadership and leases, executes recorded Slack operations, and serves the local protocol.
- The **CLI** performs setup and sends explicit requests to the gateway. During normal operation it does not write SQLite directly.

One executable simplifies installation and version compatibility, while internal package boundaries keep the gateway and client independent.

### SQLite ledger

SQLite is the sole authority for:

- tasks and their current versions;
- Slack source-event deduplication;
- the active delivery leader;
- delivery attempts and exceptional outcomes;
- outbound Slack operation intents and results;
- file metadata; and
- mutation request deduplication.

Logs, Slack reactions, Pi conversation text, and terminal output are never authoritative task state.

### TypeScript Pi extension

The extension integrates only with the Pi process in which it is loaded. It:

- provides `/mosscap`, `/mosscap status`, and `/mosscap off`;
- claims the single active delivery-leader generation;
- maintains heartbeats while opted in;
- inserts leased work as visible custom Pi messages;
- triggers model processing;
- records delivery keys in Pi session entries; and
- reports insertion outcomes to the gateway.

It does not connect to Slack, write SQLite, infer task completion from model output, or manage the Pi process.

### Slack

Slack is both a source of requests and an explicit communication surface. Mosscap is configured for exactly one workspace and one channel. It admits work only from explicitly trusted users who directly mention the bot.

The CLI can read history and threads and can send, edit, delete, react, unreact, and transfer files. Every operation implicitly targets the configured channel.

### Local protocol

A private, versioned Unix-socket protocol connects the gateway, CLI, and extension. It carries typed requests, responses, and server notifications. Request IDs prevent duplicate local mutations; generations fence stale extension connections.

## Task model

A task is a generic todo item:

```text
open → working → done
  ▲       │        │
  └───────┴────────┘
```

Returning a task to `open` covers abandoning active work and reopening completed work.

Each task has:

- an opaque ID;
- a monotonic version;
- one current text value;
- `open`, `working`, or `done` state;
- timestamps; and
- either a local source or a reference to its originating Slack message.

Every eligible Slack mention creates one task automatically. Local callers can create the same task type. A Slack source reference lets Pi find the original message or thread, but task state and Slack activity remain independent:

- replying does not complete a task;
- completing a task does not post a reply;
- reactions do not represent task state.

An edit to the originating Slack message replaces the task's current text and increments its version. Mosscap does not retain previous text. When Pi is connected, the live update may include a best-effort diff; recovery requires only the latest complete text.

## Request flow

### 1. Admission

A trusted user directly mentions the bot in the configured channel. The gateway validates the workspace, channel, immutable user ID, sender type, and mention.

In one SQLite transaction it deduplicates the source event and creates the task. Only after commit does it acknowledge Slack. The task therefore survives even when Pi is absent.

### 2. Delivery

A human-started Pi process runs `/mosscap` and claims delivery leadership. The gateway leases eligible task versions to that extension.

The extension reports insertion start, writes a visible custom message containing the task and delivery key into Pi's persisted JSONL session, and starts model processing. If Pi is busy, the message waits as a follow-up until the current run settles.

After observing the persisted session entry, the extension acknowledges the lease. The gateway marks the delivery `persisted`.

### 3. Work

Pi and the human use ordinary Pi conversation and tools. Task state changes only through explicit Mosscap operations; free-form model claims such as “finished” do not update SQLite.

### 4. Slack activity

A CLI request to send or modify Slack first creates a durable effect intent. The gateway also creates a fixed `no_entry` reaction intent for every configured-channel event rejected because its sender is untrusted. Every mutation uses the same durable effect lifecycle before the gateway performs the API call and records its result. Slack activity does not implicitly change task state.

### 5. Completion

Pi or the human explicitly changes the task to `done`. Done tasks remain inspectable until an operator runs `mosscap purge` with an explicit cutoff and confirmation.

## Files

Mosscap keeps durable file identity and transfer outcomes, not durable file bytes.

For an admitted Slack attachment, the task contains file metadata and a retrieval command. The gateway downloads bytes only when `mosscap file get` is invoked. It either places them at an explicit user destination or returns a private temporary path.

Outgoing commands read and validate a local regular file, compute its size and hash, and upload it without retaining a permanent copy. Temporary bytes expire independently of durable task and effect metadata.

## Two persistence domains

Delivery crosses two stores:

1. SQLite says what work exists and what delivery was attempted.
2. Pi JSONL says what a particular session has seen.

They cannot share a transaction. Mosscap therefore uses stable delivery keys and explicit states:

```text
pending → leased → inserting → persisted
                       ├──────→ uncertain
                       └──────→ rejected
```

A lease that expires before insertion starts can safely return to `pending`. Once insertion may have started, failure becomes `uncertain` and requires inspection before requeue.

Slack operations have the analogous boundary:

```text
prepared → submitted → confirmed
                       ├──────→ failed
                       └──────→ ambiguous
```

A possibly submitted operation is never retried automatically.

## Leadership and takeover

Only one extension generation receives deliveries. A confirmed takeover increments the generation, invalidates the old connection's leases, and rejects stale acknowledgements.

This is delivery fencing, not process control. It cannot stop an old Pi process or a tool call already running there. Takeover therefore reports that prior work may remain in flight.

## Recovery

- If the gateway exits, committed SQLite state remains.
- If Slack reconnects and redelivers an event, source identity deduplicates it.
- If Pi exits before insertion starts, the lease eventually becomes pending again.
- If Pi exits after insertion starts, the delivery becomes uncertain.
- If a Slack request times out after submission, its effect becomes ambiguous.
- When an opted-in Pi reconnects, the gateway reconciles delivery keys and presents current open work plus exceptional outcomes.

Recovery follows durable state rather than terminal history or inferred model behavior.

## Explicitly outside Mosscap

Mosscap does not:

- start, stop, supervise, select, or reset Pi;
- run hidden or headless agents;
- infer task transitions from conversation text;
- automatically retry ambiguous external actions;
- keep historical task text;
- permanently archive transferred file bytes; or
- automatically delete durable metadata.

A feature belongs in the core when it strengthens durable work, explicit authority, visible human control, or honest recovery.
