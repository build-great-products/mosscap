# Mosscap Pi extension

This is the Phase 2 source-loaded extension for exactly Pi `0.83.0`. It has no
background connection, timer, or watcher until a human runs `/mosscap` in a
persisted interactive TUI session.

## Development

```sh
pnpm install --frozen-lockfile
pnpm test
pnpm typecheck
```

Load it from a checked-out Mosscap tree (not as a published package):

```sh
pi -e /absolute/path/to/mosscap/pi-extension/src/index.ts
```

`/mosscap` opts in the current persisted TUI session, `/mosscap status` shows
safe compact state, and `/mosscap off` releases the connection. The extension
adds no model-callable tools and does not alter task state or contact Slack.
See `docs/trials/phase-2.md` for the deliberately deferred human-owned trial.
