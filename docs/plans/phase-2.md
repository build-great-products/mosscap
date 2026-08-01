# Mosscap Phase 2: Pi delivery MVP

**Status:** ready for an agentic implementation team after Phase 1

## 0. Authority and working rule

This plan is the authoritative completion contract for Phase 2.

[`docs/SPEC.md`](../SPEC.md) remains a design hypothesis and reference. If it asks for leadership generations, takeover, leases, notifications, packaging, files, or recovery behavior beyond this plan, **this plan wins for the MVP**.

The governing rule is:

> Complete one useful Slack → Mosscap → Pi → Slack workflow in a human-owned interactive session. Do not turn the manual trial into production delivery infrastructure.

Intentional deviations are prototype learnings, not defects. Record them in [`phase-2-findings.md`](phase-2-findings.md) rather than silently expanding scope.

Phase 2 begins only after the Phase 1 prototype gate passes. Existing Phase 0/1 behavior is a substrate; review or change it only when it blocks this MVP or fails a focused regression test.

## 1. MVP question and trial outcome

Phase 2 answers:

> Can a human explicitly connect one ordinary Pi session to Mosscap, receive trusted Slack work visibly, let Pi act with its normal tools, and recover honestly enough for a small manual trial?

The required end-to-end journey is:

1. a human starts the gateway and one persisted interactive Pi session;
2. the human runs `/mosscap` to opt that Pi runtime in;
3. a trusted Slack mention creates one durable task;
4. the task appears as a visible Pi custom message and triggers a model turn;
5. Pi uses the ordinary `mosscap` CLI to read the Slack thread and send a reply;
6. Pi explicitly marks the task `done` through the CLI;
7. reconnecting does not insert that task version again; and
8. `/mosscap off` stops further delivery without stopping Pi or changing tasks.

This is a text-only, single-Pi MVP. It is not a production release gate.

## 2. Non-negotiable MVP invariants

Only these delivery invariants are release-blocking:

1. **Human-owned Pi:** Mosscap never starts, stops, selects, resets, or configures Pi, its model, thinking level, working directory, or tools.
2. **Explicit opt-in:** no extension connection or work delivery begins until the user runs `/mosscap` in that Pi runtime.
3. **Persisted sessions only:** `/mosscap` refuses an in-memory/ephemeral Pi session.
4. **One active Pi connection:** the gateway accepts one opted-in extension connection and rejects a second until the first disconnects.
5. **Visible work:** delivery uses a Pi custom message with `display: true`; hidden context injection does not count.
6. **Stable session-wide identity:** the delivery key is derived from task ID, task version, and Pi session ID and is stored in the custom message details.
7. **Evidence before success:** the gateway marks a delivery `persisted` only after the extension observes the matching message in the persisted Pi session entries.
8. **Honest uncertainty:** work that may have begun insertion but lacks persistence evidence becomes `uncertain` and is never automatically repeated.
9. **No duplicate current version:** an already persisted task version is not delivered again to the same Pi session, including after branch navigation or reconnect.
10. **Explicit task state:** message insertion, model output, Slack activity, and `/mosscap off` do not change task state.
11. **No second command authority:** Pi uses the existing `mosscap` CLI through normal shell tools; the extension does not register Mosscap-specific model tools.
12. **Untrusted text boundary:** task text is visibly delimited from Mosscap metadata/instructions and is never interpreted as a local protocol command by the extension.

Do not add further release-blocking invariants without deliberately updating this plan.

## 3. Deliberate MVP simplifications

The MVP supports one connected Pi process through one persistent Unix-socket connection. The claim exists only in gateway memory and is released when that connection closes.

It deliberately has no:

- durable leader singleton;
- leader generation or stale-generation fencing;
- takeover command;
- heartbeat or leader expiry;
- expiring delivery leases;
- server-pushed notifications;
- automatic restoration of opt-in after Pi reload/session replacement/process restart;
- multi-Pi fairness or arbitration; or
- claim that an already-running old Pi process can be controlled.

A gateway restart forgets the in-memory claim. An extension that remains opted in reconnects and claims the slot again. If two extension instances race, the first accepted connection wins and the other receives a typed conflict.

## 4. Delivery behavior

### 4.1 Eligibility

For one Pi session, the gateway makes these tasks eligible:

- state is `open` or `working`;
- source may be `local` or `slack`;
- task version is the current version; and
- that `(task ID, version, Pi session ID)` has no persisted delivery and no unresolved delivery without an explicit requeue.

Order eligible work by oldest `updated_at`, then task ID. Deliver one item at a time.

