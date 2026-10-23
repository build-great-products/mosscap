package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"

	"github.com/stayradiated/mosscap/internal/client"
	"github.com/stayradiated/mosscap/internal/config"
	"github.com/stayradiated/mosscap/internal/protocol"
)

const (
	taskWatchDefaultInterval = time.Second
	taskWatchMinInterval     = 100 * time.Millisecond
	// The gateway closes an idle unopened connection after 15 seconds, so the
	// poll interval must stay comfortably below it to keep one connection.
	taskWatchMaxInterval    = 10 * time.Second
	taskWatchPageLimit      = 20
	taskWatchInitialBackoff = 500 * time.Millisecond
	taskWatchMaxBackoff     = 10 * time.Second
)

type taskWatchOptions struct {
	since        string
	sinceSet     bool
	cursorFile   string
	noBacklog    bool
	stateChanges bool
	interval     string
	jsonOut      bool
}

// taskWatcher is the long-running harness-neutral consumer of task.changes.
// Printing a line is not an acknowledgement and does not change task or
// Slack state.
type taskWatcher struct {
	app          App
	config       config.Config
	opts         taskWatchOptions
	interval     time.Duration
	cursor       int64
	haveCursor   bool
	started      bool
	backlogHead  int64
	backlog      bool
	limit        int
	disconnected bool
}

func (a App) taskWatchTyped(opts taskWatchOptions) error {
	if opts.sinceSet && opts.cursorFile != "" {
		return usage("--since and --cursor-file are mutually exclusive")
	}
	if opts.sinceSet && opts.noBacklog {
		return usage("--no-backlog applies only when no cursor is given")
	}
	w := &taskWatcher{app: a, opts: opts, interval: taskWatchDefaultInterval, limit: taskWatchPageLimit}
	if opts.interval != "" {
		parsed, err := time.ParseDuration(opts.interval)
		if err != nil || parsed < taskWatchMinInterval || parsed > taskWatchMaxInterval {
			return usage("--interval must be a duration between 100ms and 10s")
		}
		w.interval = parsed
	}
	if opts.sinceSet {
		cursor, err := parseTaskWatchCursor(opts.since)
		if err != nil {
			return usage("--since must be a non-negative integer cursor")
		}
		w.cursor, w.haveCursor = cursor, true
	}
	if opts.cursorFile != "" {
		if !filepath.IsAbs(opts.cursorFile) {
			return usage("--cursor-file must be an absolute path")
		}
		b, err := os.ReadFile(opts.cursorFile)
		switch {
		case err == nil:
			cursor, parseErr := parseTaskWatchCursor(strings.TrimSpace(string(b)))
			if parseErr != nil {
				return usage("--cursor-file does not contain a non-negative integer cursor")
			}
			w.cursor, w.haveCursor = cursor, true
		case errors.Is(err, os.ErrNotExist):
		default:
			return &commandError{code: ExitUnsafe, message: "read --cursor-file: " + err.Error()}
		}
	}
	w.backlog = !w.haveCursor && !opts.noBacklog
	c, err := loadCommandConfig()
	if err != nil {
		return err
	}
	w.config = c
	parent := a.ctx
	if parent == nil {
		parent = context.Background()
	}
	ctx, stop := signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
	defer stop()
	return w.run(ctx)
}

func parseTaskWatchCursor(value string) (int64, error) {
	cursor, err := strconv.ParseInt(value, 10, 64)
	if err != nil || cursor < 0 {
		return 0, errors.New("invalid cursor")
	}
	return cursor, nil
}

// run reconnects with capped exponential backoff. Only gateway unavailability
// is retried; typed refusals such as cursor_ahead or a missing capability end
// the watch with their normal exit category.
func (w *taskWatcher) run(ctx context.Context) error {
	backoff := taskWatchInitialBackoff
	for {
		err := w.session(ctx, func() { backoff = taskWatchInitialBackoff })
		if ctx.Err() != nil {
			return nil
		}
		if !taskWatchRetriable(err) {
			return err
		}
		w.disconnected = true
		fmt.Fprintf(w.app.Err, "mosscap task watch: gateway unavailable (%v); reconnecting in %s\n", err, backoff)
		if sleepContext(ctx, backoff) != nil {
			return nil
		}
		backoff = min(backoff*2, taskWatchMaxBackoff)
	}
}

func taskWatchRetriable(err error) bool {
	var ce *commandError
	return errors.As(err, &ce) && ce.code == ExitUnavailable
}

func sleepContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (w *taskWatcher) timeout() time.Duration {
	if w.app.timeout > 0 {
		return w.app.timeout
	}
	return 10 * time.Second
}

