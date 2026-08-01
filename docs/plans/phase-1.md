# Mosscap Phase 1: Slack prototype

**Status:** ready for an agentic implementation team

## 0. Authority and working rule

This plan is the authoritative completion contract for Phase 1.

[`SPEC.md`](../SPEC.md) is a design hypothesis and reference. It is not the Phase 1 completion contract. If the SPEC, the archived production backlog, and this plan differ, **this plan wins**.

The original production-grade plan is preserved as [`phase-1-production.md`](phase-1-production.md). Do not implement work from it unless this plan explicitly requires that work.

The governing rule is:

> Build enough to exercise the proposed Slack workflow and learn from it. Do not turn prototype gaps into production-hardening projects.

Intentional SPEC deviations are learnings, not defects. Record useful findings in `docs/plans/phase-1-findings.md` rather than expanding scope during implementation.

## 1. Prototype question

Phase 1 answers this question:

> Can Mosscap reliably turn a trusted Slack mention into one durable task and support the proposed explicit Slack CLI, including a durable threaded reply?

The representative workflow is:

1. an operator starts the gateway with one Slack workspace, bot, and channel;
2. a trusted human directly mentions the bot;
3. Mosscap commits one `open` task before acknowledging the Slack envelope;
4. `task show` exposes the task's Slack source and thread timestamp;
5. the operator reads the thread and sends an explicit reply with the Slack CLI; and
6. Mosscap records the write before making one Slack API call.

A real development-workspace smoke test must exercise this workflow. The prototype is intended to reveal changes needed in the SPEC and CLI—not to certify production readiness.

## 2. Existing foundation: reuse, do not perfect

Checkpoint `760cfddd` already added approximately 8,700 lines of passing Slack foundation code:

- SDK-free Slack interfaces and deterministic Web/Socket fakes;
- real `slack-go` Web and Socket Mode adapters;
- bounded DTOs, validation, error classification, and one-shot acknowledgements;
- schema migration 0003;
- Slack source, source-event, file-metadata, and effect repositories; and
- durable effect preparation, transitions, reconciliation, and retry primitives.

Treat this code as a **frozen, passing substrate**.

Agents must not perform a comprehensive SPEC audit, redesign the foundation, remove already-implemented behavior, or add hardening merely because the archived plan asks for it. Change foundation code only when it blocks the prototype, fails a focused test, or contains a directly encountered correctness defect.

Existing foundation behavior may exceed this plan—for example, it may persist bounded ignored-event or file metadata. It can be reused as-is. Such behavior does not create a requirement to expose, integrate, or comprehensively test that feature.

## 3. Non-negotiable prototype invariants

Only these Phase 1 invariants are release-blocking:

1. **Single authority boundary:** all Slack reads and writes use the configured workspace/channel. No public command accepts a workspace or channel selector.
2. **Strict admission:** a new task requires the configured workspace and channel, a currently trusted immutable user ID, a non-bot sender, and a literal direct `<@BOT_USER_ID>` mention.
3. **Commit before acknowledgement:** an eligible event or known duplicate is acknowledged only after its durable result commits. A failed commit is not acknowledged.
4. **One source, one task:** redelivery of one source event/message cannot create a second task.
5. **Intent before I/O:** every Slack write commits its exact effect intent before its API call.
6. **No uncertain automatic retry:** a possibly submitted write becomes `ambiguous` and is not automatically retried.
7. **Secret-safe operation:** tokens and authorization headers do not enter SQLite, protocol output, status, logs, or test diagnostics.
8. **Task independence:** Slack reads and writes never change task state.

Do not add further release-blocking invariants without updating this plan deliberately.

## 4. Required product behavior

### 4.1 Startup and Slack runtime

For a Slack-enabled production run, require lexically valid non-empty:

- app token;
- bot token;
- team/workspace ID;
- bot user ID; and
- channel ID.

At startup:

1. create the existing real Web and Socket adapters;
2. verify bot authentication, configured team, configured bot identity, and configured channel access;
3. reconcile any leftover `submitted` effects to `ambiguous` before accepting writes;
4. open one Socket Mode stream; and
5. receive events until shutdown.

Use a simple bounded reconnect loop with the existing configured backoff and jitter. Basic `connecting`, `connected`, and `reconnecting` state is sufficient. Keep tokens out of errors and status.