A new task version gets a new stable delivery key and becomes eligible. `done` tasks are not delivered. Returning a task to `open` or `working` does not redeliver an already persisted unchanged version.

On opt-in, show the current eligible backlog count before polling begins.

### 4.2 Minimal state machine

Use only:

```text
pending → inserting → persisted
                ├──→ uncertain
                └──→ rejected
```

Meanings:

- `pending`: a durable attempt exists and may be safely offered again because insertion has not started;
- `inserting`: Pi has begun processing the custom message but persistence has not yet been confirmed;
- `persisted`: the matching custom-message entry and key were observed in the session;
- `uncertain`: insertion may have happened but durable session evidence is absent;
- `rejected`: the extension conclusively rejected invalid/unsupported delivery content before successful insertion.

There is no `leased` state in the MVP. There are no lease tokens or expirations.

On extension connection loss, mark that connection's `inserting` attempts `uncertain`; leave `pending` attempts safe to offer again. On gateway startup, reconcile every leftover `inserting` attempt to `uncertain` before accepting an extension.

`uncertain` and `rejected` block automatic redelivery. Explicit requeue creates a new linked `pending` attempt and preserves its predecessor.

### 4.3 Delivery key and record

Derive a bounded stable key with a domain-separated SHA-256 over:

```text
mosscap-pi-delivery-v1\0TASK_ID\0TASK_VERSION\0PI_SESSION_ID
```

Represent it as a stable lowercase `sha256:` value. The extension treats it as opaque.

Add schema migration 0004 with a focused delivery table. It needs only:

- delivery ID (`dlv_…`);
- task ID and version;
- Pi session ID;
- stable delivery key;
- optional predecessor delivery ID;
- ephemeral connection ID used to reconcile disconnects;
- state;
- bounded diagnostic/result code and optional bounded operator requeue reason;
- offered, inserting, persisted/completed, created, and updated timestamps.

Do not persist task text, Pi transcript content, session-file paths, model/provider identity, credentials, or arbitrary exceptions in delivery rows.

The repository must atomically:

- select/create or replay one pending attempt for the next eligible task;
- validate current task version before insertion starts;
- transition pending → inserting;
- transition inserting/uncertain → persisted when exact session evidence arrives;
- mark inserting attempts for a dead connection uncertain;
- list and inspect bounded delivery metadata; and
- explicitly requeue only uncertain/rejected attempts as linked pending attempts.

Use partial indexes/transactional checks to avoid concurrent active attempts for one task version/session. An explicit retry may reuse the stable delivery key but has a new delivery ID.

### 4.4 Session-wide reconciliation

At `/mosscap` and after reconnect, the extension scans:

```typescript
ctx.sessionManager.getEntries()
```

Scan all entries, not only `getBranch()`. Accept only well-formed Mosscap custom-message entries whose details contain the expected schema version, delivery ID, delivery key, task ID, task version, and current Pi session ID.

Send the bounded set of observed `(delivery ID, delivery key)` pairs to the gateway before polling. The gateway may resolve an exact matching:

```text
pending|inserting|uncertain → persisted
```

The `pending` case covers a custom message that reached Pi when the connection failed before the insertion-start report committed. Exact persisted-session evidence makes this safe. This is conclusive late evidence, not a retry. Reconciliation completes before new work is offered.

Cap the reported key count and frame size. If a session exceeds the cap, fail visibly rather than silently treating an incomplete scan as authoritative.

## 5. Pi extension behavior

### 5.1 Supported Pi API

Target exactly:

```text
@earendil-works/pi-coding-agent 0.83.0
```

The MVP relies on:

- `ctx.sessionManager.isPersisted()`;
- `getSessionId()`, `getSessionFile()`, and `getEntries()`;
- `pi.registerCommand()`;
- `pi.sendMessage()`;
- `message_start` and `message_end`;
- `ctx.isIdle()`;
- `ctx.ui.notify()` and `ctx.ui.setStatus()`; and
- `session_shutdown`.

Only interactive TUI mode is supported for the manual trial. Other Pi modes should return a clear unsupported message from `/mosscap` rather than partially opting in.

Start no socket, timer, poller, or watcher in the extension factory. Start long-lived resources only after `/mosscap`, and close them idempotently on `/mosscap off` and `session_shutdown`.

### 5.2 Extension commands

Implement:

```text
/mosscap
/mosscap status
/mosscap off
```

`/mosscap`:

1. rejects unexpected arguments (`--takeover` is not supported);
2. requires TUI mode and a persisted session;
3. scans session-wide Mosscap delivery entries;
4. opens the private gateway socket and negotiates the delivery capability;
5. claims the one in-memory extension slot with session ID and observed keys;
6. shows connection/backlog status; and
7. begins sequential polling.

`/mosscap status` shows only compact safe metadata:

- off/connecting/connected/reconnecting/delivering/error;
- current Pi session ID;
- eligible backlog count;
- current delivery ID/task ID when active; and
- uncertain/rejected count.

`/mosscap off` stops polling/reconnect, releases/closes the connection, clears the footer status, and leaves tasks/deliveries/Pi untouched.

Running `/mosscap` twice in the same extension instance is idempotent. A second Pi session receives a conflict naming only the active session ID and warning that the old Pi process is not controlled.

### 5.3 Connection and polling

Implement the existing 32-bit big-endian length-prefixed JSON protocol directly with `node:net`. Reuse protocol version 1 and capability negotiation. Enforce the configured/default frame bound before allocation and parse strict bounded DTOs.

Keep one request in flight at a time on the persistent socket. While opted in:

- poll for one eligible delivery about once per second;
- wait for the current delivery to resolve before polling another;
- use bounded per-request deadlines; and
- reconnect with a simple bounded backoff after socket/gateway loss.

The server's existing generic 15-second idle read deadline must not terminate an opened extension connection while Pi is legitimately busy for longer than 15 seconds. After `delivery.open`, either use a harmless bounded transport keepalive or disable that idle deadline for the connection and rely on socket closure/gateway shutdown. Do not reinterpret this as a durable leader heartbeat or expiry mechanism.

Automatic reconnect applies only while the same extension instance remains opted in. Pi `/reload`, `/new`, `/resume`, `/fork`, process exit, and extension re-instantiation reset opt-in and require `/mosscap` again.

Do not add server push, notification queues, a second framing protocol, or extension access to SQLite.

### 5.4 Visible message and Pi event timing

Send one custom message with:

```typescript
{
  customType: "mosscap.task",
  display: true,
  content: "...visible bounded task message...",
  details: {
    schemaVersion: 1,
    deliveryId,
    deliveryKey,
    taskId,
    taskVersion,
    sessionId
  }
}
```

Visible content includes:

- task ID/version/state;
- local or Slack origin;
- Slack immutable author and message/thread timestamp when applicable;
- concise relevant CLI examples;
- a statement that task state changes are explicit; and
- complete bounded task text inside unmistakable `BEGIN/END UNTRUSTED TASK TEXT` delimiters.

Do not include tokens, raw Slack envelopes, filenames/files, historical task text, model instructions from configuration, or arbitrary diagnostics.

Delivery timing uses Pi 0.83.0 behavior:

1. receive a durable `pending` delivery;
2. call `pi.sendMessage()` with `triggerTurn: true`;
3. if Pi is busy, use `deliverAs: "followUp"` so work waits until the agent settles;
4. on the matching custom `message_start`, report `inserting` before allowing delivery processing to continue;
5. on matching `message_end`, return from the event handler, wait until a later event-loop turn, and scan `sessionManager.getEntries()`;
6. report `persisted` only when the exact custom-message details are now present; and
7. if the message cannot be conclusively validated, report rejected when failure is provably pre-insertion, otherwise uncertain.

Pi 0.83.0 calls extension `message_end` handlers before it appends the custom message to the session manager. Do not check and acknowledge persistence synchronously inside that handler; perform the post-handler scan on a later event-loop turn.

When idle, `triggerTurn: true` starts processing immediately. When busy, `followUp` waits for the current agent/tool work and does not steer or interrupt it.

### 5.5 Existing CLI, no custom tools

Do not register model-callable Mosscap tools. The message tells Pi that it may use ordinary shell commands such as:

```text
mosscap task show TASK --json
mosscap task working TASK --request-id ID
mosscap slack thread THREAD_TS --json
mosscap slack send --thread THREAD_TS --task TASK --text TEXT --request-id ID
mosscap task done TASK --request-id ID
```

These are suggestions, not automatic actions. The human/model may choose different work. Conversation text never updates SQLite by itself.

## 6. Gateway protocol and public CLI

### 6.1 Additive extension protocol

Keep protocol version 1 and add a negotiated `pi_delivery_mvp` capability. Define strict params/results for logically equivalent operations:

```text
delivery.open       claim active connection and reconcile observed keys
delivery.poll       get/replay one pending item or bounded empty result
delivery.inserting  confirm Pi message processing has begun
delivery.persisted  confirm exact session entry evidence
delivery.rejected   record conclusive rejection
delivery.close      release active connection
```

