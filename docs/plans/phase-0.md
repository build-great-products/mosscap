# Mosscap v1 Phase 0 implementation plan

**Status:** implementation-ready plan

**Normative source:** [`SPEC.md`](../SPEC.md), especially sections 2–5 and 11–17. If this plan and the specification differ, the specification wins.

## 1. Goal and release gate

Phase 0 proves one durable local vertical slice:

1. `mosscap setup` creates a private XDG layout and initializes a new SQLite ledger.
2. `mosscap gateway run` validates configuration and state, opens the ledger, and serves a private Unix socket in the foreground.
3. short-lived CLI processes negotiate protocol v1 and create, read, replace, and transition local tasks through the gateway;
4. every mutation is request-ID deduplicated and task state transitions are audited;
5. `gateway status` and `doctor` expose typed, secret-safe diagnostics; and
6. tasks and original mutation outcomes survive a gateway restart.

The gate is an automated restart test: repeat a create request with the same request ID before and after restarting the gateway; both responses identify one task, and `task list/show` report that task unchanged.

Phase 0 deliberately has no Slack network client, Pi extension, delivery/effect/file records, or purge implementation. Its schema and protocol leave room for those phases without claiming their behavior. It also does **not** retain immutable text revisions: `tasks.version` is the current-content version and `task_state_events` audit state transitions only. This resolves the brief's “revisions/audit” wording in favor of SPEC sections 1.2, 4.1, and 4.3, which prohibit historical task text.

## 2. Toolchain and dependencies

Use module `github.com/stayradiated/mosscap` and the current stable Go release. Commit `go.mod` and `go.sum`; build with `CGO_ENABLED=0`.

Direct dependencies (pin these initially, then let the implementation PR update only if required by the supported Go toolchain):

| Dependency | Initial version | Purpose |
| --- | --- | --- |
| `github.com/BurntSushi/toml` | `v1.4.0` | strict TOML decoding/encoding |
| `modernc.org/sqlite` | `v1.36.1` | pure-Go SQLite driver |
| `golang.org/x/sys` | `v0.31.0` | Linux peer credentials, `flock`, and no-follow file checks |

Everything else uses the standard library: `flag`, `encoding/json`, `embed`, `database/sql`, `net`, `os/signal`, `crypto/rand`, `crypto/sha256`, and `log/slog`. Do not add Cobra/Viper, an ORM, a migration framework, UUID/ULID packages, or a logging framework.

Phase 0's packaged runtime target is Linux with Unix-domain sockets. Keep peer credential checks behind a small platform-specific package: Linux checks `SO_PEERCRED`; a Unix fallback may rely on private filesystem permissions and explicitly reports `peer_credentials_supported=false`. Windows is unsupported.

Opaque IDs are `<prefix><base64url>` using 128 random bits from `crypto/rand`, raw URL-safe base64, and prefixes `tsk_`, `ste_`, and `req_`. A caller-supplied request ID is accepted only when it is 1–128 ASCII characters from `[A-Za-z0-9._~-]`; generated request IDs use `req_`.

## 3. Repository layout

```text
cmd/mosscap/main.go                 process entry point only
internal/app/                       command tree, text/JSON rendering, exit mapping
internal/config/                    TOML model, XDG defaults, load/redact/atomic set
internal/securepath/                ownership, mode, symlink, regular-file checks
internal/identity/                  prefixed random IDs and request-ID validation
internal/clock/                     UTC microseconds; injectable test clock
internal/store/                     SQLite open, pragmas, transactions, repositories
internal/store/migrations/          embedded numbered SQL migrations
internal/task/                      task types, normalization, transition rules/service
internal/protocol/                  v1 wire envelopes, framing, commands, errors
internal/server/                    socket lifecycle, peer auth, dispatch, shutdown
internal/client/                    dial, handshake, request/response client
internal/doctor/                    non-destructive checks and typed check results
internal/status/                    stable status snapshot types
docs/plans/phase-0.md               this plan
```

Package direction is `app -> client/config`, `server -> protocol + task + store + doctor/status`, and `store -> task` only for persistence DTOs. `client` must not import `store`. Keep implementation packages under `internal`; the only public compatibility surface in Phase 0 is the executable and protocol JSON.

