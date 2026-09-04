package bot_test

import (
	"context"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Rolyani/mp-telegram-bot/internal/bot"
)

// testDSN returns the connection string for a Postgres to test against, or skips.
//
// ⚠️ A SKIPPED TEST IS NOT A PASSING TEST. `go test ./...` prints ok for a package whose
// integration tests all skipped, which is the same false green that a mistyped -run pattern
// gives — it looks like proof and is the absence of proof. If you are working on the store,
// run these with -v and check the word PASS actually appears.
//
// The environment variable rather than a hardcoded DSN because the database differs per
// machine, and because a test that can find a server without being told is a test that might
// find the WRONG server. Locally:
//
//	export TEST_DATABASE_URL='postgres://vscode:vscode@127.0.0.1:5432/mpbot_test'
func testDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("SKIPPED, NOT PASSED: set TEST_DATABASE_URL to run the Postgres store tests")
	}
	return dsn
}

// uniqueChatID returns a chat ID no other run has used.
//
// It is what lets these tests share one database without truncating it between runs. Truncation
// would mean the test file knowing the table names, which is the store's business and would
// have to change every time the schema does; a chat ID nobody else uses needs no such knowledge
// and no cleanup. Real Telegram chat IDs are in this range too, so nothing is being faked.
func uniqueChatID() int64 {
	return time.Now().UnixNano()
}

// Slice F2 (session 4, Postgres): the tracer bullet. A chat recorded through the store is still
// there when a completely new store connects to the same database.
//
// ⚠️ This is the whole reason Postgres is in the plan, so it is the first thing to prove. The
// bot is about to run in a Kubernetes homelab where a pod restart is routine — every Flux
// rollout, every node reboot — and MemoryStore silently wipes every follow for every user each
// time. Every other Postgres method is detail; this is the property.
//
// ⚠️ Note the SECOND NewPostgresStore rather than a second call on the same one. Reading back
// through the store that wrote it would pass against an in-memory cache and prove nothing about
// the database. A fresh pool is the closest a test can stand to a restarted process.
func TestPostgresStore_aRecordedChatSurvivesAReconnect(t *testing.T) {
	dsn := testDSN(t)
	chatID := uniqueChatID()

	store, err := bot.NewPostgresStore(dsn)
	if err != nil {
		t.Fatalf("NewPostgresStore() returned error: %v", err)
	}
	defer store.Close()

	if err := store.AddChat(chatID); err != nil {
		t.Fatalf("AddChat(%d) returned error: %v", chatID, err)
	}

	// A new process, as far as the database is concerned.
	reopened, err := bot.NewPostgresStore(dsn)
	if err != nil {
		t.Fatalf("second NewPostgresStore() returned error: %v", err)
	}
	defer reopened.Close()

	chats, err := reopened.Chats()
	if err != nil {
		t.Fatalf("Chats() returned error: %v", err)
	}

	found := false
	for _, id := range chats {
		if id == chatID {
			found = true
		}
	}
	if !found {
		t.Errorf("Chats() = %v, want it to contain %d — the chat was written by one connection and must be readable by the next, or a pod restart loses every subscriber", chats, chatID)
	}
}

// ⚠️ /start is sent more than once by real users — Telegram itself offers the button again on an
// existing chat — so recording the same ID twice must not fail. MemoryStore gets this free from
// a map; a table needs to be told, either with a primary key and ON CONFLICT DO NOTHING or by
// checking first. The first is one statement and cannot race, which is why the store owes it.
func TestPostgresStore_addChatTwice_isNotAnError(t *testing.T) {
	dsn := testDSN(t)
	chatID := uniqueChatID()

	store, err := bot.NewPostgresStore(dsn)
	if err != nil {
		t.Fatalf("NewPostgresStore() returned error: %v", err)
	}
	defer store.Close()

	if err := store.AddChat(chatID); err != nil {
		t.Fatalf("first AddChat(%d) returned error: %v", chatID, err)
	}
	if err := store.AddChat(chatID); err != nil {
		t.Fatalf("second AddChat(%d) returned error: %v — /start is sent twice by real users and must be idempotent", chatID, err)
	}

	chats, err := store.Chats()
	if err != nil {
		t.Fatalf("Chats() returned error: %v", err)
	}

	seen := 0
	for _, id := range chats {
		if id == chatID {
			seen++
		}
	}
	if seen != 1 {
		t.Errorf("Chats() contains %d %d times, want exactly 1 — a duplicated chat would be pushed every division twice", chatID, seen)
	}
}

// Slice F3 (session 4, Postgres): the same restart property as F2, now on the data a user
// would actually miss. A chat ID is cheap to lose — Telegram hands it back the next time
// they speak. A follow list is not: nothing outside this database knows that this chat
// follows this MP, so if a pod restart drops it, the user has to rebuild it by hand and the
// bot silently goes quiet in the meantime.
//
// ⚠️ Same shape as F2 and for the same reason: the SECOND store is the point. Reading back
// through the store that wrote the follow would pass against a map held in the struct and
// prove nothing about Postgres.
//
// ⚠️ Note that both fields are asserted, not just the ID. A follows table storing only
// member_id would satisfy a looser test and then make /follows print a list of numbers —
// the name is what the user reads, so the name has to make the round trip too.
func TestPostgresStore_aFollowSurvivesAReconnect(t *testing.T) {
	dsn := testDSN(t)
	chatID := uniqueChatID()
	mp := bot.Member{ID: 4514, Name: "Sir Keir Starmer"}

	store, err := bot.NewPostgresStore(dsn)
	if err != nil {
		t.Fatalf("NewPostgresStore() returned error: %v", err)
	}
	defer store.Close()

	if err := store.FollowMP(chatID, mp); err != nil {
		t.Fatalf("FollowMP(%d, %+v) returned error: %v", chatID, mp, err)
	}

	// A new process, as far as the database is concerned.
	reopened, err := bot.NewPostgresStore(dsn)
	if err != nil {
		t.Fatalf("second NewPostgresStore() returned error: %v", err)
	}
	defer reopened.Close()

	follows, err := reopened.Follows(chatID)
	if err != nil {
		t.Fatalf("Follows(%d) returned error: %v", chatID, err)
	}

	want := []bot.Member{mp}
	if !reflect.DeepEqual(follows, want) {
		t.Errorf("Follows(%d) = %+v, want %+v — a follow written by one connection must be readable by the next, or every Flux rollout empties every user's list", chatID, follows, want)
	}
}

// Slice F4: following the same MP twice leaves one follow, not two.
//
// ⚠️ Exactly the bug AddChat already has the fix for, one table over. A user who forgets they
// already follow someone, or taps an old /follow button again, would otherwise get every
// division from that MP pushed to them TWICE — and the duplicate is invisible in the database
// until it is a stream of doubled messages on someone's phone.
//
// ⚠️ No reconnect in this one. F2 and F3 needed a second store because they were asking whether
// the data reached Postgres at all; that is now established, and this test asks a different
// question — what the table does with a repeat — which one connection can answer. A test that
// reconnects out of habit is a slower test that proves the same thing twice.
func TestPostgresStore_followingTheSameMPTwice_isNotADuplicate(t *testing.T) {
	dsn := testDSN(t)
	chatID := uniqueChatID()
	mp := bot.Member{ID: 4514, Name: "Sir Keir Starmer"}

	store, err := bot.NewPostgresStore(dsn)
	if err != nil {
		t.Fatalf("NewPostgresStore() returned error: %v", err)
	}
	defer store.Close()

	if err := store.FollowMP(chatID, mp); err != nil {
		t.Fatalf("first FollowMP(%d, %+v) returned error: %v", chatID, mp, err)
	}
	if err := store.FollowMP(chatID, mp); err != nil {
		t.Fatalf("second FollowMP(%d, %+v) returned error: %v — a repeated /follow must be accepted, not rejected", chatID, mp, err)
	}

	follows, err := store.Follows(chatID)
	if err != nil {
		t.Fatalf("Follows(%d) returned error: %v", chatID, err)
	}

	want := []bot.Member{mp}
	if !reflect.DeepEqual(follows, want) {
		t.Errorf("Follows(%d) = %+v, want %+v — a doubled follow means every division from that MP is pushed to that chat twice", chatID, follows, want)
	}
}

// Slice F5: a chat that follows nobody gets an empty list, not a nil one.
//
// ⚠️ This is the decision we deliberately left open in F3, now being made on purpose. Both
// answers work for anyone who only ranges or takes len — the difference shows up the moment
// something compares, encodes, or checks for nil, and by then the choice was made years ago by
// whoever typed the line.
//
// ⚠️ The reason for empty rather than nil is CONSISTENCY WITH Chats, six methods up, which
// already returns make([]int64, 0). Two methods on one store answering "there is nothing here"
// two different ways is a coin-flip every caller has to remember, and the one that gets it wrong
// finds out at runtime.
//
// ⚠️ reflect.DeepEqual is doing real work here and slices.Equal could not: a nil []Member and an
// empty []Member have the same length and the same (zero) elements, so an element-walking
// comparison calls them equal and this test would pass against either. The strict comparison is
// the only reason this is a test and not a comment.
func TestPostgresStore_followsForAChatThatFollowsNobody_isEmptyNotNil(t *testing.T) {
	dsn := testDSN(t)
	// Never followed anyone, never even spoken to the bot.
	chatID := uniqueChatID()

	store, err := bot.NewPostgresStore(dsn)
	if err != nil {
		t.Fatalf("NewPostgresStore() returned error: %v", err)
	}
	defer store.Close()

	follows, err := store.Follows(chatID)
	if err != nil {
		t.Fatalf("Follows(%d) returned error: %v", chatID, err)
	}

	want := []bot.Member{}
	if !reflect.DeepEqual(follows, want) {
		t.Errorf("Follows(%d) = %#v, want %#v — an unknown chat follows nobody, and that must read the same way as Chats() reporting no chats", chatID, follows, want)
	}
}

// Slice F7: an item already sent to a chat is still known to have been sent after a restart.
//
// ⚠️ This is the one where forgetting is LOUD. Losing a chat ID costs a subscriber who comes
// back on their next message; losing a follow costs a list the user can retype. Losing the sent
// record costs every follower a re-run of every activity item the bot has ever pushed them, on
// the first poll after the pod restarts — because CheckActivity asks WasSent before each push
// and a fresh MemoryStore says no to all of it. That is the spam this table exists to prevent.
//
// ⚠️ Both assertions matter, and the second is what gives the first its meaning. A WasSent that
// ignored its arguments and returned true would satisfy the marked case perfectly well; only
// asking about an item that was never marked can tell a real lookup from a stub.
func TestPostgresStore_aSentItemIsStillMarkedSentAfterAReconnect(t *testing.T) {
	dsn := testDSN(t)
	chatID := uniqueChatID()
	const sentID = "division-1901"
	const neverSentID = "division-1902"

	store, err := bot.NewPostgresStore(dsn)
	if err != nil {
		t.Fatalf("NewPostgresStore() returned error: %v", err)
	}
	defer store.Close()

	if err := store.MarkSent(chatID, sentID); err != nil {
		t.Fatalf("MarkSent(%d, %q) returned error: %v", chatID, sentID, err)
	}

	// A new process, as far as the database is concerned.
	reopened, err := bot.NewPostgresStore(dsn)
	if err != nil {
		t.Fatalf("second NewPostgresStore() returned error: %v", err)
	}
	defer reopened.Close()

	sent, err := reopened.WasSent(chatID, sentID)
	if err != nil {
		t.Fatalf("WasSent(%d, %q) returned error: %v", chatID, sentID, err)
	}
	if !sent {
		t.Errorf("WasSent(%d, %q) = false, want true — the item was marked sent before the restart, and saying no here pushes it to the user a second time", chatID, sentID)
	}

	unsent, err := reopened.WasSent(chatID, neverSentID)
	if err != nil {
		t.Fatalf("WasSent(%d, %q) returned error: %v", chatID, neverSentID, err)
	}
	if unsent {
		t.Errorf("WasSent(%d, %q) = true, want false — nothing ever marked this item, and saying yes here silently drops an item the user should have seen", chatID, neverSentID)
	}
}

// countSentRows counts the rows in the sent table for one (chat, activity) pair.
//
// ⚠️ This helper KNOWS THE SCHEMA, and it is the only thing in this file that does. That is a
// deliberate exception to the rule uniqueChatID sets out, not a lapse — and it is worth naming
// why, because the next person to touch the store will have to decide whether to keep it.
//
// The property this slice is about is invisible through the Store interface. WasSent answers
// true whether there is one matching row or fifty, so a test written only against MarkSent and
// WasSent passes today and would go on passing however badly the table duplicated. There is no
// behavioural vantage point; either the test reaches into the schema or the property stays
// unproven. It reaches in.
//
// ⚠️ Its own connection, not the store's pool. Counting through the store would mean adding a
// method to Store that only a test wants, and every future implementation would then owe an
// answer to a question the bot never asks.
func countSentRows(t *testing.T, dsn string, chatID int64, activityID string) int {
	t.Helper()

	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("test connection to %q failed: %v", dsn, err)
	}
	defer conn.Close(ctx)

	var n int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM sent
		WHERE chat_id = $1 AND activity_id = $2`, chatID, activityID).Scan(&n); err != nil {
		t.Fatalf("counting sent rows for (%d, %q) failed: %v", chatID, activityID, err)
	}
	return n
}

// Slice F8: marking the same item sent twice leaves one row, not two.
//
// ⚠️ MarkSent already ends in ON CONFLICT DO NOTHING, so this looks handled and is not. That
// clause needs a unique constraint to conflict AGAINST; the sent table has none, so there is
// nothing for the insert to collide with and the second row goes in silently. It does not
// error, which is what makes it worth a test — the code reads as if the problem were solved.
//
// ⚠️ The path that hits it is live today. /follow writes a baseline by marking every existing
// activity for that MP as sent (bot.go:421), and F4 deliberately made a repeated /follow legal
// rather than an error. So a user who follows the same MP a second time re-runs that entire
// loop and writes a duplicate of every item, every time, on a table nothing ever deletes from.
//
// ⚠️ Note what is NOT asserted: that an index exists, or what it is called. One row per
// (chat, activity) is the property; a unique index is only the cheapest way to get it, and a
// primary key or a check-then-insert would satisfy this test exactly as well. A test naming the
// mechanism would have to be rewritten the day the mechanism changed, and would have been
// asserting the implementation's own opinion of itself in the meantime.
func TestPostgresStore_markingTheSameItemSentTwice_storesOneRow(t *testing.T) {
	dsn := testDSN(t)
	chatID := uniqueChatID()
	const activityID = "division-1903"

	store, err := bot.NewPostgresStore(dsn)
	if err != nil {
		t.Fatalf("NewPostgresStore() returned error: %v", err)
	}
	defer store.Close()

	if err := store.MarkSent(chatID, activityID); err != nil {
		t.Fatalf("first MarkSent(%d, %q) returned error: %v", chatID, activityID, err)
	}
	if err := store.MarkSent(chatID, activityID); err != nil {
		t.Fatalf("second MarkSent(%d, %q) returned error: %v — re-marking an item is what a repeated /follow does, and it must be accepted, not rejected", chatID, activityID, err)
	}

	if n := countSentRows(t, dsn, chatID, activityID); n != 1 {
		t.Errorf("sent holds %d rows for (%d, %q), want 1 — every repeat of /follow duplicates the whole baseline into a table nothing ever prunes", n, chatID, activityID)
	}
}
