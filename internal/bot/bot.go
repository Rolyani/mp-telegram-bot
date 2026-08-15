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

	// Choices offers the user a set of ready-made replies, each holding the EXACT text that
	// sending it back would produce. Phase E renders them as a Telegram reply keyboard,
	// where a button's text is what tapping it sends — so a choice arrives as an ordinary
	// message and is handled like any other, and the bot never has to remember that it asked
	// a question. Keeping whole commands in here rather than bare labels is what buys that:
	// the user's choice travels in the user's own message, so there is no pending state to
	// store, expire, or cancel when they type something else instead.
	//
	// Empty for every reply that asks nothing.
	Choices []string
}

// MemoryStore remembers chat IDs
type MemoryStore struct {
	chats   map[int64]bool
	follows map[int64][]Member
	seen    map[int64]map[string]bool
}

// Activity is one item of an MP's parliamentary activity (a vote, question, or speech).
// ID uniquely identifies the item so it can be de-duplicated; Text is what subscribers see.
type Activity struct {
	ID   string
	Text string
}

// ActivitySource fetches the recent activity for one MP, identified by their Parliament
// member ID. Implementations may hit the Parliament APIs; tests supply an in-memory fake.
//
// It takes an ID rather than a name because a display name identifies nobody reliably:
// two sitting MPs can share one, and the API matches names by substring, so the
// ambiguity cannot be filtered away.
//
// It takes a bare ID rather than a whole Member so that a source has no name available
// to key on even by accident. If a reply should mention the MP, that wording belongs to
// CheckActivity, which is already holding the Member — the same split as elsewhere: the
// layer that fetches fetches, the layer with a user does the words.
type ActivitySource interface {
	Activity(memberID int) []Activity
}

// NameResolver looks up sitting MPs by name, returning every match so the caller can offer
// a choice. *Resolver satisfies this against the live Members API; tests supply an
// in-memory fake. It is declared here, next to the code that needs it rather than beside
// the code that implements it, so the resolver stays unaware of the bot entirely.
type NameResolver interface {
	ResolveName(name string) ([]Member, error)
}

// Bot holds the collaborators a command handler needs. Commands are answered by methods on
// Bot rather than free functions so that dependencies arrive as fields: /find needs a name
// resolver and /latest will need an activity source, and neither should become another
// parameter on every call site.
type Bot struct {
	store    *MemoryStore
	resolver NameResolver
	source   ActivitySource
}

// New returns a Bot that records subscriptions and follows in store, and looks MPs up
// through resolver.
//
// /find and /follow both consult the resolver, so a nil one panics for them — though only
// once a name is supplied, since both reject an empty name before making any request.
// Every other command works with a nil resolver, and the tests pass nil deliberately to
// record that they never reach the network.
func New(store *MemoryStore, resolver NameResolver, source ActivitySource) *Bot {
	return &Bot{store: store, resolver: resolver, source: source}
}

// NewMemoryStore returns a ready to use *MemoryStore
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		chats:   make(map[int64]bool),
		follows: make(map[int64][]Member),
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

// FollowMP records that chatID follows mp. The MP arrives already resolved: the store
// keeps identities, and deciding WHICH member a typed name meant belongs to the caller,
// which is the only layer with a user to ask.
func (s *MemoryStore) FollowMP(chatID int64, mp Member) {
	s.follows[chatID] = append(s.follows[chatID], mp)
}

// UnfollowMP removes the MP with the given member ID from chatID's follow list, leaving any
// others intact, and reports whether it was there to remove. Go has no built-in slice remove,
// so it filters in place: kept items are appended back over the same backing array.
//
// It takes an ID, not a name, for the same reason FollowMP takes an already-resolved Member:
// the store keeps identities, and working out WHICH member a typed name meant is the caller's
// job, because the caller is the only layer with a user it can ask. Matching here as well
// would mean the same decision being made in two places, and only one of them can ask.
func (s *MemoryStore) UnfollowMP(chatID int64, id int) bool {
	keep := s.follows[chatID][:0]
	removed := false
	for _, f := range s.follows[chatID] {
		if f.ID != id {
			keep = append(keep, f)
		} else {
			removed = true
		}
	}
	s.follows[chatID] = keep
	return removed
}