## 4. Configuration and private paths

### 4.1 TOML model

Use this initial file shape:

```toml
version = 1

[paths]
state_dir = "/home/alice/.local/state/mosscap"
database = "/home/alice/.local/state/mosscap/mosscap.sqlite3"
endpoint = "/home/alice/.local/state/mosscap/control.sock"
tmp_dir = "/home/alice/.local/state/mosscap/tmp"

[slack]
team_id = ""
bot_user_id = ""
channel_id = ""
trusted_user_ids = []
secrets_file = ""

[database]
busy_timeout = "5s"

[protocol]
max_frame_bytes = 1048576
```

Slack token values are never valid TOML keys. Later phases read `MOSSCAP_SLACK_APP_TOKEN` and `MOSSCAP_SLACK_BOT_TOKEN`, or parse an owner-only `slack.secrets_file`. Phase 0 preserves and redacts the secrets-file path but does not require Slack fields to be populated because it has no Slack capability. Phase 1 must make all required Slack identity/trust/token checks startup-fatal.

Unknown TOML keys, duplicate keys, unknown config versions, invalid durations, a frame maximum outside 64 KiB–16 MiB, relative paths, and paths containing NUL are errors. Defaults derive once from the effective user's home and `XDG_CONFIG_HOME`/`XDG_STATE_HOME`; empty or relative XDG variables are rejected rather than interpreted relative to the current directory. The default config is `$XDG_CONFIG_HOME/mosscap/config.toml`, with `~/.config` fallback.

`config set` supports only:

- `paths.state_dir`, `paths.database`, `paths.endpoint`, `paths.tmp_dir`;
- `slack.team_id`, `slack.bot_user_id`, `slack.channel_id`, `slack.trusted_user_ids`, `slack.secrets_file`;
- `database.busy_timeout`; and
- `protocol.max_frame_bytes`.

It must reject token-like keys, perform typed parsing, validate the complete resulting document, and atomically replace the config using a random same-directory `0600` file, `fsync`, rename, and parent-directory `fsync`. `config show` returns a typed config view; `secrets_file` is shown as `<configured>`/`<unset>`, environment token presence as booleans, and token values never appear. Path-changing configuration is documented as offline-only; `config set` refuses when the configured socket accepts a connection.

### 4.2 Security validation

Centralize checks in `securepath`; do not scatter `os.Stat` checks through commands.

`setup`:

1. resolve the effective user's home without trusting `HOME` when it disagrees with `os/user.Current`;
2. inspect every existing path component with `Lstat`; reject symlinks;
3. create config and state roots with `0700` and files with `0600`, using no-follow/exclusive creation;
4. require config root, state root, and `tmp/` to be directories owned by effective UID and exactly `0700`;
5. require config, secrets file when configured, database, and lock file to be owner-owned regular files, mode `0600`, with no symlink or extra hard link;
6. require database, endpoint, lock, and temporary directory to be lexical descendants of the state root after cleaning; and
7. reject any group/world-writable component below the user's home and reject endpoints under shared temporary roots (the descendant requirement already excludes normal `/tmp`).

The gateway repeats all applicable checks at every start and fails with exit 5 before serving. Open sensitive files with `O_NOFOLLOW` where available and re-check `fstat` after opening. Pre-create a new database exclusively at `0600` before passing it to SQLite. Use a `0600` `gateway.lock` plus non-blocking `flock` for the process lifetime.

Socket handling must never unlink an arbitrary path. For an existing endpoint: reject symlinks and non-sockets; fail if it accepts a connection; only remove a stale, owner-owned socket inside the validated state root after a failed probe. Bind under an owner-only root, then `chmod 0600` and verify. Accept only same-EUID peers when peer credentials are available. Set a bounded connection deadline for handshake and frames.

## 5. SQLite and migration 0001

### 5.1 Open and compatibility policy

Use one `database/sql` handle with a small bounded pool (`MaxOpenConns=4`, `MaxIdleConns=4`). For every connection enforce `foreign_keys=ON`, WAL journal mode, and the configured 5-second default busy timeout. Verify those settings after open. Use `synchronous=NORMAL`; durability claims begin at a successful SQLite commit, not an `fsync` of every WAL frame.

