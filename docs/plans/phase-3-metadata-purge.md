# Phase 3B: durable metadata purge contract

**Status:** authoritative implementation contract; implementation intentionally deferred.

This plan owns the metadata-purge half of Phase 3. [`docs/SPEC.md`](../SPEC.md) is authoritative except where this plan makes a more restrictive implementation decision. It follows the completed file-transfer schema in migration `0005_file_transfer.sql`; it must not create a byte archive or redefine file transfer.

## 1. Command, cutoff, and operator intent

Expose only:

```text
mosscap purge --before RFC3339 --dry-run
mosscap purge --before RFC3339 --confirm
```

Exactly one mode is required. `--before` is required, parsed strictly as RFC 3339 with an explicit UTC-capable offset (`Z` or `±HH:MM`), rejects date-only/local/ambiguous input, and is normalized to an integer UTC microsecond cutoff. Fractional precision beyond microseconds is rejected rather than rounded. Eligibility is **strictly older**: a timestamp equal to the cutoff is retained. The rendered cutoff is canonical UTC RFC 3339 with microseconds.

`--dry-run` is read-only: it performs the same eligibility traversal and reports no mutation, maintenance, temporary cleanup, or request-dedup record write. `--confirm` is irreversible and must use the gateway mutation request ID/deduplication contract. Replay of a completed confirmed request returns its original typed report without deleting another batch; reuse with a different normalized cutoff or mode conflicts. The request record itself remains live during its retention period and is not selected by that same request.

Both modes report grouped counts by record type and disposition:

```text
eligible, skipped_live, skipped_referenced, removed
```

The report includes task/state-event/source-event/delivery/effect/file/temporary-transfer/request/consumer counts, batch count, cutoff, and whether SQLite maintenance ran. A zero report is success.

## 2. Retention graph and eligibility

Purge is graph-based, not independent table-age deletion. It may remove only finalized, cutoff-old roots and dependencies that become exclusively owned by removed roots.

### Eligible roots

- `tasks` only when `state='done'`, `done_at < cutoff`, and every dependent delivery/effect is itself removable.
- `slack_effects` only when `state='confirmed'` and its terminal/completion timestamp is before cutoff. A task-related effect is removable only with its removable task; an old standalone confirmed effect may be a root.
- `slack_source_events` only when old ignored/duplicate records are unreferenced by retained source/task state, or when exclusively owned by a removable task.
- `mutation_requests` only when `expiry_eligible_at < cutoff` and no replay/liveness rule retains it.
- disconnected or fenced consumer records only when old and not current leadership.
- incoming/outgoing `files` only when unreferenced after removable task/effect deletion. Detached incoming metadata is still durable metadata and follows this rule.
- `file_temporary_transfers` only as expired temporary-byte lifecycle work; its parent file metadata is not automatically deleted merely because temporary bytes expire.

### Non-negotiable skips

Always retain `open`/`working` tasks; current leader and connected consumers; pending, leased, or inserting deliveries; uncertain or rejected deliveries; prepared, submitted, failed, or ambiguous effects; live request records; trust/config/schema data; and every ancestor/dependent row needed by retained state.

A done task with **any** unresolved delivery or effect is skipped as a whole, together with its state events, Slack source linkage/events, and files. An effect predecessor/successor relationship is retained unless every linked effect selected for removal is removable in the same batch. Any foreign-key edge to a retained row turns the candidate into `skipped_referenced`; do not null, rewrite, or weaken evidence to make it eligible.

The implementation must express this graph explicitly in store queries and tests rather than relying on `ON DELETE` behavior. Existing `ON DELETE RESTRICT` foreign keys remain a final safety net.

## 3. Transaction ordering and bounded batches

Before implementation, inventory the current schema at the target migration and freeze the exact delete queries/indexes. Each confirmed batch uses `BEGIN IMMEDIATE` (with the configured bounded busy timeout), enables `foreign_keys=ON`, selects a stable bounded root set ordered by `(eligible_timestamp, primary_key)`, rechecks all eligibility inside that transaction, then deletes in child-before-parent order:

1. live temporary-transfer rows only after verified byte unlink (see §4);
2. outgoing/incoming `files` and task-owned attachment rows only when their owners are selected;
3. task state events and removable Slack source records/source links;
4. removable deliveries and effects, including fully removable predecessor chains;
5. removable tasks, standalone confirmed effects, requests, and stale consumers.

The exact ordering may add currently discovered child tables, but may never delete a parent before all selected children or leave a retained child dangling. Commit each bounded atomic batch before selecting the next one. Batch size and a maximum per invocation are fixed, documented configuration-independent constants chosen to keep writer locks short; no unbounded `DELETE` or candidate materialization is allowed.

Only one purge runs at a time. Acquire a transactionally fenced singleton purge lease/lock before confirmation work; concurrent purge returns conflict. Ordinary gateway writers continue between batches. Every batch re-evaluates graph eligibility, so a newly created reference causes a skip, never a stale deletion. Dry-run does not reserve candidates and is explicitly advisory.

On busy/lock/full/interrupt/cancellation, roll back the current transaction, preserve already committed earlier batches, report the last committed batch cursor/counts, and return a typed retriable result. Re-running the same or a new confirmed request is safe: selection is idempotent, strict-cutoff, and referentially checked again. No claim of one global transaction is made.

## 4. Temporary-file boundary

Purge must not become a broad filesystem cleaner. Automatic removal remains limited to expired `file_temporary_transfers` under the verified private `tmp/` directory. For each expired transfer, verify the generated-name pattern and direct-child, owner, mode, type, and inode expectations using directory-descriptor/no-follow helpers; unlink the verified byte first, then delete its transfer row transactionally/with recoverable reconciliation. If verification or unlink fails, retain the row and report it as skipped/action-required.

Unknown names, symlinks, wrong-owner/mode entries, and paths outside the private root are never followed or removed. Purging a file metadata row requires that no live transfer row remains. Dry-run never touches bytes. Temporary expiry/reconciliation remains available at startup and normal file-transfer sweeps independently of purge.

## 5. SQLite maintenance

After one or more successful metadata batches, maintenance is best-effort and never changes logical eligibility. First checkpoint WAL safely (bounded mode; do not block readers indefinitely). Run incremental vacuum only if the database uses `auto_vacuum=INCREMENTAL`, has reclaimable freelist pages above a documented threshold, and no purge transaction/lease is active. Do not run `VACUUM` inside a transaction, do not rewrite/copy the database, and do not treat maintenance failure as rollback-worthy after committed deletion. Report maintenance separately with a typed diagnostic and preserve successful removal counts.

## 6. Protocol and output contract

Add an additive protocol-v1 `purge` capability/route only once both modes are wired. The request carries cutoff, mode, and request ID; no arbitrary SQL, table selection, paths, or byte payloads are accepted. Result DTOs are bounded typed counters and canonical timestamps. CLI human output warns that confirmed removal is irreversible; JSON is stable and contains no decorative text. Credentials, raw file bytes, and private transfer URLs remain absent.

## 7. Implementation increments

1. **Store contract (Terra):** freeze schema inventory, candidate graph queries, bounded cursor selection, FK-order deletion, request replay, and transactional lock. Add migration only if a durable purge-lock/required index cannot be represented safely otherwise.
2. **Gateway/protocol/CLI (Terra):** strict timestamp parsing, exclusive flags, capability, typed reports/errors, and request-ID wiring.
3. **Temporary/maintenance integration (Terra):** verified expiry handoff, interruption reporting, checkpoint/incremental-vacuum policy, status observability.
4. **Adversarial tests and docs (Terra):** complete gates below and operational documentation. No background retention scheduler is introduced.

Each increment is separately reviewable; the integration owner owns central routing and final full-suite validation.

## 8. Required automated gates

- Reject missing/duplicate modes, local/date-only/no-offset timestamps, invalid RFC3339, and precision beyond microseconds; prove UTC normalization and strict equality retention.
- Dry-run performs zero writes (including no request record, temp unlink, checkpoint, or vacuum) and groups eligible/skipped results deterministically.
- Confirm requires a request ID, deduplicates exact replay, conflicts on changed cutoff/mode, and never purges its own live request record.
- Done-old task graphs delete only when every delivery/effect is resolved/removable; open/working and done tasks with pending/inserting/uncertain/rejected deliveries or non-confirmed effects survive unchanged.
- Confirmed standalone effects, ignored/duplicate events, expired requests, stale consumers, and unreferenced files are removed only at strict cutoff and only without retained references.
- FK checks (`PRAGMA foreign_key_check`) and integrity checks pass after every successful batch and after injected failures at every delete boundary.
- Deterministic concurrent writer/reference creation and concurrent purge tests prove recheck/skip behavior and singleton conflict behavior.
- Cancellation, SQLite busy/full, process-style interruption, and restart prove no partial batch, valid prior batches, accurate report, and safe rerun.
- Expired temporary bytes are removed only after strict no-follow verification; unknown/unsafe entries and unlink failure are retained/reported; metadata survives.
- Maintenance is bounded, optional, separately reported, and does not run for dry-run or corrupt logical state.
- Full `go test ./...`, race coverage for purge/store concurrency, and existing Phase 0–3 file-transfer regression suites pass.

## 9. Completion gate

Phase 3B is complete only when confirmed purge removes eligible finalized metadata in bounded atomic batches, dry-run is non-mutating and predictive, all retained/live/unresolved evidence survives with referential integrity, interruption can be safely rerun, and SQLite/temporary-file maintenance never broadens deletion authority.
