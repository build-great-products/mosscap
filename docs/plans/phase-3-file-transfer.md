# Mosscap Phase 3: bounded file transfer

**Status:** ready for an agentic implementation team after the Phase 2 acceptance gate

## 0. Authority and scope

This plan is the authoritative completion contract for the **file-transfer half** of Phase 3.

[`docs/SPEC.md`](../SPEC.md), especially §§2–4, 8–9, and 11–18, remains the product authority. The Phase 1/2 plans describe the implemented prototype boundaries. Where those prototype plans defer files, this plan restores only the file-transfer requirements frozen below. If an implementation choice in this plan is narrower or more explicit than the SPEC, **this plan wins for this increment** and the finding must be recorded rather than silently broadening scope.

The governing rule is:

> Transfer explicitly requested bytes within one trusted Slack channel, while making every local and remote uncertainty visible and retaining no permanent byte archive.

The SPEC groups file transfer and purge into Phase 3. This plan does **not** implement root-level durable-metadata purge. Purge needs its own referential-graph plan after the file schema is stable. Completing this plan therefore proves the bidirectional file-transfer gate, but does not by itself complete all of SPEC Phase 3.

No implementation task may use real Slack credentials except the opt-in manual run in §15. Live tokens and authorization headers never belong in source, SQLite, logs, fixtures, protocol/Pi output, or handoff evidence. Raw transferred file bytes and authenticated or capability-bearing private Slack download/upload URLs are transient transfer data and must not be deliberately persisted in durable records. Private Slack message text is retained only through the SPEC-authorized current task/read/delivery data flow. An ordinary configured-channel permalink remains existing source metadata.

Local filesystem paths and ordinary operational diagnostics are not redacted on trusted same-user operator or opted-in agent surfaces; §3.1 freezes that decision.

## 1. Outcome and representative workflows

Phase 3 file transfer answers:

> Can Mosscap retrieve an admitted Slack attachment only when requested and explicitly upload validated local files, without losing durable identity or lying about races and ambiguous Slack outcomes?

### 1.1 Incoming

1. Slack admission stores bounded attachment metadata and a stable `fil_…` identity, but no URL or bytes.
2. `mosscap file get FILE` asks the gateway to fetch fresh authenticated metadata and bytes.
3. The gateway streams into a generated `0600` file under the private state `tmp/` directory, enforces the active size/capacity limits, and computes SHA-256.
4. Without `--output`, the CLI returns the private path and immutable expiry.
5. With `--output`, the CLI copies from that validated transfer into a generated `0600` destination-side staging file and atomically installs it. It then best-effort releases the gateway temporary transfer.
6. Expiry removes temporary bytes and their live-path row, but never the durable `files` record or task.

### 1.2 Outgoing

1. `mosscap slack file --file PATH` or `mosscap slack send ... --file PATH ...` resolves CLI-relative paths to lexical absolute paths.
2. The gateway securely opens regular, non-symlink inputs, enforces count/logical-size bounds, hashes each file, and records one exact `file_upload` effect plus ordered `to_slack` file rows before Slack I/O.
3. It transitions the effect to `submitted` before requesting Slack upload URLs.
4. It streams the already-open files to Slack staging, re-hashes and revalidates identity/size, and calls Slack completion only if every file still matches the prepared intent.
5. Conclusive completion atomically records the effect and Slack file identities. Any possibly completed operation is `ambiguous` and is never retried automatically.
6. A prepared upload recovered after restart is re-opened and must match every recorded path, size, and SHA-256 before submission. A mismatch fails locally without an API call.

## 2. Existing substrate and gaps

Reuse the passing Phase 0–2 implementation; do not redesign tasks, Slack admission, delivery, or the TypeScript controller.

Existing useful substrate:

- complete file size/count/lifetime configuration and private XDG `tmp/` setup;
- `fil_…` identities and Phase 1 incoming metadata from Socket Mode;
- a `files` table with reserved direction/hash/path/state fields;
- `file show` metadata-only CLI/protocol routing;
- a durable `file_upload` effect enum, response file ID column, effect list/inspect/retry, and startup submitted-to-ambiguous reconciliation;
- exact intent-before-I/O and fake Slack pre/post-call fault boundaries;
- owner/path helpers using `O_NOFOLLOW`, inode checks, and bounded reads;
- a redirect-refusing Slack Web adapter and secret-safe classifiers;
- additive protocol-v1 capability negotiation; and
- Phase 2 visible delivery and current Slack source metadata.

Known gaps that must be addressed deliberately:

1. `SlackFile` and its query expose only `metadata_only` incoming rows; no transfer lifecycle exists.
2. Source edits currently delete removed attachment rows and only reconcile metadata-only rows. Once transfer evidence exists this would either lose durable metadata or violate uniqueness.
3. The current `files.temporary_path` shape permits only one path per file and cannot represent concurrent/repeated downloads safely.
4. `file_upload` is intentionally rejected by effect canonicalization and Slack contracts have no download/upload methods.
5. Existing `securepath.ReadRegularFile` loads the whole file and requires private `0600` ownership; it is not the outgoing-file contract.
6. The current adapter refuses all redirects. Downloads require a separate bounded redirect policy that never forwards credentials cross-origin.
7. The current effect schema cannot truthfully represent a prepared upload that fails local restart revalidation before submission.
8. Protocol DTOs omit transfer hashes/expiries, upload paths, outgoing file relationships, and attachment metadata in Pi delivery.
9. Status omits temporary byte usage; doctor does not reconcile the temporary directory or verify file-write authority.
10. The Phase 1 app manifest deliberately omits `files:write`.

Baseline `go test ./...` passes at revision `aad08b2`; retain that as the starting regression expectation. The authorized Phase 2 real Slack/Pi trial has passed: one trusted Slack task produced one persisted visible Pi delivery, gateway restart/reconnect caused no duplicate, and explicit off was safe. Phase 3 still requires its own manual file-transfer Slack trial in §15.

## 3. Non-negotiable invariants

