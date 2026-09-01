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

	// A single command name out of the /help reply rather than a sentence of it. What this
	// slice proves is the WIRING — a line of input becomes a reply on the writer — so it must
	// survive the help rewrite that is happening now. It previously pinned a whole sentence,
	// which would have gone red the moment that sentence was reworded, and gone red HERE, in
	// cmd, for a change made in internal/bot: the worst kind of failure to read.
	got := out.String()
	want := "/forgetme"
	if !strings.Contains(got, want) {
		t.Errorf("run() wrote %q, want it to contain %q", got, want)
	}
}