Phase 0 tests may inject a disabled/fake Slack runtime. Local-only `setup` remains valid. Live configuration changes may require a gateway restart.

Not required: candidate runtime preparation, failure-atomic Slack connection swapping, connection draining across reload, exact scope discovery, detailed liveness telemetry, or a full Slack `doctor` implementation.

### 4.2 Initial mention admission

Support initial `app_mention` and ordinary message envelopes. For each bounded, identifiable initial event:

1. normalize it through the existing Slack boundary DTOs;
2. pass the receipt-time team/channel/bot policy to the store;
3. use the current committed trusted-user set;
4. commit the canonical result through the existing source repository; and
5. acknowledge only after a successful commit.

A committed duplicate is acknowledged and returns the existing canonical result. A store/commit error leaves the envelope unacknowledged so Slack may redeliver it.

Only an admitted event must create a task. Ignored-event persistence and operator inspection are not prototype requirements. If the existing repository persists bounded ignored results as part of the simplest coordinator path, retain that behavior without adding UI or broad tests.

Unsupported edit/delete events may be acknowledged and ignored. Do not wire source edits, deletion, stale ordering, or incoming-file behavior into the runtime for this phase.

### 4.3 Slack source on task output

Expand task read output so a Slack-origin task can show:

- source kind;
- team ID;
- channel ID;
- immutable author user ID;
- source message timestamp; and
- thread timestamp.

Local tasks retain their current output shape and semantics. Message text must not appear in logs merely because source metadata is joined.

The CLI does not infer a write target from `--task`. To reply to a task's source, the operator reads its thread timestamp and explicitly supplies `--thread`.

### 4.4 Slack reads

Implement bounded gateway operations and CLI commands for:

- configured-channel history; and
- one configured-channel thread.

Use the existing Web interface. Return the existing bounded Slack message DTOs. Do not persist read responses. Reject malformed limits/timestamps through typed protocol errors.

A history limit must have a conservative default and an upper bound. Thread reads use a conservative fixed/default bound. Pagination beyond one bounded page is not required for the prototype.

### 4.5 Slack writes

Implement these writes through one shared effect service:

- send;
- edit;
- delete;
- react; and
- unreact.

For each request:

1. validate and normalize the operation;
2. bind it to the active configured team/channel;
3. preserve an optional task relationship as audit metadata only;
4. call the existing `PrepareSlackEffect` with the request ID;
5. transition the effect to `submitted` before Slack I/O;
6. make exactly one synchronous Web API call; and
7. commit `confirmed`, `failed`, or `ambiguous` before returning.

Use the existing adapter-level error classification. Conclusive success is `confirmed`; conclusive rejection is `failed`; timeout, cancellation, disconnect, malformed success response, or uncertainty after submission is `ambiguous`.

A replayed request ID returns the same effect and must not submit it again. Never transition `submitted` back to `prepared`. Do not automatically execute leftover `prepared` records during this prototype.

Do not perform a separate preflight read for edit/delete/react/unreact. The configured channel is always supplied to Slack, so a timestamp that does not belong to it fails without broadening authority.

No background executor, worker pool, claims beyond the existing single-effect transition, cancellation, automatic retry, or public effect-recovery workflow is required.

### 4.6 Basic status and logging

Status needs only:

- Slack state (`disabled` for injected/local tests, otherwise `connecting|connected|reconnecting`);
- configured team, bot, and channel IDs;
- last safe diagnostic code if available; and
- effect counts by state if already easy to expose.

Do not block the prototype on detailed admission counters, timestamps, backoff telemetry, or expanded doctor checks.

Logs may contain bounded event, task, effect, and request IDs and enum-like result codes. They must not contain tokens, raw Slack payloads, message/effect/task text, or authorization headers.

## 5. Required public CLI

Implement the Slack CLI surface proposed by the SPEC:

```text
mosscap slack history [--limit N] [--json]
mosscap slack thread THREAD_TS [--json]
mosscap slack send [--thread THREAD_TS] (--text TEXT|--text-file PATH) [--task TASK] [--request-id ID] [--json]
mosscap slack edit MESSAGE_TS (--text TEXT|--text-file PATH) [--task TASK] [--request-id ID] [--json]
mosscap slack delete MESSAGE_TS --reason TEXT [--task TASK] [--request-id ID] [--json]
mosscap slack react MESSAGE_TS EMOJI [--task TASK] [--request-id ID] [--json]
mosscap slack unreact MESSAGE_TS EMOJI [--task TASK] [--request-id ID] [--json]
```

