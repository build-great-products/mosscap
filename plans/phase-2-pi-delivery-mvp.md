# Phase 2 implementation plan: Pi delivery MVP

**Status:** implementation-ready
**Base:** `origin/main` at `4d83838`
**Authority:** `docs/plans/phase-2.md`, except for the deliberate gate-order override below
**Execution:** three sequential, substantial Terra slices; one owner at a time

## 1. Outcome and deliberate gate override

Build the smallest working delivery path from the current Phase 0/1 substrate:

```text
committed open/working task -> gateway delivery record -> opted-in Pi extension
                            -> visible custom message -> persisted-session evidence
```

The extension remains explicitly human-controlled. It starts no resource before `/mosscap`, supports one persisted interactive Pi session, uses no custom model tools, and never changes task state or performs Slack activity by itself. Pi may use the existing `mosscap` CLI through ordinary shell tools.

The user has deliberately changed the implementation order:

- Phase 1 operator UX remediation and the Phase 1 live-workspace smoke are deferred until after Phase 2 code.
- The Phase 2 live Slack/Pi trial in `docs/trials/phase-2.md` is also deferred from this implementation gate.
- This is an accepted, non-blocking risk, not permission to report either manual gate as passed.
- Do not reopen, implement, or polish Phase 1 operator UX while doing this work.
- No Terra increment may use Slack credentials, contact live Slack, or run production `mosscap gateway run`. Automated work uses the existing fake Slack boundary or local tasks only.

Current main is an acceptable substrate because `go test ./...` passes and Phase 1 already has deterministic fake-Slack admission/operation coverage. The outstanding live setup, credential ergonomics, and adapter evidence may still expose trial blockers later. Record those only when the deferred trial is run; do not broaden Phase 2 now.

## 2. Current-tree evidence

The implementation should extend, not redesign, these seams:

- `internal/store/store.go` owns migration registration/checksums/fingerprints, task versioning, SQLite transactions, and mutation request replay. Schema is currently 3; migration 0004 must be append-only.
- `internal/store/phase1_queries.go` already joins current Slack source metadata without copying it into another durable row.
- `internal/protocol/frame.go` is the sole 32-bit big-endian framed transport. `internal/protocol/protocol.go` is strict protocol v1 and already accepts `client_kind: "extension"`.
- `internal/server/server.go` owns same-EUID connections, hello, central routing, store replacement, and shutdown. Its generic 15-second deadline is incompatible with an opened extension connection and must be changed only for that connection.
- `internal/app/phase1.go` provides the existing list/inspect/retry CLI pattern.
- `internal/status/status.go` already separates bounded status metadata from task text and secrets.
- `pi-extension/` contains no implementation or package tooling.
- Node `v26.5.0`, pnpm `11.17.0`, and Go `1.26.5` are available. npm publishes `@earendil-works/pi-coding-agent@0.83.0`; the locally installed Pi docs are 0.82.1, so actual 0.83.0 types are the implementation authority.

Planning baseline:

```text
go test ./...: pass
working tree: clean
schema: 3
protocol: 1
```

## 3. Frozen MVP contracts

Freeze these names and shapes before the first Terra slice. A later ambiguity stops the current slice for a narrow plan amendment; it does not justify leases, durable leaders, takeover, push, packaging, or broad polish.

### 3.1 Durable delivery model

Add `internal/store/migrations/0004_pi_delivery.sql` and one `pi_deliveries` table with only:

- `delivery_id` (`dlv_` plus the repository's 128-bit base64url identity);
- `task_id`, `task_version`, and bounded `pi_session_id`;
- lowercase `sha256:` delivery key;
- optional `predecessor_delivery_id`;
- optional gateway-generated connection ID;
- state: `pending|inserting|persisted|uncertain|rejected`;
- optional lower-snake-case diagnostic code and optional operator requeue reason;
- offered, inserting, completed, created, and updated microsecond timestamps.

Never store task text, transcript content, Pi session paths, model/provider data, credentials, raw exceptions, or visible-message content in this table.

Use these invariants:

- Stable key input is exactly `mosscap-pi-delivery-v1\0TASK_ID\0TASK_VERSION\0PI_SESSION_ID`, SHA-256 encoded as `sha256:` plus 64 lowercase hex characters.
- An initial poll first replays an existing `pending` attempt for that session. Otherwise it creates an attempt only for the oldest `open|working` current task version, ordered by `updated_at, task_id`, for which no delivery row yet exists for that session.
- A new task version has no row and is independently eligible. State-only task transitions do not create a new key.
- A partial unique index permits at most one `pending|inserting` attempt for a task/version/session. A unique non-null predecessor permits one direct requeue successor. Explicitly requeueing a later uncertain/rejected successor forms a chain.
- Normal transitions are `pending -> inserting -> persisted|uncertain|rejected`; exact late session evidence may promote `pending|inserting|uncertain -> persisted`.
- `delivery.rejected` may also take `pending -> rejected` when strict validation conclusively fails before `pi.sendMessage`. This is the necessary narrow interpretation of the plan's pre-insertion rejection rule and must be noted in `phase-2-findings.md` without expanding the state set.
- Disconnect marks only that connection's `inserting` rows uncertain. Its `pending` rows remain replayable and may be rebound to the next connection for the same session.
- Startup changes all leftover `inserting` rows to uncertain before the endpoint becomes reachable.
- `uncertain` and `rejected` never become eligible automatically. Requeue creates a new linked `pending` row with the same stable key and explicit duplicate-risk wording; it never mutates away predecessor evidence.

All state-changing store methods accept a request ID where a client retry can occur, hash bounded canonical input, and use the existing `mutation_requests` authority or equivalent transactionally durable replay. SQLite indexes and transactions, not only `Store.mu`, enforce attempt uniqueness.

### 3.2 Protocol v1

Keep protocol version 1 and add capability:

```text
pi_delivery_mvp
```

Freeze these extension commands:

```text
delivery.open
delivery.poll
delivery.inserting
delivery.persisted
delivery.rejected
delivery.close
```

Strict request/result contracts:

- `delivery.open`: current Pi session ID plus at most 256 distinct observed `{delivery_id, delivery_key}` pairs; result has the claimed session ID, eligible backlog count, and uncertain/rejected counts.
- `delivery.poll`: no caller-supplied session selector; result contains either no work or one pending work item plus refreshed bounded counts.
- Work contains delivery ID/key, current task ID/version/state/text/source kind, and only the Slack immutable author/message/thread timestamps needed by the visible message. Task text is capped at 40,000 UTF-8 bytes for delivery; an oversized current task is conclusively rejected with a bounded code rather than producing an oversized frame.
- `delivery.inserting` and `delivery.persisted`: exact delivery ID/key only.
- `delivery.rejected`: exact delivery ID/key and a bounded allowlisted diagnostic code only.
- `delivery.close`: empty params and idempotent release.

The common envelope supplies request IDs. The server binds hello client kind, connection ID, opened session, and current delivery in connection state. It rejects client-kind changes, delivery commands from CLI connections, operator commands from extension connections, operations before open, and identifiers not owned by that connection/session.

Add operator commands on the same capability:

```text
mosscap delivery list [--state pending|inserting|persisted|uncertain|rejected] [--json]
mosscap delivery inspect DELIVERY [--json]
mosscap delivery requeue DELIVERY --reason TEXT [--request-id ID] [--json]
```

List defaults to a bounded page of 100 and contains metadata only. Inspect may include bounded diagnostic/requeue metadata but no task body or Pi path. Requeue is valid only for uncertain/rejected and always warns that duplicate insertion is possible.

Do not advertise `pi_delivery_mvp` from hello until the gateway slice implements every route and disconnect cleanup. Commit exact Go request/response frame fixtures in the foundation slice; Node must consume those fixtures rather than inventing a second wire description.

### 3.3 One in-memory active connection

Implement one gateway-owned coordinator, preferably `internal/pidelivery`:

- the first successfully opened extension connection claims the slot;
- a second receives a typed conflict containing only the active Pi session ID;
- repeating open on the same connection/session is idempotent;
- explicit close, socket loss, and gateway shutdown release the slot and reconcile inserting work before a successor is admitted;
- pending work survives disconnect and is replayed to the same Pi session;
- after `delivery.open`, clear the generic server read deadline for that connection and rely on request deadlines, socket closure, and gateway shutdown;
- before open and after close, retain the normal bounded server deadline.

This claim has no row, generation, heartbeat, expiry, takeover, lease, or process-control meaning. A gateway restart forgets it.

### 3.4 Pi 0.83.0 extension

Create the source-loaded package exactly under the layout in `docs/plans/phase-2.md`. `package.json` pins `@earendil-works/pi-coding-agent` to `0.83.0` for development types, uses Node 26 and pnpm, and has only native TypeScript tests plus `tsc --noEmit`. No transpiler, bundler, generated JavaScript, root workspace, publication metadata, or runtime framework is added.

Keep boundaries strict:

- `src/index.ts`: registration and lifecycle only; factory starts no socket, timer, poller, or watcher.
- `src/client.ts`: `node:net`, one in-flight framed request, strict response parsing, deadlines, capability check, and close.
- `src/controller.ts`: opt-in/off, polling, reconnect, active-delivery event coordination, and status.
- `src/messages.ts`: visible bounded content/details validation and all-entry reconciliation.
- `src/types.ts`: strict local DTOs and bounds.

`/mosscap` rejects arguments, non-TUI mode, and `!ctx.sessionManager.isPersisted()` before opening anything. It scans all `getEntries()` entries, deduplicates exact valid Mosscap pairs, and fails visibly before connecting if more than 256 pairs would be reported. It does not use `getBranch()` as delivery authority.

One delivered message is exactly:

```typescript
{
  customType: "mosscap.task",
  display: true,
  content: "...BEGIN UNTRUSTED TASK TEXT...END UNTRUSTED TASK TEXT...",
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

Content includes bounded identity/source metadata, concise existing-CLI examples, and an explicit statement that task state changes are separate. It contains no tokens, raw envelopes, files, session path, history, configured model instructions, or arbitrary diagnostics.

Delivery timing is:

1. validate pending work locally;
2. call `pi.sendMessage(..., {triggerTurn: true})` when idle, or add `deliverAs: "followUp"` when busy;
3. await the matching custom `message_start` report to `delivery.inserting` before that handler returns;
4. on matching `message_end`, return from the handler and schedule a later macrotask/event-loop turn;
5. scan `getEntries()` for the exact details and report persisted only if found;
6. keep the poll loop blocked until the current attempt resolves.

A conclusive pre-send validation failure reports rejected. Once matching `message_start` occurred, loss of evidence is uncertain, never rejected or automatically retried. `/mosscap off` and every `session_shutdown` reason close idempotently, clear status, stop reconnect, and leave Pi/tasks/deliveries untouched. Reload/new/resume/fork creates a fresh off instance.

## 4. Three substantial sequential Terra slices

There is no parallel implementation. Each Terra gets one cohesive slice, a clean handoff, and one review. The manager validates the hard gate before assigning the next slice. Agents do not perform opportunistic cleanup.

### T0 — delivery foundation, recovery repository, protocol, and operator CLI

**First code increment.**
**Commit:** `feat: add Pi delivery foundation`
**Exclusive scope:**

```text
internal/store/migrations/0004_pi_delivery.sql
internal/store/store.go
internal/store/delivery*.go
internal/store/*delivery*_test.go
internal/store/store_test.go                 # migration expectations only
internal/identity/identity.go
internal/identity/identity_test.go
internal/protocol/**
internal/app/app.go                          # root delivery branch only
internal/app/delivery.go
internal/app/*delivery*_test.go
```

Implement the complete durable model, key derivation, eligibility, request replay, transitions, startup/dead-connection reconciliation, exact observed-key reconciliation, bounded list/inspect/requeue, strict protocol DTOs/validators, Go golden frames, and public recovery CLI. Define but do not yet advertise the capability.

**Hard gate:**

- schema 3 -> 4 upgrade is atomic; migrations 0001-0003 remain byte-identical;
- new/old/failing-upgrade fingerprint tests pass;
- local and Slack tasks order oldest-first; done excluded; version/session identity behaves exactly;
- concurrency cannot create two active attempts;
- pending replay, late evidence, startup/disconnect uncertainty, and linked requeue pass with a fake clock and deterministic IDs;
- delivery rows and list/inspect output contain no task text/path/transcript canaries;
- every protocol command rejects unknown fields, bad enums/IDs/keys, over-cap observations, and oversized work;
- CLI help, human/JSON output, filters, typed errors, request replay/conflict, and duplicate-risk warning pass;
- `go test -race ./internal/store ./internal/protocol ./internal/app`, `go test ./...`, `go vet ./...`, and `git diff --check` pass without a network credential.

### T1 — gateway coordinator and connection-bound vertical slice

**Commit:** `feat: serve one opted-in Pi session`
**Depends on:** T0 green and reviewed
**Exclusive scope:**

```text
internal/pidelivery/**                       # new coordinator/service
internal/server/**
internal/status/**
```

Implement the in-memory slot, connection/session binding, all extension and operator routes, startup reconciliation before bind, disconnect cleanup, delivery-specific deadline handling, capability advertisement, typed conflict/recovery errors, and additive secret-safe gateway status. Preserve Phase 1 Slack composition except where the existing store pointer must remain dynamically valid across reload.

Use a fake extension client against a real Unix socket and real temporary SQLite ledger. No production Slack runtime is started; server tests use `Options{Disabled:true}` or deterministic fake Slack adapters.

**Hard gate:**

- first open succeeds, second conflicts, same-open is idempotent, and close/disconnect releases only after reconciliation;
- all operations are rejected before hello/open, across client-kind changes, wrong sessions, wrong keys, and stale connections;
- pending disconnect replays the same delivery; inserting disconnect/startup becomes uncertain and is not polled;
- exact observed evidence reconciles before backlog polling;
- opened extension remains connected while idle/busy for more than 15 seconds, without heartbeat/lease machinery;
- reload/store replacement cannot leave a coordinator using a closed store;
- status exposes only active session ID and bounded state counts; canaries do not escape;
- protocol capability is advertised only now and every advertised route works;
- `go test -race ./internal/pidelivery ./internal/server ./internal/status`, `go test -race ./...`, `go vet ./...`, `go build ./cmd/mosscap`, and `git diff --check` pass with no live Slack.

### T2 — source-loaded extension and deterministic cross-runtime gate

**Commit:** `feat: deliver tasks visibly to Pi`
**Depends on:** T1 green and reviewed
**Exclusive scope:**

```text
pi-extension/**
internal/e2e/phase2_test.go                 # new only
docs/plans/phase-2-findings.md              # automated findings/deferred-trial note only
docs/trials/phase-2.md                      # only if commands drifted
```

Pin the actual Pi 0.83.0 package/types first. Implement the native-TypeScript client, controller, messages, registration/lifecycle, tests, and minimum source-loading README. Add deterministic Go/Node integration using a real Unix socket/SQLite ledger, fake extension/Pi adapter, and existing fake Slack admission where Slack-origin metadata is needed. Consume T0's Go frame fixtures in Node tests and prove Node frames are accepted by Go.

The E2E gate proves a fake-Slack or local task becomes one pending delivery, one visible custom message, one persistence report, and no duplicate after reconnect/branch-wide key reconciliation. It also proves `/off` behavior through the controller fake. It does not contact Slack, start a model/provider turn, or claim the deferred manual trial passed.

**Hard gate:**

- no connection/timer exists before explicit opt-in; non-TUI and ephemeral sessions refuse;
- repeated `/mosscap` is idempotent; off and all shutdown reasons clean up; same-instance loss reconnects with bounded injected backoff;
- all-entry scan finds off-branch keys, ignores malformed/nonmatching entries, and fails closed at the cap;
- visible content/details, delimiters, source metadata, CLI hints, and secret/path exclusions are exact;
- idle uses `triggerTurn`; busy uses follow-up; matching start is reported before handler return; persistence is checked only on a later event-loop turn;
- pre-insertion invalid work is rejected; post-start missing evidence remains uncertain;
- client handles fragmented/coalesced frames, invalid UTF-8/JSON, zero/oversized frames, response correlation, deadlines, capability absence, and one request in flight;
- Go <-> Node golden frames pass in both directions;
- exact Pi 0.83.0 types compile and no tool is registered;
- the deterministic fake-extension E2E proves reconnect suppression and leaves task state unchanged;
- all commands in section 5 pass without live Slack or model credentials.

If T2 exposes a product defect outside its scope, stop and assign a narrow correction to the owning prior slice; do not let the E2E agent make unreviewed cross-cutting edits.

## 5. Final automated gate

Run from a clean tree:

```sh
gofmt -w <changed Go files>
go test ./...
go test -race ./...
go vet ./...
go build ./cmd/mosscap
go mod tidy -diff
pnpm --dir pi-extension install --frozen-lockfile
pnpm --dir pi-extension test
pnpm --dir pi-extension typecheck
git diff --check
```

Then perform a scope audit:

- schema is exactly 4; prior migration bytes are unchanged;
- protocol is exactly 1 and delivery is capability-gated;
- one active in-memory connection exists; there are no leaders, generations, leases, heartbeats, takeover, or push;
- no extension model tool, automatic task transition, Slack action, file handling, packaging/publication, auto-opt-in restore, or non-TUI partial support exists;
- delivery metadata has no task/transcript/path/model/credential content;
- existing Phase 0/1 tests remain green;
- no command used live Slack, Slack credentials, or a real provider.

Passing this gate means **Phase 2 MVP code complete, manual gates deliberately deferred**. It does not mean Phase 1 or Phase 2 is complete under the original manual-trial wording.

## 6. Deferred post-code gate

After the user returns to operator UX/live validation:

1. implement/review the separate Phase 1 operator UX plan;
2. rotate/supply disposable Slack credentials through the approved local workflow;
3. run the deferred Phase 1 smoke;
4. run `docs/trials/phase-2.md` in one safe, human-owned Pi 0.83.0 TUI session; and
5. record actual findings without credentials, raw bodies, or session paths.

Until then, retain these explicit risks:

| Risk | Current mitigation |
| --- | --- |
| Phase 1 live setup/adapter behavior is unverified. | Existing deterministic fake-Slack suite remains mandatory; no claim of live readiness. |
| Pi 0.83.0 runtime append/event order is not proven in a real TUI/model turn. | Exact types are pinned; fake timing tests enforce our ordering; deferred manual trial remains required. |
| A trusted Slack message can trigger a fully capable Pi once opted in. | Workspace/channel/user admission remains Phase 1 authority; message text is visibly delimited and no hidden/custom-tool authority is added. |
| SQLite and Pi JSONL cannot commit atomically. | Stable keys, exact evidence, uncertain blocking, and explicit linked requeue preserve honest recovery. |
| Central server lifecycle changes can regress Phase 1. | T1 owns all central edits and must pass the complete Go race suite before T2. |
| Scope may drift toward the longer SPEC. | `docs/plans/phase-2.md` MVP simplifications and this three-slice boundary win; stop on any proposed deferred feature. |

## 7. Efficient review rule

Each Terra handoff contains only: commit, changed-file inventory, focused gate output, full relevant gate output, invariant evidence, and remaining risks. The manager reviews one cohesive diff per slice, checks the claimed tests and scope mechanically, and assigns the next slice only after acceptance. Do not split these slices into artificial micro-commits or add independent polish/review rounds unless a hard gate fails.
