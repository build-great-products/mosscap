# Phase 1 Slack operations

Phase 1 connects **one configured Slack channel** to the local Mosscap ledger. It is an operator-controlled gateway, not a Slack-to-Pi sandbox. Do not put tokens or authorization headers into tickets, logs, shell history, or this document. Raw event/message data belongs only in the SPEC-authorized task/read flow, and private file-transfer URLs remain transient adapter data rather than durable evidence.

Mosscap does not redact local filesystem paths or ordinary operational diagnostics in owner-only CLI/protocol output, status, doctor, gateway logs, or opted-in Pi messages. Pi-visible values can enter the configured model provider's context and persist in Pi JSONL. Operators decide whether to publish that local observability elsewhere; Mosscap makes no path-confidentiality claim.

## Slack app setup

Import [`../../manifest.json`](../../manifest.json) in Slack's app-manifest UI for the configured public `C...` channel. The manifest enables Socket Mode, requests only its listed bot scopes (including `files:read` and Phase 3's narrowly required `files:write`), and subscribes only to `app_mention` and `message.channels`; it deliberately has no app-level scope property, interactivity, slash commands, or request URLs. Generate the separate app-level `connections:write` Socket Mode token after importing the manifest. Slack's manifest UI validation is authoritative: correct any schema or scope feedback there before proceeding.

For one configured private `G...` channel, use the same manifest except replace only `channels:read` and `channels:history` with `groups:read` and `groups:history`, and replace `message.channels` with `message.groups`. Retain every other scope and event, including `app_mention`, `files:read`, and `files:write`, unchanged. Install (or reinstall after this scope change) the app in the intended workspace, then explicitly invite the bot to the configured channel.

Generate an app-level Socket Mode token (`xapp-…`) and a bot token (`xoxb-…`) for the same installed app. Do not add IM/MPIM, administrative, broad join, interactivity, or any scopes beyond the listed bounded file read/write authority.

Record only immutable IDs, not names: channel ID is configured; workspace/team ID and bot user ID are derived from the bot token. Mosscap rejects an identity, scope, membership, or configured-channel mismatch before Socket intake becomes active.

## Credentials and first start

Use owner-only XDG roots (the defaults are appropriate for a single local account):

```sh
mosscap setup
# Edit the owner-only config.toml; do not paste it into chat or source control.
# Set slack_app_token, slack_bot_token, and slack_channel_id. The workspace and bot user IDs are derived from auth.test at startup and verified before intake.
mosscap gateway run
```

`config.toml`, the state directory, SQLite database, control socket, and temporary directory must remain private to the owning account. Tokens belong only in `config.toml`; they are redacted from config output and never belong in SQLite. Exact local paths and ordinary failure details are not redacted on same-user surfaces, and there is no protection from another process controlling the same UID. The gateway is foreground-owned: run it under an operator's terminal or service manager, not as an unattended authority with shared credentials.

In another terminal inspect non-mutating health:

```sh
mosscap gateway status --json
mosscap doctor --json
```

Status distinguishes a healthy process from action-required failed/ambiguous Slack effects. Status and doctor may render complete local paths and ordinary diagnostics; only credential values are redacted. Private transfer URLs and file payload bytes are absent because doctor performs no transfer and those values are not durable state. Doctor does not acknowledge Slack envelopes, mutate Slack, transfer files, or invoke Pi.

## Trust and source inspection

A configured channel is still not sufficient to admit work. Add only people who may trigger the attached Pi session:

```sh
mosscap trust list
mosscap trust add U01234567 --request-id trust-alice
mosscap task list --all
mosscap task show tsk_EXAMPLE --json
mosscap file show fil_EXAMPLE --json
```

An eligible direct bot mention from a trusted human is committed to SQLite before its Socket acknowledgement. Redeliveries resolve to the canonical committed source/task and do not create another task. Edits and deletion metadata are visible through `task show`; incoming files are metadata only until an explicit Phase 3 `file get`. Mosscap never archives file bytes.

A trusted user can cause local Pi work when the later delivery component is enabled. Trust is therefore authorization to trigger a capable local session, not a sandbox. Keep the allowlist small and review it regularly.

## Reads and writes

All Slack reads and writes are permanently bound to the configured channel; there is no workspace or channel selector.

```sh
mosscap slack history --limit 50 --json
mosscap slack thread 1700000000.000001 --json
mosscap slack send --text 'operator reply' --request-id reply-1 --json
mosscap slack edit 1700000000.000001 --text 'correction' --request-id edit-1
mosscap slack delete 1700000000.000001 --reason 'superseded' --request-id delete-1
mosscap slack react 1700000000.000001 eyes --request-id react-1
# Explicit, single-channel file operations; paths are never supplied by Slack metadata.
mosscap file get fil_EXAMPLE --output ./safe-output
mosscap slack file --file ./operator-selected-file --request-id upload-1
```

Use a stable request ID while resolving an interrupted command. A read does not change SQLite task state; a Slack effect does not transition a task. Mutation output intentionally reports durable identity/state rather than echoing message bodies. File and path failures may include the complete local path and ordinary diagnostic without redaction.

## Failed and ambiguous effects

A conclusive Slack rejection is `failed`. A timeout, disconnect, or malformed response after durable submission is `ambiguous`: Slack may have acted, so Mosscap will **never automatically retry it**, including after restart.

```sh
mosscap effect list --state ambiguous --json
mosscap effect inspect eff_EXAMPLE --json
# Inspect Slack manually first. Retry is a new linked effect and requires explicit risk consent.
mosscap effect retry eff_EXAMPLE --reason 'confirmed absent in Slack' \
  --confirm-duplicate-risk --request-id retry-1 --json
```

Preserve the predecessor and the reason in the operational record. Do not use a fresh `slack send` as a substitute for inspecting an ambiguous predecessor.

## Reconnect, reload, and shutdown

Socket disconnection stops receipt/acknowledgement while offline, then reconnects with bounded backoff and jitter. Watch `gateway status`; correct Slack configuration/access outside Mosscap, then reload only after the file is complete:

```sh
mosscap gateway reload --request-id reload-1
```

A failed reload retains the prior verified configuration/runtime. A successful reload verifies the candidate before intake switches; work already started uses its prior snapshot.

For shutdown, interrupt the foreground gateway and wait for it to exit. It stops/drains Socket intake before closing SQLite, so committed tasks and effects remain available after the next `mosscap gateway run`. On restart, leftover submitted effects become ambiguous; inspect them rather than assuming they were not sent.

## Optional disposable-workspace smoke checklist

This is exploratory evidence only, never a replacement for deterministic tests:

1. Use a disposable workspace/channel and disposable app credentials kept outside source control.
2. Verify app identity, channel membership, scopes, and one trusted user.
3. Send one direct mention and confirm one task/source record before Slack acknowledgement behavior is observed.
4. Exercise one read and one harmless reaction; inspect status and doctor.
5. Simulate a reconnect and a controlled gateway shutdown/restart; inspect any ambiguous effect.
6. Revoke the disposable tokens and delete local disposable state afterwards. Do not retain message bodies, event captures, or tokens in test evidence.