Set a fixed nonzero SQLite `application_id`, maintain `PRAGMA user_version=1`, and record migration number/checksum in `schema_migrations`. `setup` applies embedded migrations only to a new or empty database. The gateway does not auto-migrate: it rejects a missing, older, newer, gapped, checksum-mismatched, or foreign schema with exit 9. This is consistent with v1's permission to refuse unknown schemas and lack of cross-version migration guarantees.

On startup run `PRAGMA quick_check`; a non-`ok` result is fatal and is not auto-repaired. `doctor` may request the more expensive `integrity_check`. Never interpolate user input into SQL.

### 5.2 Migration `0001_phase0.sql`

Implement the equivalent of the following (SQL may be formatted, but constraints and indexes are required):

```sql
CREATE TABLE schema_migrations (
  version INTEGER PRIMARY KEY CHECK (version > 0),
  checksum TEXT NOT NULL CHECK (length(checksum) = 64),
  applied_at INTEGER NOT NULL CHECK (applied_at > 0)
) STRICT;

CREATE TABLE tasks (
  task_id TEXT PRIMARY KEY,
  version INTEGER NOT NULL CHECK (version >= 1),
  state TEXT NOT NULL CHECK (state IN ('open','working','done')),
  text TEXT NOT NULL CHECK (length(text) > 0 AND instr(text, char(0)) = 0),
  source_kind TEXT NOT NULL CHECK (source_kind IN ('local','slack')),
  created_at INTEGER NOT NULL CHECK (created_at > 0),
  updated_at INTEGER NOT NULL CHECK (updated_at >= created_at),
  done_at INTEGER,
  CHECK ((state = 'done' AND done_at IS NOT NULL) OR
         (state <> 'done' AND done_at IS NULL))
) STRICT;

CREATE TABLE task_state_events (
  event_id TEXT PRIMARY KEY,
  task_id TEXT NOT NULL REFERENCES tasks(task_id) ON DELETE RESTRICT,
  prior_state TEXT NOT NULL CHECK (prior_state IN ('open','working','done')),
  new_state TEXT NOT NULL CHECK (new_state IN ('open','working','done')),
  actor TEXT NOT NULL CHECK (length(actor) BETWEEN 1 AND 128),
  note TEXT CHECK (note IS NULL OR length(note) <= 4096),
  request_id TEXT NOT NULL UNIQUE,
  committed_at INTEGER NOT NULL CHECK (committed_at > 0),
  CHECK (prior_state <> new_state)
) STRICT;

CREATE TABLE mutation_requests (
  request_id TEXT PRIMARY KEY,
  operation TEXT NOT NULL,
  input_hash TEXT NOT NULL CHECK (length(input_hash) = 64),
  outcome TEXT NOT NULL CHECK (outcome IN ('success','error')),
  result_type TEXT,
  result_json TEXT,
  error_code TEXT,
  error_message TEXT,
  created_at INTEGER NOT NULL CHECK (created_at > 0),
  completed_at INTEGER NOT NULL CHECK (completed_at >= created_at),
  expiry_eligible_at INTEGER NOT NULL CHECK (expiry_eligible_at >= completed_at),
  CHECK ((outcome='success' AND result_type IS NOT NULL AND result_json IS NOT NULL
          AND error_code IS NULL AND error_message IS NULL) OR
         (outcome='error' AND result_type IS NULL AND result_json IS NULL
          AND error_code IS NOT NULL AND error_message IS NOT NULL))
) STRICT;

CREATE INDEX tasks_state_updated_idx
  ON tasks(state, updated_at, task_id);
CREATE INDEX task_state_events_task_committed_idx
  ON task_state_events(task_id, committed_at, event_id);
CREATE INDEX mutation_requests_expiry_idx
  ON mutation_requests(expiry_eligible_at);
```

Migration checksums are SHA-256 of embedded SQL bytes. Use UTC Unix microseconds in storage. Wire/output timestamps are UTC RFC3339 with exactly six fractional digits.

Request records become purge-eligible 30 days after completion but are not automatically deleted. This constant should live in `store`, ready to become configuration only if operations require it.

### 5.3 Transaction and data invariants

