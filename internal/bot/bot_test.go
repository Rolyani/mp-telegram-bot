package bot_test

import (
	"strings"
	"testing"

	"github.com/Rolyani/mp-telegram-bot/internal/bot"
)

// fakeSource is an in-memory ActivitySource: canned activity items keyed by MP, so the
// poll slice stays offline (real HTTP feeds arrive in Phase C).
type fakeSource struct {
	items map[string][]bot.Activity
}

func (f fakeSource) Activity(mp string) []bot.Activity {
	return f.items[mp]
}

// Slice 10 (Phase D, poll loop — newness half): polling twice must not re-push an item
// already sent to a chat. The first CheckActivity delivers the item; the second, over the
// same unchanged source, delivers nothing. This drives a per-chat "already sent" high-water
// mark in the store, keyed on Activity.ID — "sent" is tracked per chat, so each follower is
// notified of a given item exactly once. (First-follow baseline — suppressing backlog for a
// brand-new follower — is the next slice and reuses this same machinery.)
func TestCheckActivity_itemAlreadySent_notPushedAgain(t *testing.T) {
	store := bot.NewMemoryStore()
	store.AddChat(1)
	store.FollowMP(1, "Keir Starmer")

	source := fakeSource{items: map[string][]bot.Activity{
		"Keir Starmer": {{ID: "v42", Text: "voted on Bill 42"}},
	}}

	first := bot.CheckActivity(source, store)
	if len(first) != 1 {
		t.Fatalf("first poll: got %d replies, want 1", len(first))
	}

	second := bot.CheckActivity(source, store)
	if len(second) != 0 {
		t.Errorf("second poll re-pushed %d already-sent item(s), want 0", len(second))
	}
}

// Slice 9 (Phase D, poll loop — fan-out half): a poll over the store turns an MP's
// activity into a reply for each chat that follows that MP. One follower, one MP, one
// item -> one reply addressed to that chat, mentioning the activity. Detecting *new*
// activity (not re-pushing already-sent items) is a deliberately separate behavior — the
// next slice — so this one proves only source -> match subscribers -> broadcast.
func TestCheckActivity_itemForFollowedMP_repliesToSubscriber(t *testing.T) {
	store := bot.NewMemoryStore()
	store.AddChat(1)
	store.FollowMP(1, "Keir Starmer")

	source := fakeSource{items: map[string][]bot.Activity{
		"Keir Starmer": {{ID: "v42", Text: "voted on Bill 42"}},
	}}

	replies := bot.CheckActivity(source, store)

	if len(replies) != 1 {
		t.Fatalf("got %d replies, want 1", len(replies))
	}
	if replies[0].ChatID != 1 {
		t.Errorf("reply addressed to chat %d, want 1", replies[0].ChatID)
	}
	if !strings.Contains(replies[0].Text, "voted on Bill 42") {
		t.Errorf("reply text %q does not mention the activity", replies[0].Text)
	}
}

// Slice 8: /list replies with the MPs the chat follows. After following two MPs, the
// reply is addressed back to the chat and mentions each by name. Substring checks keep
// the exact formatting and ordering free to change. (The "follows nobody" case is a
// distinct behavior — its own later slice, not this one.)
func TestHandleUpdate_list_repliesWithFollowedMPs(t *testing.T) {
	store := bot.NewMemoryStore()

	mps := []string{"Keir Starmer", "Rishi Sunak"}
	for _, mp := range mps {
		if _, err := bot.HandleUpdate(bot.Update{ChatID: 42, Text: "/follow " + mp}, store); err != nil {
			t.Fatalf("following %q: %v", mp, err)
		}
	}

	reply, err := bot.HandleUpdate(bot.Update{ChatID: 42, Text: "/list"}, store)
	if err != nil {
		t.Fatalf("HandleUpdate(/list) returned error: %v", err)
	}

	if reply.ChatID != 42 {
		t.Errorf("reply addressed to chat %d, want 42", reply.ChatID)
	}
	for _, mp := range mps {
		if !strings.Contains(reply.Text, mp) {
			t.Errorf("reply %q does not mention followed MP %q", reply.Text, mp)
		}
	}
}

// Slice 11 (Phase A): /list when the chat follows nobody must NOT emit the dangling
// "You follow: " reply (which is what strings.Join over an empty list produces today).
// It should send a non-empty message that reads distinctly from the populated list, so a
// brand-new user is told they follow no one rather than shown an empty list. Wording is
// not pinned — only non-empty, and distinct from the with-follows reply.
func TestHandleUpdate_list_whenFollowingNobody_distinctReply(t *testing.T) {
	store := bot.NewMemoryStore()

	empty, err := bot.HandleUpdate(bot.Update{ChatID: 7, Text: "/list"}, store)
	if err != nil {
		t.Fatalf("HandleUpdate(/list) returned error: %v", err)
	}

	if empty.ChatID != 7 {
		t.Errorf("reply addressed to chat %d, want 7", empty.ChatID)
	}
	if strings.TrimSpace(empty.Text) == "" {
		t.Errorf("empty-follows /list reply is blank, want a non-empty message")
	}

	// A chat that DOES follow someone gets the normal list reply. The no-follows reply
	// must not merely be that list with an empty body — i.e. not a prefix of the
	// populated reply ("You follow: " is a prefix of "You follow: Keir Starmer"). It has
	// to be its own message, so a user following nobody isn't shown a dangling list.
	if _, err := bot.HandleUpdate(bot.Update{ChatID: 7, Text: "/follow Keir Starmer"}, store); err != nil {
		t.Fatalf("follow setup failed: %v", err)
	}
	populated, err := bot.HandleUpdate(bot.Update{ChatID: 7, Text: "/list"}, store)
	if err != nil {
		t.Fatalf("HandleUpdate(/list) after follow returned error: %v", err)
	}
	if strings.HasPrefix(populated.Text, empty.Text) {
		t.Errorf("no-follows reply %q is just a prefix of the with-follows reply %q (dangling empty list); want a distinct message", empty.Text, populated.Text)
	}
}

// Slice 7: /follow with no MP name must not record an empty follow, and must reply
// with a usage hint distinct from the success confirmation. Covers both a bare
// "/follow" (no argument) and "/follow   " (whitespace-only argument) — the latter
// pins that the guard trims before deciding, so spaces alone don't count as a name.
func TestHandleUpdate_followWithoutName_recordsNothingAndHints(t *testing.T) {
	// Capture the success confirmation behaviorally so we can assert the hint differs
	// from it without hardcoding either string.
	confirm, err := bot.HandleUpdate(bot.Update{ChatID: 1, Text: "/follow Keir Starmer"}, bot.NewMemoryStore())
	if err != nil {
		t.Fatalf("HandleUpdate(/follow <name>) returned error: %v", err)
	}

	for _, text := range []string{"/follow", "/follow   "} {
		t.Run(text, func(t *testing.T) {
			store := bot.NewMemoryStore()

			reply, err := bot.HandleUpdate(bot.Update{ChatID: 7, Text: text}, store)
			if err != nil {
				t.Fatalf("HandleUpdate(%q) returned error: %v", text, err)
			}

			if got := store.Follows(7); len(got) != 0 {
				t.Errorf("store.Follows(7) = %v, want nothing recorded for %q", got, text)
			}
			if reply.ChatID != 7 {
				t.Errorf("reply addressed to chat %d, want 7", reply.ChatID)
			}
			if reply.Text == "" {
				t.Errorf("reply.Text is empty, want a usage hint")
			}
			if reply.Text == confirm.Text {
				t.Errorf("got the success confirmation %q, want a distinct usage hint", reply.Text)
			}
		})
	}
}

