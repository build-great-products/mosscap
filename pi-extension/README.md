# Mosscap Pi extension

This is the Phase 2 source-loaded extension for exactly Pi `0.84.1`. It has no
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
pi -e /absolute/path/to/mosscap/pi-extension
```

To install that checkout persistently, use:

```sh
pi install /absolute/path/to/mosscap/pi-extension
```

`/mosscap` opts in the current persisted TUI session, `/mosscap status` shows
safe compact state, and `/mosscap off` releases the connection. While connected,
the extension adds per-turn context explaining that remote users do not see
normal Pi output and that Pi should use the ordinary `mosscap` CLI to reply.
Delivered tasks use concise, visibly delimited user-message text and include the
exact Slack reply and completion commands. Attached Slack files are rendered as
escaped, bounded metadata with `file show` and explicit `file get --output`
hints only; the extension never receives bytes, hashes, private URLs, temporary
paths, or local upload paths. The extension adds no model-callable tools and
does not itself alter task state or contact Slack. See `docs/trials/phase-2.md`
for the deliberately deferred human-owned trial.