- Creation inserts one `local`, `open`, version-1 task and the successful request result in one transaction. Creation itself is not a state transition and creates no state event.
- Normalize task text by converting CRLF/CR to LF and removing spaces/tabs at each line end. Preserve all other content. Reject invalid UTF-8, NUL, and text empty after normalization. The 1 MiB protocol frame bounds request size.
- Update replaces current text. If normalized text differs, increment version exactly once and set `updated_at`; otherwise return `changed=false` without changing version or timestamps.
- State-only changes never increment version. A real transition updates state/`updated_at`, sets `done_at` only on first transition to done, clears it on leaving done, and appends exactly one state event in the same transaction.
- A request to the current state is successful and idempotent: do not alter timestamps or append an event.
- Legal transitions are every edge stated by the SPEC: `open -> working|done`, `working -> open|done`, and `done -> open|working`; same-state calls are no-ops.
- Notes are audit metadata only and are ignored on same-state no-ops. They never become task text.
- A mutation transaction first looks up `request_id`. A matching operation and canonical input hash returns the stored outcome without touching domain rows. Reuse with different operation/input returns `request_id_conflict` (exit 4).
- Store compact mutation results that omit task text, such as `{task_id,version,state,changed,created_at,updated_at}`. This prevents request deduplication from becoming prohibited historical task-text storage. Hash canonical operation inputs; never persist input text in request rows or diagnostics.
- Deterministic semantic errors reached after a valid mutation request (not found, ambiguous prefix, invalid transition/input) are committed as error outcomes. Framing, authentication, unavailable-database, and other pre-dispatch failures are not recorded.
- All writes use explicit transactions. Map busy/constraint/corruption failures to typed errors and never report success before commit.

Identifier lookup accepts an exact ID or an unambiguous prefix that includes its type prefix. Zero matches is not found (exit 3); multiple matches is conflict (exit 4).

## 6. Local protocol v1

### 6.1 Framing and connection behavior

Each frame is a 4-byte unsigned big-endian payload length followed by one UTF-8 JSON object. Reject zero-length, invalid JSON/UTF-8, trailing JSON values, and lengths above the configured maximum before allocating the payload. The first request must be `hello`; incompatible versions close after one typed error. Phase 0 permits multiple ordered requests per connection, but the CLI normally opens one connection per invocation.

Use bounded read/write deadlines and a bounded outbound channel (16 frames). A full queue or deadline disconnects the client; no durable work is discarded. Cap concurrent connections (initially 64). On SIGINT/SIGTERM, stop accepting, allow in-flight requests up to 5 seconds, close clients, checkpoint WAL best-effort, close the database, remove only the socket inode created by this process, release the lock, and exit.

### 6.2 Envelopes and types

Use snake_case JSON and reject unknown fields in Phase 0. Ignoring fields is allowed only after a future handshake explicitly declares a compatible minor-version policy.

```go
type Request struct {
    ProtocolVersion int             `json:"protocol_version"` // 1
    RequestID       string          `json:"request_id"`
    ClientKind      string          `json:"client_kind"` // "cli"; "extension" reserved
    ClientVersion   string          `json:"client_version"`
    Command         string          `json:"command"`
    Params          json.RawMessage `json:"params"`
}

type Response struct {
    ProtocolVersion int             `json:"protocol_version"`
    RequestID       string          `json:"request_id"`
    ServerVersion   string          `json:"server_version"`
    ResultType      string          `json:"result_type,omitempty"`
    Result          json.RawMessage `json:"result,omitempty"`
    Error           *Error          `json:"error,omitempty"`
}

type Error struct {
    Code      string            `json:"code"`
    Category  string            `json:"category"`
    Message   string            `json:"message"`
    Retriable bool              `json:"retriable"`
    Details   map[string]string `json:"details,omitempty"`
}
```

A response has exactly one of result or error. Messages/details are bounded and contain no task text, tokens, SQL, or arbitrary paths outside already user-visible configured paths. Categories map to SPEC exit codes: `usage=2`, `not_found=3`, `conflict=4`, `unsafe=5`, `unavailable=6`, `external_failed=7`, `ambiguous=8`, `unsupported=9`; success is 0.

Commands and result types:

| Command | Parameters | Result type |
| --- | --- | --- |
| `hello` | supported versions, client capabilities | `hello` with selected version and `status`, `doctor`, `local_tasks`, `request_dedup` capabilities |
| `status.get` | empty | `gateway_status` |
| `doctor.run` | empty | `doctor_report` |
| `task.create` | `text` | `task_mutation` |
| `task.list` | optional `state`, `include_done`, `limit`, `cursor` | `task_list` |
| `task.get` | task selector | `task` |
| `task.update` | selector, `text` | `task_mutation` |
| `task.transition` | selector, target state, optional note | `task_mutation` |

All requests have IDs. Mutation IDs supplied by `--request-id` or generated by the CLI are used for durable deduplication; read request IDs are correlation-only. Require a successful hello on the same connection before dispatch.

Define stable JSON DTOs rather than encoding database structs. A `task` has `task_id`, `version`, `state`, `text`, `source_kind`, `created_at`, `updated_at`, and nullable `done_at`. Lists are ordered by `updated_at`, then `task_id`, ascending, and pagination cursors encode that pair. Default list includes `open` and `working`; `--all` includes done; `--state` selects exactly one state and is mutually exclusive with `--all`.

## 7. Executable commands

Implement this Phase 0 subset exactly, with help at every node:

```text
mosscap setup [--json]
mosscap config show [--json]
mosscap config set KEY VALUE [--json]
mosscap gateway run
mosscap gateway status [--json]
mosscap doctor [--json]

mosscap task create (--text TEXT|--text-file PATH) [--request-id ID] [--json]
mosscap task list [--state open|working|done] [--all] [--limit N] [--json]
mosscap task show TASK [--json]
mosscap task update TASK (--text TEXT|--text-file PATH) [--request-id ID] [--json]
mosscap task open TASK [--note TEXT] [--request-id ID] [--json]
mosscap task working TASK [--note TEXT] [--request-id ID] [--json]
mosscap task done TASK [--note TEXT] [--request-id ID] [--json]
```

Use a small standard-library subcommand dispatcher. Global `--config PATH` is allowed before the subcommand for testability and explicit alternate installations. Do not add database/endpoint overrides that bypass validated config.

`--text` and `--text-file` are exactly-one. Read only regular, non-symlink files whose bytes fit in one request frame; reject a file that changes identity/size while read. `--request-id` is mutation-only; when omitted print the generated ID in human output and include it in JSON. JSON mode emits one typed object and no decorations. Human output is concise and sends errors to stderr.

`setup` is idempotent for already-private paths/current schema but refuses to initialize a nonempty or foreign database. It does not start the gateway. Except for `setup` and `config set`, commands never write SQLite directly.

`gateway status` always uses the socket. Its Phase 0 DTO includes process uptime, server/protocol/schema versions, PID, database and endpoint identity, task counts, request-record count, SQLite/WAL sizes, connection count, and health. Future-domain fields are present as `capability: "not_implemented"`, not misleading zero operational counts.

`doctor` first performs local read-only config/path/redaction/storage-space checks, then asks a running gateway for SQLite integrity, pragma verification, a rolled-back write test, endpoint/peer compatibility, and schema compatibility. If unavailable, return a report with those checks `skipped` and overall exit 6; do not open SQLite read-write behind the gateway. Doctor never mutates durable rows, creates a task, contacts Slack, or takes leadership. Checks have stable names and `pass|warn|fail|skipped` status.

Use `slog` structured text to stderr for the gateway. Include request/task IDs and timestamps, never text bodies, notes, token values, raw JSON frames, or SQL argument values.

## 8. Test plan

### 8.1 Unit tests

- `config`: XDG set/unset behavior, strict TOML, allowlisted set keys, atomic rewrite, and redaction snapshots.
- `securepath`: exact modes, wrong owner where test privileges permit, symlinked root/file/component, hard-linked files, writable descendant components, endpoint escaping state root, stale/live socket handling, and secrets-file permissions.
- `identity`: prefix, entropy-read failure, allowed request IDs, and invalid lengths/characters.
- `task`: line-ending/trailing-space normalization, invalid/empty input, no-op update, version increments, every legal transition, done timestamp clearing, same-state behavior, and note bounds.
- `protocol`: golden JSON for every DTO/error; fragmented reads; multiple frames; zero/oversized lengths; malformed UTF-8/JSON; trailing objects; unknown fields/enums; and response exclusivity.
- `app`: flag conflicts, exit-code table, generated/provided request IDs, and byte-for-byte human/JSON golden output with fixed clock/IDs.

