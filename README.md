# Mosscap

Mosscap connects a trusted Slack conversation to a **human-observable Pi session**.

It is a clean-slate successor to `pi-tag-slack`. Rather than owning a hidden Pi RPC child, Mosscap lets a person run normal interactive Pi inside a named Zellij session and opt that session into Slack work with `/mosscap`.

```text
Slack Socket Mode
       │
       ▼
mosscap gateway ─── SQLite task/effect ledger
       │             durable source of truth
       │ local authenticated protocol
       ▼
Pi extension (/mosscap)
       │
       ▼
interactive Pi TUI in Zellij
```

## Goals

- Never lose an eligible Slack event after the local gateway commits it.
- Keep the active Pi session visible and attachable by a human.
- Deliver Slack work directly into a real Pi session through an extension.
- Keep Slack durability independent from Pi, SSH, and Zellij lifetimes.
- Make all authority, delivery, and recovery boundaries explicit.

## Non-goals

- Mosscap does not spawn, select models for, reset, or own Pi sessions.
- Mosscap does not install or manage a systemd service.
- Zellij is not a work queue or a source of truth; it is the human observation surface.
- The first version routes work to one active Pi consumer, not an implicit pool of agents.

## Operating model

A human starts and owns the interactive session:

```bash
zellij --session mosscap
pi
/mosscap
```

The `/mosscap` extension command claims the current persisted Pi session as the one Slack-work consumer. The session remains an ordinary Pi TUI: the human can observe, steer, interrupt, resume, compact, or exit it normally.

Later, the human can discover or observe it with:

```bash
zellij list-sessions --short
zellij attach mosscap
zellij watch mosscap
```

`zellij watch` is read-only observation. Mosscap must not scrape pane output, inject keys, or infer delivery correctness from terminal state.

## Components

### Gateway

`mosscap gateway run` is a foreground process which owns:

- Slack Socket Mode connection and event admission;
- SQLite configuration, task/event/effect ledger, reactions, and leases;
- the local private control protocol used by CLI commands and the extension;
- Slack reply/reaction reconciliation and bounded retries.

The gateway is deliberately independent of Pi and Zellij. Operators choose their own supervisor (a terminal, systemd, launchd, container, etc.); Mosscap documents recipes but does not install one.

### Pi extension

The same package ships a Pi extension exposing `/mosscap` and `/mosscap off`.

When enabled, it:

1. handshakes with the local gateway using Pi session identity, cwd, extension version, and Zellij session name when available;
2. acquires a fenced single-consumer lease;
3. receives leased inbox revisions;
4. injects them using `pi.sendMessage()` as visible `mosscap.slack` custom messages with stable delivery keys;
5. acknowledges persisted deliveries to the gateway; and
6. reconnects and reconciles persisted delivery keys after restart or reload.

The extension renders Slack work distinctly in Pi and shows connection/binding/pending-work status in a compact widget or footer. UI enhancements must be guarded for Pi TUI mode; delivery never depends on a modal dialog.

### CLI

The CLI has explicit process and state boundaries:

```text
mosscap setup|config show|config set
mosscap gateway run|status
mosscap doctor

mosscap task create|list|show|update|working|respond|resolve|reopen|delete
mosscap slack history|thread|send|edit|delete|react|unreact|file
mosscap trust list|add|remove

mosscap zellij list|attach|watch
```

`setup` creates no service and starts no Pi. It creates or verifies the private state/config roots, initializes SQLite and the local endpoint, then requires explicit Slack credentials, one channel ID, and explicit trusted Slack user IDs before `gateway run` will admit work. `config show` redacts secrets; `config set` validates values but never treats a display name, a wildcard, or channel membership as trust.

By default configuration lives under `$XDG_CONFIG_HOME/mosscap`, mutable state (including SQLite) under `$XDG_STATE_HOME/mosscap`, and the same-user Unix control socket under that state root. Each root is owner-only (`0700`), secret/config and database files are owner-readable only (`0600`), and the socket directory is never shared or placed in `/tmp`. An explicit path override must be absolute, private, and owned by the invoking user; setup and doctor reject unsafe ownership or permissions. The endpoint is a local transport convenience, not an authorization boundary.

State-changing CLI commands use that private control protocol so the gateway remains SQLite's sole runtime writer. Offline/read-only diagnostics may inspect validated local configuration.

There are intentionally no `daemon install`, `daemon start`, Pi model, Pi thinking, or Pi session-reset commands. Those belong to the operator and Pi.

## Durable Slack flow

The SQLite task ledger is authoritative. Pi transcript entries are delivery receipts, not a task database.

```text
Slack event
  → validate channel, trusted user, mention, and event identity
  → transactionally persist event and task revision
  → acknowledge Slack envelope
  → lease revision to active Pi binding
  → Pi persists custom message with delivery key
  → extension acknowledges delivery key
  → Pi uses normal durable task commands to work/respond/resolve
```

