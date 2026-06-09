package bot

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
	chats map[int64]bool
}

// NewMemoryStore returns a ready to use *MemoryStore
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{chats: make(map[int64]bool)}
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

// Chats returns the recorded chat IDs
func (s *MemoryStore) Chats() []int64 {
	keys := make([]int64, 0, len(s.chats))
	for k := range s.chats {
		keys = append(keys, k)
	}
	return keys
}

// HandleUpdate processes an update and returns a reply
func HandleUpdate(update Update, store *MemoryStore) (Reply, error) {
	switch update.Text {
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
	default:
		return Reply{
			ChatID: update.ChatID,
			Text:   "Use /start to begin.",
		}, nil
	}
}
