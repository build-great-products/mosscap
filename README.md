# Mosscap

**Durable Slack work for a Pi session that stays yours.**

Mosscap connects one trusted Slack channel to an interactive [Pi](https://github.com/earendil-works/pi-coding-agent) session. Eligible Slack mentions and local requests become durable tasks in SQLite. An explicitly enabled Pi extension inserts those tasks as visible messages and starts model processing.

Mosscap does not start, supervise, select, or reset Pi. A person starts Pi and runs `/mosscap` in the session that should receive work.

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

## What is implemented

The current v1 workflow includes:

- single-workspace, single-channel Slack Socket Mode admission;
- a live SQLite-backed allowlist of trusted Slack user IDs;
- local and Slack-origin tasks with `open`, `working`, and `done` states;
- explicit Slack reads, replies, edits, deletes, reactions, and file uploads;
- lazy incoming file download and private temporary-file handling;
- explicit Pi opt-in, visible delivery, persisted delivery evidence, and recovery;
- `task watch`, a one-line-per-event task stream for any agent harness;
- durable ambiguous/uncertain outcomes that are never retried automatically; and
- explicit metadata purge with dry-run and confirmation modes.

The previous numbered milestones described implementation order. They are complete history, not current product modes or an active roadmap.

## Install and configure

Requirements are Go, Node/pnpm for extension development, and Pi `0.84.1` for the source-loaded extension.

```sh
just build
./bin/mosscap setup
./bin/mosscap config set slack_channel_id C01234567
./bin/mosscap config secret set slack_app_token
./bin/mosscap config secret set slack_bot_token
```

Import [`manifest.json`](manifest.json) into a Slack development app, create an app-level Socket Mode token with `connections:write`, install the app in the intended workspace, and invite it to the configured channel. The manifest targets a public channel with `message.channels`, `channels:read`, and `channels:history`. For a private `G...` channel, use `message.groups`, `groups:read`, and `groups:history` instead. Do not add administrative, DM, slash-command, interactivity, or broad channel-join permissions.

Start the gateway in the foreground:

```sh
./bin/mosscap gateway run
```

Then start a persisted interactive Pi session with the checked-out extension:

```sh
pi -e /absolute/path/to/mosscap/pi-extension
```

In Pi, opt in explicitly:

```text
/mosscap
```

Use `/mosscap status` to inspect the connection and `/mosscap off` to stop delivery without stopping Pi.

## Trust and tasks

An empty trust set admits no Slack work. Add immutable Slack user IDs only for people authorized to trigger the local Pi session:

```sh
mosscap trust list
mosscap trust add U01234567
mosscap trust remove U01234567
```

Admission also requires the configured workspace and channel, a human sender, and a literal direct mention of the bot. A trusted Slack user can trigger a fully capable local Pi while `/mosscap` is active; these checks are authorization gates, not a sandbox.

Task state remains locally authoritative. Slack-origin tasks also show a durable lifecycle projection: `:hourglass_flowing_sand:` while queued, then `:eyes:` after Pi has durably persisted the current version; completion removes `:eyes:`. These are separately recorded Slack effects, so an interrupted or ambiguous Slack call remains inspectable rather than being silently retried or atomically replaced.

```sh
mosscap task create --text "review the release"
mosscap task list
mosscap task working AbCdEfGh
mosscap task show AbCdEfGh --json
# Prefer this bounded durable-plus-live view before handling a Slack task.
mosscap task context AbCdEfGh --thread-limit 20
mosscap task reply AbCdEfGh --stdin --done <<'REPLY'
I found the problem.
REPLY
```

`task reply --done` is a resumable composition: it completes the task only after Slack confirms the reply. A plain Slack send never changes task state, and marking a task done never posts to Slack.

For exact multiline or piped bodies, `task create`, `task update`, `task reply`, `slack send`, and `slack edit` accept `--stdin` or `--text-file`; prefer those sources over shell-quoted `--text`. Provide exactly one of `--text`, `--text-file`, or `--stdin`; stdin is read completely (including trailing newlines) before contacting the gateway. `--text` is literal: a typed `\\n` remains a backslash followed by `n`, not a newline.

Every command that contacts the gateway accepts `--timeout DURATION` (default `10s`). The budget starts only immediately before dialing the gateway, covers dial, handshake, request, and frame I/O, and never extends an earlier caller deadline. Stdin and local input validation happen before that budget begins.

Default output is compact human-readable cards, rows, and labelled details; use `--json` for the unchanged command DTOs. Message cards preserve full multiline Unicode text and visibly escape terminal control bytes. Colors require a TTY and are disabled by `NO_COLOR`.

## Watching tasks from any harness

Agents that cannot load the Pi extension can consume tasks with `mosscap task watch`. It stays connected to the gateway and prints one line per task event on stdout until interrupted:

```text
cursor=7 change=created task=AbCdEfGhIjKlMnOpQrStUv version=1 state=open source=slack text=<@U012BOT> review the release\nthanks
```

- `change` is `created`, `edited` (content version changed, for example a Slack edit), or `state` (the task re-entered `open`).
- `text` is the last field, never truncated, with backslash, newlines, tabs, and other control characters escaped so one event is always one line.
- `--json` prints one JSON object per line instead: `{"change_seq":7,"change":"created","task":{...}}`, where `task` is the same DTO as `task show --json`.
- `--state-changes` also reports transitions to `working` and `done`.
- Without a cursor it first prints every task that is currently `open`; `--no-backlog` skips them.
- `--cursor-file PATH` makes restarts resume where the last run stopped. The file is updated after each printed line, so a crash can repeat at most one line and never skips one. `--since CURSOR` resumes from an explicit cursor instead.
- If the gateway restarts, the watcher reconnects with backoff and says so on stderr; stdout carries events only.

Watching is read-only. It records no delivery evidence, adds no Slack reactions, and does not claim the Pi delivery slot, so claim work explicitly with `task working` and finish it with `task reply --done` or `task done`.

### Claude Code

Run the watcher under Claude Code's Monitor tool (or any background runner that turns stdout lines into notifications):

```sh
mosscap task watch --cursor-file "$HOME/.local/state/mosscap-watch.cursor"
```

For each notification, the agent claims and handles the task, then replies (Slack-origin tasks; use `task done` for local ones):

```sh
mosscap task working AbCdEfGhIjKlMnOpQrStUv
mosscap task context AbCdEfGhIjKlMnOpQrStUv
mosscap task reply AbCdEfGhIjKlMnOpQrStUv --stdin --done <<'REPLY'
Done: the release checklist passes.
REPLY
```

## Files and exceptional outcomes

Incoming Slack attachments are stored as metadata and downloaded only on request:

```sh
mosscap file show EXAMPLE --json
mosscap file get EXAMPLE --output ./attachment.bin
mosscap slack file --file ./result.bin
```

Mosscap retains file metadata and transfer outcomes, not permanent copies of transferred bytes.

SQLite is authoritative for tasks, Slack effects, and delivery outcomes. Pi JSONL records what one Pi session has seen. Mosscap does not claim exactly-once execution across SQLite, Pi, or Slack:

- a delivery that may have reached Pi becomes `uncertain` and requires inspection before requeue;
- a Slack operation that may have reached Slack becomes `ambiguous` and requires inspection before retry; and
- retries preserve the previous attempt and make duplicate risk explicit.

```sh
mosscap delivery list --state uncertain --json
mosscap effect list --state ambiguous --json
mosscap gateway status --json
mosscap doctor --json
```

## Configuration and local state

`config.toml` contains all scalar settings. Slack tokens are redacted from command output and are never stored in SQLite. Paths are fixed under owner-only XDG roots:

```text
$XDG_CONFIG_HOME/mosscap/config.toml
$XDG_STATE_HOME/mosscap/mosscap.sqlite3
$XDG_STATE_HOME/mosscap/control.sock
$XDG_STATE_HOME/mosscap/tmp/
```

`config set` stages one validated change while preserving comments and formatting. `gateway reload` verifies and activates the complete candidate snapshot atomically; a failed reload leaves the active runtime unchanged.

Every mutation has a request ID. When the CLI generates one, it reports it in the result. After an interrupted mutation, retry exactly the same input with the same ID; never reuse an ID for changed input.

## Documentation

- [`pi-extension/README.md`](pi-extension/README.md) — Pi extension installation and behavior
- [`docs/SYSTEM_MODEL.md`](docs/SYSTEM_MODEL.md) — architecture and authority boundaries
- [`docs/SPEC.md`](docs/SPEC.md) — normative v1 product contract

## Development

```sh
just check
just build
cd pi-extension && pnpm install --frozen-lockfile && pnpm test && pnpm typecheck
```
