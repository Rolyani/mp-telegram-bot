// Command mp-bot runs the bot against Telegram until it is stopped. It does two things on two
// clocks: it polls for messages and answers them, and it pushes MPs' new votes to the people
// following them.
//
// It needs TELEGRAM_TOKEN in the environment and reads nothing else. Run it with
//
//	set -a; source .env; set +a
//	go run ./cmd/mp-bot
//
// The stdin mode this file used to hold — a line typed at the terminal treated as an incoming
// message — was deleted once the Telegram loop worked. It existed to make the package runnable
// at all when nothing else could execute it, and had no caller left.
//
// ⚠️ The two cycles run at deliberately different rates. Answering a message must feel
// immediate, so pollOnce runs every couple of seconds. Asking Parliament what an MP has been
// voting on has no such need and a real cost — it is somebody else's API, divisions happen a
// handful of times on a sitting day and never overnight — so pushOnce runs hourly. They are
// separate functions rather than one, because folding the push into the poll would weld the
// polite rate to the responsive one.
//
// ⚠️ The push is only safe because /follow records a baseline. Without it the first push would
// deliver the MP's entire back catalogue, one message per division, to a phone.
package main

import (
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/Rolyani/mp-telegram-bot/internal/bot"
)

const (
	pollEvery = 2 * time.Second
	pushEvery = time.Hour
)

// main wires the program up and owns the loop, and is deliberately the one function no test
// covers. It takes no arguments and returns nothing, so there is no seam to push a fixture
// through — which is exactly why everything with a decision in it lives in telegramFromEnv
// and pollOnce instead, both of which are tested.
//
// ⚠️ Note the asymmetry between the two failures, which is the only judgement here. A missing
// token EXITS: it cannot fix itself, and a bot that runs on unauthenticated 401s looks like a
// network fault for as long as it takes someone to check. A failed cycle PRINTS AND CARRIES
// ON: Parliament being unreachable for thirty seconds must not take the bot down until a human
// notices.
func main() {
	tg, err := telegramFromEnv()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	b := bot.New(bot.NewMemoryStore(), bot.NewResolver("https://members-api.parliament.uk"), bot.NewVotesSource("https://commonsvotes-api.parliament.uk"))

	polls := time.NewTicker(pollEvery)
	pushes := time.NewTicker(pushEvery)

	for {
		select {
		case <-polls.C:
			if err := pollOnce(b, tg); err != nil {
				fmt.Fprintln(os.Stderr, err)
			}
		case <-pushes.C:
			if err := pushOnce(b, tg); err != nil {
				fmt.Fprintln(os.Stderr, err)
			}
		}
	}
}

// pollOnce runs one poll cycle: it fetches whatever Telegram is holding, answers each message,
// and sends every reply back. It returns when the batch is done, so the caller owns the pacing.
//
// One cycle rather than a loop, deliberately. A loop here would never return, which makes it
// untestable except by building a stop condition into production code purely so a test can end
// it. Everything worth getting wrong is in the cycle; the loop around it is a `for` and a sleep.
//
// ⚠️ A failing update does not abandon the batch. One MP's lookup failing during a busy poll
// must not cost the other subscribers their replies, so every failure is collected and the
// cycle carries on — errors.Join returns nil when nothing failed, so the happy path needs no
// special case. A failure to REACH Telegram at all is different and returns immediately: there
// is no point walking a batch that cannot be answered.
//
// ⚠️ The reply is sent before HandleUpdate's error is recorded, and that order is required
// rather than tidy. Its two return values are not either/or: a lookup that failed still carries
// the sentence explaining so, and returning early would answer that user with silence. See the
// doc comment on HandleUpdate.
func pollOnce(b *bot.Bot, tg *bot.Telegram) error {
	updates, err := tg.GetUpdates()
	if err != nil {
		return err
	}

	var failures []error
	for _, update := range updates {
		reply, err := b.HandleUpdate(update)
		if sendErr := tg.SendMessage(reply.ChatID, reply.Text); sendErr != nil {
			failures = append(failures, sendErr)
		}
		if err != nil {
			failures = append(failures, err)
		}
	}

	return errors.Join(failures...)
}

// pushOnce runs one push cycle: it asks the bot what its followers have not been told yet and
// sends each of those messages. It is the first thing the bot does without being spoken to,
// and it is the point of the product — request/response is how you configure it, the push is
// what it is FOR.
//
// One cycle rather than a loop, for the same reason as pollOnce: the pacing belongs to the
// caller, and a function that never returns cannot be tested.
//
// ⚠️ It must not call GetUpdates. That would acknowledge the messages in the batch it fetched
// without answering any of them — Telegram moves the offset past everything it hands over, so
// those commands would be dropped, unanswered and unrecoverable.
//
// ⚠️ One failed send does not abandon the batch, as in pollOnce, and it matters more here. A
// push fans one division out to everyone following that MP, so a single blocked chat — 403,
// the commonest failure there is — would otherwise silence that division for every other
// subscriber in the same cycle.
//
// ⚠️ The items are marked as sent by CheckActivity while it BUILDS the replies, before this
// function has sent anything. A send that fails therefore loses that division rather than
// retrying it: harmless for a chat that has blocked the bot, a dropped notification for a
// timeout. Logged as issue 20; the fix is to confirm sends back to the store, which is a shape
// change best made when the store moves to Postgres.
func pushOnce(b *bot.Bot, tg *bot.Telegram) error {
	replies := b.CheckActivity()

	var failures []error
	for _, reply := range replies {
		if sendErr := tg.SendMessage(reply.ChatID, reply.Text); sendErr != nil {
			failures = append(failures, sendErr)
		}
	}

	return errors.Join(failures...)
}

func telegramFromEnv() (*bot.Telegram, error) {
	token := os.Getenv("TELEGRAM_TOKEN")
	if token == "" {
		return nil, errors.New("TELEGRAM_TOKEN is not set")
	} else {
		return bot.NewTelegram("https://api.telegram.org", token), nil
	}
}
