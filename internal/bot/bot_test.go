package bot_test

import (
	"testing"

	"github.com/Rolyani/mp-telegram-bot/internal/bot"
)

// Slice 5: Broadcast sends one reply per recorded subscriber, each addressed to
// that chat and carrying the same message. Chats() is unsorted, so we compare the
// replies as a set (chatID -> text), never by position.
func TestBroadcast_sendsMessageToEverySubscriber(t *testing.T) {
	store := bot.NewMemoryStore()
	for _, id := range []int64{1, 2} {
		if _, err := bot.HandleUpdate(bot.Update{ChatID: id, Text: "/start"}, store); err != nil {
			t.Fatalf("subscribing chat %d: %v", id, err)
		}
	}

	const msg = "Division at 7pm"
	replies := bot.Broadcast(msg, store)

	got := make(map[int64]string)
	for _, r := range replies {
		got[r.ChatID] = r.Text
	}
	want := map[int64]string{1: msg, 2: msg}

	if len(replies) != len(want) {
		t.Fatalf("Broadcast returned %d replies, want %d: %+v", len(replies), len(want), replies)
	}
	for id, text := range want {
		if got[id] != text {
			t.Errorf("reply to chat %d = %q, want %q", id, got[id], text)
		}
	}
}

// Slice 4: /stop unsubscribes — a chat that previously /start-ed is removed
// from the store, and gets a goodbye reply addressed to it, distinct from the
// welcome. Drives a remove side-effect (the mirror of /start's record).
func TestHandleUpdate_stop_unsubscribesAndDistinctReply(t *testing.T) {
	store := bot.NewMemoryStore()

	// Arrange: chat 42 is subscribed. Capture the welcome behaviorally so we
	// can assert the goodbye differs from it without hardcoding either string.
	welcome, err := bot.HandleUpdate(bot.Update{ChatID: 42, Text: "/start"}, store)
	if err != nil {
		t.Fatalf("HandleUpdate(/start) returned error: %v", err)
	}
	if !store.HasChat(42) {
		t.Fatalf("precondition failed: chat 42 not recorded after /start")
	}

	// Act: chat 42 sends /stop.
	reply, err := bot.HandleUpdate(bot.Update{ChatID: 42, Text: "/stop"}, store)
	if err != nil {
		t.Fatalf("HandleUpdate(/stop) returned error: %v", err)
	}

	// Assert: removed from the store.
	if store.HasChat(42) {
		t.Errorf("chat 42 still recorded after /stop, want it removed")
	}
	// Assert: goodbye addressed back to the chat, non-empty, distinct from welcome.
	if reply.ChatID != 42 {
		t.Errorf("reply addressed to chat %d, want 42", reply.ChatID)
	}
	if reply.Text == "" {
		t.Errorf("reply.Text is empty, want a goodbye message")
	}
	if reply.Text == welcome.Text {
		t.Errorf("/stop got the welcome reply %q, want a distinct goodbye", reply.Text)
	}
}

// Slice 3: /start is idempotent — a repeated /start from the same chat must
// not duplicate the chat in the store. Pinning that requires enumerating the
// store via Chats(), the accessor broadcasting will need anyway.
func TestHandleUpdate_repeatedStart_recordsChatOnce(t *testing.T) {
	store := bot.NewMemoryStore()

	for i := 0; i < 2; i++ {
		if _, err := bot.HandleUpdate(bot.Update{ChatID: 7, Text: "/start"}, store); err != nil {
			t.Fatalf("HandleUpdate call %d returned error: %v", i+1, err)
		}
	}

	got := store.Chats()
	if len(got) != 1 || got[0] != 7 {
		t.Fatalf("store.Chats() = %v, want exactly [7]", got)
	}
}

// Slice 2: a non-/start message must not be recorded in the store, and must
// get a non-empty reply of its own — not the /start welcome.
func TestHandleUpdate_unknownMessage_notRecordedAndDistinctReply(t *testing.T) {
	// Establish what the welcome looks like, behaviorally, rather than
	// hardcoding the string here.
	welcome, err := bot.HandleUpdate(bot.Update{ChatID: 1, Text: "/start"}, bot.NewMemoryStore())
	if err != nil {
		t.Fatalf("HandleUpdate(/start) returned error: %v", err)
	}

	store := bot.NewMemoryStore()
	reply, err := bot.HandleUpdate(bot.Update{ChatID: 99, Text: "hello"}, store)
	if err != nil {
		t.Fatalf("HandleUpdate returned error: %v", err)
	}

	if store.HasChat(99) {
		t.Errorf("store recorded chat 99 for %q, want only /start to record", "hello")
	}
	if reply.ChatID != 99 {
		t.Errorf("reply addressed to chat %d, want 99", reply.ChatID)
	}
	if reply.Text == "" {
		t.Errorf("reply.Text is empty, want a hint for unrecognized input")
	}
	if reply.Text == welcome.Text {
		t.Errorf("unknown message got the welcome reply %q, want a distinct reply", reply.Text)
	}
}

// Slice 1: an incoming /start update should be recorded in the subscriber store
// and produce a welcome reply addressed back to the same chat.
func TestHandleUpdate(t *testing.T) {
	tests := []struct {
		name        string
		update      bot.Update
		wantReplyTo int64 // chat the reply is addressed to
		wantStored  int64 // chat expected to be recorded in the store
	}{
		{
			name:        "start command records chat and replies",
			update:      bot.Update{ChatID: 12345, Text: "/start"},
			wantReplyTo: 12345,
			wantStored:  12345,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := bot.NewMemoryStore()

			reply, err := bot.HandleUpdate(tt.update, store)
			if err != nil {
				t.Fatalf("HandleUpdate returned error: %v", err)
			}
			if reply.ChatID != tt.wantReplyTo {
				t.Errorf("reply addressed to chat %d, want %d", reply.ChatID, tt.wantReplyTo)
			}
			if reply.Text == "" {
				t.Errorf("reply.Text is empty, want a welcome message")
			}
			if !store.HasChat(tt.wantStored) {
				t.Errorf("store did not record chat %d", tt.wantStored)
			}
		})
	}
}