Names may be adjusted once before implementation, but freeze them before parallel work starts.

Every operation binds to the extension connection and opened Pi session. A client cannot mutate another session's delivery by supplying identifiers. Require request IDs for correlation; use repository idempotence/request deduplication for mutations so reconnect cannot create duplicate attempts or transitions.

The work DTO contains the current task text and source metadata needed for the visible message. It is bounded and exists only in protocol memory; delivery rows do not copy the text.

Connection close must release the in-memory active slot and reconcile its `inserting` attempts before another extension is accepted.

### 6.2 Delivery operator CLI

Implement the SPEC-inspired recovery surface:

```text
mosscap delivery list [--state pending|inserting|persisted|uncertain|rejected] [--json]
mosscap delivery inspect DELIVERY [--json]
mosscap delivery requeue DELIVERY --reason TEXT [--request-id ID] [--json]
```

Rules:

- list/inspect return bounded metadata, not transcript/task text;
- selectors may use the existing unambiguous `dlv_` prefix style;
- requeue is allowed only from `uncertain` or `rejected`;
- requeue creates a linked new `pending` attempt and never mutates away predecessor evidence;
- reason is bounded operator metadata;
- pending, inserting, and persisted attempts reject requeue; and
- requeue output makes possible duplicate insertion risk explicit.

Add basic delivery counts and active Pi session ID to gateway status when straightforward. Do not expand doctor for Phase 2.

## 7. TypeScript package and repository layout

Build the source-loaded extension under:

```text
pi-extension/
  README.md
  package.json
  pnpm-lock.yaml
  tsconfig.json
  src/
    index.ts          Pi registration and lifecycle only
    client.ts         bounded Unix-socket protocol client
    controller.ts     opt-in, polling, reconnect, delivery coordination
    messages.ts       visible message/details validation and key scan
    types.ts          strict local DTOs
  test/
```

Use:

- Node 26 native TypeScript execution and test runner;
- `node --test` directly on `.ts` tests;
- `pnpm`;
- `tsc --noEmit` for static type checking;
- erasable TypeScript syntax only; and
- exact Pi 0.83.0 development types.

Do not add `tsx`, ts-node, a transpiler, bundler, generated JavaScript, root JavaScript workspace, or runtime framework. Prefer Node built-ins. Keep Pi API calls behind a small adapter/controller boundary so most extension behavior can be tested without starting real Pi.

The trial loads source directly:

```bash
pi -e /absolute/path/to/mosscap/pi-extension/src/index.ts
```

Npm publication, `pi install`, package metadata, global auto-discovery, and broad Pi compatibility are deferred.

## 8. Explicitly deferred

The following are not Phase 2 completion work:

- more than one active Pi process;
- `/mosscap --takeover`;
- durable consumer/leader records;
- generations, fencing, heartbeats, leader expiry, lease tokens, or lease timers;
- server-pushed delivery notifications;
- automatic opt-in restoration;
- Slack source edits/deletions and live Pi diffs;
- incoming or outgoing files;
- custom model-callable Mosscap tools;
- automatic task state changes, Slack replies, or reactions;
- rich recovery summary messages;
- extension packaging/publication/global installation;
- non-TUI Pi modes;
- full Pi compatibility matrix;
- purge or automatic retention;
- expanded doctor/production telemetry;
- exhaustive concurrency, clock, process-crash, security, or fault matrices; and
- production readiness certification.

Do not implement these because they appear in the SPEC. Record evidence that one is necessary in the findings document.

## 9. Agent-team execution plan

### 9.1 Ownership

Assign one integration owner. That owner controls migration registration, `internal/server/server.go`, protocol contract convergence, and final cross-runtime integration. Avoid concurrent edits to central routing files.

#### Agent A — delivery migration and repository

Own the 0004 migration, delivery identity/types/repository, and focused store tests.

Deliver eligibility, transitions, startup/dead-connection reconciliation, key reconciliation, list/inspect, and linked requeue. Do not add production consumers/leaders/leases.

#### Agent B — extension protocol service

Own new Go delivery service/coordinator packages and package tests, but not central server routing.

Deliver one-active-connection semantics, session binding, poll coordination, disconnect reconciliation, and strict DTO mapping.

#### Agent C — TypeScript extension

Own only `pi-extension/**`.

Deliver the native-TypeScript protocol client, controller, Pi adapter, slash commands, status, visible messages, session-wide scanning, reconnect, and unit tests. Do not register tools or add package/distribution machinery.