1. **Metadata first, bytes lazy:** admission and Slack reads never download bytes or retain authenticated download URLs.
2. **Temporary bytes only:** durable rows contain identity, metadata, size, hash, path metadata, and outcomes—not byte blobs. No outgoing permanent copy is created.
3. **Owner-only staging:** every Mosscap-created transfer/staging file is atomically created with mode `0600`; generated names never contain an external filename.
4. **Private lifecycle authority:** retained incoming bytes live only under the configured private state `tmp/`; the gateway owns creation, expiry, release, and restart reconciliation.
5. **Bounded transfer:** actual streamed bytes, declared lengths, logical local size, files per operation, live temporary count, live temporary bytes, redirects, metadata, and protocol frames are bounded before unbounded allocation.
6. **Stable file identity:** one admitted Slack attachment retains its `fil_…` record through source edits, download failures, expiry, and task state changes. Detached metadata is purged only by the future explicit purge feature.
7. **Untrusted file data:** name, extension, MIME label, permalink, content, and declared size are never execution or trust signals. Mosscap does not execute, extract, preview as HTML, or infer a type from bytes.
8. **Race-safe local input:** outgoing paths are opened without following symlinks, are regular files, remain bound to one open descriptor, and match prepared identity/size/hash through Slack staging.
9. **Race-safe output:** default output is exclusive. `--force` never follows or targets a symlink and replaces only an observed regular file through the reviewed atomic replacement helper.
10. **Safe redirects:** every download hop is HTTPS and on the frozen Slack file-host allowlist. Authorization is sent only to the exact original origin and stripped on every cross-origin hop.
11. **Intent before Slack I/O:** the complete ordered upload intent and all file hashes commit before the first Slack upload API exchange.
12. **Honest upload outcome:** after durable submission, a timeout, disconnect, malformed proof, process death, or unknown completion result is `ambiguous`; restart makes no duplicate Slack call.
13. **No wrong-byte completion:** Slack completion is not called if any staged local source changed identity, logical size, or SHA-256. A restart mismatch makes the prepared effect conclusively failed without Slack I/O.
14. **Explicit retry only:** retry creates a linked effect. An ambiguous predecessor requires duplicate-risk confirmation; every current path must match the predecessor's ordered size/hash set.
15. **Task independence:** downloads, uploads, output replacement, expiry, and file failures do not change task state. Task transitions do not transfer a file.
16. **Single-channel authority:** upload commands contain no team/channel selector. The active verified runtime supplies the only destination.
17. **Credential and transfer-data handling:** bot tokens and authorization headers are always redacted and never enter diagnostics, status, logs, Pi messages, or mutation-result JSON. Raw file bytes and authenticated/private Slack transfer URLs are handled transiently and are not deliberately persisted in durable records; this is a lifecycle rule, not a generic output-redaction rule.
18. **Same-user boundary:** filesystem and socket checks prevent accidental or less-privileged races; Mosscap does not claim defense against a compromised account with the same effective UID.

### 3.1 Operational observability boundary

Mosscap intentionally does not redact local filesystem paths or ordinary operational diagnostics. Owner-only CLI/protocol output, status, doctor, gateway logs, explicit inspection, and opted-in Pi messages may render their complete values. This includes local source, destination, temporary, database, socket, and state paths and underlying local OS/application errors. Summary views may remain compact as a presentation choice, but no basename substitution, path allowlist, path scrubber, or path-redaction test gate is required.

Pi-visible output enters model context and may persist in Pi JSONL, so the configured model provider is part of this observability boundary. Shared tickets and committed trial findings are governed by the operator's publication choices, not by a Mosscap path-confidentiality claim.

Credential values remain the non-negotiable redaction class. Adapter contracts should avoid embedding tokens or authorization headers into returned errors. Existing typed Slack diagnostics may remain for stable behavior, but ordinary local diagnostic detail must not be stripped merely because it contains a path or machine-local name.

File bytes and authenticated/private Slack transfer URLs remain transient by design: they are not durable metadata and routine Pi task projection does not copy them. That data-flow constraint does not create a broad output sanitizer. Pi may read an explicitly retrieved file through ordinary tool authority.

The boundary offers no protection from same-effective-UID compromise and does not attempt to hide local machine structure from the operator, their agent, or their chosen model provider.

## 4. Frozen bounds and semantics

Use the active immutable configuration snapshot for each attempt:

| Bound | Rule |
| --- | --- |
| File bytes | `maximum_file_size`, default 100 MiB; enforce declared length when present and streamed bytes with `limit+1` |
| Files in one upload | `maximum_files_per_slack_operation`, default 20; `slack file` remains exactly one |
| Incoming get | exactly one durable `fil_…` selector per command |
| Live retained downloads | at most `maximum_files_per_slack_operation` paths and at most `maximum_file_size × maximum_files_per_slack_operation` bytes after an expiry sweep |
| Retention | creation snapshot's `temporary_download_lifetime`, default 24 hours; reload never extends an existing expiry |
| Redirects | at most 5 hops; no loops, userinfo, fragments, IP literals, non-HTTPS, or non-default ports |
| Paths | at most 4096 UTF-8 bytes, absolute on the wire, no NUL |
| Filename/MIME/permalink | retain current 1024/255/4096-byte bounds |
| SHA-256 | 64 lowercase hexadecimal characters in durable/protocol values |
| Protocol | paths and IDs only; file bytes never enter the length-prefixed JSON frame |

An empty regular file is within the local size bound. The adapter must not reject it solely because slack-go's convenience helper rejects size zero; use the lower-level three-step API. If real Slack conclusively rejects zero-byte upload, return a typed failed effect and record that product finding rather than inventing bytes.

Incoming declared size is untrusted. A mismatch with actual bytes is retained as declared versus validated metadata and is not by itself a failure when actual bytes fit. The first successful SHA-256 becomes the durable identity evidence for that Slack file record. A later download of the same Slack file ID that produces a different hash is rejected as `file_content_changed`; delete the new temporary bytes and preserve the prior validated size/hash.

## 5. Durable model and migration 0005

Add `internal/store/migrations/0005_file_transfer.sql`, set schema version 5, embed/checksum it, and preserve exact migration-history verification. The migration must atomically rebuild affected tables because SQLite cannot safely weaken the existing checks in place.

### 5.1 Durable `files`

Rebuild `files` as the byte-free identity/outcome table. Preserve existing `fil_…` IDs and incoming metadata. The final logical fields are:

```text
file_id                    fil_… primary key
direction                  from_slack | to_slack
slack_file_id              incoming required; outgoing set after confirmation
permalink                   nullable
original_name               bounded untrusted basename/Slack name
declared_mime               nullable bounded untrusted metadata
declared_size_bytes         nullable, incoming Slack declaration
validated_size_bytes        nullable, actual bounded bytes
sha256                      nullable lowercase SHA-256
related_task_id             incoming required; outgoing audit relation may remain on effect
related_effect_id           outgoing required
effect_ordinal              outgoing 0-based ordered position
source_attached             incoming boolean
source_detached_at_us       nullable
source_path                 outgoing lexical absolute path only
source_device/source_inode  outgoing prepare-time path metadata
source_mtime_ns             outgoing prepare-time metadata, diagnostic not authority
transfer_state              closed direction-specific enum
diagnostic_code             nullable allowlisted code
created_at_us/updated_at_us
```

Direction-specific states:

- incoming: `metadata_only`, `validated`, `unavailable`, `oversized`, `content_changed`;
- outgoing: `prepared`, `submitted`, `confirmed`, `failed`, `ambiguous`.

Constraints must enforce:

- incoming has Slack file ID, task, no effect/path/ordinal, and one row per `(slack_file_id, task_id)`;
- outgoing has effect, ordinal, absolute source path, validated size/hash, and one row per `(effect_id, effect_ordinal)`;
- hashes and validated sizes appear together;
- source attachment fields apply only to incoming;
- response Slack IDs/permalinks never replace local `fil_…` identity; and
- a file row can never reference both unrelated task/effect authorities accidentally.

Source reconciliation becomes append-preserving:

- update/re-add the existing `(Slack file ID, task)` row and retain its hash/outcomes;
- mark absent current attachments `source_attached=false` with a detached timestamp instead of deleting them;
- list only attached rows on current `task show` and Pi work;
- allow explicit `file show/get fil_…` for a detached but retained incoming record; and
- never clear validated hash/size merely because Slack metadata changes.

### 5.2 Live temporary transfers

Add a separate ephemeral `file_temporary_transfers` table:

```text
transfer_id       xfr_… primary key
file_id           FK files ON DELETE RESTRICT
generated_name    unique basename matching one strict Mosscap pattern
size_bytes
sha256
expires_at_us
created_at_us
```

The table stores a generated basename, not a user filename or arbitrary destination. The absolute path is derived from the active fixed state `tmp/` root. Add indexes for expiry and file lookup.

A row exists only while the corresponding private regular file exists. Normal release/expiry unlinks verified bytes first and then deletes the row. If unlink fails, retain the row and expose an action-required safe diagnostic; never claim expiry succeeded. Recognized orphan temporary names left by a crash before DB commit are removed on startup. Unknown names, symlinks, wrong-owner entries, or wrong-mode entries are never followed or silently removed; startup/doctor reports them safely.

Deleting the ephemeral transfer row is permitted automatic byte lifecycle, not durable metadata purge. The parent `files` row remains.

### 5.3 Upload effects

Extend/rebuild `slack_effects` checks so `file_upload` is a public exact intent with:

- optional thread timestamp;
- optional normalized initial comment in `text`;
- optional task audit relation;
- no message target, emoji, or delete reason;
- payload hash over operation version, active team/channel, thread, comment, task relationship as currently defined, and every ordered `(source path, original name, validated size, SHA-256)` tuple; and
- child outgoing file rows inserted in the same transaction as the prepared effect and request-dedup result.

Allow a file-upload-only local transition `prepared -> failed` with `attempt_count=0`, no `submitted_at`, and a completed timestamp/diagnostic. This is used only when a recovered prepared path no longer matches before any Slack exchange. Generic text/reaction effects retain their current state checks.

Confirmed upload outcome and all child Slack file IDs/permalinks commit in one transaction. If SQLite commit fails after Slack completion, leave recoverable submitted evidence; startup converts it to ambiguous.

### 5.4 Request replay

`file.get` is a gateway mutation because it creates temporary bytes. It requires a request ID (auto-generated by the CLI) and stores a compact result containing only `file_id` and `transfer_id`. The path is resolved from the live transfer table when replying; mutation-result JSON never stores it.

- replay while live returns the same transfer/path/expiry without downloading again;
- replay after release/expiry returns typed `file_transfer_expired`, never a dead path;
- request-ID reuse with another file conflicts; and
- a new request ID is an explicit new download attempt.

`file.release` is connection-safe and idempotent for the exact transfer. A CLI crash may leave the bytes until expiry; it does not corrupt the output file or durable metadata.

## 6. Filesystem and temporary-file boundary

Create a focused `internal/fileio` package rather than expanding `securepath.ReadRegularFile` into incompatible roles. Keep Linux-specific syscall use explicit; this repository already relies on Linux peer credentials.

### 6.1 Secure outgoing source

`fileio.OpenSource` must:

1. accept an absolute lexical path only;
2. walk/open it with no symlink traversal (`openat2` with `RESOLVE_NO_SYMLINKS`/`RESOLVE_BENEATH` where applicable, or an equally reviewed dirfd walk);
3. open `O_RDONLY|O_CLOEXEC|O_NOFOLLOW`;
4. require a regular file and reject directory, device, FIFO, socket, and final/parent symlink;
5. check logical `stat.Size()` against the limit before reading—this rejects an oversized sparse file without allocating its holes;
6. stream SHA-256 through a bounded reader rather than loading bytes in memory;
7. re-stat the descriptor and path, comparing device, inode, mode, and size; and
8. retain the descriptor for the prepare/upload attempt.

Outgoing inputs need not be owner-owned or mode `0600`; they are operator-selected readable files. Hard links are regular inputs and are not rejected merely for link count. Identity/size/hash, not filename extension or MIME, authorizes bytes.

During Slack staging, seek the retained descriptor to zero, stream exactly the prepared size while computing a second hash, reject early EOF or an extra byte, then re-stat descriptor and path. The adapter must not call Slack completion until every source passes this second validation.

### 6.2 Private retained download

