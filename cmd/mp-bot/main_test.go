package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Rolyani/mp-telegram-bot/internal/bot"
)

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

// Slice T2 (critical path, session 3): the token comes from the environment, and a missing
// one stops the program with a sentence that says which variable is missing.
//
// ⚠️ This is the slice with a security point rather than a functional one. The token is the
// whole of the bot's identity — anyone holding it can read every message sent to the bot and
// post as it — so it must never be a string in the source, where it would reach GitHub on the
// next push and stay in the history after being deleted. The environment is where it lives,
// and the program's job is to notice when it is not there.
//
// The failure has to be LOUD for a reason worth understanding. Starting with an empty token is
// not a quiet degradation: every call to Telegram comes back 401 Unauthorized, which looks
// exactly like an outage or a network problem from the logs. A bot that refuses to start and
// names the variable turns an hour of confused debugging into one line of output.
//
// Tested with t.Setenv rather than by threading a getenv function through the code: it sets
// the real variable, restores it when the test ends, and needs no seam in production code that
// exists only for tests. (It also makes the test unable to run in parallel, which is the cost.)
func TestTelegramFromEnv(t *testing.T) {
	t.Run("missing token is refused by name", func(t *testing.T) {
		t.Setenv("TELEGRAM_TOKEN", "")

		tg, err := telegramFromEnv()
		if err == nil {
			t.Fatalf("telegramFromEnv() with no token returned nil error, want a refusal")
		}

		// The variable's name has to be IN the message. "missing token" sends the reader
		// looking; "TELEGRAM_TOKEN is not set" tells them exactly what to do next.
		if !strings.Contains(err.Error(), "TELEGRAM_TOKEN") {
			t.Errorf("error is %q, want it to name TELEGRAM_TOKEN so the reader knows what to set", err)
		}

		// Nothing usable may come back alongside the error. A non-nil Telegram here invites a
		// caller to press on with an unauthenticated client.
		if tg != nil {
			t.Errorf("telegramFromEnv() returned a Telegram %v alongside the error, want nil", tg)
		}
	})

	t.Run("token present gives a usable Telegram", func(t *testing.T) {
		t.Setenv("TELEGRAM_TOKEN", "123456:FAKE-TOKEN-FOR-TESTS")

		tg, err := telegramFromEnv()
		if err != nil {
			t.Fatalf("telegramFromEnv() with a token returned error: %v", err)
		}
		if tg == nil {
			t.Fatal("telegramFromEnv() returned nil Telegram with no error")
		}
	})
}
