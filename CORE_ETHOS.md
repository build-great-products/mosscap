# Mosscap Core Ethos

**Keep the coordination durable; keep the agent human-owned.**

Mosscap is a deliberately small alternative to OpenClaw-style self-managing agent systems. Instead of hiding an agent behind a daemon that chooses, starts, supervises, and resets its own runtime, Mosscap connects durable work to an ordinary interactive Pi session that a person already owns.

```text
Slack / schedules / CLI
          │
          ▼
 foreground gateway ─── SQLite message, task, and effect ledger
          │
          ▼
     Pi extension
          │
          ▼
 human-owned Pi session in Zellij
```

The result is not an autonomous agent platform. It is a dependable bridge between remote requests, explicit local commands, and a visible working session.

## Design principles

### 1. The human owns the agent

A person starts Pi, chooses its model and thinking level, selects its working directory, and decides when the session ends. Running `/mosscap` explicitly opts that session into work; `/mosscap off` opts it out. Mosscap does not create a second, hidden agent with a separate context or agenda.

This preserves the best property of interactive Pi: the operator can see the conversation, steer it, interrupt it, compact it, resume it, or walk away using normal Pi controls.

### 2. Minimal does not mean fragile

Mosscap removes runtime-management machinery, not reliability. The gateway commits an eligible event before acknowledging it, and SQLite—not a prompt, terminal pane, or Pi transcript—is the durable source of truth. Messages become revisioned inbox items; local and scheduled requests become tasks; outbound Slack actions become recorded effects with explicit outcomes.

Delivery is durable and idempotent, not falsely advertised as exactly once. A Pi message carries a stable delivery key, leases expire safely, and reconnecting sessions reconcile persisted keys. When an external result is ambiguous, Mosscap records the ambiguity and asks for inspection rather than retrying blindly.

### 3. Observability is participation, not surveillance

Zellij makes the real Pi session easy to find, attach to, and watch. It is an observation surface, never a queue or database. Mosscap does not scrape pane output, inject keystrokes, or infer success from what a terminal appears to show.

The extension delivers work as visible Pi messages and exposes compact connection, binding, and backlog state. Correctness does not depend on a modal UI or on a human continuously watching.

### 4. Boundaries stay explicit

The foreground gateway owns Slack admission, the SQLite ledger, leases, scheduling, and outbound-effect reconciliation. The Pi extension owns delivery into the current session. The CLI provides inspectable, scriptable state transitions. Zellij owns only terminal lifecycle and visibility.

Trust is equally concrete: one configured Slack conversation, explicitly trusted user IDs, private local state, and a same-user control endpoint. A trusted Slack user can influence a fully capable local agent; Mosscap says so plainly instead of pretending that a socket or terminal multiplexer is a security sandbox.

### 5. Recovery follows durable state

Pi, SSH, Zellij, Slack, and the gateway may each disappear independently. Already committed work remains. A resumed or newly opted-in Pi session receives open work from the ledger, not from reconstructed terminal history. Scheduled work is materialized as ordinary durable tasks, never held in an in-memory agent timer.

This keeps failures unsurprising: restore the missing component, reconnect, inspect open work, and continue.

### 6. Prefer one clear path over an implicit agent society

Version one has one active, fenced Pi consumer. The CLI uses explicit verbs to create, inspect, update, respond to, resolve, reopen, or tombstone work. There is no invisible pool choosing who acts, no background agent spawning another agent, and no terminal state masquerading as coordination.

A single understandable path is easier to trust, test, and operate than a larger system whose autonomy exceeds its observability.

## Essential vs. intentionally absent

| Essential to Mosscap | Deliberately omitted |
| --- | --- |
| A human-owned interactive Pi session | Daemon-owned or headless Pi children |
| A durable message/task inbox and outbound-effect ledger | Prompts, transcripts, or panes as the task database |
| Commit-before-ack admission, deduplication, leases, fencing, and recovery | Claims of end-to-end exactly-once execution |
| A foreground Slack gateway with a private local protocol | Installed service management or a mandatory supervisor |
| An explicit CLI for state and Slack operations | Hidden action inferred from free-form output |
| A Pi extension for visible, acknowledged delivery | Key injection, pane scraping, or UI-dependent correctness |
| Zellij attach/watch observability | Zellij as queue, scheduler, or authority |
| Explicit channel and user trust | Wildcard trust, display-name trust, or ambient channel membership |
| One active consumer with explicit takeover semantics | Implicit multi-agent routing, swarms, or self-replication |
| Durable schedules that create normal tasks | In-memory timers tied to Pi lifetime |

## The product test

A Mosscap feature belongs in the core only if it strengthens one of four promises: **durable work, explicit authority, visible human control, or honest recovery**. Convenience may be added around those promises, but it must not make Mosscap own Pi, conceal consequential state, or blur which component is authoritative.

That restraint is the point. Mosscap provides enough infrastructure for an interactive agent to receive and complete durable remote work—and refuses to become the agent’s owner.
