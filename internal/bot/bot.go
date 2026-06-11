package bot

import "strings"

// Update represents an incoming message
type Update struct {
	ChatID int64
	Text   string
}

// Reply is what HandleUpdate returns
type Reply struct {
	ChatID int64
	Text   string
}

// MemoryStore remembers chat IDs
type MemoryStore struct {
	chats   map[int64]bool
	follows map[int64][]string
}

// NewMemoryStore returns a ready to use *MemoryStore
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{chats: make(map[int64]bool), follows: make(map[int64][]string)}
}

// AddChat records a chat ID
func (s *MemoryStore) AddChat(chatID int64) {
	s.chats[chatID] = true
}

// RemoveChat forgets a chat ID
func (s *MemoryStore) RemoveChat(chatID int64) {
	delete(s.chats, chatID)
}

// HasChat reports whether the chat was recorded
func (s *MemoryStore) HasChat(chatID int64) bool {
	return s.chats[chatID]
}

// FollowMP records that chatID follows the named MP.
func (s *MemoryStore) FollowMP(chatID int64, mp string) {
	s.follows[chatID] = append(s.follows[chatID], mp)
}

// Follows returns the MPs that chatID follows.
func (s *MemoryStore) Follows(chatID int64) []string {
	return s.follows[chatID]
}

// Chats returns the recorded chat IDs
func (s *MemoryStore) Chats() []int64 {
	keys := make([]int64, 0, len(s.chats))
	for k := range s.chats {
		keys = append(keys, k)
	}
	return keys
}

// Broadcast builds one reply per recorded subscriber, carrying msg.
func Broadcast(msg string, store *MemoryStore) []Reply {
	chats := store.Chats()
	replies := make([]Reply, 0, len(chats))
	for _, id := range chats {
		replies = append(replies, Reply{ChatID: id, Text: msg})
	}
	return replies
}

// HandleUpdate processes an update and returns a reply
func HandleUpdate(update Update, store *MemoryStore) (Reply, error) {
	cmd, arg, _ := strings.Cut(update.Text, " ")
	switch cmd {
	case "/start":
		store.AddChat(update.ChatID)
		return Reply{
			ChatID: update.ChatID,
			Text:   "Welcome! Send /start to get going.",
		}, nil
	case "/stop":
		store.RemoveChat(update.ChatID)
		return Reply{
			ChatID: update.ChatID,
			Text:   "Your details have been removed.",
		}, nil
	case "/follow":
		store.FollowMP(update.ChatID, arg)
		return Reply{
			ChatID: update.ChatID,
			Text:   "Now following " + arg + ".",
		}, nil
	default:
		return Reply{
			ChatID: update.ChatID,
			Text:   "Use /start to begin.",
		}, nil
	}
}
