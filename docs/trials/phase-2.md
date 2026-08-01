# Phase 2 manual trial: Slack task through Pi

**Status:** run after the Phase 2 automated gate passes

## Purpose

Prove the MVP workflow in one ordinary, human-started interactive Pi session:

```text
trusted Slack mention → Mosscap task → visible Pi turn
                      → explicit Slack reply → explicit task completion
```

This is a usability trial, not a production reliability or security certification.

## Safety and prerequisites

Use:

- a disposable Slack development workspace/channel;
- locally supplied Slack credentials that are never pasted into chat, logs, screenshots, or committed files;
- a safe trial repository in which the selected Pi model may use its normal tools;
- Mosscap built from the revision under test and available on `PATH`;
- Pi `@earendil-works/pi-coding-agent` 0.83.0; and
- the source extension at `pi-extension/src/index.ts`.

Before starting:

1. complete the Phase 1 Slack app setup and smoke prerequisites;
2. run the complete automated Phase 2 validation gate;
3. confirm the trial Slack user is in Mosscap's trusted-user set;
4. confirm the trial repository contains no secrets or unsafe unfinished work;
5. start with no second opted-in Pi process; and
6. choose harmless work that can be inspected and completed during the trial.

Do not deliberately induce uncertain delivery, process crashes, competing Pi sessions, token rotation, or Slack redelivery in this manual run. Those are automated/deferred scenarios.

## Record before the trial

Record only non-sensitive identifiers:

- date/time:
- Mosscap revision:
- Go version:
- Node and pnpm versions:
- Pi version:
- trial repository path or safe label:
- Slack workspace/channel IDs if useful:

Do not record tokens or raw Slack message/task text.

## Procedure

### 1. Start Mosscap

Configure and start the gateway using the Phase 1 runbook.

Verify:

```bash
mosscap gateway status --json
mosscap trust list
```

Expected:

- gateway is healthy enough for the trial;
- Slack is connected;
- no Pi session is active; and
- the test Slack user is trusted.

### 2. Start a human-owned Pi session

From the safe trial repository, start Pi with the source extension:

```bash
pi -e /absolute/path/to/mosscap/pi-extension/src/index.ts
```

Ensure this is a persisted interactive session. The human chooses the working directory, model, thinking level, and when Pi exits.

Expected before opt-in:

- the extension has not connected to the gateway;
- no tasks are delivered; and
- Pi otherwise behaves normally.

### 3. Opt in explicitly

In Pi, run:

```text
/mosscap
/mosscap status
```

Expected:

- `/mosscap` reports connected and shows a bounded backlog count;
- status identifies the current Pi session safely;
- the footer/status is compact; and
- no model turn is triggered merely by status output.

### 4. Send one trusted Slack task

From the trusted Slack user in the configured channel, directly mention the bot with one harmless, specific task suitable for the trial repository.

Expected:

- Mosscap creates exactly one `open` task;
- Pi displays one visible Mosscap custom message;
- the message includes task/source identifiers and clearly delimits untrusted Slack text;
- Pi starts a model turn without a second human prompt; and
- no hidden message or terminal keystroke injection is used.

Record task ID, version, delivery ID, and delivery state only.

### 5. Let Pi use the existing CLI

Allow or instruct Pi to use ordinary shell tools and the existing Mosscap CLI. The successful path should include logically equivalent commands:

```bash
mosscap task show TASK --json
mosscap slack thread THREAD_TS --json
mosscap slack send --thread THREAD_TS --task TASK --text "..." --request-id UNIQUE_ID
mosscap task done TASK --request-id UNIQUE_ID
```

Expected:

- the thread read targets only the configured channel;
- Slack reply is explicit and has a durable effect result;
- task state changes only after the explicit task command;
- the reply itself does not complete the task; and
- the model can understand and use the CLI without a custom Mosscap tool.

Inspect from the human side before allowing destructive or inappropriate actions, as with any normal Pi session.

### 6. Verify durable delivery and task state

Run:

```bash
mosscap delivery list --state persisted --json
mosscap delivery inspect DELIVERY --json
mosscap task show TASK --json
```

Expected:

- one persisted delivery exists for the task/version/session;
- delivery metadata contains no task transcript or session-file path;
- task is `done`; and
- Slack operation and task transition remain independent records.

### 7. Verify reconnect deduplication

Restart only the gateway while leaving the same opted-in Pi extension instance running. Wait for its bounded reconnect, then run:

```text
/mosscap status
```

Expected:

- extension reconnects to the restarted gateway;
- persisted session keys reconcile before polling;
- the completed task/version is not inserted again; and
- no second model turn occurs for that delivery.

If restarting the gateway necessarily ends the extension connection for longer than expected, allow the documented reconnect window; do not re-run `/mosscap` unless the Pi extension instance itself was reloaded/restarted.

### 8. Verify explicit off

In Pi, run:

```text
/mosscap off
/mosscap status
```

Then create one harmless local open task or send another eligible Slack mention.

Expected:

- status reports off;
- no connection/reconnect remains active;
- new work is durable in Mosscap but does not appear in Pi; and
- Pi continues running normally.

Run `/mosscap` again only if useful to verify that the waiting task is then delivered. Opt-in must remain explicit.

## Pass criteria

The trial passes only when:

- one trusted mention produces one visible task delivery and automatic model turn;
- Pi successfully uses the ordinary CLI to inspect context, reply, and mark done;
- one persisted delivery record and JSONL key exist;
- reconnect does not duplicate the task version;
- off prevents delivery until explicit re-opt-in;
- no automatic task transition or Slack action occurs; and
- no credentials, raw envelopes, or private session paths appear in output or durable delivery metadata.

Code may be declared complete pending manual trial when disposable credentials/model access are unavailable. Phase 2 is complete only after this runbook passes.

## Stop conditions

Stop the trial and record a failure if:

- work arrives before `/mosscap`;
- an ephemeral session is accepted;
- task content is hidden or not clearly delimited;
- one version is inserted twice without explicit requeue;
- uncertain work is automatically retried;
- Slack activity or model prose changes task state;
- a second Pi connection silently replaces the first;
- Pi or Mosscap exposes credentials/session paths; or
- Mosscap attempts to start, stop, or reconfigure Pi.

## Findings

After the trial, update [`docs/plans/phase-2-findings.md`](../plans/phase-2-findings.md), especially:

- whether explicit opt-in felt natural;
- whether the visible message gave enough context;
- whether existing CLI commands were sufficient for the model;
- whether polling latency was noticeable;
- whether delivery IDs/states helped recovery;
- whether single-session behavior was adequate; and
- which deferred SPEC requirements should be restored, changed, or removed.