Open the configured `tmp/` once as a verified owner-owned `0700` directory descriptor. Create random names with at least 128 bits of entropy using `openat(O_CREAT|O_EXCL|O_NOFOLLOW|O_CLOEXEC, 0600)`. Verify regular type, owner, mode, and single link. Write and hash through the open descriptor, `fsync` content before publishing the DB row, and never derive the name from Slack metadata.

Failure cleanup closes and unlinks only the verified generated entry. No partial path is returned or persisted.

### 6.3 Atomic explicit output

The CLI performs destination placement after receiving a validated transfer reference:

1. verify the transfer path is a direct generated child of the configured private `tmp/`, owner-owned regular `0600`, and matches the returned size/hash while copying;
2. open the destination parent by dirfd without following symlinks;
3. create a random sibling staging file exclusively at `0600`;
4. copy, hash, `fsync`, and verify exact length/hash;
5. without `--force`, atomically install with no-replace semantics (`renameat2(RENAME_NOREPLACE)` or a reviewed equivalent);
6. with `--force`, require the observed destination to be a regular non-symlink and use the reviewed exchange/identity-check replacement helper so a symlink or non-regular race is never followed;
7. `fsync` the parent when supported; and
8. clean the sibling staging name on every failure.

A platform lacking the required atomic/no-follow primitives fails closed with typed `unsupported`; it must not fall back to `os.WriteFile`, truncate-before-validation, or check-then-follow behavior. Cross-filesystem output works because copying is into a sibling staging file before same-directory rename.

The final newly created/replaced output is mode `0600`. `--force` is invalid without `--output`; it cannot replace a directory, symlink, FIFO, socket, or device.

## 7. Slack adapter contracts

Add a narrow SDK-free `slackapi.FileTransfer` boundary and include it in a verified runtime snapshot. Tests inject it independently from Socket Mode.

### 7.1 Incoming adapter

Expose a logical operation such as:

```text
Download(file_id, bounded_writer) -> actual Slack file metadata
```

The real adapter:

1. calls `files.info` with the durable Slack file ID to obtain a fresh private download URL and current safe metadata;
2. validates file identity and bounds;
3. keeps the authenticated URL only in memory;
4. performs a manually controlled HTTPS GET with at most five redirects;
5. approves hosts by normalized ASCII hostname and dot-boundary suffix, with an initial frozen allowlist of exact `files.slack.com` plus Slack-documented `*.slack.com` and `*.slack-edge.com` file hosts;
6. sends `Authorization: Bearer …` only when the request origin exactly equals the original approved origin; strips it on every changed host, scheme, or port;
7. rejects unknown host, userinfo, IP literal, port, downgrade, loop, malformed location, oversized content length, non-success response, and streamed oversize with typed safe errors; and
8. never exposes or logs the URL/header/body.

The manual run must record only whether the observed real host matched the allowlist. If Slack now uses another host, verify it against current Slack documentation and update the allowlist plus tests deliberately; never accept arbitrary redirect hosts to make the trial pass.

### 7.2 Outgoing adapter

Use Slack's external upload sequence directly rather than the convenience method:

1. `files.getUploadURLExternal` for each ordered file;
2. stream each file to its returned staging URL;
3. invoke the service's post-stage source validator; and
4. only then call one `files.completeUploadExternal` with the configured channel, optional thread/comment, and ordered file IDs.

The adapter contract must make “no completion after validator failure” testable. Upload URLs remain in adapter memory only. Apply the same HTTPS/host/no-credential-forwarding policy to staging URLs; never attach the bot token to a different upload origin.

Validate completion proof strictly: returned file count/order/IDs must match the reservation. Resolve permalink metadata from the conclusive completion response when present, otherwise perform bounded `files.info` reads by confirmed ID. A metadata-read failure after proven completion does not turn an applied upload into ambiguous: commit confirmed IDs, leave permalink null with allowlisted `permalink_unavailable`, and permit later inspection to refresh it.

Classify each external call through one adapter-level classifier. No SDK error string, URL, or response body crosses into domain diagnostics.

### 7.3 Fake Slack

Extend the deterministic fake with download, reserve/stage/validate/complete operations and explicit barriers:

- pre-call failure proves no exchange;
- post-call failure proves possible external action;
- staged bytes are bounded and inspected only in test memory;
- tests can mutate/replace a local source between prepare, stage, validation, and completion; and
- the fake records whether effect state was already submitted when the first upload exchange occurred.

## 8. Incoming transfer service

Add `internal/filetransfer` with store, clock, file I/O, and Slack adapter interfaces. It owns incoming retrieval and temporary cleanup, not output destinations.

For `Get`:

1. resolve an incoming `fil_…` record; reject outgoing IDs;
2. replay a live request if present;
3. sweep expired transfers and enforce live count/byte capacity;
4. reject known declared oversize before byte download, but retain/update typed file metadata;
5. acquire one gateway-wide transfer slot to avoid multiplying 100 MiB temporary allocations;
6. create a private unpublished temp file;
7. ask the active Slack snapshot adapter to stream by Slack file ID through `limit+1` and SHA-256;
8. validate actual size/hash and immutable prior hash when one exists;
9. `fsync`, then transactionally update the durable file metadata, insert the live transfer row, and commit compact request evidence;
10. return the transfer reference/path only after commit; and
11. unlink on every pre-commit failure.

Unavailable, oversized, unsafe-redirect, interrupted, and content-changed downloads leave the task and durable file identity intact. Use a new request ID to retry retriable failures.

Startup, an unref'ed bounded ticker (for example once per minute), and every `file.get` perform bounded cleanup. Reconciliation operates by directory descriptor and strict generated basename. It must handle:

- expired row + file: unlink then delete row;
- live row + missing file: delete the stale live row and retain a safe file diagnostic;
- recognized orphan file + no row: securely unlink;
- wrong inode/type/owner/mode or unknown name: fail/warn safely without following; and
- cancellation/restart at each filesystem/transaction boundary.

Shutdown stops the sweeper before SQLite closes. Reloaded operations use the new size/count/lifetime for new work; existing transfer expiries and paths remain unchanged.

## 9. Outgoing effect service

Extend `internal/slackops` behind a focused upload-source interface; keep current text/reaction behavior unchanged.

