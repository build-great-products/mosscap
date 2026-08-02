# Phase 2 Pi delivery MVP findings

**Status:** automated implementation pending final validation; manual trial deliberately deferred

Use this document to record evidence that may change [`docs/SPEC.md`](../SPEC.md). Keep entries concise. Do not include Slack tokens, raw envelopes, private transcript content, task/message bodies, model credentials, or session-file paths.

## Automated implementation note

- The source-loaded extension pins Pi development types to `@earendil-works/pi-coding-agent` `0.83.0`; automated fake timing tests cover post-handler persistence scanning and no live Slack, gateway, provider, or manual Pi trial was run. The Phase 2 manual trial remains required and is not claimed as passed.

## Workflow observations

- Explicit `/mosscap` opt-in:
- Persisted-session requirement:
- Visible task message and untrusted-text boundary:
- Immediate turn when idle:
- Follow-up behavior when Pi is busy:
- Existing CLI usability from Pi:
- Slack thread read/reply workflow:
- Explicit task state transitions:
- `/mosscap status` and `/mosscap off`:
- Reconnect and duplicate prevention:
- Delivery inspection/requeue usability:

## Suggested SPEC changes

| SPEC area | Observation | Suggested change | Evidence |
| --- | --- | --- | --- |
| _Pending_ | | | |

## MVP simplifications that worked

- Single in-memory active Pi connection:
- One-second request/response polling:
- No leadership generations or leases:
- Existing CLI instead of custom Pi tools:
- Source-loaded extension:

## Deferred requirements that proved necessary

- _Pending_

## Requirements that appeared premature

- _Pending_

## Manual trial result

- Date:
- Mosscap revision:
- Pi version: `@earendil-works/pi-coding-agent` 0.83.0
- Model/provider: record only if relevant; no credentials
- Trial repository:
- Disposable Slack environment:
- Automated gate passed:
- Manual gate passed:
- Follow-up decisions:
