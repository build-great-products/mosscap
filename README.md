# Mosscap

**Durable Slack work for a long-running agent session.**

Mosscap connects the Slack channels its bot is a member of to an agent session such as Claude Code. Eligible Slack mentions and local requests become durable tasks in SQLite. The agent follows them with `mosscap task watch`, which prints one line per task event, and handles each task with explicit CLI commands.

Mosscap does not start, supervise, or prompt the agent. A person (or a supervisor) runs the agent and points its background monitor at `task watch`.

```text
Slack ───────┐
             ├──> foreground Go gateway ───> SQLite
Go CLI ──────┘              │
                            │ private local protocol
                            ▼
                 mosscap task watch (one line per event)
                            │
                            ▼
                     agent session
```

## What is implemented

The current v1 workflow includes:

- single-workspace Slack Socket Mode admission from every channel the bot is a member of, optionally narrowed by an allowlist;
- a live SQLite-backed allowlist of trusted Slack user IDs;
- local and Slack-origin tasks with `open`, `working`, and `done` states;
- `task watch`, a one-line-per-event task stream with a durable resume cursor;
- explicit Slack reads, replies, edits, deletes, reactions, and file uploads;
- Slack lifecycle reactions that mirror task state;
- lazy incoming file download and private temporary-file handling;
- durable ambiguous outcomes that are never retried automatically; and
- explicit metadata purge with dry-run and confirmation modes.

## Install and configure

Requirements are Go and `just`.

```sh
just build
./bin/mosscap setup
./bin/mosscap config secret set slack_app_token
./bin/mosscap config secret set slack_bot_token
```

Import [`manifest.json`](manifest.json) into a Slack development app, create an app-level Socket Mode token with `connections:write`, install the app in the intended workspace, and invite it to each channel that should give it work. The manifest covers public (`message.channels`, `channels:read`, `channels:history`) and private (`message.groups`, `groups:read`, `groups:history`) channels; the gateway verifies every listed scope at startup. Do not add administrative, DM, slash-command, interactivity, or broad channel-join permissions.

By default the gateway admits mentions from every channel the bot is a member of. To narrow that, set a comma-separated allowlist of channel IDs; each listed channel must be one the bot belongs to:

```sh
./bin/mosscap config set slack_channel_allowlist C0BAQM6JEAJ,C0CC6JQ4A2U
./bin/mosscap config set slack_channel_allowlist ""   # every member channel
```

Start the gateway in the foreground:

```sh
./bin/mosscap gateway run
```

Upgrading from a release with Pi delivery or a single channel: remove `delivery_lease`, `leader_heartbeat`, and `leader_expiry` from `config.toml`, and replace `slack_channel_id = "C..."` with `slack_channel_allowlist = "C..."` (same ID keeps single-channel admission) or `slack_channel_allowlist = ""` (every member channel). The gateway names each obsolete key it finds. The ledger migrates on first start and drops its Pi delivery records; existing tasks and effects already record their channel, so nothing is backfilled.

## Trust and tasks

An empty trust set admits no Slack work. Add immutable Slack user IDs only for people authorized to give the agent work:

```sh
mosscap trust list
mosscap trust add U01234567
mosscap trust remove U01234567
```

Admission also requires the authenticated workspace, a channel the bot is a member of (and that passes the allowlist, when one is set), a human sender, and a literal direct mention of the bot. Messages that do not mention the bot are ignored without a reaction, whoever sends them; a mention from an untrusted user gets `:no_entry:`. A trusted Slack user can direct a fully capable local agent; these checks are authorization gates, not a sandbox.

Task state remains locally authoritative. Slack-origin tasks also show a lifecycle reaction on their source message that mirrors task state:

| Task state | Reaction |
| --- | --- |
| `open` (queued) | `:hourglass_flowing_sand:` |
| `working` (claimed with `task working`) | `:eyes:` |
| `done` | none |

Each transition adds the new marker before removing the old one. These are separately recorded Slack effects, so an interrupted or ambiguous Slack call remains inspectable rather than being silently retried or atomically replaced. Edits to the source message change the task text, not the reaction.

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

## Watching tasks

`mosscap task watch` is how an agent receives work. It stays connected to the gateway and prints one line per task event on stdout until interrupted:

```text
cursor=7 change=created task=AbCdEfGhIjKlMnOpQrStUv version=1 state=open source=slack channel=C0BAQM6JEAJ text=<@U012BOT> review the release\nthanks
```

- `change` is `created`, `edited` (content version changed, for example a Slack edit), or `state` (the task re-entered `open`).
- `channel` is the task's Slack channel ID, empty for local tasks.
- `text` is the last field, never truncated, with backslash, newlines, tabs, and other control characters escaped so one event is always one line.
- `--json` prints one JSON object per line instead: `{"change_seq":7,"change":"created","task":{...}}`, where `task` is the same DTO as `task show --json`.
- `--state-changes` also reports transitions to `working` and `done`.
- Without a cursor it first prints every task that is currently `open`; `--no-backlog` skips them.
- `--cursor-file PATH` makes restarts resume where the last run stopped. The file is updated after each printed line, so a crash can repeat at most one line and never skips one. `--since CURSOR` resumes from an explicit cursor instead.
- If the gateway restarts, the watcher reconnects with backoff and says so on stderr; stdout carries events only.

Watching is read-only. It changes no task state and adds no Slack reactions, so claim work explicitly with `task working` and finish it with `task reply --done` or `task done`.

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

## Slack commands and channels

Every `mosscap slack ...` command works in one channel. Name it with `--channel C...`; otherwise the channel comes from `--task` (a Slack task's own channel), and failing that from a single-entry allowlist. With several possible channels and neither flag, the command fails with `channel_required`. A channel outside the allowlist fails with `channel_not_admitted`.

```sh
mosscap slack history --channel C0BAQM6JEAJ --limit 20
mosscap slack thread 1700000000.000001 --task AbCdEfGh
mosscap slack send --task AbCdEfGh --thread 1700000000.000001 --stdin <<'REPLY'
Long multiline reply.
REPLY
mosscap slack file --channel C0CC6JQ4A2U --thread 1700000000.000001 --file ./demo.mp4
mosscap slack react 1700000000.000001 white_check_mark --task AbCdEfGh
```

`task reply` and `task context` always use the task's own channel and thread.

## Files and exceptional outcomes

Incoming Slack attachments are stored as metadata and downloaded only on request:

```sh
mosscap file show EXAMPLE --json
mosscap file get EXAMPLE --output ./attachment.bin
mosscap slack file --file ./result.bin
```

Mosscap retains file metadata and transfer outcomes, not permanent copies of transferred bytes.

SQLite is authoritative for tasks and Slack effects. Mosscap does not claim exactly-once execution across SQLite and Slack:

- a Slack operation that may have reached Slack becomes `ambiguous` and requires inspection before retry; and
- retries preserve the previous attempt and make duplicate risk explicit.

```sh
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

- [`docs/SYSTEM_MODEL.md`](docs/SYSTEM_MODEL.md): architecture and authority boundaries
- [`docs/SPEC.md`](docs/SPEC.md): normative v1 product contract

## Development

```sh
just check   # gofmt check, go test ./..., go vet ./...
just build
```
