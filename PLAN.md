# Mosscap improvement plan

## Purpose

This plan responds to the agent evaluation with a smaller set of changes that solve demonstrated problems. It deliberately excludes speculative features that do not fit Mosscap's current single-session, push-based model.

The work must preserve Mosscap's core safety properties:

- Slack writes are durable effects prepared before submission.
- Ambiguous Slack outcomes and uncertain Pi deliveries are never retried automatically.
- Request IDs make exact mutation replay safe.
- Task state remains locally authoritative.
- `task reply --done` completes only after Slack confirms the reply.
- Reads remain bounded.
- Slack operations remain pinned to the configured workspace and channel.

## Decisions at a glance

### Implement

1. Replace the hand-written CLI parser/help tree with a single Cobra command tree.
2. Generate complete help and provide high-confidence typo suggestions.
3. Make successful human output compact and intentional for every command.
4. Make `--json` reliable for valid commands without changing existing success DTO shapes.
5. Add complete pagination to every unbounded list.
6. Accept explicit stdin for generated message bodies.
7. Honor caller cancellation and add a shared gateway-call timeout option.
8. Add a bounded, task-centric `task context` read.

### Do not implement now

- shell completion;
- machine-readable help;
- generic `--fields`, `--summary`, or `--verbose` output modes;
- automatic `--all-pages` traversal;
- optimistic `--if-version` task guards;
- `task next --claim`;
- an aggregated action inbox;
- renaming `show` or `inspect`; or
- live thread-reply file admission/retrieval.

---

# Milestone 1 — CLI foundation

## 1. Replace duplicated command parsing and help with Cobra

### Problem

Command syntax, parsing, and help are currently defined in separate places. They have already drifted:

- task leaf help shows only `[arguments]`;
- `gateway run --help` says the implemented command is unavailable;
- `file show --help` says bytes are unsupported without directing the user to `file get`;
- branch help lists command names without explaining them; and
- `reorderTaskFlags` manually rearranges argv to work around limitations of Go's standard `flag` package.

Fixing individual help strings would address today's mistakes but leave the duplication that caused them.

### Benefit

One command tree becomes the source of truth for parsing and discovery. Syntax, defaults, validation, help, and suggestions evolve together, reducing maintenance and making commands reliably discoverable for both people and agents.

### Decision

Adopt `spf13/cobra`/`pflag` for the complete command tree. Migrate command families in internal commits, but release one atomic CLI cutover; do not ship a mixture of old and new parsers.

Cobra command definitions must own:

- command paths and one-line descriptions;
- positional arguments;
- typed flags, defaults, and enum values;
- required and mutually exclusive flag groups;
- examples and warnings;
- read versus mutation classification;
- gateway capability requirements; and
- handler registration.

Business logic remains in ordinary application functions. This is a CLI-layer refactor, not a rewrite of the store, server, Slack, or delivery services.

### Command behavior

- Support flags before or after positional arguments.
- Honor `--` as the end-of-options marker.
- Support `--help` and `-h`; do not preserve positional forms such as `mosscap task help`.
- Running the root or a command branch without a leaf prints useful help and exits 0.
- Unknown commands and invalid flags exit 2 and show the relevant usage line.
- Disable Cobra's generated shell-completion command.
- Do not expose machine-readable help.

Example branch output:

```text
Manage durable tasks.

Usage:
  mosscap task <command>

Commands:
  create    Create a local task
  list      List tasks
  show      Show current durable task state
  context   Show task state with bounded Slack context
  update    Replace task text
  open      Move a task to open
  working   Move a task to working
  done      Move a task to done
  reply     Reply to a Slack task, optionally completing it

Run "mosscap task <command> --help" for details.
```

### Correctness fixes included in the cutover

- Replace every placeholder leaf synopsis with its actual syntax.
- Give every flag a value name and useful description.
- Explain required/mutually exclusive input groups.
- Change missing setup for `gateway run` from “not implemented” to an unsafe/missing-configuration error with the action `run mosscap setup`.
- Describe `file show` as metadata inspection and point to `file get` for explicit retrieval.
- Preserve warnings for duplicate-risk retries, requeues, irreversible purge, and reply/completion semantics.