// Slice 12 (Phase A): /unfollow <name> removes just that MP from the chat's follow
// list, leaving any others intact. Mirror of /follow. The chat follows two MPs, then
// unfollows one; only the other remains. There's no built-in slice remove, so this
// drives an UnfollowMP store method (filter into a new slice). Confirmation is
// addressed back to the chat and non-empty; exact wording stays free.
func TestHandleUpdate_unfollow_removesNamedMPOnly(t *testing.T) {
	store := bot.NewMemoryStore()

	const kept = "Rishi Sunak"
	const removed = "Keir Starmer"
	for _, mp := range []string{removed, kept} {
		if _, err := bot.HandleUpdate(bot.Update{ChatID: 42, Text: "/follow " + mp}, store); err != nil {
			t.Fatalf("follow setup for %q failed: %v", mp, err)
		}
	}

	reply, err := bot.HandleUpdate(bot.Update{ChatID: 42, Text: "/unfollow " + removed}, store)
	if err != nil {
		t.Fatalf("HandleUpdate(/unfollow) returned error: %v", err)
	}

	// Assert: only the unfollowed MP is gone; the other remains.
	got := store.Follows(42)
	if len(got) != 1 || got[0] != kept {
		t.Fatalf("store.Follows(42) = %v, want exactly [%q]", got, kept)
	}

	// Assert: confirmation addressed back to the chat and non-empty.
	if reply.ChatID != 42 {
		t.Errorf("reply addressed to chat %d, want 42", reply.ChatID)
	}
	if reply.Text == "" {
		t.Errorf("reply.Text is empty, want an unfollow confirmation")
	}
}

// Slice 13 (Phase A): /unfollow with no MP name must not touch the chat's existing
// follows, and must reply with a usage hint — NOT the bogus "You have unfollowed ."
// success message it sends today. Mirror of the /follow guard (slice 7). Covers both a
// bare "/unfollow" and a whitespace-only "/unfollow   " so the guard is pinned to trim
// before deciding, just like /follow.
func TestHandleUpdate_unfollowWithoutName_changesNothingAndHints(t *testing.T) {
	// Capture a real success confirmation behaviorally, then blank out the name. The
	// confirmation embeds the name verbatim, so removing it collapses the template down
	// to exactly the reply the empty-name case wrongly produces today ("You have
	// unfollowed ."). Asserting the guard reply differs from that pins it as a genuine
	// usage hint rather than a name-less "success" — and avoids hardcoding any wording.
	const realName = "Keir Starmer"
	confirmStore := bot.NewMemoryStore()
	if _, err := bot.HandleUpdate(bot.Update{ChatID: 1, Text: "/follow " + realName}, confirmStore); err != nil {
		t.Fatalf("follow setup for confirmation failed: %v", err)
	}
	confirm, err := bot.HandleUpdate(bot.Update{ChatID: 1, Text: "/unfollow " + realName}, confirmStore)
	if err != nil {
		t.Fatalf("HandleUpdate(/unfollow <name>) returned error: %v", err)
	}
	namelessConfirm := strings.Replace(confirm.Text, realName, "", 1)

	for _, text := range []string{"/unfollow", "/unfollow   "} {
		t.Run(text, func(t *testing.T) {
			store := bot.NewMemoryStore()
			const followed = "Rishi Sunak"
			if _, err := bot.HandleUpdate(bot.Update{ChatID: 7, Text: "/follow " + followed}, store); err != nil {
				t.Fatalf("follow setup failed: %v", err)
			}

			reply, err := bot.HandleUpdate(bot.Update{ChatID: 7, Text: text}, store)
			if err != nil {
				t.Fatalf("HandleUpdate(%q) returned error: %v", text, err)
			}

			// The existing follow must be untouched — a nameless /unfollow removes nothing.
			if got := store.Follows(7); len(got) != 1 || got[0] != followed {
				t.Errorf("store.Follows(7) = %v, want unchanged [%q] for %q", got, followed, text)
			}
			if reply.ChatID != 7 {
				t.Errorf("reply addressed to chat %d, want 7", reply.ChatID)
			}
			if reply.Text == "" {
				t.Errorf("reply.Text is empty, want a usage hint")
			}
			if reply.Text == namelessConfirm {
				t.Errorf("got the name-less success confirmation %q, want a distinct usage hint", reply.Text)
			}
		})
	}
}

