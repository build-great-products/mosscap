package identity

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestGeneratorPrefixesAndEntropy(t *testing.T) {
	generator := NewGenerator(bytes.NewReader(make([]byte, 48)))
	for _, test := range []struct {
		name string
		new  func() (string, error)
		want string
	}{
		{"task", generator.NewTaskID, "tsk_"},
		{"state event", generator.NewStateEventID, "ste_"},
		{"request", generator.NewRequestID, "req_"},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := test.new()
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(got, test.want) || len(got) != len(test.want)+22 {
				t.Fatalf("ID %q does not have expected prefix and 128-bit payload", got)
			}
		})
	}
}

func TestGeneratorPropagatesEntropyFailure(t *testing.T) {
	want := errors.New("entropy unavailable")
	_, got := NewGenerator(errorReader{want}).NewRequestID()
	if !errors.Is(got, want) {
		t.Fatalf("NewRequestID error = %v, want %v", got, want)
	}
}

func TestValidRequestID(t *testing.T) {
	valid := []string{"req_abc", "a", "ABC-_.~09", strings.Repeat("a", 128)}
	for _, id := range valid {
		if !ValidRequestID(id) {
			t.Errorf("ValidRequestID(%q) = false, want true", id)
		}
	}
	invalid := []string{"", strings.Repeat("a", 129), "has space", "has/slash", "é", "line\nbreak"}
	for _, id := range invalid {
		if ValidRequestID(id) {
			t.Errorf("ValidRequestID(%q) = true, want false", id)
		}
	}
}

type errorReader struct{ err error }

func (r errorReader) Read([]byte) (int, error) { return 0, r.err }