### Typo suggestions

Suggest only high-confidence command or flag corrections:

```text
Error: unknown task command "udpate"
Did you mean "update"?

Usage: mosscap task <command>
```

Never suggest or rewrite positional values such as task IDs, Slack timestamps, text, filenames, or emoji.

### Acceptance criteria

- Every command in `docs/SPEC.md` is represented by one Cobra leaf.
- Every leaf has a complete synopsis, description, flag documentation, and examples where syntax is non-trivial.
- Table-driven tests walk the entire command tree and reject placeholder or stale help.
- Tests validate actual accepted flags, defaults, enum values, required groups, and descriptions—not only the `Usage:` prefix.
- Help never loads configuration, contacts the gateway, or mutates state.
- Characterization tests preserve request construction and business behavior across the cutover.

---

## 2. Make JSON reliable for valid command invocations

### Problem

`--json` does not currently guarantee JSON. Mutation failures may be structured, while read-side not-found errors, setup/configuration failures, transport failures, and some diagnostic failures are plain text. Some commands can emit a JSON result followed by a separate text error. Automated callers cannot reliably parse one stream after invoking valid syntax.

A universal success envelope would solve more than is necessary and would break every existing successful JSON consumer.

### Benefit

A caller that uses valid command syntax can request JSON and always receive exactly one machine-readable object for either success or runtime failure. Existing successful DTO consumers do not need to migrate.

### Decision

Keep `--json` as a leaf flag. Do not add a root/global JSON flag.

Distinguish invocation errors from runtime errors:

- parsing, missing arguments, invalid flag combinations, and other usage failures always produce friendly text on stderr and exit 2, even if `--json` appeared;
- after command syntax and local argument validation succeed, `--json` governs all success and runtime failure output.

This deliberately treats malformed CLI use as a caller programming error rather than part of the machine-result contract.

### Successful JSON

Preserve existing command-specific success shapes. Do not introduce a universal envelope, `ok` field, or schema-version field.

Examples that currently return a task DTO, task list DTO, effect wrapper, or mutation request/result wrapper should retain those shapes except for additive fields required by pagination or new commands.

### Runtime JSON errors

Every valid `--json` invocation that fails at runtime emits one common object:

```json
{
  "code": "task_not_found",
  "category": "not_found",
  "message": "task was not found",
  "retriable": false,
  "request_id": "req_...",
  "details": {},
  "guidance": ""
}
```

Required fields:

- `code` — stable machine-readable error code;
- `category` — stable exit category;
- `message` — concise safe explanation; and
- `retriable` — whether retry is meaningful.

Optional fields:

- `request_id` — only when one exists;
- `details` — bounded structured values;
- `guidance` — safe recovery guidance;
- `progress` — existing purge progress; and
- `partial_result` — intentionally preserved completed work.

Use `partial_result` narrowly. For example, `doctor --json` with successful local checks and an unavailable gateway returns exit 6 with the local report embedded in the single error object. Do not discard useful checks or print a second object.

### Streams and exit codes

For a valid invocation in JSON mode:

- write exactly one JSON object and one trailing newline to stdout;
- write nothing to stderr, including warnings and request-ID guidance;
- retain exit codes 0 and 2–9; and
- represent warnings and duplicate-risk notices as structured fields.

Human mode continues to use stdout for successful results and stderr for errors/warnings.

### Error coverage

The shared runtime renderer must normalize:

- setup/configuration and unsafe-path failures;
- gateway dial, handshake, protocol, and capability failures;
- read-side not-found failures;
- mutation request-ID conflicts;
- destination placement conflicts;
- conclusive Slack failures;
- ambiguous Slack outcomes; and
- partial doctor failure.

Do not inspect argv heuristically to decide output mode. Cobra must determine whether `--json` was actually parsed as a flag rather than consumed as a value.

### Acceptance criteria

- Golden tests cover successful JSON and every runtime exit category for local, gateway-read, and gateway-mutation commands.
- Every runtime JSON test asserts valid JSON, exactly one object, one trailing newline, and empty stderr.
- Usage-error tests assert friendly text, relevant usage, stderr output, and exit 2.
- Existing successful JSON DTO shapes remain compatible aside from documented additive fields.
- No secret, authorization header, or raw Slack envelope can enter generic error details.

---

## 3. Render compact, intentional human output everywhere

### Problem

Many commands print pretty JSON even when `--json` was not requested. This is difficult to scan in a terminal and makes human fallback feel like a debug interface. A one-line task table would be compact, but it would hide the user message—the most important part of a task.

Raw Slack/task text can also contain terminal control bytes that should not be allowed to recolor, clear, or disguise terminal output.

### Benefit

Every default command becomes easy to read while preserving complete user messages. JSON appears only when explicitly requested. Safe presentation prevents terminal-control injection without rewriting stored or machine-readable content.

### Decision

Every successful command gets a purpose-built human renderer:

- mutations show concise resource IDs, resulting state, request ID, and warnings;
- task and Slack message reads use cards;
- metadata/detail reads use labelled fields;
- effect, delivery, and trust lists use compact rows/tables;
- configuration uses labelled redacted values;
- diagnostics/status lead with health and action-required information.

Do not add `--summary` or `--verbose`. Full structured detail remains available through `--json`.

### Task and Slack message cards

Task lists must show complete text rather than a truncated preview:

```text
AbCdEfGh  open  slack  v3  updated 12m ago
│ Please investigate the release failure.
│
│ The error is:
│   FAILED during migration
│
│ Do not skip the integration tests.
```

Preserve message newlines, indentation, printable Unicode, and full content. Escape non-printing terminal controls in presentation only:

```text
│ Status: \u001b[31mFAILED\u001b[0m
```

Do not alter the durable text. JSON returns the original string with normal JSON escaping.

### Gateway status

The default should lead with a concise operational view, for example:

```text
Gateway  healthy  uptime 2h14m
Slack    connected
Pi       connected
Tasks    4 open, 1 working
Action   1 ambiguous effect, 0 uncertain deliveries
```

Detailed paths, versions, database sizes, transfer counts, and reconnect diagnostics remain in `gateway status --json`.

### Pagination footer

When a human listing has another page, print a ready-to-run continuation command that preserves filters and limit:

```text
More results available.
Next: mosscap task list --state open --limit 20 --cursor tsk_...
```

Quote values safely where necessary. JSON returns `next_cursor` but not shell command strings.

### Acceptance criteria

- Golden tests cover empty and populated output for every command family.
- Tests cover TTY, non-TTY, `NO_COLOR`, multiline text, Unicode, and terminal control bytes.
- Human rendering never changes protocol results, stored data, or exit codes.
- Default output contains no JSON serialization unless `--json` was explicitly parsed.
- Request IDs and duplicate-risk warnings remain visible in human mutation output.

---

# Milestone 2 — Bounded and reliable I/O

## 4. Make every unbounded listing pageable

### Problem

Several lists expose only their first bounded page:

- `task list` returns `next_cursor`, and the protocol/store accept a cursor, but the CLI cannot send one;
- effect and delivery lists cap their results without cursors;
- Slack history/thread reads can return a cursor but cannot continue from it; and
- the trusted-user set is an unbounded SQLite collection with no paging.

Older durable records can become unreachable through supported commands.

### Benefit

Every record remains inspectable through bounded requests. A common `--limit`/`--cursor` interface is easy for agents and humans to follow, while avoiding unexpectedly large responses.

### Decision

Add `--limit` and `--cursor` to:

- `task list`;
- `effect list`;
- `delivery list`;
- `trust list`;
- `slack history`; and
- `slack thread`.

Do not add `--all-pages` or any unbounded automatic traversal.

Add task filtering where operational investigation needs it:

```sh
mosscap effect list --task TASK
mosscap delivery list --task TASK
```

`--task` composes with `--state`, `--limit`, and `--cursor`.

### Cursor design

Keep SQLite cursors simple and token-efficient:

- task cursor: full task ID of the last returned task;
- effect cursor: full effect ID of the last returned effect;
- delivery cursor: full delivery ID of the last returned delivery;
- trust cursor: last Slack user ID;
- Slack history/thread cursor: Slack's opaque cursor unchanged.

Do not accept short selectors as cursors. Full resource IDs avoid ambiguity.

For task/effect/delivery lists, look up the cursor record's immutable creation key and continue from `(created_at, id)`. Trust continues lexicographically after the supplied Slack user ID and does not require the anchor row to remain present.

If a well-formed task/effect/delivery cursor was removed by explicit purge, return `cursor_expired` as a typed conflict (exit 4) with guidance to restart without `--cursor`. It is not a syntax error because it may have been valid previously.

### Stable order

- Tasks: oldest creation first, then ID.
- Effects: newest preparation/creation first, then ID.
- Deliveries: newest creation first, then ID.
- Trust: Slack user ID lexicographically.
- Slack history/thread: preserve and document Slack's native order.

Changing a task no longer moves it within task-list pagination.

### Page sizes

| Collection | Default | Maximum |
| --- | ---: | ---: |
| Tasks | 20 | 100 |
| Effects | 50 | 100 |
| Deliveries | 50 | 100 |
| Trusted users | 50 | 100 |
| Slack history | 50 | 100 |
| Slack thread | 50 | 100 |
| Thread within `task context` | 20 | 100 |

Treat `--limit` as a maximum. The server may return fewer task records with `next_cursor` when needed to stay within the configured protocol frame budget. Never silently cut text within a returned task. If one record cannot fit by itself, return a typed bounded-result error.

### Consistency model

Pagination is stable keyset traversal, not a snapshot:

- tasks created during oldest-first traversal may appear on a later page;
- new effects/deliveries created after the first newest-first page require a fresh traversal to appear;
- purge may expire a task/effect/delivery cursor; and
- Slack pages follow Slack's own consistency behavior.

Do not create server-side cursor sessions or cursor-expiry tables.

### Protocol work

- Add cursor fields to effect, delivery, trust, history, and thread request DTOs as needed.
- Add `next_cursor` to effect, delivery, and trust result DTOs.
- Pass Slack cursors through validation and the Slack API adapter.
- Add capability negotiation for parameter/result shapes older gateways cannot understand.
- Keep cursor values bounded, valid UTF-8, and free of control/whitespace characters.

### Acceptance criteria

- Tests traverse multiple pages of every SQLite collection without omission or duplication.
- Tests cover equal creation timestamps, concurrent insertion, filter composition, invalid IDs, and purged cursor anchors.
- Trust traversal remains valid if the previous user is removed.
- Fake Slack tests prove limit/cursor forwarding and next-cursor return for history and thread.
- Human output gives a usable continuation command.
- Help documents defaults, maximums, ordering, cursor requirements, and non-snapshot behavior.

---

## 5. Accept explicit stdin for generated message bodies

### Problem

Generated multiline text currently must be shell-escaped or written to a regular temporary file. `--text-file /dev/stdin` is correctly rejected by regular-file safety checks, but that leaves no clean pipeline for agent-generated replies.

### Benefit

An explicit stdin mode accepts exact multiline content without weakening file safety or overloading a literal text value such as `-`.

### Decision

Add `--stdin` to:

- `task create`;
- `task update`;
- `task reply`;
- `slack send`; and
- `slack edit`.

Require exactly one of:

- `--text TEXT`;
- `--text-file PATH`; or
- `--stdin`.

Do not expand stdin support to short operational fields such as transition notes, retry/delete reasons, file comments, or emoji unless real use demonstrates a need.

### Input semantics

