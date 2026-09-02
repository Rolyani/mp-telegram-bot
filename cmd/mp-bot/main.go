// Command mp-bot runs the bot against Telegram: it polls for messages, answers them, and
// sends the replies back, until it is stopped.
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
// ⚠️ It answers messages; it does not yet send anything unprompted. CheckActivity, which
// pushes an MP's new votes to the people following them, is not wired to this loop — that
// needs a baseline recorded at the moment someone follows an MP, or the first push would be
// the MP's entire back catalogue arriving at once.
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
