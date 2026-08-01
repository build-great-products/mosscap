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

## Phase 0 local-task quick start

Phase 0 implements only the private local ledger, foreground gateway, and task CLI. Slack, Pi delivery, files, effects, and purge are not implemented yet.

```sh
mosscap setup
mosscap gateway run                    # keep this foreground process running

# In another terminal:
mosscap task create --text "review the release" --request-id release-create
mosscap task working tsk_EXAMPLE --request-id release-working
mosscap task list
```

Mutations always go through the gateway. A request ID is durable: retry the *same* command with the same ID after a disconnect or gateway restart to receive its original outcome rather than repeat the mutation.

```sh
mosscap task create --text "review the release" --request-id release-create --json
# Restart `mosscap gateway run`, then safely replay:
mosscap task create --text "review the release" --request-id release-create --json
```

### Phase 0 security and failure matrix

| Condition | Phase 0 behavior | Operator action |
| --- | --- | --- |
| Gateway stops or restarts | Committed tasks and request outcomes remain in SQLite; same-ID replays do not duplicate work. | Restart `mosscap gateway run`, then replay the original mutation ID. |
| Unsafe mode, owner, symlink, hard link, or escaped state path | Setup/start fails closed. | Restore owner-only `0700` directories and `0600` regular files; remove unsafe links. |
| Live or unsafe socket / second gateway | Startup refuses to replace the endpoint. | Stop the owner gateway; inspect and remove only a known stale socket. |
| Unsupported protocol or ledger schema | Connection/start fails closed without reinterpretation. | Use the matching Mosscap binary; do not edit the ledger to force compatibility. |
| Gateway unavailable | CLI returns exit 6 and does not access SQLite directly. | Start the gateway and retry a mutation with its original request ID. |
| Request ID reused for different input | CLI returns conflict (exit 4); neither mutation is repeated. | Generate a new request ID for the new intent. |
| Corrupt or failed SQLite checks | Gateway refuses unsafe startup; doctor reports failures non-destructively. | Restore a known-good ledger; do not auto-repair. |
| Secret-bearing configuration | Config, status, doctor, logs, and mutation records redact tokens and omit historical task text. | Keep tokens in environment or an owner-only secrets file. |

## Design documents

- [SYSTEM_MODEL.md](SYSTEM_MODEL.md) explains the components, authority boundaries, and end-to-end flow.
- [SPEC.md](SPEC.md) is the normative implementation contract.