### 9.1 Prepare and execute

For a new upload command:

1. validate command shape and file count;
2. securely open/hash every source in caller order while retaining descriptors;
3. derive bounded untrusted basenames; do not persist MIME guesses;
4. transactionally prepare one `file_upload` effect and all outgoing file rows;
5. on request replay, return the existing effect and never create another;
6. revalidate retained descriptors/path identity before claim;
7. transition effect and child rows to submitted before the first Slack exchange;
8. stage exact bytes while rehashing, then run post-stage descriptor/path checks;
9. skip completion and commit `failed/file_changed` if local validation fails;
10. call one Slack completion only after all sources validate; and
11. atomically commit confirmed/failed/ambiguous effect and child file outcomes.

Use one effect for a multi-file `slack send`; the normalized text is Slack's initial comment, not a second send effect. This prevents task-state coupling and avoids a text post succeeding separately from the file operation.

Any process death with a submitted upload becomes ambiguous at startup, regardless of which external sub-step was reached. No staged reservation is resumed and no API call is made automatically.

### 9.2 Prepared recovery

Extend `Recover`:

- reconcile all submitted effects to ambiguous first, as today;
- for a prepared file upload, securely reopen every recorded path and hash in order;
- if all size/hash values match, retain descriptors and execute it once;
- if any path is missing, unsafe, changed, or oversized, atomically transition prepared upload and child rows to failed with zero attempts and no Slack call; and
- continue recovering other prepared effects without spinning on the failed row.

Prepare-time device/inode/mtime are diagnostic evidence. Across restart, byte-identical size/hash at the same recorded path is sufficient even when the inode changed; within one active attempt, path identity must remain stable.

### 9.3 Explicit retry

For a failed or ambiguous upload predecessor:

1. apply existing state and duplicate-risk rules;
2. securely reopen the exact ordered paths;
3. require count, path, size, and SHA-256 to match predecessor rows;
4. on mismatch, record the request's deterministic conflict but create no successor—operator must issue a new `slack file/send` command;
5. on match, create a linked prepared effect and cloned new `fil_…` rows in one transaction; and
6. execute only the successor.

A retry never changes predecessor file/effect rows and never relies on retained staged bytes.

## 10. Protocol, CLI, and Pi delivery

### 10.1 Additive protocol v1

Keep protocol version 1 and advertise a frozen `file_transfer_v1` capability only after all routes and cleanup are wired. Add strict bounded DTOs/commands:

```text
file.show
file.get       { file }
file.release   { transfer_id }          # internal lifecycle route
slack.file     { thread_ts?, file_path, comment?, task? }
slack.send     + ordered file_paths[]
```

Transfer DTOs carry IDs and local paths rather than raw file bytes or private Slack transfer URLs. Credentials remain forbidden. Paths are absolute and structurally bounded for protocol safety, not redacted. `file.get` result contains:

```text
transfer_id, file_id, temporary_path, validated_size_bytes, sha256, expires_at
```

Expanded file inspection contains direction, Slack identity/permalink, bounded untrusted metadata, declared and validated sizes, SHA-256, relations, attachment status, transfer state/diagnostic, timestamps, and live temporary count. A successful `file.get` returns its temporary path. Effect lists may remain compact, while inspection, status, errors, and diagnostics may render complete local paths and ordinary diagnostic detail without redaction.

Fix typed `fil_`/`eff_`/optional `tsk_` protocol selectors to accept the full identity generator's raw-base64url alphabet before adding new file routes. The Phase 2 finding already documents the older lowercase-only gate; do not copy it into `xfr_` validation.

Golden/strict tests must prove unknown fields, too many files, oversized paths/frames, invalid hashes/IDs, channel/team injection, and byte-bearing fields are rejected.

### 10.2 CLI

Expose exactly:

```text
mosscap slack send [--thread THREAD_TS] (--text TEXT|--text-file PATH)
  [--file PATH ...] [--task TASK] [--request-id ID] [--json]
mosscap slack file [--thread THREAD_TS] --file PATH
  [--comment TEXT] [--task TASK] [--request-id ID] [--json]
mosscap file show FILE [--json]
mosscap file get FILE [--output PATH] [--force] [--request-id ID] [--json]
```

Rules:

- `slack send` still requires text; with no files it remains the existing `send` effect.
- `slack file` accepts exactly one file; repeated `--file` is reserved for `slack send` and bounded by active config.
- relative upload and output paths are resolved against the short-lived CLI's working directory before wire use/placement; no gateway-working-directory interpretation is allowed.
- `--force` requires `--output`.
- output conflict leaves the validated gateway temp live until release/expiry so the operator can recover it; no partial destination remains.
- without `--output`, human output prominently states expiry and retention responsibility.
- with `--output`, success JSON reports final destination, size, hash, and file ID but not the consumed temporary path.
- human rendering preserves complete local paths and diagnostics; quoting control characters in untrusted filenames is delimiter safety, not path redaction.

Exit mappings:

| Condition | Category/code |
| --- | --- |
| syntax/count/path encoding | usage / 2 |
| unknown file/transfer | not found / 3 |
| existing destination, stale retry input | conflict / 4 |
| symlink/non-regular/unsafe redirect or destination | unsafe / 5 |
| retriable Slack/storage unavailability | unavailable / 6 |
| conclusive Slack rejection or terminal local upload failure | external failed / 7 |
| possibly completed upload | ambiguous / 8 |
| absent capability/platform primitive | unsupported / 9 |

Use stable safe codes including `file_too_large`, `file_changed`, `file_content_changed`, `unsafe_file_type`, `unsafe_file_path`, `unsafe_file_redirect`, `temporary_capacity`, `destination_exists`, `file_transfer_expired`, and `slack_file_unavailable`.

### 10.3 Pi attachment metadata

Phase 2 deliberately delivered text only. Restore the SPEC's useful attachment projection now:

- add attached incoming file metadata to `DeliveryWorkDTO` (file ID, safely bounded name/MIME/declared size, transfer state only);
- update the native TypeScript strict parser and `Work` type;
- render each filename/MIME as escaped untrusted metadata, never as control text or HTML;
- include `mosscap file show fil_… --json` and `mosscap file get fil_… --output PATH` examples;
- keep credentials out of Pi output and keep routine attachment projection metadata-only rather than copying raw file bytes or transient private transfer URLs;
- allow operational/recovery output to render complete local paths and diagnostics under §3.1; and
- preserve the existing delivery key/details schema so persisted-session deduplication is unchanged.

