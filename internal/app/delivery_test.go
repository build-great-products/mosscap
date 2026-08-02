package app

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestDeliveryHelpAndLocalArgumentValidation(t *testing.T) {
	for _, args := range [][]string{{"delivery", "--help"}, {"delivery", "list", "--help"}, {"delivery", "inspect", "--help"}, {"delivery", "requeue", "--help"}} {
		var out, stderr bytes.Buffer
		if got := (App{Out: &out, Err: &stderr, Version: "test"}).Run(context.Background(), args); got != ExitSuccess || !strings.Contains(out.String(), "Usage: mosscap delivery") || stderr.Len() != 0 {
			t.Fatalf("%v exit=%d out=%q err=%q", args, got, out.String(), stderr.String())
		}
	}
	for _, args := range [][]string{{"delivery", "list", "--request-id", "x"}, {"delivery", "list", "--state", "bad"}, {"delivery", "inspect"}, {"delivery", "requeue", "dlv_a", "--reason", ""}} {
		var out, stderr bytes.Buffer
		if got := (App{Out: &out, Err: &stderr, Version: "test"}).Run(context.Background(), args); got != ExitUsage || !strings.HasPrefix(stderr.String(), "usage:") {
			t.Fatalf("%v exit=%d stderr=%q", args, got, stderr.String())
		}
	}
}