- Accept stdin from either a pipe or a TTY; the flag is explicit.
- Preserve bytes represented by valid UTF-8 exactly, including trailing newlines.
- Do not trim, normalize line endings, or create a temporary file.
- Apply existing non-empty, UTF-8, NUL, and text-size validation.
- Bound reads before allocation and validate the complete encoded protocol request against the configured frame limit.
- Fail before contacting the gateway when input is invalid or too large.
- Keep `config secret set --stdin` separate with its existing single-line, bounded, non-TTY secret rules.

The normal ten-second gateway timeout begins after stdin has been read; it must not force a person to finish typing within ten seconds. Parent-process cancellation should still be honored where the input primitive permits it.

### Acceptance criteria

- Tests cover piped and TTY-like input, multiline Unicode, retained trailing newline, no trailing newline, empty input, NUL/invalid UTF-8, over-limit input, and mutually exclusive sources.
- The accepted durable task/effect text exactly matches stdin.
- No temporary file is created.
- Invalid input performs no gateway call and prepares no durable mutation.

---

## 6. Honor caller cancellation and add a gateway-call timeout

### Problem

`App.Execute` currently discards its caller context, and gateway calls use `context.Background()`. The client then silently imposes a fixed ten-second deadline when no context deadline exists. Callers cannot reliably cancel work or choose a longer timeout for Slack/file operations.

For mutations, timeout semantics must remain honest: a caller timeout after Slack submission cannot be treated as proof that nothing happened.

### Benefit

Context propagation makes Mosscap composable in agent harnesses and scripts. An explicit timeout bounds dial/handshake/request work while preserving durable intent, exact replay, and explicit ambiguity.

### Decision

Add `--timeout DURATION` as a shared option only on commands that call the running gateway. Do not make it a root flag and do not accept it on `setup`, offline configuration edits, or `gateway run`.

Examples:

```sh
mosscap task show TASK --timeout 20s
mosscap file get FILE --timeout 60s --output ./file
```

Retain a documented ten-second default. Require a positive duration; there is no unlimited mode. If the caller's parent context has an earlier deadline, honor the earlier deadline.

### Timeout scope

Start the default/explicit timeout immediately before the first gateway interaction. It covers:

- Unix-socket dial;
- protocol handshake;
- command request/response;
- server-side Slack or file-transfer work associated with the request; and
- any immediate gateway follow-up that is part of the command.

It does not include Cobra parsing, stdin/TTY entry, secret prompts, or local output rendering. Parent-context cancellation should still propagate throughout the command where practical.

`gateway run` combines its parent context with SIGINT/SIGTERM lifecycle cancellation but has no CLI request timeout.

### Mutation cancellation semantics

Propagate cancellation to server-side request work; do not detach mutations and merely stop waiting.

Durable state determines the outcome:

1. **Before preparation:** no effect exists; exact retry may begin normally.
2. **Prepared, not submitted:** the durable intent remains prepared. Exact replay or normal gateway recovery may continue that same effect; no duplicate effect is created.
3. **Possibly submitted:** cancellation/timeout is committed as durable `ambiguous`; it is never automatically submitted again.
4. **Confirmed before disconnect:** confirmation remains durable and exact request-ID replay returns it without resubmission.

If the CLI deadline expires before it receives the server's conclusion, report gateway unavailability with the generated request ID and exact-replay guidance. The durable effect may be inspected/recovered by replay; the CLI must not claim certainty it did not receive.

Never automatically retry a transport timeout.

### Implementation work

- Pass `Run`/`Execute` context through handlers, dial, hello, and call.
- Remove CLI request-path uses of `context.Background()`.
- Set socket deadlines from the effective context.
- Ensure cancellation interrupts frame reads/writes.
- Preserve outcome commits with `context.WithoutCancel` after a Slack call has crossed the external boundary.
- Keep request IDs visible in human errors and structured runtime JSON errors.

### Acceptance criteria

