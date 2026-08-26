# Passive Mosscap prompting plan

**Status:** proposed, documentation-only design; no product behavior is changed by this document.

## Goals and non-goals

Add an opt-in passive loop that lets an already connected, human-owned Pi session notice durable Mosscap work without Slack reminders. The loop must be extension-owned, visible, bounded, and safe across restarts and uncertain Pi insertion. The gateway remains the SQLite authority and only exposes a capability-gated eligibility view for current task versions persisted in the active Pi session.

This is not a gateway scheduler, a Slack notification/reminder system, a headless Pi runner, a second delivery channel, or an automatic retry of uncertain delivery. It must not send Slack messages, infer task completion, or silently opt a session in.

## Current evidence and constraints

- `docs/SPEC.md` and `docs/SYSTEM_MODEL.md` require explicit `/mosscap` opt-in, one active extension slot, visible custom messages, session-entry reconciliation, and no automatic retry after possible insertion.
- `pi-extension/src/controller.ts` currently polls once per second and immediately delivers the first eligible result; it has no settled-event idle grace period or editor inspection.
- `pi-extension/src/index.ts` receives Pi lifecycle events and can own timers, commands, status, and notifications. The current adapter exposes `isIdle`, entries, and `send`, but not editor contents.
- `pi-extension/src/types.ts` defines protocol version 1, `pi_delivery_mvp`, current-version `Work`, and delivery counts. Gateway protocol/server/store code is the place to add capability negotiation and eligibility persistence.
- `pi-extension/README.md` explicitly promises no background connection/timer until `/mosscap`; passive mode therefore remains subordinate to explicit opt-in.

## Recommended architecture

Keep normal delivery and passive prompting in the extension. On `/mosscap`, the extension negotiates a new optional capability (suggested name `pi_passive_prompt_v1`) while opening the existing single session slot. The gateway enables the capability only when it supports the contract and projects safe eligibility from its SQLite delivery records for that active Pi session. It never schedules a wakeup, creates a reminder ledger, or emits Slack work.

On Pi `agent_settled`, the extension starts/restarts a 60-second one-shot timer. At expiry it evaluates locally: the extension is enabled, the same session is still active, Pi is idle, the editor is empty, no delivery is active or pending, and the gateway still reports a candidate for that session. If all checks pass, it makes exactly one visible custom message (a local prompt, not a task delivery) and triggers one model turn. The timer is cancelled on user input, agent start, editor non-empty state, delivery activity, disconnect, `/mosscap off`, shutdown, or any failed precondition. A subsequent settled event starts the next interval; after a prompt, the next interval also requires a new settled event.

The prompt is deliberately generic (for example, “Mosscap has eligible work. Review the pending task list when ready.”), contains no task text or Slack content, and is marked with a structured `mosscap.passive_prompt` type/details and a unique prompt key. It must not be mistaken for a delivery or cause duplicate task insertion. The model still uses ordinary CLI commands to inspect and act on tasks.

## Eligibility, cadence, and safety semantics

Eligibility is capability-gated and session-scoped. A candidate exists only when an `open`/`working` task's current version has an exact delivery already durably `persisted` in the active Pi session. Exclude pending/undelivered work (the normal delivery path handles it first), rejected or uncertain deliveries, deliveries persisted in another session, done tasks, and stale persisted versions after an edit. The projection is recomputed from current task and delivery records, so task edit, completion, session change, or requeue naturally invalidates it. A response must include a bounded count and one task ID/version (or opaque candidate generation) only as needed for diagnostics, never task text, credentials, or Slack tokens.

The extension treats the final gateway check as advisory and fail-closed: unavailable, stale, malformed, unsupported, or timed-out responses suppress the prompt. It rechecks immediately before sending and must not send more than one prompt per 60-second settled window. It must not prompt while the normal delivery path has work, even if the candidate count is nonzero. User activity wins races: a prompt send must be serialized with input/agent-start and delivery state, and cancellation after Pi insertion begins follows the existing uncertain-delivery rules (the prompt itself is non-task work and must be reconciled by its key).

Passive mode is off by default and never restored merely because a prior session enabled it. Suggested controls: `/mosscap passive on|off|status`, with `on` requiring an active `/mosscap` connection and persisted TUI session. `/mosscap off` disables both delivery and passive prompting. A config/feature flag may gate rollout but must not override the per-session opt-in.