A task is a durable, revisioned record, whether it originated in Slack, a schedule, or `task create`. `update`, `working`, `respond`, `resolve`, `reopen`, and `delete` append or apply validated transitions through the gateway; delete is a tombstone, never erasure of the event/effect history. Restart recovery lists nonterminal tasks, restores pending effects and expired leases, and lets a newly bound Pi receive an open-work summary. A Slack-origin task retains its source event/thread identity; a local task has no implied Slack destination until an explicit response/effect supplies one.

Every outbound Slack operation—send, edit, delete, react/unreact, and file upload—has a durable effect row with an idempotency key, target identity, request fingerprint, attempt history, and outcome of `pending`, `confirmed`, `failed`, or `ambiguous`. The gateway records the intent before calling Slack and records the returned message/file/reaction identity only after confirmation. A timeout or disconnect after submission is `ambiguous`: it is neither retried blindly nor treated as complete; reconciliation searches the target/history where Slack permits it, then reports a human-actionable unresolved effect. Reactions are best-effort projections of task state, not source-of-truth transitions. Slack replies resolve work only after their effect is confirmed. File size/type/download and upload policy is validated before an effect is created.

### Delivery and deduplication

Inbox lifecycle and delivery lifecycle are separate:

```text
task:     open → resolved
delivery: pending → leased → acknowledged
```

A lease includes task revision, delivery key, Pi session ID, fence token, and expiry. If Pi disconnects before acknowledgement, the lease becomes pending after expiry.

Pi persists the injected `mosscap.slack` custom message in its JSONL session. On reconnect, the extension scans that session for delivery keys and re-acknowledges them before asking for pending work. This prevents normal restart duplicates for the same Pi session.

No transaction can atomically commit both SQLite and Pi JSONL. The system therefore provides durable, idempotent delivery rather than claiming mathematical exactly-once behavior. A fresh Pi session can intentionally receive open work again; prompts and tools must consult the durable task ledger.

## Scheduled work

Scheduling is a gateway capability: scheduled work is materialized into the same durable task ledger that backs recovery, then delivered through the active Pi binding like other work. It is never an in-memory Pi timer.

```text
mosscap schedule add --title "Follow up" --at 2026-08-01T09:00:00+12:00 --instructions "…"
mosscap schedule add --title "Weekly review" --cron "0 9 * * 1" --timezone Pacific/Auckland --instructions "…"
mosscap schedule list|show|enable|disable|remove
```

A schedule is exactly one of:

- **one-off:** an ISO-8601 time with an explicit UTC offset, normalized and persisted as UTC;
- **recurring:** a five-field cron expression evaluated in a required IANA timezone.

The gateway transactionally materializes due schedules into open task occurrences with stable occurrence keys, then advances the schedule in the same transaction. A one-off schedule disables itself after materialization. A recurring schedule advances to its first future occurrence. The extension receives the resulting durable task, not a timer callback.

### Downtime and safety

- A scheduler tick and startup recovery materialize due work under the gateway's single-writer coordinator.
- SQLite uniqueness prevents duplicate occurrences across restart or concurrent attempts.
- Missed recurring runs are coalesced into one recovery task carrying the first/last missed instant and count; Mosscap does not surprise Pi with a burst of stale prompts.
- Disabled schedules have no next run and do not accumulate missed work. Enabling recomputes from now.
- A committed task remains open if Pi delivery fails, so recovery can safely redeliver it without recreating it.
- Completed schedule occurrences never reopen automatically. Schedule removal is blocked while durable occurrence history requires it.

This keeps a useful human workflow simple: ask Pi to add a reminder or recurring review, inspect/disable it from the CLI, and trust the gateway to retain it while Pi/Zellij is offline.

## Failure and recovery

| Failure | Result | Recovery |
| --- | --- | --- |
| Pi exits or SSH disconnects | Gateway continues admitting Slack work | Resume Pi or start a new Pi session and run `/mosscap` |
| Zellij exits | Pi may exit; committed task work remains | Restore Zellij/Pi, reconnect the extension |
| Extension dies mid-delivery | Lease expires unless the persisted key is reconciled | Reconnect; re-ack persisted keys, retry pending work |
| Gateway restarts | SQLite ledger remains authoritative; no active lease | External supervisor restarts gateway; extension reconnects |
| Slack disconnects | Existing work is safe; new delivery depends on Slack retry/reconnect | Reconnect Socket Mode and deduplicate by Slack event ID |
| Another Pi session claims work | Claim is rejected unless explicit takeover is chosen | Show both Pi/Zellij identities and fence the old owner |
| Pi fork/reset | New identity never silently inherits a live claim | Run `/mosscap` explicitly and receive an open-work recovery summary |

