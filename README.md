# Mosscap

Mosscap connects a trusted Slack conversation to a **human-observable Pi session**.

It is a clean-slate successor to `pi-tag-slack`. Rather than owning a hidden Pi RPC child, Mosscap lets a person run normal interactive Pi inside a named Zellij session and opt that session into Slack work with `/mosscap`.

```text
Slack Socket Mode
       │
       ▼
mosscap gateway ─── SQLite inbox/outbox
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
- SQLite configuration, inbox, event ledger, outbox, reactions, and leases;
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
mosscap setup
mosscap gateway run|status
mosscap doctor

mosscap inbox list|show|working|respond|resolve
mosscap slack history|thread|send|file
mosscap trust list|add|remove

mosscap zellij list|attach|watch
```

State-changing CLI commands use the gateway's private control protocol so the gateway remains SQLite's sole runtime writer. Offline/read-only diagnostics may inspect validated local configuration.

There are intentionally no `daemon install`, `daemon start`, Pi model, Pi thinking, or Pi session-reset commands. Those belong to the operator and Pi.

## Durable Slack flow

The SQLite inbox is authoritative. Pi transcript entries are delivery receipts, not an inbox database.

```text
Slack event
  → validate channel, trusted user, mention, and event identity
  → transactionally persist event and inbox revision
  → acknowledge Slack envelope
  → lease revision to active Pi binding
  → Pi persists custom message with delivery key
  → extension acknowledges delivery key
  → Pi uses normal durable inbox commands to work/respond/resolve
```

Slack reactions are best-effort projections of inbox state, not source-of-truth transitions. Slack replies resolve inbox work only after their API outcome is confirmed. Ambiguous network outcomes remain explicit for recovery instead of being assumed successful.

### Delivery and deduplication

Inbox lifecycle and delivery lifecycle are separate:

```text
inbox:    open → resolved
delivery: pending → leased → acknowledged
```

A lease includes inbox revision, delivery key, Pi session ID, fence token, and expiry. If Pi disconnects before acknowledgement, the lease becomes pending after expiry.

Pi persists the injected `mosscap.slack` custom message in its JSONL session. On reconnect, the extension scans that session for delivery keys and re-acknowledges them before asking for pending work. This prevents normal restart duplicates for the same Pi session.

No transaction can atomically commit both SQLite and Pi JSONL. The system therefore provides durable, idempotent delivery rather than claiming mathematical exactly-once behavior. A fresh Pi session can intentionally receive open work again; prompts and tools must consult the durable inbox.

## Failure and recovery

| Failure | Result | Recovery |
| --- | --- | --- |
| Pi exits or SSH disconnects | Gateway continues admitting Slack work | Resume Pi or start a new Pi session and run `/mosscap` |
| Zellij exits | Pi may exit; committed inbox work remains | Restore Zellij/Pi, reconnect the extension |
| Extension dies mid-delivery | Lease expires unless the persisted key is reconciled | Reconnect; re-ack persisted keys, retry pending work |
| Gateway restarts | SQLite ledger remains authoritative; no active lease | External supervisor restarts gateway; extension reconnects |
| Slack disconnects | Existing work is safe; new delivery depends on Slack retry/reconnect | Reconnect Socket Mode and deduplicate by Slack event ID |
| Another Pi session claims work | Claim is rejected unless explicit takeover is chosen | Show both Pi/Zellij identities and fence the old owner |
| Pi fork/reset | New identity never silently inherits a live claim | Run `/mosscap` explicitly and receive an open-work recovery summary |

“Never missed” means that once the gateway has received and committed an eligible event, Pi/Zellij failure cannot lose it. It does not mean the gateway can receive events while it is indefinitely offline.

## State ownership

| State | Owner |
| --- | --- |
| Slack credentials, configured channel, trusted users | Gateway private config / SQLite |
| Slack event ledger, inbox revisions, reply outcomes, reactions | Gateway SQLite |
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

1. **Foundation** — package, strict SQLite inbox, Slack admission/deduplication, foreground gateway, local control protocol, crash-boundary tests.
2. **Pi bridge** — `/mosscap`, fenced binding, custom-message delivery, reconnect/reconciliation, lifecycle tests using a fake gateway.
3. **Outbound work** — inbox and Slack commands, reply/reaction reconciliation, edits/deletes, files, unknown-outcome handling.
4. **Operator UX** — status/doctor, Pi widget, Zellij list/attach/watch wrappers, supervisor-neutral deployment recipes.
5. **Hardening** — lease-chaos tests, migrations/backups, version negotiation, live Slack tests, security/release review.

## Open product decisions

Before implementation, decide:

1. **Takeover:** should a second Pi session require an interactive explicit takeover confirmation, or should it always be rejected until the current lease expires?
2. **Gateway operation:** which supervisor recipes should the documentation prioritize while keeping the product supervisor-neutral?
3. **Routing:** is a single active Pi consumer sufficient for v1, or should workspaces/channels be a first-class future route key?

## Relationship to pi-tag-slack

Mosscap keeps the durable Slack boundary from `pi-tag-slack`—Socket Mode admission, SQLite-first acknowledgement, inbox revisions, explicit outbound effects, and recoverable ambiguity—while replacing its daemon-owned headless Pi RPC process with a user-owned interactive Pi extension in Zellij.
