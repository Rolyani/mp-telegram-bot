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
	seen    map[int64]map[string]bool
}

// Activity is one item of an MP's parliamentary activity (a vote, question, or speech).
// ID uniquely identifies the item so it can be de-duplicated; Text is what subscribers see.
type Activity struct {
	ID   string
	Text string
}

// ActivitySource fetches the recent activity for a given MP. Implementations may hit the
// Parliament APIs; tests supply an in-memory fake.
type ActivitySource interface {
	Activity(mp string) []Activity
}

// NewMemoryStore returns a ready to use *MemoryStore
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		chats:   make(map[int64]bool),
		follows: make(map[int64][]string),
		seen:    make(map[int64]map[string]bool),
	}
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

// UnfollowMP removes mp from chatID's follow list, leaving any others intact.
// Go has no built-in slice remove, so it filters in place: kept items are
// appended back over the same backing array.
func (s *MemoryStore) UnfollowMP(chatID int64, mp string) bool {
	keep := s.follows[chatID][:0]
	removed := false
	for _, f := range s.follows[chatID] {
		if f != mp {
			keep = append(keep, f)
		} else {
			removed = true
		}
	}
	s.follows[chatID] = keep
	return removed
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

// CheckActivity polls the source for every followed MP and builds one reply per activity
// item, addressed to each chat that follows that MP. It does not yet suppress
// already-sent items — that's the next slice.
func CheckActivity(source ActivitySource, store *MemoryStore) []Reply {
	chats := store.Chats()
	replies := make([]Reply, 0, len(chats))
	for _, id := range chats {
		follows := store.Follows(id)
		for _, mp := range follows {
			data := source.Activity(mp)
			for _, act := range data {
				if store.WasSent(id, act.ID) {
					continue
				}
				replies = append(replies, Reply{ChatID: id, Text: act.Text})
				store.MarkSent(id, act.ID)
			}
		}
	}
	return replies
}

func (s *MemoryStore) MarkSent(chatID int64, activityID string) {
	if s.seen[chatID] == nil {
		s.seen[chatID] = make(map[string]bool)
	}
	s.seen[chatID][activityID] = true
}

func (s *MemoryStore) WasSent(chatID int64, activityID string) bool {
	return s.seen[chatID][activityID]
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
		name := strings.TrimSpace(arg)
		if name == "" {
			return Reply{
				ChatID: update.ChatID,
				Text:   "You must enter a name.",
			}, nil
		}
		store.FollowMP(update.ChatID, name)
		return Reply{
			ChatID: update.ChatID,
			Text:   "Now following " + name + ".",
		}, nil
	case "/unfollow":
		name := strings.TrimSpace(arg)
		if name == "" {
			return Reply{
				ChatID: update.ChatID,
				Text:   "Enter an MPs name to unfollow.",
			}, nil
		}
		removed := store.UnfollowMP(update.ChatID, name)
		if removed == false {
			return Reply{
				ChatID: update.ChatID,
				Text:   "You were not following this MP.",
			}, nil
		}
		return Reply{
			ChatID: update.ChatID,
			Text:   "You have unfollowed " + name + ".",
		}, nil

	case "/list":
		follows := store.Follows(update.ChatID)
		if len(follows) == 0 {
			return Reply{
				ChatID: update.ChatID,
				Text:   "You are not following any MPs yet.",
			}, nil
		}
		return Reply{
			ChatID: update.ChatID,
			Text:   "You follow: " + strings.Join(follows, ", "),
		}, nil
	default:
		return Reply{
			ChatID: update.ChatID,
			Text:   "Use /start to begin.",
		}, nil
	}
}