“Never missed” means that once the gateway has received and committed an eligible event, Pi/Zellij failure cannot lose it. It does not mean the gateway can receive events while it is indefinitely offline.

## Health and acceptance contract

`mosscap gateway status` is healthy only when it can open the configured private SQLite state, its schema is current, the private endpoint is listening with safe permissions, and it reports Slack Socket Mode as connected or a bounded reconnecting state. It reports the active binding/fence, counts of open tasks, pending leases, and pending/ambiguous Slack effects; it must not claim delivery health merely because the process exists.

The extension's visible status is healthy only after protocol/version negotiation, a valid lease, and reconciliation of its persisted delivery keys. It distinguishes disconnected, waiting-for-lease, reconciling, and ready, and shows pending work/effect warnings without blocking Pi. `/mosscap off` releases the binding but leaves tasks and effects durable.

`mosscap doctor` exits successfully only if configuration is complete and private, SQLite opens and migrates cleanly, the gateway endpoint is reachable and protocol-compatible, Slack credentials/channel/trust configuration pass a non-destructive check, and any enabled extension binding is version-compatible. It exits nonzero with a specific failed check and repair command for missing trust/config, unsafe paths, stale schema, unreachable gateway, Slack/auth failure, lease conflict, or unresolved ambiguous effects. Doctor never sends Slack messages or changes task state.

A release gate is satisfied only when tests demonstrate: commit-before-ack event recovery; task CRUD/tombstone and restart recovery; lease expiry and Pi JSONL key reconciliation; each Slack effect's confirmed and ambiguous path; and the status/doctor distinctions above. Live Slack checks remain an explicit later gate, not a substitute for deterministic fake-transport tests.

## State ownership

| State | Owner |
| --- | --- |
| Slack credentials, configured channel, trusted users | Gateway private config / SQLite |
| Task records/revisions, Slack event ledger, effect outcomes, reactions | Gateway SQLite |
| Consumer leases and fencing | Gateway SQLite/runtime |
| Conversation, model, thinking level, injected delivery keys | Pi JSONL session |
| Extension enabled/disabled checkpoints | Pi custom entries |
| Terminal view and attach/watch lifecycle | Zellij |

## Security model

- Admit only configured-channel messages from explicitly trusted users that mention the bot.
- Persist Slack event identity before acknowledging it.
- Use private filesystem permissions and a same-user local control endpoint.
- Treat trusted Slack users as capable of influencing a fully capable local agent.
- Do not treat same-UID sockets, Zellij panes, or extension code as privilege boundaries.
- Bound attachment download/upload size, type, and outcome handling.

## Delivery phases

1. **Foundation** — owner-private setup/config/trust contract; strict SQLite task/event/effect ledger; Slack admission/deduplication; foreground gateway and local control protocol. **Gate:** unsafe/missing config is rejected, and commit-before-ack plus task CRUD/tombstone/restart tests pass.
2. **Pi bridge** — `/mosscap`, fenced binding, visible custom-message delivery, reconnect/key reconciliation, and extension health states. **Gate:** fake-gateway lifecycle tests prove lease expiry, no normal-restart duplicate, and recovery into a new explicit binding.
3. **Outbound work and scheduling** — task commands; one-off/recurring schedule CRUD/materialization; send/edit/delete/react/unreact/file effects and reconciliation. **Gate:** every effect persists intent before I/O and has confirmed, failed, and ambiguous-outcome tests; schedule downtime behavior passes.
4. **Operational UX** — concrete gateway status/doctor, task/schedule visibility, Zellij list/attach/watch wrappers, and supervisor-neutral deployment recipes. **Gate:** doctor has non-mutating pass/fail coverage for paths, config/trust, database, endpoint, Slack, binding/version, and ambiguous effects.
5. **Later hardening and convenience** — migrations/backups/restore drills, lease/scheduler chaos and live Slack tests, version negotiation/security/release review; then optional richer task search/filter/export, attachment preview/download helpers, notification polish, and additional supervisor recipes. These conveniences must not weaken the private-path, durable-effect, or human-owned Pi contracts.

## Open product decisions

Before implementation, decide:

1. **Takeover:** should a second Pi session require an interactive explicit takeover confirmation, or should it always be rejected until the current lease expires?
2. **Gateway operation:** which supervisor recipes should the documentation prioritize while keeping the product supervisor-neutral?
3. **Routing:** is a single active Pi consumer sufficient for v1, or should workspaces/channels be a first-class future route key?
4. **Catch-up:** should missed recurring runs be coalesced by default (recommended), or is there a product need to replay every missed occurrence?

## Relationship to pi-tag-slack

Mosscap keeps the durable Slack boundary from `pi-tag-slack`—Socket Mode admission, SQLite-first acknowledgement, inbox revisions, explicit outbound effects, and recoverable ambiguity—while replacing its daemon-owned headless Pi RPC process with a user-owned interactive Pi extension in Zellij.