// Slice 14 (Phase A): /unfollow <name> for an MP the chat does NOT follow is a no-op
// and must not falsely claim a removal happened. The SAME name is used in both arms so
// the only variable is followed-vs-not, not the name (slice 13's lesson): a chat that
// really follows the MP gets the success confirmation; a chat that never followed them
// must get a DIFFERENT reply. This drives UnfollowMP to report whether it actually
// removed anything — today it silently filters and the handler always claims success.
func TestHandleUpdate_unfollowUnknownName_noOpAndDistinctReply(t *testing.T) {
	const mp = "Keir Starmer"

	// Arm A: chat genuinely follows mp, then unfollows — capture the real success reply.
	following := bot.NewMemoryStore()
	if _, err := bot.HandleUpdate(bot.Update{ChatID: 1, Text: "/follow " + mp}, following); err != nil {
		t.Fatalf("follow setup failed: %v", err)
	}
	success, err := bot.HandleUpdate(bot.Update{ChatID: 1, Text: "/unfollow " + mp}, following)
	if err != nil {
		t.Fatalf("HandleUpdate(/unfollow followed) returned error: %v", err)
	}

	// Arm B: chat follows someone ELSE, then unfollows mp it never followed.
	store := bot.NewMemoryStore()
	const other = "Rishi Sunak"
	if _, err := bot.HandleUpdate(bot.Update{ChatID: 7, Text: "/follow " + other}, store); err != nil {
		t.Fatalf("follow setup failed: %v", err)
	}
	reply, err := bot.HandleUpdate(bot.Update{ChatID: 7, Text: "/unfollow " + mp}, store)
	if err != nil {
		t.Fatalf("HandleUpdate(/unfollow unknown) returned error: %v", err)
	}

	// No-op: the unrelated follow is left untouched.
	if got := store.Follows(7); len(got) != 1 || got[0] != other {
		t.Errorf("store.Follows(7) = %v, want unchanged [%q]", got, other)
	}
	if reply.ChatID != 7 {
		t.Errorf("reply addressed to chat %d, want 7", reply.ChatID)
	}
	if reply.Text == "" {
		t.Errorf("reply.Text is empty, want a 'not following them' message")
	}
	// Same name in both arms, so an identical reply can only mean the handler claimed a
	// removal that never happened.
	if reply.Text == success.Text {
		t.Errorf("unknown-name /unfollow reply %q equals the real success confirmation; want a distinct 'you weren't following them' message", reply.Text)
	}
}

// Slice 14b (Phase A): /unfollow an MP the chat follows AMONG OTHERS must report
// success — it WAS removed — and reply with the success confirmation, not the "weren't
// following them" message. Pins UnfollowMP's bool to mean "did mp get removed?", not
// "did the list become empty / contain only mp". Reference success reply is captured
// from a chat that follows ONLY the target, so the two replies must match.
func TestHandleUpdate_unfollowFollowedAmongOthers_reportsSuccess(t *testing.T) {
	const target = "Keir Starmer"
	const other = "Rishi Sunak"

	// Reference: the success confirmation when the chat follows ONLY the target.
	solo := bot.NewMemoryStore()
	if _, err := bot.HandleUpdate(bot.Update{ChatID: 1, Text: "/follow " + target}, solo); err != nil {
		t.Fatalf("solo follow setup failed: %v", err)
	}
	success, err := bot.HandleUpdate(bot.Update{ChatID: 1, Text: "/unfollow " + target}, solo)
	if err != nil {
		t.Fatalf("solo unfollow failed: %v", err)
	}

	// The chat under test follows the target AND someone else, then unfollows the target.
	store := bot.NewMemoryStore()
	for _, mp := range []string{target, other} {
		if _, err := bot.HandleUpdate(bot.Update{ChatID: 7, Text: "/follow " + mp}, store); err != nil {
			t.Fatalf("follow setup for %q failed: %v", mp, err)
		}
	}
	reply, err := bot.HandleUpdate(bot.Update{ChatID: 7, Text: "/unfollow " + target}, store)
	if err != nil {
		t.Fatalf("HandleUpdate(/unfollow) returned error: %v", err)
	}

	// The target is gone, the other remains.
	if got := store.Follows(7); len(got) != 1 || got[0] != other {
		t.Fatalf("store.Follows(7) = %v, want exactly [%q]", got, other)
	}
	// The target WAS removed, so the reply must be the success confirmation — identical
	// to the solo case (same name, removal genuinely happened in both).
	if reply.Text != success.Text {
		t.Errorf("reply %q, want the success confirmation %q (the MP was actually removed)", reply.Text, success.Text)
	}
}