- Tests cover parent cancellation and explicit timeout during dial, hello, read call, and Slack/file work.
- Mutation tests separately cover cancellation before preparation, while prepared, after possible submission, and after confirmation.
- Post-submission timeout leaves an inspectable ambiguous effect and never causes automatic resubmission.
- Exact request-ID replay recovers the same prepared/confirmed/ambiguous effect.
- The timeout does not run while a person is entering stdin text.

---

# Milestone 3 — Task-centric workflow

## 7. Add a bounded `task context` command

### Problem

Handling a Slack task currently requires several calls:

1. show the task;
2. extract its Slack thread timestamp;
3. read the thread;
4. inspect reply/effect state; and
5. inspect delivery state if duplicate insertion may be relevant.

This costs calls and tokens and makes it easy to overlook attachments or unresolved external outcomes.

### Benefit

One task-centric read gives an agent the bounded information needed to understand and safely act on a task while preserving the distinction between durable local data and live Slack data.

### Decision

Add a separate read-only command:

```sh
mosscap task context TASK [--thread-limit N] [--json]
```

Keep `task show` as a fast durable local read. `task context` clearly signals a potentially slower live Slack fetch.

The command must not:

- claim or transition the task;
- download files;
- prepare effects;
- requeue deliveries;
- acknowledge Slack events; or
- otherwise mutate state.

### Result shape

Return a purpose-built DTO with four components.

#### Task

Embed the current task DTO, including:

- full current text and content version;
- task state and timestamps;
- durable Slack source/deletion metadata;
- durable Slack reply/completion projection; and
- durable metadata for files attached to the task source.

Do not create a second top-level attachment collection.

#### Thread

Return one live Slack thread page:

- status: `available`, `unavailable`, or `not_applicable`;
- messages;
- fetch timestamp;
- next Slack cursor;
- diagnostic code when unavailable.

Default to 20 messages and allow `--thread-limit 1..100`. Continuation uses `slack thread THREAD_TS --cursor CURSOR`; `task context` itself does not recursively traverse pages.

For the first implementation, do not expose files attached only to live thread replies. Those files are not durable admitted task attachments and cannot safely be presented as retrievable through `file get`. No file bytes or private URLs are returned.

#### Effects

Return actionable bounded related-effect state:

- latest reply/thread-send effect, if any;
- at most 10 newest non-confirmed task-related effects (`prepared`, `submitted`, `failed`, or `ambiguous`);
- total non-confirmed related-effect count; and
- `effects_truncated` when more than 10 exist.

Include effect identity, kind, state, timestamps, target/thread identity, and diagnostic code. Do not include effect body text or operator reasons in context.

Full task-specific investigation uses:

```sh
mosscap effect list --task TASK
```

Do not embed shell command strings in protocol/JSON DTOs. Human guidance may print a recommended command.

#### Delivery

Return only the latest delivery for the current task version:

- delivery ID;
- task version;
- state;
- diagnostic code, if any; and
- `action_required` for `uncertain` or `rejected`.

Do not include delivery history or aggregate counts in task context. Full investigation uses `delivery list --task TASK` and `delivery inspect`.

### Partial Slack availability

If durable task/context data is available but the live Slack thread fetch fails, return a successful partial result and exit 0:

```json
{
  "thread": {
    "status": "unavailable",
    "messages": [],
    "diagnostic_code": "timeout"
  },
  "warnings": ["Slack thread was unavailable"]
}
```

The caller must inspect `thread.status` before assuming replies are current. A missing task remains a normal not-found runtime failure.

This best-effort behavior is specific to the documented context read; it does not turn ordinary Slack read failures into success.

### Bounds and authority

- Durable task, source-file, effect, and delivery data come from SQLite.
- Thread data is a timestamped live Slack read and is not a transactional snapshot with SQLite.
- Result assembly must stay within the configured protocol frame.
- Never silently truncate a message or task body; use documented item bounds and return a typed bounded-result failure if one record cannot fit.
- Add a protocol capability so a newer CLI produces useful guidance when connected to an older gateway.

### Human rendering

