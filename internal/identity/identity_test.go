package identity

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestGeneratorIDsArePrefixFreeAndHaveEntropy(t *testing.T) {
	generator := NewGenerator(bytes.NewReader(make([]byte, 128)))
	for _, test := range []struct {
		name string
		new  func() (string, error)
		want string
	}{
		{"task", generator.NewTaskID, ""},
		{"state event", generator.NewStateEventID, ""},
		{"effect", generator.NewEffectID, ""},
		{"delivery", generator.NewDeliveryID, ""},
		{"file", generator.NewFileID, ""},
		{"request", generator.NewRequestID, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := test.new()
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(got, test.want) || len(got) != 22 {
				t.Fatalf("ID %q is not a 22-character prefix-free opaque ID", got)
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
