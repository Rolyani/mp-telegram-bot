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

// stubSource is an activity source that answers every member with the same canned items. The
// bot package has its own fake for this; that one is unexported and lives in package bot_test,
// and ActivitySource is a one-method interface, so satisfying it here costs three lines and
// keeps this test from depending on the other package's fixtures.
//
// It ignores memberID deliberately: which MP the items belong to is the bot's business and is
// already tested there. What is under test here is the wiring between CheckActivity and
// Telegram, so the source only has to produce something to send.
type stubSource struct {
	items []bot.Activity
}

func (s stubSource) Activity(memberID int) []bot.Activity { return s.items }

// Slice T3 (critical path, session 3): the bot speaks WITHOUT being spoken to. Everything up
// to here has been request/response — a message arrives, a reply goes back. This is the first
// thing the bot does on its own initiative, and it is the whole point of the product: you
// follow an MP and later your phone tells you how they voted.
//
// ⚠️ It is a SEPARATE cycle from pollOnce, not a step inside it, and the reason is cadence.
// Answering a message must feel immediate, so pollOnce runs every couple of seconds. Asking
// Parliament what an MP has been voting on has no such need and a real cost — it is somebody
// else's API, it is not paid for by us, and once per followed MP per two seconds would be
// abusive. Two cycles let main run them on two clocks. Folding the check into pollOnce would
// weld the polite rate to the responsive one.
//
// The store is populated directly rather than through /start and /follow. Those commands have
// their own tests in the bot package, including the baseline that makes this safe
// (TestHandleUpdate_follow_baselinesExistingActivity); driving them again here would test them
// twice and this — whether CheckActivity's replies reach Telegram — not at all.
func TestPushOnce_newActivity_isSentToTheFollower(t *testing.T) {
	// Every sendMessage the bot made, in order: chat_id then text.
	type sent struct{ chatID, text string }
	var messages []sent
	var getUpdatesCalls int

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/sendMessage"):
			if err := r.ParseForm(); err != nil {
				t.Errorf("sendMessage body did not parse as a form: %v", err)
			}
			messages = append(messages, sent{r.FormValue("chat_id"), r.FormValue("text")})
			fmt.Fprint(w, `{"ok":true}`)
		case strings.HasSuffix(r.URL.Path, "/getUpdates"):
			getUpdatesCalls++
			fmt.Fprint(w, `{"ok":true,"result":[]}`)
		default:
			t.Errorf("unexpected request to %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	store := bot.NewMemoryStore()
	store.AddChat(4242)
	store.FollowMP(4242, bot.Member{ID: 4514, Name: "Lindsay Hoyle"})

	src := stubSource{items: []bot.Activity{
		{ID: "division-1", Text: "Voted Aye on: Something Bill"},
		{ID: "division-2", Text: "Voted No on: Another Bill"},
	}}

	// nil resolver: pushing activity never resolves a name. A nil here is a statement that
	// this path does not use one, and would panic loudly if that stopped being true.
	b := bot.New(store, nil, src)
	tg := bot.NewTelegram(srv.URL, "TESTTOKEN")

	if err := pushOnce(b, tg); err != nil {
		t.Fatalf("pushOnce() returned error: %v", err)
	}

	// One message per new item. Batching them into one would read better on a phone and is a
	// different decision from this one; what must not happen is an item going missing.
	if len(messages) != 2 {
		t.Fatalf("sendMessage called %d times, want 2: %+v", len(messages), messages)
	}

	for _, m := range messages {
		if m.chatID != "4242" {
			t.Errorf("activity sent to chat %q, want %q — a push goes to the follower, not to whoever spoke last", m.chatID, "4242")
		}
	}

	// Both items, in whichever order the bot produced them: the ordering of a batch is not
	// what this slice decides.
	texts := messages[0].text + "\n" + messages[1].text
	for _, want := range []string{"Something Bill", "Another Bill"} {
		if !strings.Contains(texts, want) {
			t.Errorf("sent texts %q, want them to include %q", texts, want)
		}
	}

	// ⚠️ A push is not a poll. Reading the update queue here would acknowledge messages the
	// bot has not answered — GetUpdates moves the offset past everything it returns, so the
	// commands in that batch would be dropped, unanswered and unrecoverable.
	if getUpdatesCalls != 0 {
		t.Errorf("pushOnce called getUpdates %d times, want 0 — pushing must not touch the update queue or it silently discards unanswered messages", getUpdatesCalls)
	}

	// Nothing new has happened since, so a second push has nothing to say. CheckActivity
	// records what it has sent; this is the check that pushOnce does not re-send it.
	messages = nil
	if err := pushOnce(b, tg); err != nil {
		t.Fatalf("second pushOnce() returned error: %v", err)
	}
	if len(messages) != 0 {
		t.Errorf("second pushOnce sent %d messages, want 0 — the same division must not arrive twice: %+v", len(messages), messages)
	}
}

// ⚠️ One follower's send failing must not cost the others theirs — the same rule pollOnce
// already follows, and it matters more here. A push fans one event out to everybody following
// that MP, so a single blocked chat (403, the commonest failure there is) would otherwise
// silence a division for every other subscriber in the batch.
//
// The failure is keyed on chat_id rather than call order because CheckActivity walks the
// store's chats, whose order comes from a map and is deliberately not fixed.
func TestPushOnce_oneFailedSend_doesNotAbandonTheRest(t *testing.T) {
	var attempted []string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/sendMessage") {
			t.Errorf("unexpected request to %s", r.URL.Path)
			return
		}
		if err := r.ParseForm(); err != nil {
			t.Errorf("sendMessage body did not parse as a form: %v", err)
		}
		chatID := r.FormValue("chat_id")
		attempted = append(attempted, chatID)

		// Chat 1 has blocked the bot. Telegram answers 403 and will do so forever.
		if chatID == "1" {
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, `{"ok":false,"error_code":403,"description":"Forbidden: bot was blocked by the user"}`)
			return
		}
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer srv.Close()

	mp := bot.Member{ID: 4514, Name: "Lindsay Hoyle"}
	store := bot.NewMemoryStore()
	for _, chatID := range []int64{1, 2} {
		store.AddChat(chatID)
		store.FollowMP(chatID, mp)
	}

	src := stubSource{items: []bot.Activity{{ID: "division-1", Text: "Voted Aye on: Something Bill"}}}
	b := bot.New(store, nil, src)
	tg := bot.NewTelegram(srv.URL, "TESTTOKEN")

	err := pushOnce(b, tg)

	// Both were tried. One attempt means the batch was abandoned at the first 403.
	if len(attempted) != 2 {
		t.Fatalf("sendMessage attempted for %d chats, want 2 — a blocked chat must not stop the others being told: %v", len(attempted), attempted)
	}

	// And the failure is still reported. Carrying on is not the same as pretending it worked:
	// main prints this, and a chat failing every push forever is how anyone finds out.
	if err == nil {
		t.Error("pushOnce() returned nil error, want the failed send reported")
	}
}

