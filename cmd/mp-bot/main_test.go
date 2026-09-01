package main

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Rolyani/mp-telegram-bot/internal/bot"
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

// Slice T1 (critical path, session 3): one poll cycle. What is waiting at Telegram is
// fetched, handled, and the reply sent back to Telegram — the whole round trip, for one
// message, through the real HTTP client.
//
// ⚠️ This is a cycle and not a loop, deliberately. A poll loop does not return, so a test
// that called one could only ever hang or be killed by the deadline. Making ONE cycle the
// unit leaves the loop itself as a `for` and a sleep in main, which contains no decision
// worth testing. The alternative — a loop with a "stop after n" parameter existing only so
// a test can end it — puts test scaffolding in production code and still proves less.
//
// /help is the message used because it is the one command that needs neither the Members
// API nor the votes source: the bot can be built with nil collaborators and the test stays
// entirely offline.
func TestPollOnce_waitingMessage_isAnsweredBackToTelegram(t *testing.T) {
	// What the bot actually sent, captured from the outgoing sendMessage form.
	var sentChatID, sentText string
	var getUpdatesCalls, sendMessageCalls int

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/getUpdates"):
			getUpdatesCalls++
			fmt.Fprint(w, `{"ok":true,"result":[
				{"update_id":900000001,"message":{"chat":{"id":4242},"text":"/help"}}
			]}`)
		case strings.HasSuffix(r.URL.Path, "/sendMessage"):
			sendMessageCalls++
			if err := r.ParseForm(); err != nil {
				t.Errorf("sendMessage body did not parse as a form: %v", err)
			}
			sentChatID = r.FormValue("chat_id")
			sentText = r.FormValue("text")
			fmt.Fprint(w, `{"ok":true}`)
		default:
			t.Errorf("unexpected request to %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	tg := bot.NewTelegram(srv.URL, "TESTTOKEN")
	b := bot.New(bot.NewMemoryStore(), nil, nil)

	if err := pollOnce(b, tg); err != nil {
		t.Fatalf("pollOnce() returned error: %v", err)
	}

	// One cycle is one fetch. A cycle that polled twice would be a loop wearing a different
	// name, and would drain the queue rather than leaving the pacing to the caller.
	if getUpdatesCalls != 1 {
		t.Errorf("getUpdates called %d times, want 1", getUpdatesCalls)
	}

	// The reply must go back to the chat it came from. Sending to the right person is not a
	// detail here — it is the difference between a bot and a leak.
	if sendMessageCalls != 1 {
		t.Fatalf("sendMessage called %d times, want 1", sendMessageCalls)
	}
	if sentChatID != "4242" {
		t.Errorf("reply sent to chat %q, want %q", sentChatID, "4242")
	}

	// A command name out of the help reply rather than a sentence of it, for the same reason
	// as the stdin test above: this proves the wiring, not the wording.
	if !strings.Contains(sentText, "/forgetme") {
		t.Errorf("sent text %q, want it to contain %q", sentText, "/forgetme")
	}
}