A file metadata edit already increments task version in Phase 1 and therefore produces a new delivery key. Enabling this projection does not force-redeliver already persisted historical task versions.

## 11. Status, doctor, scopes, and logs

### 11.1 Status

Add a stable `file_transfer` block:

- capability `available`;
- live temporary count and total bytes;
- nearest expiry age/time (bounded metadata);
- incoming counts by transfer state;
- outgoing counts by transfer state; and
- unsafe/orphan temporary entry count as action required.

Existing effect counts remain the authority for prepared/submitted/failed/ambiguous uploads. An ambiguous upload or unsafe temp entry is action required but does not make a responsive gateway process unhealthy. Status may render complete local filenames, paths, and ordinary diagnostics without redaction. It must redact credential values. Private transfer URLs and raw file bytes are absent because status does not own that transient transfer data, not because paths share their classification.

### 11.2 Doctor

Doctor remains non-destructive and must never call `files.info`, request an upload URL, download, upload, or remove a temp entry. Add checks for:

- configured size/count/lifetime validity;
- `tmp/` owner/mode/type and no symlink path components;
- read-only reconciliation of live rows versus generated entries (counts only);
- enough filesystem availability for at least one maximum-sized transfer plus SQLite headroom;
- required `files:read` and `files:write` scopes in the already verified Slack identity;
- file schema/index integrity and adapter capability; and
- platform support for required no-follow/atomic replacement operations.

### 11.3 Slack setup and logging

Update `manifest.json` and the Phase 1 operations documentation to add only `files:write`; retain `files:read` and all single-channel restrictions. Reinstallation is required after scope changes.

Structured logs may include request/effect/file/transfer IDs, operation phase enums, byte counts, durations, diagnostic codes, complete local names/paths, hashes, and ordinary local diagnostic detail. Do not redact machine-local values. Logs must redact tokens and authorization headers. Private Slack transfer URLs and file payload bytes should never be deliberately supplied to the logger because they remain transient adapter/transfer data; this is data minimization rather than a generic output filter.

## 12. Explicitly deferred

- root `purge` and durable metadata retention deletion;
- permanent file cache, archival, cross-file hash deduplication, antivirus, content sniffing, previews, archive extraction, or HTML rendering;
- automatic download on admission/read/Pi delivery;
- automatic upload or Slack reply caused by task state/model prose;
- more than one Slack workspace/channel;
- resumable/chunked Mosscap uploads, background retry, or reconciliation by searching Slack after ambiguity;
- upload effect cancellation;
- destination selection from Slack's untrusted filename;
- public sharing links;
- arbitrary redirect hosts or operator-configurable host allowlists;
- upload/download progress protocol, server push, or file bytes in JSON;
- extension model tools or direct extension file access; and
- broad non-Linux portability when required race-safe primitives are absent.

## 13. Commit-sized implementation sequence

One integration owner freezes schema, state enums, DTO names, diagnostic codes, and adapter interfaces before parallel work. That owner controls migration registration, `internal/server/server.go`, runtime snapshot composition, and final route convergence.

### Increment 1 — `feat: evolve durable file transfer metadata`

Owner: store Terra. Scope: `internal/store/migrations/0005_file_transfer.sql`, migration registration/schema tests, `internal/store/slack_file*`, and narrowly required `slack_effect*` schema/types.

Deliver migration/preservation, append-preserving attachment reconciliation, outgoing ordered file rows, live-transfer repository, file counts, upload intent hashing, pre-submit local failure, and upload retry cloning primitives. No filesystem or Slack I/O.

Gate: v4→v5 rollback/preservation, exact fingerprints/checksums, constraints, request replay, source detach/re-add, legal transitions, retry predecessor preservation, and `go test ./internal/store`.

### Increment 2 — `feat: add race-safe file I/O primitives`

Owner: independent Terra. Scope only `internal/fileio/**` (plus focused platform build files/tests).

Deliver secure source open/hash/revalidation, private generated temp creation/reconciliation helpers, and destination no-replace/force atomic placement. Use deterministic race hooks only in tests.

Gate: regular/text/binary/zero/sparse-within-limit success; oversized sparse, symlinked parent/final, directory/device/FIFO/socket, early/extra bytes, truncate/replace/rename races, wrong mode/owner temp, destination conflict/force, cleanup, and `go test -race ./internal/fileio`.

### Increment 3 — `feat: add bounded Slack file adapters`

Owner: independent Terra. Scope `internal/slackapi/**` and adapter/fake tests; no store/server/app edits.

Deliver frozen download/upload contracts, strict host/redirect/auth policy, lower-level three-step upload, completion proof, fake stages/barriers, safe classifier additions, and `files:write` scope vocabulary.

Gate: `httptest` redirect/token canaries, overlong/loop/downgrade/IP/port redirects, content-length and streamed oversize, interrupted download, zero and multiple upload staging, validator prevents completion, malformed completion, post-call ambiguity evidence, and `go test -race ./internal/slackapi/...`.

Increments 1–3 are safe to run in parallel after contract freeze because their file ownership is disjoint.

### Increment 4 — `feat: download Slack files lazily`

Owner: incoming-service Terra. Scope new `internal/filetransfer/**` and focused tests.

Integrate store, `fileio`, Slack download, hash/immutable-file checks, capacity, request replay, release, startup/ticker cleanup, and reload snapshot behavior behind interfaces. Do not edit central server routing.

Gate: metadata-only until get, submitted bytes never in DB, owner `0600`, exact hash/expiry, replay no second call, declared/streamed oversize, unavailable/redirect/content-change failures preserve task, capacity, expiry/release/orphan/missing/unsafe reconciliation, and race tests.

### Increment 5 — `feat: execute durable Slack file uploads`

Owner: outgoing-effect Terra. Scope `internal/slackops/**` and focused store-facing service tests; coordinate repository calls frozen in Increment 1.

