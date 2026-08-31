package bot

import (
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

// SendMessage delivers text to the chat with the given ID.
//
// ⚠️ It reports an error only if the request could not be MADE — a refused connection, a
// DNS failure, a timeout. Any response Telegram actually sends is treated as success,
// including the ones that mean the message was not delivered:
//
//	400 Bad Request — malformed request, or a chat that does not exist
//	403 Forbidden   — the user has blocked the bot, or deleted the chat
//	429 Too Many Requests — rate limited; the reply is simply lost
//
// So a message can vanish silently and this returns nil. That is the next slice, and 403
// is the one with real consequences: it means the chat should be PRUNED rather than
// retried forever, which is a compliance matter as much as a hygiene one.
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

	return nil
}
