package bot

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
)

// Telegram is the outbound half of the bot's Telegram I/O: it turns a Reply into a message
// on someone's phone. The inbound half (getUpdates) is a separate slice.
//
// It is a concrete type rather than an interface because nothing yet needs a second
// implementation — the tests drive it through an httptest.Server, which is a real HTTP
// server and so needs no fake. An interface arrives if and when a caller must be tested
// without one.
//
// ⚠️ The token is held separately from baseURL rather than being pasted into it by the
// caller. It is the only secret in this program, and keeping it in its own field means
// there is exactly one place it lives — so a log line that prints an endpoint cannot
// accidentally print the credential too. Telegram's URL scheme invites the opposite: it
// carries the token IN THE PATH, so a "base URL" with the token already in it looks
// perfectly natural right up until it appears in a stack trace.
type Telegram struct {
	baseURL string
	token   string
	client  *http.Client
	offset  int64
}

// NewTelegram returns a Telegram that talks to the Bot API at baseURL, authenticating as
// the bot that token belongs to.
//
// baseURL is "https://api.telegram.org" in production and an httptest.Server's URL under
// test — the same injectable-base-URL seam as NewResolver and NewVotesSource, and for the
// same reason: it lets a test assert the request that actually went out rather than trust
// that the URL was assembled correctly.
//
// token is the string BotFather issues. Anyone holding it can read every message sent to
// the bot and post as it, so it belongs in the environment, never in the source.
func NewTelegram(baseURL, token string) *Telegram {
	return &Telegram{
		baseURL: baseURL,
		token:   token,
		client:  &http.Client{},
	}
}

// SendMessage delivers text to the chat with the given ID, returning an error if Telegram
// did not accept it — either because the request could not be MADE (refused connection, DNS
// failure, timeout) or because Telegram answered with anything other than 200.
//
// ⚠️ Those are two separate checks, and the second is not optional. Go's http.Client
// returns a non-nil error only for the first kind: a response that ARRIVES is a success as
// far as PostForm is concerned, whatever its status line says. A 403 sails straight past
// `if err != nil` with err still nil. Hence the explicit StatusCode comparison below —
// which reads like belt and braces and is not.
//
// ⚠️ What it does NOT do is tell the failures apart, and they are not alike:
//
//	400 Bad Request       — malformed request, or a chat that does not exist
//	401 Unauthorized      — the token is wrong or has been revoked
//	403 Forbidden         — the user has BLOCKED the bot, or deleted the chat
//	429 Too Many Requests — rate limited; worth retrying, unlike the rest
//
// A caller gets one undifferentiated error and can only give up. 403 is the one that needs
// telling apart: it means this chat will never accept another message, so it should be
// PRUNED rather than retried forever — a compliance matter (docs/COMPLIANCE.md) as much as
// a hygiene one. That needs a sentinel error and errors.Is, and it is its own slice.
//
// ⚠️ Telegram caps a message at 4096 characters and rejects longer ones with a 400. Nothing
// here splits or truncates, and the /latest reply is already capable of exceeding it for a
// chat following enough MPs — a limit that was theoretical while replies went to stdout,
// which has no such cap. This method is what makes it real. Logged in docs/ISSUES.md.
//
// Note that chat_id is snake_case: it is a name on Telegram's wire, not a Go identifier,
// and their API is snake_case throughout. The Go parameter is chatID, as Go's own
// convention requires. The two conventions meet on exactly one line, below.
func (t *Telegram) SendMessage(chatID int64, text string) error {
	values := url.Values{}
	values.Set("chat_id", strconv.FormatInt(chatID, 10))
	values.Set("text", text)
	endpoint := t.baseURL + "/bot" + t.token + "/sendMessage"

	// POST rather than GET: a reply can run to 4096 characters, and a body has no length
	// limit where a URL does. Telegram accepts both.
	resp, err := t.client.PostForm(endpoint, values)
	if err != nil {
		return err
	}

	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("telegram API returned %s", resp.Status)
	}

	return nil
}

// GetUpdates fetches the messages waiting for the bot and returns them in the flat shape the
// rest of the package works in.
//
// This is where Telegram's vocabulary stops. What arrives is an envelope containing a list
// of updates, each wrapping a message, each holding a chat — four levels for two values.
// Everything downstream of here sees only Update{ChatID, Text}, which is why the nested
// structs below are declared inside this function: they describe a wire format, not a domain
// concept, and nothing else has any business knowing that "chat" and "message" are separate
// things to Telegram.
//
// Each call acknowledges the one before it. Telegram holds an update in the queue until a
// later call passes offset = update_id + 1, and keeps redelivering it until then — so without
// this the bot would answer every message it has ever received, on every single poll. The
// high-water mark is kept on the Telegram value rather than passed in by the caller: it is
// bookkeeping about a conversation with Telegram, and nothing outside this type has any use
// for it or any way to get it right.
//
// ⚠️ The offset only ever moves FORWARD, and only when a batch actually arrives. "Highest id
// seen plus one" computed over an EMPTY batch is 1, which would wind the offset back to the
// start of the queue and redeliver everything; the comparison in the loop below is what makes
// an empty batch a no-op instead.
//
// An empty queue is not an error: it is the normal outcome of most calls, and comes back as
// a nil slice with a nil error.
func (t *Telegram) GetUpdates() ([]Update, error) {
	values := url.Values{}
	values.Set("offset", strconv.FormatInt(t.offset, 10))

	endpoint := t.baseURL + "/bot" + t.token + "/getUpdates?" + values.Encode()

	resp, err := t.client.Get(endpoint)
	if err != nil {
		return nil, err
	}

	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("telegram API returned %s", resp.Status)
	}

	// Only the fields that are actually read are declared. encoding/json ignores the rest —
	// from, date, message_id and the dozen others Telegram sends — so the struct stays the size
	// of what the bot needs rather than the size of the document.
	//
	// ⚠️ update_id needs the struct tag and the others do not. encoding/json matches a name
	// exactly, then case-insensitively, which is why Chat/chat and Text/text bind with no tag —
	// but the underscore in update_id defeats that, and an untagged UpdateID would stay 0 with
	// NO error reported. Note also that it sits ALONGSIDE message on the wire, not inside it.
	var payload struct {
		Result []struct {
			UpdateID int64 `json:"update_id"`
			Message  struct {
				Chat struct {
					ID int64
				}
				Text string
			}
		}
	}

	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("get updates: %w", err)
	}

	// Reach through the nesting for the two values that matter and drop the container.
	var updates []Update
	for _, result := range payload.Result {
		if result.UpdateID >= t.offset {
			t.offset = result.UpdateID + 1
		}
		updates = append(updates, Update{
			ChatID: result.Message.Chat.ID,
			Text:   result.Message.Text,
		})
	}

	return updates, nil
}
