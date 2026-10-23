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

- single-workspace Slack Socket Mode admission from every channel the bot is a member of, for every member of that workspace;
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

The gateway admits work from every channel the bot is a member of. Inviting the bot to a channel is how you give that channel's members a way to send it work, and removing it is how you take that away. There is no channel or user allowlist.

Start the gateway in the foreground:

```sh
./bin/mosscap gateway run
```

Upgrading from an earlier release (schema migrations 8 to 10 run automatically; they are one-way): remove `delivery_lease`, `leader_heartbeat`, `leader_expiry`, `slack_channel_id`, `slack_channel_allowlist`, and `trusted_user_ids` from `config.toml`. The gateway refuses to start while any of them is present and names every one it finds. The ledger migrates on first start: it drops its Pi delivery records and its trusted-user set (`mosscap trust` no longer exists). Existing tasks and effects already record their channel, so nothing is backfilled.

## Admission and tasks

Anyone in the workspace may send the bot work. A Slack message becomes a task when all of these hold:

- it arrives from a channel the bot is a member of in the authenticated workspace;
- its sender is a human: not a bot, not the bot itself, and not a `bot_message` or other system subtype;
- its sender is a member of the workspace, not a Slack Connect external user from another organization; and
- it mentions the bot directly, or it is a reply in a thread the bot already tracks (see below).

Messages that neither mention the bot nor reply in a tracked thread are ignored without a reaction. A mention from an external Slack Connect user is ignored the same way, recorded as `ignored_workspace`. Mosscap reads the sender's workspace from the event Slack delivers (`user_team`, else `user_profile.team`, else `team`) and makes no per-message `users.info` call; an event that names no sender workspace is treated as a member's.

Any member of the workspace can direct a fully capable local agent. Choose its working directory, model, and channels with that in mind: these checks decide who may send work, they are not a sandbox.

### Thread follow-ups

People usually answer in the thread without mentioning the bot again ("Perfect", "it's out of sync"). A new reply from any workspace member in a tracked thread is admitted without a mention, as its own **follow-up task**, whether or not that person started the thread. A thread is tracked when it already holds an admitted task, or when the bot posted its root or a reply in it (a confirmed `slack send`, `task reply`, or upload). Each follow-up:

- is a separate task with the reply's text, so nothing overwrites earlier text, and gets its own lifecycle reaction on the reply;
- records the thread (`thread_ts`, the thread root) and `parent_task_id`, the thread's original task: the task from the root message, else the task the bot's root post was for, else the earliest task in the thread;
- is replied to in the same thread by `task reply`, and `task context` shows the whole thread.

Replies from external Slack Connect users, the bot's own messages, and edits of replies that were never admitted do not create follow-ups. A reply that also mentions the bot is admitted the same way and gets the same link. Edits of an admitted follow-up update it like any other task. A reply with files but no text is admitted with the text `(attachment without message text)`. Replies with the `thread_broadcast` and `file_share` subtypes count as human messages.

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
cursor=7 change=created task=AbCdEfGhIjKlMnOpQrStUv version=1 state=open source=slack channel=C0BAQM6JEAJ thread=1700000000.000001 parent= text=<@U012BOT> review the release\nthanks
cursor=9 change=created task=ZyXwVuTsRqPoNmLkJiHgFe version=1 state=open source=slack channel=C0BAQM6JEAJ thread=1700000000.000001 parent=AbCdEfGhIjKlMnOpQrStUv text=Perfect
```

- `change` is `created`, `edited` (content version changed, for example a Slack edit), or `state` (the task re-entered `open`).
- `channel` is the task's Slack channel ID and `thread` its reply thread (the root message timestamp); both are empty for local tasks.
- `parent` is set on a thread follow-up and names the thread's original task; it is empty otherwise.
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

For each notification, the agent claims and handles the task, then replies (Slack-origin tasks; use `task done` for local ones). A line with `parent=` is a follow-up in an existing thread: `task context` shows the conversation it answers, and its reply lands in the same thread.

```sh
mosscap task working AbCdEfGhIjKlMnOpQrStUv
mosscap task context AbCdEfGhIjKlMnOpQrStUv
mosscap task reply AbCdEfGhIjKlMnOpQrStUv --stdin --done <<'REPLY'
Done: the release checklist passes.
REPLY
```

## Slack commands and channels

Every `mosscap slack ...` command works in one channel. Name it with `--channel C...`, or pass `--task` to use a Slack task's own channel. With neither, the command fails with `channel_required`. A target that is not a public (`C...`) or private (`G...`) channel in the active workspace fails with `invalid_channel`; Slack itself refuses channels the bot is not a member of.

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
