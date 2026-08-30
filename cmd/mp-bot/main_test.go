package main

import (
	"bytes"
	"strings"
	"testing"
)

// This test is in package main, not package main_test, because a main package cannot be
// imported — so a black-box test has nothing to import and run would be unreachable. It is
// the one place the usual preference for external test packages cannot apply.
func TestRun_help_writesTheHelpReply(t *testing.T) {
	in := strings.NewReader("/help\n")
	var out bytes.Buffer

	if err := run(in, &out); err != nil {
		t.Fatalf("run() returned error: %v", err)
	}

	// A distinctive line of the /help reply rather than the whole thing: what this slice
	// proves is the wiring — a line of input becomes a reply on the writer — and the help
	// wording is known to be wrong and due a rewrite. The wiring test should survive it.
	got := out.String()
	want := "/forgetme will wipe all of your data."
	if !strings.Contains(got, want) {
		t.Errorf("run() wrote %q, want it to contain %q", got, want)
	}
}
