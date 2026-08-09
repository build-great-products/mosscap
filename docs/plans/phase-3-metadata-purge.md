# Phase 3B: metadata purge contract

**Status:** authoritative implementation contract; implementation intentionally deferred.

This plan owns the metadata-purge half of Phase 3. [`docs/SPEC.md`](../SPEC.md) remains authoritative. This contract is deliberately limited to the durable schema through `0005_file_transfer.sql`; it does not create a byte archive or redefine file transfer.

## 1. Command and request semantics

Expose only:

```text
mosscap purge --before RFC3339 --dry-run
mosscap purge --before RFC3339 --confirm
```

Exactly one mode and `--before` are required. Parse the cutoff as RFC 3339 with `Z` or an explicit `±HH:MM` offset, reject date-only/local/ambiguous inputs and precision beyond microseconds, then normalize it to an integer UTC-microsecond cutoff. Eligibility is strictly older (`<`); a value equal to the cutoff is retained. Render the cutoff as canonical UTC RFC 3339 with microseconds.

`--dry-run` is read-only: no durable writes, temporary-byte unlink, checkpoint, or vacuum. `--confirm` uses the existing `mutation_requests` request-ID/deduplication mechanism. Its record has actual columns `request_id`, `operation`, `input_hash`, `outcome`, `created_at`, `completed_at`, and `expiry_eligible_at`; exact replay returns the stored typed result, while a changed normalized cutoff or mode conflicts. A live request record is excluded from selection by its own request.

Both modes report bounded grouped `eligible`, `skipped_live`, `skipped_referenced`, and `removed` counts for actual tables: `tasks`, `task_state_events`, `slack_source_events`, `slack_task_sources`, `pi_deliveries`, `slack_effects`, `files`, `file_temporary_transfers`, and `mutation_requests`. A zero report is success. There is no consumer count: migrations `0001`–`0005` contain no durable consumer or leader table, so the SPEC's consumer-record purge requirement is not applicable at this schema version.

## 2. Schema inventory and cutoff rules

Implementation starts by freezing SQL against this inventory; no named column below is inferred from the SPEC.

| Record | Existing eligibility timestamp / rule |
| --- | --- |
| `tasks` | `state = 'done'` and `done_at < cutoff` (`done_at` is the only completion timestamp). |
| `task_state_events` | Owned by a selected task; its actual timestamp is `committed_at`. It is not an independent root. |
| `slack_task_sources` | Owned by a selected task; its actual timestamps include `latest_event_at_us`, `source_deleted_at_us`, `created_at_us`, and `updated_at_us`. It is not an independent root. |
| `slack_source_events` | An event linked to a selected task is removed only with that task. An unlinked row with `task_id IS NULL` is independently eligible only when `admission_result IN ('duplicate', 'ignored_workspace', 'ignored_channel', 'ignored_untrusted', 'ignored_unmentioned', 'ignored_bot', 'ignored_stale', 'ignored_untracked', 'malformed')` and `committed_at_us < cutoff`. |
| `pi_deliveries` | Only `state = 'persisted'` can be considered as task-owned finalized history. The implementation schema-inventory decision must define whether `completed_at_us` is required for its cutoff (it is nullable in `0004`) or whether `updated_at_us` is the conservative fallback; no nonexistent lease or consumer fields may be used. |
| `slack_effects` | Only `state = 'confirmed'` and non-null `completed_at_us < cutoff`. A task-related effect is removable only with its selected task; an unrelated confirmed effect may be a root if its predecessor graph and files are also removable. |
| `files` | `0005` requires `related_task_id IS NOT NULL OR related_effect_id IS NOT NULL`; therefore no ownerless file row exists. Delete it only with selected related owner(s), after all `file_temporary_transfers` for it are gone. Its actual timestamps are `created_at_us`, `updated_at_us`, and, for detached incoming metadata, `source_detached_at_us`; the latter is not an automatic purge timestamp. |
| `file_temporary_transfers` | Expiry is `expires_at_us < cutoff`. It is temporary-byte lifecycle work, not proof that parent `files` metadata is removable. |
| `mutation_requests` | `expiry_eligible_at < cutoff`. This column exists in `0001`; retain requests not yet eligible and the request executing this purge. |

The migrations have no `consumers`, leader, generation, fenced-consumer, purge-lease, or purge-lock table. They also have no `leased` delivery state: `0004` permits only `pending`, `inserting`, `persisted`, `uncertain`, and `rejected`. Any later schema change needed for different retention semantics must be separately designed and migrated; this plan does not assume it.

## 3. Retention graph and skips

Purge is graph-based, not independent table-age deletion. A task root is eligible only when it satisfies the `tasks` rule and every referencing `pi_deliveries`, `slack_effects`, `files`, `slack_task_sources`, `slack_source_events`, and `task_state_events` row is selected in the same batch or otherwise does not reference it. In particular, any delivery other than the finalized rule above, any effect other than the confirmed rule above, or any retained file makes the task `skipped_live` or `skipped_referenced` as applicable.

Always retain `open` and `working` tasks; `pending`, `inserting`, `uncertain`, and `rejected` deliveries; `prepared`, `submitted`, `failed`, and `ambiguous` effects; requests whose `expiry_eligible_at` is not older than cutoff; `trusted_users`; `schema_migrations`; and every row referenced by retained state. Do not null, rewrite, or weaken evidence to make a candidate eligible.

For effects, account for the actual self-reference `predecessor_effect_id`: a confirmed candidate is removable only if no retained effect references it. For source linkage, account for `slack_task_sources`' composite foreign key to `(source_event_id, task_id)` in `slack_source_events`. Existing `ON DELETE RESTRICT` foreign keys remain the final safety net, not the selection algorithm.