### 8.2 Store tests

Use a real temporary-file SQLite database, never an in-memory database (WAL and multi-connection behavior differ). Test:

- migration checksum/current/older/newer/foreign/partial schema rejection;
- required pragmas on more than one pooled connection;
- constraints and foreign keys;
- atomic create/update/transition plus request result;
- repeated request returning the identical compact result;
- same request ID with altered operation/input conflict;
- concurrent duplicate create requests producing one task;
- no-op updates/transitions producing no version/event/timestamp change;
- state event ordering and restart persistence;
- busy timeout and injected pre-commit failure returning no success; and
- quick/integrity-check failure handling where feasible with a corrupted copy.

Assert that after updating text, neither `mutation_requests` nor `task_state_events` contains the old text.

### 8.3 Server and black-box tests

Start the real server on a temporary private tree and use the real client:

- refuse insecure mode/symlink and an already-running gateway;
- socket is `0600`, same-user client succeeds, malformed/slow/oversized clients are disconnected;
- hello/version/capability negotiation and unsupported-version exit 9;
- graceful signal shutdown removes only its own socket and preserves DB;
- status reports exact counts and doctor rolls its write back;
- CLI output contains no configured token canaries; and
- prefix ambiguity and all stable exit categories used by Phase 0.

The release-gate test builds `mosscap`, runs `setup`, starts the gateway, creates and transitions a task with fixed request IDs, kills/restarts the gateway, replays both mutations, and verifies one task, one actual state event, unchanged versions, and matching typed results. Run it under `go test` without shell timing races by polling hello with a deadline.

CI commands for every commit phase:

```text
gofmt -w ...
go vet ./...
go test ./...
go test -race ./...
go build ./cmd/mosscap
git diff --check
```

Also run `govulncheck ./...` in CI (tooling, not a module dependency). Test Linux with `CGO_ENABLED=0` on the current stable Go release.

## 9. Commit phases

Each phase must build and pass its tests; do not land one giant implementation commit.

1. **`build: establish Go executable and internal boundaries`** — module, `cmd/mosscap`, standard-library command dispatcher, version injection, identity/clock helpers, CI, and help tests.
2. **`feat: add private XDG configuration and setup`** — strict TOML model, secure-path validation, atomic config operations, setup behavior, redaction, and filesystem tests.
3. **`feat: initialize the Phase 0 SQLite ledger`** — embedded migration 0001, safe SQLite open/pragmas/compatibility checks, task/request repositories, transaction invariants, and store tests.
4. **`feat: define local protocol v1`** — frame codec, DTOs, handshake/capability/error contracts, client package, golden/fuzz tests, but no domain dispatch yet.
5. **`feat: serve durable local tasks from the foreground gateway`** — lock/socket/peer checks, server lifecycle, task service/dispatch, task CLI commands, logs, and concurrency/restart tests.
6. **`feat: add gateway status and doctor foundations`** — stable status/report DTOs, rolled-back database test, CLI renderers, secret-canary tests, and action-oriented diagnostics.
7. **`test: prove the Phase 0 restart and deduplication gate`** — built-binary end-to-end test, race run, documentation examples, and final security/failure matrix.

## 10. Phase 0 completion checklist and handoff

Phase 0 is complete only when:

- unsafe ownership, modes, links, schema, endpoint, and protocol versions fail closed with the specified category;
- no normal CLI command accesses SQLite directly;
- all domain mutation and deduplication rows commit atomically;
- task updates retain only current text while state transitions remain auditable;
- JSON objects and exit codes have golden compatibility tests;
- restart/replay produces one task and one mutation outcome; and
- logs, status, doctor, config output, request records, and state events contain no token or historical-text canaries.

Before Phase 1, add source/source-event tables in a new migration; do not overload `mutation_requests` for Slack deduplication. Before Phase 2, add consumer/leader/delivery tables and protocol notifications with explicit capability negotiation. Before Phase 3, add effects/files/purge records. No later phase may silently reinterpret the Phase 0 task states or protocol enums.