// Slice 15 (Phase A): /forgetme wipes EVERYTHING the bot holds for a chat — its
// subscription AND its entire follow list — in one command, then confirms. This is more
// than /stop (which only unsubscribes, leaving follows behind): the chat under test both
// subscribed and follows two MPs, so a minimal "just unsubscribe" implementation leaves
// the follows and fails. Asserting on both the chat record and the follow list forces a
// store method that clears the two together. Confirmation is addressed back to the chat
// and non-empty; wording stays free.
func TestHandleUpdate_forgetme_wipesSubscriptionAndFollows(t *testing.T) {
	store := bot.NewMemoryStore()

	// The chat is fully present: subscribed via /start and following two MPs.
	if _, err := bot.HandleUpdate(bot.Update{ChatID: 42, Text: "/start"}, store); err != nil {
		t.Fatalf("/start setup failed: %v", err)
	}
	for _, mp := range []string{"Keir Starmer", "Rishi Sunak"} {
		if _, err := bot.HandleUpdate(bot.Update{ChatID: 42, Text: "/follow " + mp}, store); err != nil {
			t.Fatalf("follow setup for %q failed: %v", mp, err)
		}
	}

	reply, err := bot.HandleUpdate(bot.Update{ChatID: 42, Text: "/forgetme"}, store)
	if err != nil {
		t.Fatalf("HandleUpdate(/forgetme) returned error: %v", err)
	}

	// Both halves must be gone: no longer a recorded chat, and no follows remain.
	if store.HasChat(42) {
		t.Errorf("chat 42 still recorded after /forgetme, want it removed")
	}
	if got := store.Follows(42); len(got) != 0 {
		t.Errorf("store.Follows(42) = %v after /forgetme, want nothing", got)
	}

	// Confirmation addressed back to the chat and non-empty.
	if reply.ChatID != 42 {
		t.Errorf("reply addressed to chat %d, want 42", reply.ChatID)
	}
	if reply.Text == "" {
		t.Errorf("reply.Text is empty, want a confirmation message")
	}
}

// Slice 6: /follow <name> records that the chat follows that MP, readable back via
// a new per-chat accessor Follows(chatID). The name carries a space (first/last), so
// this pins that HandleUpdate splits the command from its argument on the FIRST space
// only — a naive whitespace split would drop the surname. Confirmation reply is
// addressed back to the chat, non-empty, and distinct from the /start welcome.
func TestHandleUpdate_follow_recordsNamedMP(t *testing.T) {
	store := bot.NewMemoryStore()

	// Capture the welcome behaviorally so we can assert the confirmation differs
	// from it without hardcoding either string.
	welcome, err := bot.HandleUpdate(bot.Update{ChatID: 42, Text: "/start"}, store)
	if err != nil {
		t.Fatalf("HandleUpdate(/start) returned error: %v", err)
	}

	const mp = "Keir Starmer"
	reply, err := bot.HandleUpdate(bot.Update{ChatID: 42, Text: "/follow " + mp}, store)
	if err != nil {
		t.Fatalf("HandleUpdate(/follow) returned error: %v", err)
	}

	// Assert: the named MP is recorded as a follow for this chat.
	got := store.Follows(42)
	if len(got) != 1 || got[0] != mp {
		t.Fatalf("store.Follows(42) = %v, want exactly [%q]", got, mp)
	}

	// Assert: confirmation addressed back to the chat, non-empty, distinct from welcome.
	if reply.ChatID != 42 {
		t.Errorf("reply addressed to chat %d, want 42", reply.ChatID)
	}
	if reply.Text == "" {
		t.Errorf("reply.Text is empty, want a follow confirmation")
	}
	if reply.Text == welcome.Text {
		t.Errorf("/follow got the welcome reply %q, want a distinct confirmation", reply.Text)
	}
}

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