## 4. Batches, concurrency, and deletion order

Each confirmed batch uses `BEGIN IMMEDIATE`, `foreign_keys=ON`, and the configured bounded SQLite busy timeout. SQLite's writer transaction serializes confirmed purge batches with ordinary writers; this plan introduces no persistent purge lease or lock. Select a stable, bounded root set ordered by the selected actual eligibility timestamp and primary key, recheck the entire graph in that transaction, and commit before the next batch. Batch size and maximum batches per invocation are fixed documented constants; no unbounded delete or candidate materialization is allowed.

The exact queries and indexes are a pre-implementation schema-inventory deliverable. Delete selected rows only in an FK-safe child-before-parent order, including:

1. verified expired `file_temporary_transfers` rows after byte handling;
2. selected `files` rows;
3. selected `slack_task_sources`, then selected `task_state_events` and `slack_source_events` as their retained references permit;
4. selected `pi_deliveries` and `slack_effects`, deleting effect dependents before their predecessors; and
5. selected `tasks`, standalone confirmed effects, and expired `mutation_requests`.

The ordering may be refined only for child tables discovered in the target schema. On busy, full, interruption, or cancellation, roll back the current transaction, preserve earlier committed batches, report the last committed cursor/counts, and return a typed retriable result. Re-running is safe because each batch rechecks strict-cutoff eligibility and references. Dry-run reserves nothing and is advisory.

## 5. Temporary-byte boundary

Purge must not become a filesystem cleaner. Only a `file_temporary_transfers` row whose actual `expires_at_us` is older than cutoff is eligible for temporary-byte handling. Resolve only its `generated_name` beneath the verified private `tmp/` directory; reject unknown names, paths outside that directory, and unsafe filesystem objects. Verify direct-child/no-follow and expected regular-file, effective-owner, and mode properties before unlinking. The schema stores no expected inode, owner, or mode values, so no inode-comparison claim is made.

Unlink the verified byte before deleting its transfer row, with recoverable reconciliation if either side fails. On verification or unlink failure, retain the row and report action required. A `files` row remains until no `file_temporary_transfers` row references it and its related task/effect is selected. Dry-run never touches bytes. Existing startup and ordinary file-transfer expiry sweeps remain independently available.

## 6. SQLite maintenance

After successful metadata batches, maintenance is best-effort and separate from logical eligibility. Safely checkpoint WAL in a bounded mode, then run incremental vacuum only when `auto_vacuum=INCREMENTAL` and reclaimable freelist pages exceed a documented threshold. Do not run `VACUUM` in a transaction, copy/rewrite the database, or treat maintenance failure as a rollback of already committed removals. Report it as a separate typed diagnostic. Dry-run does none of this.

## 7. Protocol and output contract

Add an additive protocol-v1 `purge` capability/route only after both modes are wired. The request carries only cutoff, mode, and request ID; it accepts no arbitrary SQL, table selector, path, or byte payload. Result DTOs contain bounded typed counters, canonical timestamps, batch progress, and separate maintenance diagnostics. CLI human output warns that confirmed removal is irreversible; JSON is stable and contains no decorative text. Credentials, raw file bytes, and private transfer URLs remain absent.

## 8. Implementation increments

1. **Store contract (Terra):** freeze the `0001`–`0005` inventory, decide the nullable `pi_deliveries.completed_at_us` cutoff policy, add only schema-supported candidate queries/indexes, bounded selection, request replay, and FK-order deletion. A migration is a separate justified design only if this contract cannot be met with the existing schema.
2. **Gateway/protocol/CLI (Terra):** strict timestamp parsing, exclusive flags, capability, typed reports/errors, and request-ID wiring.
3. **Temporary/maintenance integration (Terra):** generated-name-safe expiry handling, interruption reporting, checkpoint/incremental-vacuum policy, and status observability.
4. **Adversarial tests and docs (Terra):** implement the gates below and operator documentation. No background metadata-retention scheduler is introduced.

## 9. Required automated gates

- Reject missing/duplicate modes, local/date-only/no-offset timestamps, invalid RFC3339, and precision beyond microseconds; prove UTC normalization and strict equality retention.
- Prove dry-run writes neither request records nor SQLite/temporary-file maintenance state, and reports actual-table counts deterministically.
- Prove confirm request-ID replay and changed-input conflict; prove the live purge request is retained.
- Prove a done task deletes only when all referencing deliveries meet the frozen finalized rule and all referencing effects/files/source/state rows are selected; prove all other delivery/effect states retain the graph unchanged.
- Prove `slack_source_events` independent eligibility only for the listed unlinked ignored/duplicate-style results and `committed_at_us`, `mutation_requests.expiry_eligible_at` eligibility, and confirmed standalone-effect predecessor protection.
- Prove no ownerless `files` candidate is assumed; files and temporary transfers respect their actual foreign keys and `expires_at_us` rule.
- Run `PRAGMA foreign_key_check` and integrity checks after every successful batch and injected failure at every delete boundary.
- Prove concurrent writers cause recheck/skip rather than stale deletion, and busy/full/cancellation/restart leaves no partial batch and supports safe rerun.
- Prove temporary-byte removal uses generated-name/direct-child/no-follow validation; unsafe entries and unlink failures retain/report their rows.
- Prove maintenance is bounded, optional, separately reported, and absent from dry-run; run full `go test ./...` plus applicable store concurrency and Phase 0–3 file-transfer regressions.

## 10. Completion gate

Phase 3B is complete only when confirmed purge removes schema-supported, finalized metadata in bounded atomic batches; dry-run is non-mutating and predictive; all live or unresolved evidence survives with referential integrity; interruption safely reruns; and temporary-file and SQLite maintenance never broaden deletion authority.
