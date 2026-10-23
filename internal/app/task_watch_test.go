package app

import (
	"bytes"
	"testing"

	"github.com/stayradiated/mosscap/internal/protocol"
)

func TestTaskWatchTextIsOneReversibleLine(t *testing.T) {
	got := escapeTaskWatchText("a\\b\nc\r\td\x1b[31m\u0085e\u2028f\u2029g café ✓")
	want := "a\\\\b\\nc\\r\\td\\u001b[31m\\u0085e\\u2028f\\u2029g café ✓"
	if got != want {
		t.Fatalf("escaped=%q want %q", got, want)
	}
	var out bytes.Buffer
	change := protocol.TaskChangeDTO{ChangeSeq: 7, Change: "edited", Task: protocol.TaskDTO{TaskID: "AbCdEfGhIjKlMnOpQrStUv", Version: 2, State: "open", SourceKind: "slack", Text: "x\ny", SlackSource: &protocol.SlackSourceDTO{ChannelID: "C0CC6JQ4A2U", ThreadTS: "1700000000.000001", ParentTaskID: "ParentTaskIdParentTask"}}}
	if err := writeTaskWatchLine(&out, change, false); err != nil {
		t.Fatal(err)
	}
	if out.String() != "cursor=7 change=edited task=AbCdEfGhIjKlMnOpQrStUv version=2 state=open source=slack channel=C0CC6JQ4A2U thread=1700000000.000001 parent=ParentTaskIdParentTask text=x\\ny\n" {
		t.Fatalf("line=%q", out.String())
	}
	out.Reset()
	local := protocol.TaskChangeDTO{ChangeSeq: 8, Change: "created", Task: protocol.TaskDTO{TaskID: "LocalTaskIdLocalTaskId", Version: 1, State: "open", SourceKind: "local", Text: "local"}}
	if err := writeTaskWatchLine(&out, local, false); err != nil {
		t.Fatal(err)
	}
	if out.String() != "cursor=8 change=created task=LocalTaskIdLocalTaskId version=1 state=open source=local channel= thread= parent= text=local\n" {
		t.Fatalf("local line=%q", out.String())
	}
	out.Reset()
	if err := writeTaskWatchLine(&out, change, true); err != nil {
		t.Fatal(err)
	}
	if bytes.Count(out.Bytes(), []byte("\n")) != 1 || !bytes.HasPrefix(out.Bytes(), []byte(`{"change_seq":7,"change":"edited","task":{`)) {
		t.Fatalf("json line=%q", out.String())
	}
}

func TestTaskWatchDefaultFilter(t *testing.T) {
	change := func(seq int64, kind, state string) protocol.TaskChangeDTO {
		return protocol.TaskChangeDTO{ChangeSeq: seq, Change: kind, Task: protocol.TaskDTO{State: state}}
	}
	w := &taskWatcher{backlog: true, backlogHead: 2}
	for _, c := range []struct {
		change protocol.TaskChangeDTO
		want   bool
	}{
		{change(1, "edited", "done"), false},   // backlog: open tasks only
		{change(2, "state", "open"), true},     // backlog: reopened task is open work
		{change(3, "created", "open"), true},   // new task
		{change(4, "edited", "done"), true},    // content version changed
		{change(5, "state", "open"), true},     // reopened
		{change(6, "state", "working"), false}, // claimed elsewhere
		{change(7, "state", "done"), false},
	} {
		if got := w.emits(c.change); got != c.want {
			t.Errorf("emits(%+v)=%v want %v", c.change, got, c.want)
		}
	}
	w.opts.stateChanges = true
	if !w.emits(change(8, "state", "done")) {
		t.Fatal("--state-changes did not include done")
	}
}

func TestTaskWatchFlagConflictsAreUsageErrors(t *testing.T) {
	for _, opts := range []taskWatchOptions{
		{since: "1", sinceSet: true, cursorFile: "/tmp/cursor"},
		{since: "1", sinceSet: true, noBacklog: true},
		{since: "-1", sinceSet: true},
		{since: "x", sinceSet: true},
		{interval: "1ms"},
		{interval: "20s"},
		{cursorFile: "relative"},
	} {
		if got := exitCode((App{}).taskWatchTyped(opts)); got != ExitUsage {
			t.Errorf("%+v exit=%d want usage", opts, got)
		}
	}
}
