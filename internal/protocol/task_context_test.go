package protocol

import "testing"

func TestTaskContextParamsAndCapability(t *testing.T) {
	if err := ValidateParams("task.context", []byte(`{"task":"AbCdEfGh","thread_limit":100}`)); err != nil {
		t.Fatal(err)
	}
	if err := ValidateParams("task.context", []byte(`{"task":"AbCdEfGh","thread_limit":101}`)); err == nil {
		t.Fatal("accepted oversized thread limit")
	}
	found := false
	for _, c := range ServerCapabilities {
		found = found || c == CapabilityTaskContext
	}
	if !found {
		t.Fatal("task context capability is not advertised")
	}
}
