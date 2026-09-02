package bot_test

import (
	"os"
	"testing"
	"time"

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