## Protocol and component plan

1. Extend the versioned hello capability list with `pi_passive_prompt_v1`; unknown capabilities remain safely ignored. Return negotiated capabilities in `delivery_open`.
2. Add a session-scoped `passive_eligibility`/`passive_candidate` result containing `eligible`, bounded count, generation, and current task-version identity only as needed for diagnostics. Add request IDs and strict size/schema validation.
3. Compute the persisted-current-version count and bounded candidate in the existing delivery-count transaction (for example, with an `EXISTS` query); do not create a scheduler table, reminder ledger, or Slack effect.
4. Add controller state for passive enabled, settled timer generation, prompt key, and serialized gating; expose adapter `editorIsEmpty()` (or equivalent Pi API) and a `sendPassivePrompt` adapter method.
5. Add lifecycle wiring for `agent_settled`, user/editor-change cancellation if Pi exposes those events, and existing message/delivery start/end cancellation. Keep ordinary polling behavior unchanged.
6. Put the generic prompt renderer and structured details in `pi-extension/src/messages.ts`; keep it visibly distinct from `mosscap.task`.
7. Update `pi-extension/src/types.ts`, `client.ts`, `controller.ts`, `index.ts`, and relevant Go protocol/server/store files. Update `docs/SPEC.md`, `docs/SYSTEM_MODEL.md`, and extension README only when implementation lands; this plan is the sole current change.

## Observability and compatibility

`/mosscap status` should distinguish delivery from passive state and report safe fields: negotiated capability, passive on/off, timer armed, last suppression reason, last prompt time/key, and bounded eligibility count. Diagnostics must not include task bodies, Slack payloads, tokens, or editor contents. Add structured counters for armed, cancelled, suppressed-by-editor/busy/delivery/no-candidate, gateway-error, and sent prompts, with rate limiting for repeated errors.

Older gateways and extensions continue normal delivery: absent capability means passive mode unavailable and `/mosscap passive on` reports a clear unsupported result. New gateways must accept old clients. Protocol/schema failures fail closed. Session ownership remains single-holder and explicit opt-in remains mandatory.

## Tests

- Unit-test eligibility filtering for current versions: persisted in the active session is included; pending/undelivered, rejected, uncertain, persisted in another session, done, and edited stale versions are excluded; also cover capability absence and bounds.
- Protocol tests cover negotiation, old/new peers, malformed/unknown fields, request replay, timeout, and fail-closed errors.
- Controller tests cover the exact 60-second delay, settled-event reset, editor/busy/delivery gates, cancellation races, one prompt per window, reconnect/off/shutdown, duplicate prompt-key reconciliation, and no task-message insertion.
- Adapter/index tests cover `/mosscap passive on|off|status`, persistence/TUI requirements, status rendering, lifecycle event wiring, and compatibility with existing commands.
- End-to-end tests use a fake gateway and Pi session to prove a candidate causes one visible local custom message only after all gates, while task delivery remains unchanged and Slack is never contacted.
- Run Go tests plus `pnpm test` and `pnpm typecheck`; add protocol fixture tests before rollout.

## Rollout

Phase 0: land protocol/storage behind capability negotiation with passive unavailable. Phase 1: ship extension controls and instrumentation, default off. Phase 2: internal opt-in with conservative 60-second cadence and kill switch. Phase 3: broaden opt-in after reviewing prompt rate, suppression, duplicate-key, reconnect, and user-feedback metrics. Roll back by disabling the capability/flag; normal `/mosscap` delivery must continue.

## Alternatives, risks, and open decisions

Rejected alternatives are a gateway timer (violates human-owned extension boundary), Slack reminders (explicitly out of scope and noisy), and automatic task delivery on settlement (too surprising and unsafe). Risks include Pi API differences for editor state, timer races with user input, stale eligibility, prompt fatigue, and confusing local prompts with task deliveries; mitigate with capability gating, final checks, structured keys, visible labels, and fail-closed behavior.

Open decisions: exact Pi editor-empty event/API; whether eligibility returns only a count or one opaque candidate generation; persistence schema versus existing active-session columns; prompt wording and UI styling; whether passive on survives `/mosscap` reconnect within one process (recommend no); metrics names/retention; and the operational kill-switch location.