Deliver secure prepare/claim/stage/validate/complete/outcome, multi-file exact intent, recovery revalidation, local prepared failure, and file-aware explicit retry. Keep current text/reaction effects green.

Gate: effect/files commit before first fake call, ordered count/hash, no completion on local change, conclusive/ambiguous outcomes, DB outcome failure evidence, startup submitted no call, prepared match one call, prepared mismatch zero calls, retry hash/duplicate-risk rules, and `go test -race ./internal/slackops`.

Increments 4 and 5 can proceed in parallel after 1–3, but they must not share a new package or edit server/app/protocol files.

### Increment 6 — `feat: expose file transfer through protocol and CLI`

Owner: protocol/CLI Terra. Scope `internal/protocol/**`, `internal/app/**`, client-side destination placement wiring, and focused tests. Freeze wire DTOs before work. This owner alone edits `internal/app/phase1.go`.

Deliver capability, strict commands/results, full base64url selectors, `file get/release`, `slack file`, repeated send files, help/JSON/human output, output placement, and exit mappings. No central server dispatch.

Gate: table-driven parsing/help/output, no team/channel flags, count/frame/path bounds, request IDs/replay, output force matrix, transfer DTOs contain paths/IDs rather than byte payloads or private transfer URLs, credential canaries remain absent, golden frames, and app/protocol tests.

### Increment 7 — `feat: project Slack attachments into Pi work`

Owner: Pi-delivery Terra. Scope the agreed delivery DTO mapping and `pi-extension/**`; avoid central dispatch except the narrow work mapper assigned by integration owner.

Deliver attached file metadata in polls, strict TypeScript validation, escaped visible metadata, retrieval hints, unchanged details/dedup identity, and source-loaded extension tests.

Gate: attached/detached projection, control-character names cannot alter delimiters, credential canaries remain absent, routine attachment projection carries metadata rather than bytes/private transfer URLs, frame bound, Node tests/typecheck, and existing delivery timing/dedup tests.

Increment 7 can run alongside Increment 6 after DTO freeze.

### Increment 8 — `feat: integrate file transfer lifecycle`

Owner: integration Terra. Scope runtime factory/snapshots, startup/shutdown/reload, `internal/server/server.go`, DTO mapping, status/doctor composition, and built-gateway tests.

Wire adapter and services, advertise capability only now, run cleanup before endpoint reachability, stop cleanup before store close, bind operations to one active config snapshot, and atomically map outcomes. Resolve all central conflicts here.

Gate: real gateway with fake Slack for incoming/output/outgoing/restart/reload, startup orphan cleanup before status, shutdown order, same-user protocol, and no regression in Phase 0–2 E2E.

### Increment 9 — `test: prove bounded file transfer faults`

Owner: test/security Terra after integration. Scope focused cross-package/E2E tests only, except fixes required by demonstrated failures.

Add the complete fault matrix in §14, built CLI/gateway credential-canary tests, durable-record checks for private transfer URLs/file bytes, and race validation. Do not add path-redaction canaries or deferred features during correction.

### Increment 10 — `docs: add Phase 3 file transfer operations`

Owner: documentation/trial Terra. Scope `manifest.json`, operations docs, [`../trials/phase-3-file-transfer.md`](../trials/phase-3-file-transfer.md), and a new `phase-3-file-transfer-findings.md`.

Document scope reinstall, temporary behavior, ambiguous upload inspection/retry, explicit destination safety, no archive/backup claims, and the manual run. Run a final scope review against this plan.

Each increment must be one reviewable commit unless a test-only follow-up is needed. Every handoff states changed files, focused/full validation, blockers, security evidence, findings, and deferred temptations.

## 14. Automated acceptance gate

The final suite must prove:

### Incoming and temporary bytes

1. Admission/task/thread reads store metadata only and make zero download calls.
2. One explicit get creates one private random `0600` file, computes exact size/SHA-256, and preserves task state.
3. Request replay while live returns the same transfer and makes no second Slack call; replay after expiry returns typed expired.
4. Explicit output is atomic/exclusive; `--force` replaces only a regular file; failed copy/hash/rename leaves no partial output.
5. Declared oversize, streamed oversize, unavailable file, interrupted body, unsafe redirect, and changed Slack content retain task/file identity and no partial temp.
6. Expiry/release remove only verified generated bytes and live rows; durable file metadata remains.
7. Startup reconciles expired, missing, and recognized orphan files; unsafe/unknown entries fail visibly and are not followed.
8. Live count/byte capacity is enforced after cleanup.

### Outgoing and effects

9. Text, image, PDF, archive, opaque binary, and zero-byte regular inputs within limit reach the fake byte-for-byte.
10. One multi-file send preserves order, enforces configured count, uses one `file_upload` effect, and posts no separate text effect.
11. Directories, devices, FIFOs, sockets, parent/final symlinks, unreadable paths, oversized logical sparse files, and path replacement fail safely.
12. Effect and ordered file hashes commit before the fake's first exchange; submitted commits before any upload URL request.
13. Mutation during staging prevents completion and produces conclusive failed evidence; no wrong bytes are shared.
14. Conclusive completion records all Slack file IDs and available permalinks atomically.
15. Post-submit timeout/disconnect/malformed proof/DB-outcome failure yields submitted/ambiguous evidence and restart makes no duplicate call.
16. Prepared recovery uploads only matching paths; missing/unsafe/size/hash mismatch becomes failed with zero Slack calls.
17. Explicit retry creates linked effect/file rows, requires ambiguity confirmation, and rejects current byte mismatch without a successor.
18. Slack/file operations never change task state.

### Security, protocol, and projection

19. Download redirects enforce HTTPS/allowlist/hop bounds and never forward authorization cross-origin.
20. Slack names/MIME/bytes cannot choose a temp/output path or alter Pi control delimiters; quoting untrusted metadata does not redact local paths or ordinary diagnostics.
21. SQLite contains no credential, authorization header, raw file-byte blob, or authenticated/private transfer URL. Exact local paths remain allowed wherever useful; compact mutation replay records omit temporary paths for lifecycle correctness, not confidentiality.
22. Credential canaries remain absent from SQLite, logs, status, doctor, protocol, and Pi output.
23. Protocol uses additive capability negotiation, strict structurally bounded DTOs, the full generated ID alphabet, and path/ID transfer references rather than raw file bytes or private transfer URLs.
24. Status reports temp count/bytes separately from health; doctor is read-only and performs zero Slack transfer calls.
25. Every existing Phase 0/1/2 Go and Node test remains green except intentional additive DTO/status/schema expectations.