// session holds one gateway connection. Each dial+hello and each poll gets its
// own --timeout budget, since the command itself has no end.
func (w *taskWatcher) session(ctx context.Context, connected func()) error {
	dialCtx, cancel := context.WithTimeout(ctx, w.timeout())
	cl, err := client.Dial(dialCtx, w.config.Paths.Endpoint, uint32(w.config.LocalProtocolMaximumFrame))
	if err != nil {
		cancel()
		return &commandError{code: ExitUnavailable, message: err.Error()}
	}
	defer cl.Close()
	hello, err := cl.Hello(dialCtx, "req_hello_task_changes", "cli", w.app.Version, nil)
	cancel()
	if err != nil {
		return remote(err)
	}
	if !hasCapability(hello.Capabilities, protocol.CapabilityTaskChanges) {
		return unsupported("gateway does not support " + protocol.CapabilityTaskChanges + "; upgrade the gateway before using this command")
	}
	connected()
	if w.disconnected {
		fmt.Fprintln(w.app.Err, "mosscap task watch: reconnected to gateway")
		w.disconnected = false
	}
	for {
		page, err := w.poll(ctx, cl)
		if err != nil {
			return err
		}
		more, err := w.handle(page)
		if err != nil {
			return err
		}
		if more {
			continue
		}
		if err := sleepContext(ctx, w.interval); err != nil {
			return err
		}
	}
}

func (w *taskWatcher) poll(ctx context.Context, cl *client.Client) (protocol.TaskChangesDTO, error) {
	var page protocol.TaskChangesDTO
	for {
		params, err := json.Marshal(protocol.TaskChangesParams{After: w.cursor, Limit: w.limit})
		if err != nil {
			return page, err
		}
		callCtx, cancel := context.WithTimeout(ctx, w.timeout())
		response, err := cl.Call(callCtx, protocol.Request{ProtocolVersion: protocol.Version, RequestID: "req_read_task_changes", ClientKind: "cli", ClientVersion: w.app.Version, Command: "task.changes", Params: params})
		cancel()
		if err != nil {
			return page, remote(err)
		}
		// A page of large task texts can exceed the frame limit. Shrink the page
		// rather than stall; a single task that cannot fit is a hard error.
		if response.Error != nil && response.Error.Code == "bounded_result_exceeded" && w.limit > 1 {
			w.limit /= 2
			continue
		}
		if response.Error != nil {
			return page, remote(&client.RemoteError{ProtocolError: *response.Error})
		}
		dec := json.NewDecoder(bytes.NewReader(response.Result))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&page); err != nil {
			return page, &commandError{code: ExitUnsupported, message: "gateway returned an invalid task change page"}
		}
		return page, nil
	}
}

// handle emits one page and reports whether another page is immediately
// available. The cursor advances past filtered changes too, so a restarted
// watcher with the same filters neither replays nor misses anything.
func (w *taskWatcher) handle(page protocol.TaskChangesDTO) (bool, error) {
	if !w.started {
		w.started = true
		w.backlogHead = page.Head
		if !w.haveCursor && w.opts.noBacklog {
			w.cursor, w.haveCursor = page.Head, true
			return false, w.saveCursor()
		}
	}
	for _, change := range page.Changes {
		if w.emits(change) {
			if err := writeTaskWatchLine(w.app.Out, change, w.opts.jsonOut); err != nil {
				return false, err
			}
		}
		w.cursor, w.haveCursor = change.ChangeSeq, true
		if err := w.saveCursor(); err != nil {
			return false, err
		}
	}
	return len(page.Changes) == w.limit, nil
}

func (w *taskWatcher) emits(change protocol.TaskChangeDTO) bool {
	if w.backlog && change.ChangeSeq <= w.backlogHead {
		return change.Task.State == "open"
	}
	if w.opts.stateChanges {
		return true
	}
	return change.Change == "created" || change.Change == "edited" || change.Task.State == "open"
}

// saveCursor atomically replaces the cursor file only after the corresponding
// line was written, so a crash can repeat at most the last line, never skip one.
func (w *taskWatcher) saveCursor() error {
	if w.opts.cursorFile == "" {
		return nil
	}
	dir := filepath.Dir(w.opts.cursorFile)
	f, err := os.CreateTemp(dir, ".mosscap-watch-cursor-*")
	if err != nil {
		return &commandError{code: ExitUnsafe, message: "write --cursor-file: " + err.Error()}
	}
	name := f.Name()
	_, err = fmt.Fprintf(f, "%d\n", w.cursor)
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(name, w.opts.cursorFile)
	}
	if err != nil {
		_ = os.Remove(name)
		return &commandError{code: ExitUnsafe, message: "write --cursor-file: " + err.Error()}
	}
	return nil
}

// writeTaskWatchLine writes exactly one complete line per event in a single
// Write call. Text is escaped, never truncated.
func writeTaskWatchLine(out io.Writer, change protocol.TaskChangeDTO, jsonOut bool) error {
	var line []byte
	if jsonOut {
		b, err := json.Marshal(change)
		if err != nil {
			return err
		}
		line = append(b, '\n')
	} else {
		t := change.Task
		channel := ""
		if t.SlackSource != nil {
			channel = t.SlackSource.ChannelID
		}
		line = fmt.Appendf(nil, "cursor=%d change=%s task=%s version=%d state=%s source=%s channel=%s text=%s\n", change.ChangeSeq, change.Change, t.TaskID, t.Version, t.State, t.SourceKind, channel, escapeTaskWatchText(t.Text))
	}
	_, err := out.Write(line)
	return err
}

// escapeTaskWatchText makes text reversible and single-line: backslash, the
// common whitespace controls, other controls, and Unicode line/paragraph
// separators are escaped; all other printable Unicode is retained.
func escapeTaskWatchText(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\\':
			b.WriteString(`\\`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case unicode.IsControl(r) || r == '\u2028' || r == '\u2029':
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