Rules:

- preserve the current strict CLI and protocol parsing style;
- `--text` and `--text-file` are mutually exclusive and exactly one is required;
- no command accepts team/channel selectors;
- `--thread` explicitly chooses the send thread;
- `--task` records a relationship but never derives or authorizes a target;
- mutating human/JSON output includes request ID, effect ID, state, operation, and target;
- an ambiguous result is visible and exits with the existing typed ambiguous/conflict behavior; and
- read output may contain message text because the user explicitly requested it, but diagnostics may not.

Add additive protocol v1 commands/capabilities for these operations. Public effect commands (`effect list`, `effect inspect`, and `effect retry`) are deferred even though the store primitives already exist.

## 6. Explicitly deferred

The following are not Phase 1 completion work:

- source message edits and deletions;
- stale source-event ordering integration;
- incoming file metadata integration or any file bytes;
- source-event list/inspection CLI;
- effect list, inspection, retry, or cancellation CLI;
- generic background effect execution;
- outbound file upload;
- target preflight reads for mutations;
- live Slack config reload and runtime swapping;
- exhaustive scope/subscription verification;
- full Slack status and doctor behavior;
- automatic replies or task transitions;
- Pi delivery/extension behavior;
- comprehensive fault matrices, fuzz campaigns, broad security audits, or repeated independent correction cycles;
- real multi-workspace or multi-channel behavior; and
- production release certification.

Do not delete already-built foundation support for these items. Simply do not integrate or expand it.

## 7. Agent-team execution plan

### 7.1 Ownership rules

Assign one **integration owner**. Only that owner should make broad changes to conflict-heavy gateway routing/startup files, especially `internal/server/server.go` and gateway construction. Agents should avoid opportunistic refactors.

Suggested workstreams:

#### Agent A — ingestion and runtime packages

Own new focused packages such as `internal/slackingest` and `internal/slackruntime` plus their tests.

Deliver:

- normalized initial-event to store coordination;
- commit-before-ack behavior;
- duplicate handling;
- simple Socket receive/reconnect loop;
- basic runtime state; and
- fake-based package tests.

Do not integrate edits, deletion, files, reload swapping, or rich diagnostics.

#### Agent B — effect service

Own a new `internal/effect` package plus tests.

Deliver:

- common validation/preparation/execution path;
- mapping for all five Web writes;
- durable state transitions and request replay;
- outcome classification;
- startup submitted-to-ambiguous reconciliation hook; and
- table-driven fake-Web tests.

Do not build public effect recovery commands, a background queue, or mutation preflight reads.

#### Agent C — protocol and CLI surface

Own `internal/protocol/**` and `internal/app/**` changes plus focused tests.

Deliver:

- strict params/results for reads and writes;
- additive capabilities;
- the exact CLI in section 5;
- human and JSON output; and
- Slack source fields in task output.

Coordinate DTO/service boundaries with the integration owner before editing central files. Do not add deferred commands to help output.

#### Integration owner — gateway composition

Deliver:

- production Slack adapter construction and startup verification;
- runtime/effect service injection into the server;
- protocol dispatch for all required operations;
- configured-channel enforcement;
- startup effect reconciliation;
- basic status; and
- shutdown that closes the Socket transport before the store.

Resolve interfaces at the boundary rather than merging package responsibilities into `server.go`.

#### Final test/documentation owner

After integration stabilizes, deliver:

- focused cross-package fake-Slack tests;
- the real-workspace smoke checklist;
- operator setup prerequisites;
- updates to `docs/plans/phase-1-findings.md`; and
- one final scope review against this plan, not the full SPEC.

### 7.2 Dependency order

1. **Baseline:** run `go test ./...`; do not conduct a fresh foundation audit.
2. **Freeze contracts:** integration owner and Agents A–C agree on minimal service and protocol DTO boundaries.
3. **Parallel implementation:** Agents A, B, and C work behind those boundaries without editing central routing concurrently.
4. **Integration:** the integration owner composes services and resolves DTO mismatches.
5. **Focused correction:** fix only failures in this plan's workflow or existing regression suite.
6. **Automated gate:** run section 9 validation.
7. **Manual gate:** an operator supplies disposable credentials locally and runs section 8.2.
8. **Findings:** record observed SPEC/UX changes before declaring Phase 1 complete.

Every agent handoff must state:

- files changed;
- focused tests run;
- remaining blockers within this plan;
- useful prototype findings; and
- any tempting production-hardening work deliberately left deferred.

## 8. Acceptance tests

### 8.1 Automated fake-Slack gate

The required focused tests are:

1. **Admission:** one trusted direct mention in the configured workspace/channel creates one `open` Slack task and is acknowledged after the store result exists.
2. **Redelivery:** two deliveries of the same event/source create one task; both are acknowledged only after the canonical durable result is available.
3. **Commit failure:** an injected store/commit error produces no acknowledgement.
4. **Rejection:** representative untrusted and unmentioned events create no tasks. Durable inspection of those events is not required.
5. **Source DTO:** `task show` returns source message/thread timestamps for Slack tasks and remains compatible with local tasks.
6. **Reads:** history and thread use only the configured channel, enforce bounds, and do not persist returned messages.
7. **Write mapping:** table-driven tests prove send/edit/delete/react/unreact call the correct fake Web method once with the configured channel and normalized input.
8. **Write durability:** one representative write proves the effect is `submitted` before the fake observes the API call and reaches `confirmed` on success.
9. **Uncertainty:** one representative post-submit timeout reaches `ambiguous`; startup reconciliation does not make another API call.
10. **Request replay:** replaying one request ID returns the same effect and makes no second API call.
11. **CLI/protocol:** parsing and output tests cover every required command, JSON mode, mutually exclusive text inputs, invalid timestamps/limits, and absence of channel/workspace flags.
12. **Regression:** all existing Phase 0 and foundation tests continue to pass except intentional additive status/DTO expectation updates.

Do not multiply every failure scenario across every command. Use table tests for operation mapping and one representative command for crash/ambiguity semantics.

### 8.2 Real Slack smoke gate

Use a disposable development workspace/channel and locally supplied credentials. Never put tokens in chat, source, committed files, or test output.

1. Configure Socket Mode, relevant message/app-mention subscriptions, and scopes needed for history, chat writes, reactions, users, and channel access.
2. Run setup, add one trusted immutable user ID, and start the gateway.
3. Confirm basic connected status.
4. Send one direct mention from the trusted user.
5. Verify exactly one open task and inspect its source/thread timestamp.
6. Redeliver or repeat the same captured fake event only in the automated test; do not require inducing Slack redelivery manually.
7. Run `slack history` and `slack thread`.
8. Post a threaded response with `slack send --thread ... --task ...`.
9. On disposable test messages, run edit, react, unreact, and delete once each.
10. Confirm command output shows durable effect IDs/states and no Slack operation changed task state.
11. Restart the gateway once and confirm it reconnects.
12. Record usability and SPEC findings without recording message bodies or secrets.

The implementation team may declare **code complete, pending operator smoke** when credentials are unavailable. Phase 1 itself is complete only after the smoke gate passes.

## 9. Completion gate

Run one final integration review and:

```text
gofmt -w <changed Go files>
go test ./...
go test -race ./...
go vet ./...
go build ./cmd/mosscap
git diff --check
```

`govulncheck`, exhaustive threat review, schema re-audit, broad fault injection, and multiple independent review/correction cycles are deferred from this prototype gate.

Phase 1 is complete when:

- the automated tests in section 8.1 pass;
- the real Slack smoke test in section 8.2 passes;
- the validation commands above pass;
- no deferred item was accidentally made a completion dependency; and
- `docs/plans/phase-1-findings.md` captures what the prototype taught us about the SPEC and CLI.

## 10. Findings to capture

The prototype report should answer:

1. Was Slack app setup and the required scope set understandable?
2. Did strict direct-mention admission match actual Slack event behavior?
3. Did one mention produce the task text/source shape users expected?
4. Was finding a thread timestamp through `task show` and supplying `--thread` intuitive?
5. Did the full Slack CLI feel coherent, or should commands become task/source-oriented?
6. Were durable effect IDs and `confirmed|failed|ambiguous` states useful to users?
7. Which deferred reliability requirements became obviously necessary?
8. Which production-plan requirements appeared costly without improving the prototype workflow?
9. What should change in `docs/SPEC.md` before Phase 2 or production hardening?