// Slice F14: the bot refuses to start without a database rather than falling back to memory.
//
// ⚠️ The refusal IS the slice. The tempting alternative — no DATABASE_URL, so use MemoryStore —
// is the worst option available: the bot starts, answers commands, accepts follows, looks
// entirely healthy, and loses every follow for every user on the first pod restart. In a cluster
// where a restart is routine that is not a degraded mode, it is silent data loss wearing a green
// tick. Refusing by name turns it into one line of output before anything is lost.
//
// ⚠️ Same shape as telegramFromEnv above, and for the same reason: a missing token exits because
// it cannot fix itself. A missing DSN cannot either.
//
// ⚠️ Only the refusal is tested here. The happy path is NewPostgresStore, which F2 to F13 cover
// against a real database; repeating it here would need this package to have a DSN too, and
// would prove nothing new about the wiring.
func TestStoreFromEnv_withoutADatabaseURL_refusesByName(t *testing.T) {
	t.Setenv("DATABASE_URL", "")

	store, err := storeFromEnv()
	if err == nil {
		t.Fatalf("storeFromEnv() with no DATABASE_URL returned nil error, want a refusal — starting on MemoryStore loses every follow on the next restart")
	}

	// The variable's name has to be IN the message, as with TELEGRAM_TOKEN: "no database"
	// sends the reader looking, "DATABASE_URL is not set" tells them what to do next.
	if !strings.Contains(err.Error(), "DATABASE_URL") {
		t.Errorf("error is %q, want it to name DATABASE_URL so the reader knows what to set", err)
	}

	// Nothing usable may come back alongside the error. A non-nil store here invites a caller
	// to press on with one that was never connected.
	if store != nil {
		t.Errorf("storeFromEnv() returned a store %v alongside the error, want nil", store)
	}
}