Render labelled task details followed by full message cards, durable source attachments, related effect warnings, and the current-version delivery state. Escape terminal controls using the same presentation rules as other task/Slack output.

### Acceptance criteria

- Tests cover local tasks, Slack tasks, deleted sources, durable source attachments, long/multipage threads, and unavailable Slack.
- Tests cover latest confirmed reply, failed/ambiguous effects, more than 10 non-confirmed effects, and current-version uncertain/rejected delivery.
- Partial Slack failure returns exit 0, durable data, component status, and a warning.
- A missing task returns the normal not-found exit/category.
- The command creates no database rows and performs no Slack operation other than the explicit read.
- Context DTOs contain no credentials, private file URLs, temporary paths, file bytes, effect body text, or shell command strings.

---

# Explicitly excluded ideas

## Optimistic `--if-version` guards

The stale-task race is real, but Mosscap currently has one Pi delivery session, serialized turns, and visible redelivery for meaningful Slack edits. Optional guards would help only when callers consistently supplied them and would add protocol, conditional-transition, partial reply/completion, and recovery complexity. Revisit if edits during active work cause real incidents or parallel consumers are introduced.

## `task next --claim`

A transition to `working` is not ownership. Mosscap has no worker identity, lease, heartbeat, expiry, or abandoned-claim recovery, and its current model pushes work to one human-owned Pi session. Do not imply parallel worker semantics without first designing that authority model.

## Summary/field/verbose projections

Compact human output and bounded full JSON should ship first. Additional projections create more schemas and grammar. Add a purpose-built projection only after measured output demonstrates a need; do not introduce a generic field-selection language speculatively.

## Shell completion and machine-readable help

Complete `--help` output and typo suggestions solve current discovery failures. Completion scripts and a machine-help schema add public surface without improving current agent correctness.

## Aggregated action inbox

Pageable, task-filterable effect/delivery lists plus concise human gateway status may already make action discovery cheap. A cross-resource inbox would need separate ordering, pagination, and recovery semantics. Reconsider only after operational usage demonstrates repeated multi-command pain.

## Renaming `show` and `inspect`

Keep the existing distinction:

- `show` returns current resource state (`task show`, `file show`);
- `inspect` returns audit/recovery evidence (`effect inspect`, `delivery inspect`).

Renaming or aliasing commands would add compatibility cost without changing behavior.

---

# Delivery sequence

## 1. CLI foundation

1. Add characterization tests for the current command tree and request construction.
2. Add Cobra and define the complete command tree.
3. Cut every command family over before release.
4. Generate complete help and typo suggestions.
5. Centralize successful human rendering and runtime JSON error rendering.
6. Add compact/card-based human output for every command.
7. Update command examples and output documentation.

## 2. Bounded I/O

1. Change SQLite list ordering to immutable keysets.
2. Add ID-based pagination to tasks, effects, deliveries, and trust.
3. Add Slack limit/cursor passthrough.
4. Add task filters to effect/delivery lists.
5. Add explicit stdin text input.
6. Propagate context and add per-command gateway timeout flags.
7. Add protocol capabilities for changed list contracts.

## 3. Task context

1. Add store queries for bounded task-related non-confirmed effects and current-version delivery.
2. Add the task-context protocol DTO/capability.
3. Add the best-effort live thread read.
4. Add CLI command and human/JSON rendering.
5. Update Pi guidance and README examples to prefer `task context` before handling complex Slack work.

Each milestone must pass the complete release gate before the next begins.

# Cross-cutting release requirements

Every milestone must:

- run `just check` and the extension test/typecheck suite;
- preserve request-ID replay behavior for all mutation paths;
- preserve structural secret redaction;
- add capability negotiation for protocol changes an older gateway cannot understand;
- update `README.md`, `docs/SPEC.md`, and `docs/SYSTEM_MODEL.md` where behavior or authority boundaries change;
- keep human warnings visible and JSON warnings structured;
- test that untrusted text cannot inject terminal control behavior; and
- retain explicit ambiguous/uncertain outcomes with no automatic retry.
