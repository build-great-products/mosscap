# Mosscap

**Durable Slack work for a Pi session that stays yours.**

> Mosscap is currently being specified. The command examples below describe the intended interface.

Mosscap connects one trusted Slack channel to an interactive [Pi](https://github.com/earendil-works/pi-coding-agent) session. Eligible Slack mentions and local requests become durable tasks in SQLite. A TypeScript Pi extension inserts that work as visible messages and immediately starts model processing.

Mosscap does not start, supervise, select, or reset Pi. A person starts Pi and explicitly runs `/mosscap` when that session should receive work.

```text
Slack ───────┐
             ├──> foreground Go gateway ───> SQLite
Go CLI ──────┘              │
                            │ private local protocol
                            ▼
                    TypeScript Pi extension
                            │
                            ▼
                   human-owned Pi session
```

## Intended experience

```text
mosscap setup
mosscap gateway run
```

Then, in a separately started Pi session:

```text
/mosscap
```

A direct mention from a trusted user in the configured Slack channel creates a task. The gateway commits it before acknowledging Slack. The extension delivers it visibly to Pi and triggers a model turn immediately, or queues it as the next follow-up if Pi is already working.

Tasks are a small todo list with `open`, `working`, and `done` states. Slack communication is explicit and independent of task state:

```text
mosscap task list
mosscap task working tsk_123
mosscap slack send --thread 1712345678.123456 --text "I found the problem."
mosscap task done tsk_123
```

All Slack commands operate on the single configured channel; they cannot select another destination.

Slack files are represented by durable metadata but downloaded only on request. Downloads and uploads use temporary bytes rather than a permanent Mosscap file archive.

## Reliability model

SQLite is the authority for tasks, Slack effects, and delivery outcomes. Pi's JSONL session records what that particular Pi session has seen.

Mosscap does not claim exactly-once execution across those two stores or across Slack. Instead it records uncertainty honestly:

- a delivery that may have reached Pi is `uncertain` and is not automatically repeated;
- a Slack operation that may have reached Slack is `ambiguous` and is not automatically retried;
- explicit inspection and retry commands preserve the prior attempt.

## Trust

A trusted Slack user can automatically trigger a fully capable local Pi session while `/mosscap` is active. The configured workspace, single channel, immutable user allowlist, and direct bot mention are admission checks—not a sandbox.

Configuration, SQLite, the local socket, and temporary files live under owner-only XDG directories. Slack tokens are never written to SQLite or inserted into Pi messages.

## Design documents

- [SYSTEM_MODEL.md](SYSTEM_MODEL.md) explains the components, authority boundaries, and end-to-end flow.
- [SPEC.md](SPEC.md) is the normative implementation contract.