// Follows returns the MPs that chatID follows.
func (s *MemoryStore) Follows(chatID int64) []Member {
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
// item, addressed to each chat that follows that MP. An item already sent to a chat is
// not sent again, so each follower sees a given item exactly once.
func CheckActivity(source ActivitySource, store *MemoryStore) []Reply {
	chats := store.Chats()
	replies := make([]Reply, 0, len(chats))
	for _, id := range chats {
		follows := store.Follows(id)
		for _, mp := range follows {
			// Polled by ID, never by name. The name in a follow record is a snapshot
			// taken when the user followed and nothing keeps it in sync; the ID is the
			// only part of it Parliament will still recognise later.
			data := source.Activity(mp.ID)
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

// ForgetChat removes the chat's subscription and its entire follow list. It does not
// yet clear the per-chat already-sent set (s.seen) — that's a separate behavior.
func (s *MemoryStore) ForgetChat(chatID int64) {
	delete(s.follows, chatID)
	delete(s.chats, chatID)
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

// reply addresses text back to the chat that sent the command. Every branch of
// HandleUpdate answers the asking chat, so the pairing is worth naming once rather than
// spelling out seventeen times — it keeps each branch about what the bot SAYS.
func reply(chatID int64, text string) Reply {
	return Reply{ChatID: chatID, Text: text}
}

// apiDown is what every command says when the Members API cannot be reached. Named once
// because it is one statement about one outage: /find and /follow both make the same
// request and both fail the same way, and rewording it in only one of them would have the
// bot describe the same problem two different ways.
const apiDown = "Sorry, I couldn't connect to the Parliament API. Try again soon."

// noSuchMP is what every command says when the Members API answered perfectly well and the
// answer was nobody. Named once for the same reason as apiDown: /find and /follow report one
// fact here, and the plural in "MPs" describes what came BACK, not what was asked for, so it
// does not vary by command. Distinct from apiDown on purpose — "we couldn't ask" and "we
// asked and nobody matched" are different situations and the user can act on the difference.
const noSuchMP = "No MPs with that name found."

// memberNames projects members onto their display names, ready to be joined into a reply.
// A projection cannot filter in place the way UnfollowMP does — []Member and []string have
// different layouts — so this allocates a fresh slice.
func memberNames(members []Member) []string {
	names := make([]string, 0, len(members))
	for _, m := range members {
		names = append(names, m.Name)
	}
	return names
}

// matchingFollows returns the MPs in follows whose display name contains fragment.
//
// Unfollowing searches the chat's OWN list and never the Members API. A follow list is local
// data, and going to the network to interpret it would mean an outage could stop you cancelling
// a subscription — worse, the resolver only finds SITTING members, so an MP who stood down
// would stop resolving and the follow could never be removed at all.
//
// Substring, to mirror what B3's probing found the real API does, so "Smith" picks out the same
// people whether the user is following or unfollowing.
func matchingFollows(follows []Member, fragment string) []Member {
	var matches []Member
	for _, f := range follows {
		if strings.Contains(f.Name, fragment) {
			matches = append(matches, f)
		}
	}
	return matches
}

// HandleUpdate processes an update and returns a reply.
//
// Note the return values are NOT the usual either/or: a non-nil error still carries a
// reply that should be sent. A lookup that fails has two audiences — the user, who needs
// a sentence rather than silence, and the caller, which needs the failure to log and
// retry on — and both are served in one call. Callers must therefore send the Reply
// before deciding what to do about the error, not return early on it.
func (b *Bot) HandleUpdate(update Update) (Reply, error) {
	cmd, arg, _ := strings.Cut(update.Text, " ")
	switch cmd {
	case "/start":
		b.store.AddChat(update.ChatID)
		return reply(update.ChatID, "Welcome! Send /start to get going."), nil
	case "/stop":
		b.store.RemoveChat(update.ChatID)
		return reply(update.ChatID, "Your details have been removed."), nil
	case "/find":
		name := strings.TrimSpace(arg)
		if name == "" {
			return reply(update.ChatID, "Please enter an MP's name to search for."), nil
		}
		members, err := b.resolver.ResolveName(name)
		if err != nil {
			// Both values are returned deliberately: the user is told the lookup failed,
			// and the error still reaches the caller. Do not "tidy" this to a bare error —
			// the failure is an outage of a service we do not control, and answering the
			// user with silence is never right. See HandleUpdate's doc comment.
			return reply(update.ChatID, apiDown), err
		}
		// Nobody matching is a successful answer, not a failure: the resolver reserves
		// errors for requests that genuinely failed. So it is answered here, in words,
		// rather than returned as an error the user would never see.
		if len(members) == 0 {
			return reply(update.ChatID, noSuchMP), nil
		}
		return reply(update.ChatID, "Found: "+strings.Join(memberNames(members), ", ")), nil
	case "/follow":
		name := strings.TrimSpace(arg)
		if name == "" {
			return reply(update.ChatID, "Please enter an MP's name to follow."), nil
		}
		members, err := b.resolver.ResolveName(name)
		if err != nil {
			// Reply AND error, as /find does: an outage the user caused nothing of still
			// owes them a sentence. See HandleUpdate's doc comment.
			return reply(update.ChatID, apiDown), err
		}
		// Answered in words and with a nil error, for the same reason /find is above: the
		// resolver reports "nobody is called that" as an empty slice, not a failure.
		if len(members) == 0 {
			return reply(update.ChatID, noSuchMP), nil
		}
		// A name several MPs share commits to nothing. Substring matching means "Smith" finds
		// eleven sitting members, and picking one silently would leave the user following an
		// MP they never asked for with no way to notice. Each choice is the command that
		// follows one of them, so choosing costs a tap and needs no state — see Reply.Choices.
		if len(members) > 1 {
			r := reply(update.ChatID, "Several MPs match that name. Which one did you mean?")
			for _, m := range members {
				r.Choices = append(r.Choices, "/follow "+m.Name)
			}
			return r, nil
		}
		mp := members[0]
		b.store.FollowMP(update.ChatID, mp)
		return reply(update.ChatID, "Now following "+mp.Name+"."), nil
	case "/unfollow":
		name := strings.TrimSpace(arg)
		if name == "" {
			return reply(update.ChatID, "Please enter an MP's name to unfollow."), nil
		}
		matches := matchingFollows(b.store.Follows(update.ChatID), name)
		if len(matches) == 0 {
			return reply(update.ChatID, "You were not following this MP."), nil
		}
		// Several of this chat's follows share the fragment, so removing one would be a
		// guess — the same argument as /follow above, answered the same stateless way. Each
		// choice is the command that unfollows exactly one of them; see Reply.Choices.
		//
		// KNOWN DEBT: following the same MP twice stores them twice, so two identical choices
		// would be offered and neither could ever resolve. Dedup on FollowMP is its own slice.
		if len(matches) > 1 {
			r := reply(update.ChatID, "Several MPs you follow match that name. Which one did you mean?")
			for _, m := range matches {
				r.Choices = append(r.Choices, "/unfollow "+m.Name)
			}
			return r, nil
		}
		// The match came out of this chat's own list, so removal cannot fail and the bool
		// UnfollowMP returns has nothing left to tell us. The confirmation names the MP
		// actually removed rather than what was typed: "Smith" is not who you unfollowed.
		mp := matches[0]
		b.store.UnfollowMP(update.ChatID, mp.ID)
		return reply(update.ChatID, "You have unfollowed "+mp.Name+"."), nil
	case "/list":
		follows := b.store.Follows(update.ChatID)
		if len(follows) == 0 {
			return reply(update.ChatID, "You are not following any MPs yet."), nil
		}
		// The store carries each MP's name alongside their ID precisely so this needs no
		// lookup: /list stays offline, and cannot fail because the Members API is down.
		return reply(update.ChatID, "You follow: "+strings.Join(memberNames(follows), ", ")), nil
	case "/latest":
		// Fetched live rather than read from the store: unlike /list, this command cannot
		// answer offline, because the whole point of it is what Parliament says right now.
		follows := b.store.Follows(update.ChatID)
		var texts []string
		for _, mp := range follows {
			activity := b.source.Activity(mp.ID)
			for _, a := range activity {
				// Attribution is built HERE because this is the only point in the function
				// where both halves are in scope at once — which MP, and which item. One
				// line later mp is gone, and nothing downstream can reconstruct whose item
				// this was. It is also why ActivitySource takes a bare ID: the name never
				// has to make the round trip.
				texts = append(texts, mp.Name+": "+a.Text)
			}
		}
		// Deliberate debt, its own slice: a chat that follows nobody gets an empty reply
		// here — the dangling-string shape slice 11 fixed for /list and B6 for /find.
		// Reading nothing is NOT recorded as sent; see the ratchet in this slice's test.
		return reply(update.ChatID, strings.Join(texts, "\n")), nil
	case "/help":
		return reply(update.ChatID,
			"Follow MPs by typing their name or the post code into /follow.\n"+
				"/start will look for the last activity for the followed MPs.\n"+
				"/forgetme will wipe all of your data."), nil
	case "/forgetme":
		b.store.ForgetChat(update.ChatID)
		return reply(update.ChatID, "Your follows and account have been removed."), nil
	case "/privacy":
		return reply(update.ChatID,
			"This bot only stores the MPs you select. No other personal information is collected.\n"+
				"The /forgetme command will wipe all of the data stored for your ID"), nil
	case "/source":
		return reply(update.ChatID, "Full code available at: https://github.com/Rolyani/mp-telegram-bot"), nil
	default:
		return reply(update.ChatID, "Use /start to begin."), nil
	}
}
