# Phase 1 Slack operations

Phase 1 connects one configured Slack channel to the local Mosscap ledger. It is an operator-controlled gateway: Slack admission creates durable local tasks, while Slack reads and explicit effects remain independent of task state. Never put tokens, authorization headers, raw events, or private message bodies in tickets, shell history, logs, or source control.

## Slack app setup

Import [`../../manifest.json`](../../manifest.json) in Slack's app-manifest UI. Enable Socket Mode, install the app in the intended workspace, and invite the bot to the configured channel. The manifest is the least-authority source of truth: do not add administrative, IM/MPIM, interactivity, slash-command, request-URL, or broad channel-join permissions. Its subscriptions are `app_mention` and the matching channel message event (`message.channels` for a public channel; use `message.groups` with the corresponding `groups:read` and `groups:history` scopes for a private `G...` channel).

Create a separate app-level Socket Mode token with `connections:write` and a bot token for the installed app. Configure immutable IDs only: the channel ID is configured, and the workspace/team and bot-user IDs are derived from `auth.test`. Startup verifies identity, channel access, membership, and the manifest's required bot scopes before Socket intake starts.

## Credentials, startup, and health

Use private owner-only XDG roots:

```sh
mosscap setup
# Set slack_app_token, slack_bot_token, and slack_channel_id in the owner-only config.toml.
mosscap gateway run
```

Tokens belong only in `config.toml`; config output redacts them and SQLite never stores them. Keep the config/state directories, ledger, and control socket private to the owning account. In another terminal, use non-mutating checks:

```sh
mosscap gateway status --json
mosscap doctor --json
```

Status reports connection state, verified IDs, reconnect information, admission/effect counts, and action-required failed or ambiguous effects separately from process health. Doctor checks configuration, SQLite, Slack identity/access/scopes, and trusted users; it does not acknowledge events, mutate Slack, or transfer file content.

## Trust, admission, and source inspection

A channel alone cannot admit work. Add only people authorized to trigger local work:

```sh
mosscap trust list
mosscap trust add U01234567 --request-id trust-alice
mosscap task list --all
mosscap task show tsk_EXAMPLE --json
mosscap file show fil_EXAMPLE --json
```

Admission requires the configured workspace and channel, a currently trusted human sender, and a literal direct mention of the verified bot user. An empty trust set admits nothing. Mosscap commits an eligible source before acknowledging its Socket envelope. Redeliveries resolve to the same task; stale events cannot roll it back. Meaningful edits and a deletion update the current source metadata/version; deletion preserves the task text and state. Incoming Slack files are bounded metadata only: Mosscap neither downloads nor uploads file bytes in Phase 1.

## Reads and explicit writes

All reads and writes are bound to the configured channel; there is no workspace or channel selector.

```sh
mosscap slack history --limit 50 --json       # default 50, maximum 100
mosscap slack thread 1700000000.000001 --json
mosscap slack send --text 'operator reply' --request-id reply-1 --json
mosscap slack edit 1700000000.000001 --text 'correction' --request-id edit-1
mosscap slack delete 1700000000.000001 --reason 'superseded' --request-id delete-1
mosscap slack react 1700000000.000001 eyes --request-id react-1
mosscap slack unreact 1700000000.000001 eyes --request-id unreact-1
```

Use a stable request ID while resolving an interrupted mutation. Targeted writes preflight the target in the configured channel before recording an effect. Every effect intent is durable before its single Slack call. Reads do not alter SQLite task state, and Slack effects do not transition tasks.

## Failed and ambiguous effects

A conclusive Slack rejection is `failed`. A timeout, disconnect, cancellation, or malformed response after durable submission is `ambiguous`: Slack may have acted, so Mosscap never retries it automatically, including after restart.

```sh
mosscap effect list --state ambiguous --json
mosscap effect inspect eff_EXAMPLE --json
# Inspect Slack manually first. This creates a linked new effect.
mosscap effect retry eff_EXAMPLE --reason 'confirmed absent in Slack' \
  --confirm-duplicate-risk --request-id retry-1 --json
```

A retry preserves its predecessor. Ambiguous work requires explicit duplicate-risk consent; do not use a new `slack send` instead of inspection.

## Reconnect, reload, and shutdown

During a Socket disconnect, Mosscap makes no offline receipt or acknowledgement claim. It reconnects with bounded backoff/jitter; watch `gateway status` and correct Slack access/configuration before reloading:

```sh
mosscap gateway reload --request-id reload-1
```

A failed reload leaves the verified active runtime usable. A successful reload verifies the candidate before switching intake, and an operation already started continues on its acquired old snapshot.

Interrupt the foreground gateway and wait for it to exit. Shutdown stops/drains Socket and operation intake before closing SQLite. On restart, committed tasks/effects remain available; any leftover `submitted` effect is converted to `ambiguous` and must be inspected explicitly.

## Optional disposable-workspace smoke checklist

This is exploratory adapter/UX evidence, not a replacement for the deterministic release gate:

1. Use a disposable workspace/channel and credentials kept outside source control.
2. Verify the app identity, channel membership/scopes, and one trusted user.
3. Send one direct mention and confirm one committed task/source before acknowledgement.
4. Exercise one read and one harmless reaction; inspect status and doctor.
5. Simulate reconnect and controlled shutdown/restart; inspect any ambiguous effect.
6. Revoke disposable credentials and delete disposable local state. Retain no tokens, event captures, or message bodies.