Use deterministic hooks for races and fake post-submit ambiguity. Do not attempt flaky real filesystem races or induce duplicate-risk behavior in a real Slack workspace.

## 15. Manual real-Slack acceptance runbook

Create [`docs/trials/phase-3-file-transfer.md`](../trials/phase-3-file-transfer.md) during Increment 10 with the following exact safety shape.

### Prerequisites

- disposable Slack workspace/channel and disposable credentials supplied only in owner-only local config;
- app reinstalled with `files:read` and `files:write`;
- harmless local fixtures containing small text, image, PDF, archive, opaque binary, and optionally zero-byte content;
- a private disposable XDG state root and safe output directory;
- full automated gate passed at the revision under test; and
- credentials remain absent from the report; raw file bytes and private Slack transfer URLs are not recorded because they are transient transfer data. Local paths and ordinary diagnostics need not be redacted.

### Procedure

1. Start the foreground gateway; verify connected status and doctor file-transfer checks without any Slack file call.
2. From a trusted user, post one direct mention with a harmless small attachment. Verify routine task/Pi metadata contains `fil_…` and a retrieval command without gratuitously adding a temporary path, authenticated/private transfer URL, or file bytes.
3. Run `file show`, then `file get` without output. Verify path is under private `tmp/`, mode `0600`, size/hash match a separately computed local fixture, and status count/bytes increase.
4. Run `file get --output` to a new destination; verify exact hash, mode `0600`, and no sibling staging residue.
5. Repeat to the same path without force (must conflict), with force over a regular file (must succeed), and with force against a symlink/directory (must fail without changing its target).
6. Upload each broad fixture once with `slack file`, including a threaded example. Inspect confirmed effect and outgoing `fil_…` records; verify visible Slack file identity/permalink when Slack supplies it and no task-state transition.
7. Run one `slack send --text ... --file ... --file ...` within the configured count and verify one comment/file operation, ordered files, and one durable upload effect.
8. Restart the gateway and verify no confirmed/ambiguous operation is repeated. Do not deliberately cause a real ambiguous completion.
9. In the disposable environment, temporarily set a short positive download lifetime, reload, create a no-output transfer, wait past expiry/sweep, and verify bytes disappear while `file show` metadata and task remain.
10. Inspect status, doctor, effect/file output, Pi diagnostics, and logs. Confirm credential values remain redacted and private Slack transfer URLs/file payload bytes were not deliberately persisted. Local paths and ordinary diagnostics may be recorded in full.
11. Restore/revoke disposable configuration and remove test outputs/state.

### Stop conditions

Stop immediately if credentials are exposed, private Slack transfer URLs or raw file bytes are deliberately persisted as durable metadata, an unknown redirect host is accepted, a symlink target is modified, partial output is exposed, bytes download before an explicit get, wrong/changed bytes complete, an ambiguous upload retries, a file operation changes task state, or temporary bytes survive verified expiry without an action-required diagnostic. A complete local path or ordinary diagnostic is not a stop condition.

The code may be reported **complete pending operator acceptance** when credentials are unavailable. This file-transfer phase passes only when the automated gate and this manual run both pass; the passed Phase 2 trial neither satisfies nor implies Phase 3 file-transfer acceptance.

## 16. Final validation

Run from a clean worktree:

```text
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

Also inspect the built-binary credential-canary tests, durable-record checks for private transfer URLs/file bytes, and migration from a real schema-v4 fixture. Do not require path-redaction canaries. `govulncheck`, durable purge, broad non-Linux portability, antivirus/content-policy certification, and repeated production threat-review cycles remain outside this increment unless separately commissioned.

## 17. Risks and decisions requiring review

| Risk/decision | Frozen response |
| --- | --- |
| Slack upload is three external steps with no Mosscap-wide idempotency token | Treat the whole claimed effect as one possibly applied boundary; submitted crash/uncertainty is ambiguous and never automatic |
| Slack may change download/CDN hosts | Fail closed to a documented allowlist; update only with Slack documentation plus adapter tests and manual evidence |
| Completion may prove upload before permalink lookup succeeds | Confirm the applied effect with durable Slack IDs; retain `permalink_unavailable` rather than falsely marking the upload ambiguous |
| Local source can change through an open inode | Hash before prepare and during staging; compare descriptor/path identity and size before completion |
| Same-user attacker can race filesystem operations | Use no-follow dirfd/atomic primitives and state the same-user non-adversarial boundary; never claim sandboxing |
| Arbitrary output may be on another filesystem | Copy into a generated sibling and rename within the destination directory; never rename directly from state `tmp/` |
| Repeated no-output downloads can fill disk before 24 hours | Derive live path/byte caps from configured file count × size and sweep before allocation |
| Existing source edit deletes file metadata | Migration and repository change to attached/detached durable rows before any transfer can update state |
| Existing one-path `files` schema cannot model repeated gets | Separate ephemeral `xfr_…` rows from durable `fil_…` identity |
| Outgoing absolute paths reveal local user/project structure | Accept that disclosure on operator, log, protocol, status, and agent/model surfaces; retain paths for restart correctness without redaction and future purge them with the effect graph |
| Prepared mismatch cannot honestly be `submitted` | Permit file-upload-only prepared→failed with zero attempts and no Slack call |
| Current protocol selectors reject generated uppercase/base64url IDs | Correct selector validation before exposing file transfer; preserve protocol version via capability negotiation |
| Pi details schema is persisted delivery evidence | Add attachment data only to visible content/work DTO, not custom-message details or delivery key |
| SPEC Phase 3 also includes purge | Keep purge explicitly separate; do not call full Phase 3 complete after this plan |

Any implementation that stores authenticated URLs/bytes durably, follows an unapproved redirect, completes an upload after failed byte revalidation, overwrites through a symlink, automatically retries ambiguity, or couples files to task state is a release blocker.
