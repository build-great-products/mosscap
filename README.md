# Mosscap

**Durable Slack work for a Pi session that stays yours.**

Mosscap connects a trusted Slack conversation to the interactive Pi session you already run. Slack requests, local tasks, schedules, and outbound Slack actions are recorded by a foreground gateway; Pi receives the work as visible messages in its normal TUI.

Mosscap does not start, select, reset, or hide an agent. You choose the Pi session, model, thinking level, working directory, and when to stop. The system keeps the coordination durable so you can keep the agent human-owned.

```text
Slack / schedules / CLI
          │
          ▼
 foreground mosscap gateway ─── SQLite task and effect ledger
          │
          ▼
       Pi extension
          │
          ▼
 human-owned Pi session in Zellij
```

## The everyday experience

1. Start the gateway in a terminal you supervise.
2. Start Pi in a named Zellij session, as you normally would.
3. Run `/mosscap` in Pi when you want that session to receive work.
4. A trusted, mentioned Slack request becomes a visible Pi message and a durable task.
5. Read it, ask for clarification, work normally in Pi, and use explicit task or Slack commands when it is time to record progress or respond.
6. Watch, attach to, interrupt, resume, compact, or exit Pi using normal Pi and Zellij controls. Run `/mosscap off` when the session should stop receiving work.

If Pi, SSH, or Zellij disappears, committed work remains in the gateway's ledger. Resume Pi—or explicitly opt in a new session—and continue from the open work rather than trying to reconstruct terminal history.

## Start a working session

Mosscap is designed around an ordinary, visible Pi session:

```bash
zellij --session mosscap
pi
/mosscap
```

`/mosscap` opts the current persisted Pi session into Slack work. It claims the single active consumer lease and shows connection, binding, and pending-work state in Pi. The incoming work is delivered as distinct visible `mosscap.slack` messages, not pasted through terminal keystrokes or hidden in another agent.

To find or observe the session later:

```bash
zellij list-sessions --short
zellij attach mosscap
zellij watch mosscap
```

`zellij watch` is read-only. Zellij is for human visibility and terminal lifecycle, not for task delivery or correctness.

To opt out without discarding durable work:

```text
/mosscap off
```

## Set up and run the gateway

The CLI makes process boundaries explicit:

```text
mosscap setup|config show|config set
mosscap gateway run|status
mosscap doctor

mosscap task create|list|show|update|working|respond|resolve|reopen|delete
mosscap slack history|thread|send|edit|delete|react|unreact|file
mosscap trust list|add|remove

mosscap zellij list|attach|watch
```

Begin with `mosscap setup`. It prepares or verifies Mosscap's private state and configuration locations, initializes SQLite and the local endpoint, and requires Slack credentials, one channel ID, and explicit trusted Slack user IDs before the gateway will admit work. It does **not** install a service or start Pi.

Run the gateway in the foreground:

```text
mosscap gateway run
```

You choose the supervisor—a terminal, systemd, launchd, container, or something else. `mosscap gateway status` reports meaningful health: the private database and endpoint, Slack connection or bounded reconnecting state, active binding, open tasks, pending leases, and pending or ambiguous Slack effects. Use `mosscap doctor` for a non-destructive configuration, permissions, database, endpoint, Slack, binding, and compatibility check.

`config show` redacts secrets. `config set` validates values, but a display name, wildcard, or channel membership never establishes trust; use explicit user IDs with `mosscap trust add`.

## Work together through durable tasks

The task ledger, not a Pi transcript, is the shared record of work. Use explicit verbs to make coordination legible:

- `mosscap task create` records local work.
- `mosscap task list` and `mosscap task show` make the current work visible.
- `mosscap task update` and `mosscap task working` record progress.
- `mosscap task respond` records an intended response.
- `mosscap task resolve`, `reopen`, and `delete` make completion, renewed work, and tombstoning explicit.

A Slack-origin task retains its source event and thread identity. A local task has no implied Slack destination until an explicit response or effect supplies one. This lets a person and Pi work in the open: inspect the queue, steer the conversation, record progress, and make consequential external actions through named commands rather than guessing from free-form terminal output.

The first version has one active, fenced Pi consumer. A second Pi session does not silently inherit a live claim; opt it in explicitly and inspect the binding. That restraint keeps it clear who is handling the work.

## Replying and acting in Slack

Slack operations are explicit CLI actions:

```text
mosscap slack history|thread|send|edit|delete|react|unreact|file
```

Before Mosscap sends, edits, deletes, reacts, unreacts, or uploads a file, the gateway records a durable effect with its target and idempotency key. It records confirmation only after Slack confirms the outcome.

If Slack times out after a submission, Mosscap marks the result **ambiguous** instead of blindly retrying and possibly duplicating an external action. Inspect the unresolved effect and take the indicated next action. Reactions are helpful task-state projections, not the task database; a Slack reply resolves work only after its effect is confirmed.

## Scheduling work

Schedules create the same durable tasks that Slack and local commands create. They are not Pi timers, so they survive a Pi session ending.

```text
mosscap schedule add --title "Follow up" --at 2026-08-01T09:00:00+12:00 --instructions "…"
mosscap schedule add --title "Weekly review" --cron "0 9 * * 1" --timezone Pacific/Auckland --instructions "…"
mosscap schedule list|show|enable|disable|remove
```

A one-off schedule uses an ISO-8601 time with an explicit UTC offset. A recurring schedule uses a five-field cron expression and an IANA timezone. After downtime, missed recurring runs are coalesced into one recovery task rather than flooding Pi with stale prompts.

## What Pi receives and does

When an eligible Slack event arrives, the gateway validates its channel, trusted user, mention, and identity; commits it to SQLite; and then leases the resulting task revision to the opted-in Pi session. The extension injects a visible message with a stable delivery key and acknowledges that persisted delivery.

That sequence has practical consequences for the person and the agent:

- Incoming work is visible in the session you own.
- Pi can use its ordinary conversation and tools to reason about the request.
- Task state and Slack actions remain inspectable outside the transcript.
- Restarting Pi normally does not duplicate already persisted delivery keys.
- A fresh Pi session may receive open work again on explicit opt-in, so consult the durable task ledger before acting.

Delivery is durable and idempotent, not an unrealistic promise of end-to-end exactly-once execution. SQLite and Pi's JSONL session cannot be committed as one transaction; Mosscap handles that boundary with delivery keys, leases, expiry, and reconciliation.

## Recovery without drama

| What happened | What to do |
| --- | --- |
| Pi exits or SSH disconnects | Resume Pi or start a new session, then run `/mosscap`. Committed work remains open. |
| Zellij exits | Restore Zellij and Pi, then reconnect the extension. |
| The gateway restarts | Restart it with your chosen supervisor; the extension reconnects. |
| Slack disconnects | Reconnect Socket Mode; event identity deduplicates retried events. |
| A Slack action is ambiguous | Inspect the recorded effect; do not assume it is safe to retry. |
| Another Pi session wants work | Use explicit opt-in and inspect the binding; it does not silently take over. |

“Never missed” applies after the gateway receives and commits an eligible event. It does not mean Mosscap receives Slack events while the gateway is indefinitely offline.

## Trust and privacy

Mosscap admits only messages from the configured Slack channel, sent by explicitly trusted users, that mention the bot. Treat a trusted Slack user as someone who can influence a fully capable local agent.

By default, configuration is under `$XDG_CONFIG_HOME/mosscap` and mutable state, including SQLite, is under `$XDG_STATE_HOME/mosscap`. These roots are owner-only; sensitive files are owner-readable only; and the same-user control socket lives under the private state root, never in `/tmp`. Mosscap rejects unsafe path ownership and permissions.

The local socket and a Zellij pane are conveniences, not security boundaries. Mosscap does not scrape panes, inject keys, or treat terminal output as proof that work was delivered or completed.

## What Mosscap deliberately is not

Mosscap is a dependable bridge, not a self-managing agent platform. It intentionally omits:

- daemon-owned or headless Pi children;
- automatic model selection, session reset, or agent spawning;
- hidden multi-agent routing or swarms;
- Zellij as a queue, scheduler, or authority;
- prompts, transcripts, or panes as the task database;
- wildcard or display-name trust; and
- claims of exactly-once execution.

A feature belongs in Mosscap only when it strengthens durable work, explicit authority, visible human control, or honest recovery.

## Relationship to pi-tag-slack

Mosscap keeps the durable Slack boundary of `pi-tag-slack`—Socket Mode admission, SQLite-first acknowledgement, inbox revisions, explicit outbound effects, and recoverable ambiguity—while replacing a daemon-owned headless Pi RPC process with a user-owned interactive Pi extension in Zellij.