#### Agent D — CLI and protocol DTOs

Own `internal/app/**` and the agreed DTO/validation portions of `internal/protocol/**` after contracts are frozen.

Deliver delivery list/inspect/requeue, help/JSON output, typed errors, capabilities, and focused tests.

#### Integration owner

Integrate migration/service/server connection lifecycle/status, resolve boundaries, and own built-binary or cross-runtime tests. Keep task/Slack behavior unchanged except for additive source fields already required by Phase 1.

#### Trial/documentation owner

Own [`../trials/phase-2.md`](../trials/phase-2.md), `pi-extension/README.md`, user-facing setup notes, and [`phase-2-findings.md`](phase-2-findings.md). Run one final scope review against this plan, not the entire SPEC.

### 9.2 Order

1. Finish and validate Phase 1.
2. Run the existing Go suite; do not reopen Phase 0/1 design review.
3. Integration owner freezes migration concepts, state enums, message details schema, protocol names, and DTOs.
4. Agents A–D implement in parallel within exclusive ownership.
5. Integration owner composes server connection lifecycle and services.
6. Run focused fake/controller integration and correct only MVP blockers/regressions.
7. Run the automated gate.
8. Run the manual trial.
9. Record findings and proposed SPEC changes.

Each agent reports changed files, focused tests, blockers within this plan, MVP findings, and production work deliberately left deferred.

## 10. Automated acceptance gate

Required focused tests:

1. **Eligibility:** open/working local and Slack tasks are returned oldest-first; done tasks are excluded.
2. **Version identity:** one task version persists once per session; a new version gets a new key and becomes eligible.
3. **Single active extension:** first open succeeds, second connection conflicts, closing first releases the slot.
4. **Pending safety:** disconnect before `message_start` leaves/replays the same pending attempt.
5. **Insertion uncertainty:** disconnect/startup after inserting produces uncertain and does not automatically poll it again.
6. **Late evidence:** an observed exact JSONL delivery ID/key promotes inserting/uncertain to persisted.
7. **Session-wide keys:** entries outside the active branch are found and suppress duplicate delivery.
8. **Explicit requeue:** uncertain/rejected requeue creates one linked pending successor; other states reject it.
9. **Visible message:** custom type, display flag, structured details, untrusted delimiters, source metadata, and bounded CLI hints are correct and secret-free.
10. **Pi timing:** idle delivery triggers a turn; busy delivery uses follow-up; insertion and persistence reports occur in order.
11. **Opt-in lifecycle:** no connection before `/mosscap`; ephemeral/non-TUI sessions refuse; `/off` and shutdown clean up; same-instance gateway loss reconnects.
12. **Protocol:** Node framing interoperates with golden Go frames, rejects oversized/invalid responses, correlates request IDs, and honors capabilities.
13. **Operator CLI:** list/inspect/requeue parsing, human output, JSON output, state filters, and typed errors work.
14. **Regression:** all existing Phase 0/1/foundation tests remain green except intentional additive schema/status/DTO updates.

Use one fake extension client plus a second connection-conflict test. Do not build production two-leader, lease-expiry, takeover, or exhaustive crash matrices.

## 11. Manual trial gate

Follow [`docs/trials/phase-2.md`](../trials/phase-2.md) using a disposable Slack development channel and an ordinary human-started Pi session in a safe trial repository.

The trial passes when:

- `/mosscap` explicitly opts in a persisted session;
- one trusted Slack mention becomes one visible Pi work message and starts a model turn;
- Pi uses the existing CLI to inspect the Slack thread, reply, and mark the task done;
- reconnect does not insert that version again;
- `/mosscap off` prevents a later task from being delivered until explicit re-opt-in; and
- no tokens, raw envelopes, or unbounded private content appear in logs/status/delivery metadata.

Busy-agent follow-up and uncertain recovery are automated-test requirements, not manual-trial requirements.

## 12. Validation and completion

Run:

```text
gofmt -w <changed Go files>
go test ./...
go test -race ./...
go vet ./...
go build ./cmd/mosscap
pnpm --dir pi-extension install --frozen-lockfile
pnpm --dir pi-extension test
pnpm --dir pi-extension typecheck
git diff --check
```

Phase 2 is complete when:

- automated acceptance tests pass;
- the manual trial passes;
- the validation commands pass;
- no deferred production feature became an accidental dependency; and
- [`phase-2-findings.md`](phase-2-findings.md) records what should change before further hardening.

`govulncheck`, npm publication tests, broad Pi compatibility, production security review, and repeated independent correction cycles are deferred from this MVP gate.
