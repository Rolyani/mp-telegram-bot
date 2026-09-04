package bot

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// PostgresStore is the Store that survives a restart. It is the reason Postgres is in this
// project at all: MemoryStore loses every chat and every follow when the process stops, and
// this bot is going to run in a cluster where a pod restart is routine.
type PostgresStore struct {
	pool *pgxpool.Pool
}

// NewPostgresStore connects to the database named by dsn and returns a store ready to use.
//
// ⚠️ A POOL, not a connection. This process runs for weeks and the database WILL restart
// underneath it — a single *pgx.Conn would fail from that moment on, where a pool notices a
// dead connection and opens another.
//
// ⚠️ pgxpool.New does not actually connect. It parses the DSN and returns; the first real
// connection is made on first use. So a wrong password gives a nil error here and a failure
// somewhere later, which is the confusing failure mode telegramFromEnv was written to avoid.
// Ping is what forces the question now, while there is still someone reading the output.
func NewPostgresStore(dsn string) (*PostgresStore, error) {
	ctx := context.Background()

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("connect to postgres: %w", err)
	}

	if err := pool.Ping(ctx); err != nil {
		// The pool is already holding resources, so it has to be handed back before the
		// error goes up. Nothing else will do it — the caller has no store to Close.
		pool.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}

	if _, err := pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS chats (
	chat_id BIGINT PRIMARY KEY
	)`); err != nil {
		pool.Close()
		return nil, fmt.Errorf("create chats table: %w", err)
	}

	if _, err := pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS follows (
		chat_id BIGINT,
		member_id INT,
		name TEXT
	)`); err != nil {
		pool.Close()
		return nil, fmt.Errorf("create follows table: %w", err)
	}

	if _, err := pool.Exec(ctx, `CREATE UNIQUE INDEX IF NOT EXISTS follows_chat_member
		ON follows (chat_id, member_id)
	`); err != nil {
		pool.Close()
		return nil, fmt.Errorf("create follows index: %w", err)
	}

	if _, err := pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS sent (
		chat_id BIGINT,
		activity_id TEXT
	)`); err != nil {
		pool.Close()
		return nil, fmt.Errorf("create sent table: %w", err)
	}

	if _, err := pool.Exec(ctx, `CREATE UNIQUE INDEX IF NOT EXISTS sent_chat_activity
		ON sent (chat_id, activity_id)
	`); err != nil {
		pool.Close()
		return nil, fmt.Errorf("create sent index: %w", err)
	}

	return &PostgresStore{pool: pool}, nil
}

func (s *PostgresStore) AddChat(chatID int64) error {
	ctx := context.Background()

	if _, err := s.pool.Exec(ctx, `INSERT INTO chats (chat_id)
		VALUES ($1) ON CONFLICT DO NOTHING`, chatID); err != nil {
		return fmt.Errorf("insert into chats: %w", err)
	}

	return nil

}

func (s *PostgresStore) FollowMP(chatID int64, mp Member) error {
	ctx := context.Background()

	if _, err := s.pool.Exec(ctx, `INSERT INTO follows (chat_id, member_id, name)
		VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`, chatID, mp.ID, mp.Name); err != nil {
		return fmt.Errorf("insert into follows: %w", err)
	}

	return nil
}

// Chats returns every recorded chat ID.
//
// ⚠️ The capacity is left at zero rather than guessed. MemoryStore could say len(s.chats)
// because the map was already in hand; here the row count is not known until the last row has
// been read, and a wrong guess only costs a copy.
func (s *PostgresStore) Chats() ([]int64, error) {
	ctx := context.Background()

	rows, err := s.pool.Query(ctx, `SELECT chat_id FROM chats`)
	if err != nil {
		return nil, fmt.Errorf("select chats: %w", err)
	}
	// ⚠️ rows holds a connection borrowed from the pool until it is closed. Miss this and the
	// bot runs for a day and then blocks forever, every query waiting for a free connection.
	defer rows.Close()

	chats := make([]int64, 0)
	for rows.Next() {
		var chatID int64
		if err := rows.Scan(&chatID); err != nil {
			return nil, fmt.Errorf("scan chat id: %w", err)
		}
		chats = append(chats, chatID)
	}

	// ⚠️ rows.Next() returns false both for "no more rows" and for "the connection died
	// mid-read", and the loop cannot tell those apart. rows.Err() is the only thing that can;
	// without it a broken connection is indistinguishable from an empty subscriber list.
	return chats, rows.Err()
}

func (s *PostgresStore) Follows(chatID int64) ([]Member, error) {
	ctx := context.Background()

	rows, err := s.pool.Query(ctx, `SELECT member_id, name FROM follows WHERE chat_id = $1`, chatID)
	if err != nil {
		return nil, fmt.Errorf("select follows: %w", err)
	}

	defer rows.Close()

	follows := make([]Member, 0)
	for rows.Next() {
		var mp Member
		if err := rows.Scan(&mp.ID, &mp.Name); err != nil {
			return nil, fmt.Errorf("scan follow: %w", err)
		}
		follows = append(follows, mp)
	}

	return follows, rows.Err()
}

func (s *PostgresStore) MarkSent(chatID int64, activityID string) error {
	ctx := context.Background()

	if _, err := s.pool.Exec(ctx, `INSERT INTO sent (chat_id, activity_id)
		VALUES ($1, $2) ON CONFLICT DO NOTHING`, chatID, activityID); err != nil {
		return fmt.Errorf("insert into sent: %w", err)
	}

	return nil
}

func (s *PostgresStore) WasSent(chatID int64, activityID string) (bool, error) {
	ctx := context.Background()

	var exists bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1 FROM sent WHERE chat_id = $1 AND activity_id = $2
	)`, chatID, activityID).Scan(&exists)

	if err != nil {
		return false, fmt.Errorf("was sent scan: %w", err)
	}

	return exists, nil
}

// Close releases the pool's connections. The program calls it once, at shutdown; the tests call
// it on every store they open.
func (s *PostgresStore) Close() {
	s.pool.Close()
}
