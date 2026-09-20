// Command mp-bot runs the bot against Telegram until it is stopped. It does two things on two
// clocks: it polls for messages and answers them, and it pushes MPs' new votes to the people
// following them.
//
// It needs TELEGRAM_TOKEN and DATABASE_URL in the environment and reads nothing else. Neither
// has a default and neither is optional: the bot exits naming whichever is missing rather than
// starting in a state that looks healthy. Run it with
//
//	set -a; source .env; set +a
//	go run ./cmd/mp-bot
//
// The stdin mode this file used to hold — a line typed at the terminal treated as an incoming
// message — was deleted once the Telegram loop worked. It existed to make the package runnable
// at all when nothing else could execute it, and had no caller left.
//
// ⚠️ The two cycles run at deliberately different rates, and since the long poll landed they
// also run on SEPARATE GOROUTINES. Answering a message must feel immediate, so pollOnce is run
// back to back forever and paced by Telegram itself — a getUpdates call now asks Telegram to
// hold the connection open for 25 seconds and answer the moment something arrives, so the poll
// loop spends nearly all its time blocked and a message is picked up the instant it is sent.
// Asking Parliament what an MP has been voting on has no such need and a real cost — it is
// somebody else's API, divisions happen a handful of times on a sitting day and never
// overnight — so pushOnce runs hourly. They are separate functions rather than one, because
// folding the push into the poll would weld the polite rate to the responsive one.
//
// ⚠️ They were one goroutine and a select until the long poll made that untenable. A pollOnce
// that blocks for 25 seconds inside a shared select delays every push by up to that long, and
// the two clocks stop being independent — the exact coupling the paragraph above says must not
// happen. Splitting them restores it, and costs a concurrency argument that is set out on main.
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
	// pollErrorBackoff is what stands between this program and a hot loop.
	//
	// ⚠️ The poll loop has no ticker any more: it calls pollOnce back to back and lets the 25s
	// long poll do the pacing. That works perfectly while Telegram is ANSWERING. The moment it
	// is not — DNS down, a revoked token returning 401, a 502 from a load balancer — GetUpdates
	// returns instantly, and a bare `for { pollOnce() }` becomes a loop that hammers the API as
	// fast as the CPU allows and writes the same error to stderr thousands of times a second.
	// The 2s ticker this replaced was quietly preventing that; nothing else does, so the sleep
	// below is load-bearing rather than polite.
	pollErrorBackoff = 5 * time.Second

	pushEvery = time.Hour
)

// main wires the program up and owns the loop, and is deliberately the one function no test
// covers. It takes no arguments and returns nothing, so there is no seam to push a fixture
// through — which is exactly why everything with a decision in it lives in telegramFromEnv
// storeFromEnv and pollOnce instead, all of which are tested.
//
// ⚠️ There are now TWO exits before the loop, and the store is the one that matters. Starting
// without a database would mean accepting follows into memory and losing them on the next
// restart — see storeFromEnv.
//
// ⚠️ Note the asymmetry between the two failures, which is the only judgement here. A missing
// token EXITS: it cannot fix itself, and a bot that runs on unauthenticated 401s looks like a
// network fault for as long as it takes someone to check. A failed cycle PRINTS AND CARRIES
// ON: Parliament being unreachable for thirty seconds must not take the bot down until a human
// notices.
//
// ⭐ WHY TWO GOROUTINES ARE SAFE HERE, since nothing in the types says so and the next person to
// touch this will be right to ask. Three things are shared between the loops, and each is fine
// for a different reason:
//
//	*bot.Bot        — immutable after New: three interface fields, never reassigned, no state.
//	*bot.Telegram   — its client is an *http.Client, which is built for concurrent use; its one
//	                  mutable field, the getUpdates offset, is touched ONLY by GetUpdates, and
//	                  GetUpdates is called only from the poll loop. The push loop sends and
//	                  never polls, which is already a rule for a different reason (see pushOnce).
//	*bot.PostgresStore — a pgxpool, which exists to be shared across goroutines.
//
// ⚠️ MemoryStore is NOT safe this way — it is bare maps, and two loops writing to it would race.
// It is unreachable from here only because storeFromEnv refuses to fall back to it, which makes
// that refusal load-bearing for concurrency as well as for data loss.
//
// ⚠️ And what got WORSE, which is the price of the isolation: a hang in one loop is now quieter
// than it was, not louder. Before, either failure stopped everything and the silence was total.
// Now the bot can answer messages perfectly while pushes have silently stopped for days, or the
// reverse. Nothing watches for that yet.
func main() {
	tg, err := telegramFromEnv()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	store, err := storeFromEnv()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	b := bot.New(store, bot.NewResolver("https://members-api.parliament.uk"), bot.NewVotesSource("https://commonsvotes-api.parliament.uk"))

	// The poll loop, on its own goroutine, with no ticker: pollOnce blocks inside the 25s long
	// poll until Telegram has something to say, so calling it back to back IS the pacing.
	go func() {
		for {
			if err := pollOnce(b, tg); err != nil {
				fmt.Fprintln(os.Stderr, err)
				// ⚠️ Only on the error path, and it is not cosmetic. A failing GetUpdates
				// returns immediately, so without this the loop spins. See pollErrorBackoff.
				time.Sleep(pollErrorBackoff)
			}
		}
	}()

	// The push loop stays on the main goroutine, and that is deliberate: main returning ends
	// the process and takes every goroutine with it, so ONE of the two loops has to block here.
	// The hourly one is the better choice — it is the cheaper loop to own, and a poll loop that
	// blocks in a goroutine reads more naturally than a push that does.
	for range time.Tick(pushEvery) {
		if err := pushOnce(b, tg); err != nil {
			fmt.Fprintln(os.Stderr, err)
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
	// ⚠️ Returned immediately rather than collected into failures below. A store that cannot be
	// read has not told us there is nothing to send — it has told us nothing at all, and the
	// difference matters: carrying on would report a successful cycle that delivered nothing.
	replies, err := b.CheckActivity()
	if err != nil {
		return err
	}

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

// storeFromEnv opens the Postgres store named by DATABASE_URL, or refuses.
//
// ⚠️ There is deliberately NO fallback to MemoryStore. It is the tempting branch to write — no
// DSN, so run in memory — and it is the worst option available: the bot starts, answers
// commands, accepts follows, looks entirely healthy, and loses every follow for every user on
// the first restart. In a cluster where a restart is routine (every Flux rollout, every node
// reboot) that is not a degraded mode, it is silent data loss wearing a green tick. Refusing by
// name costs one line of output and loses nothing.
//
// ⚠️ Two failures, not one. An unset variable is caught here; an unreachable or misconfigured
// database is caught by NewPostgresStore, which pings before returning. Both have to happen
// before main builds anything, which is why this is a separate function rather than a line
// inside main — a function can be tested, and main cannot.
func storeFromEnv() (*bot.PostgresStore, error) {
	db := os.Getenv("DATABASE_URL")
	if db == "" {
		return nil, errors.New("DATABASE_URL is not set")
	}
	store, err := bot.NewPostgresStore(db)
	if err != nil {
		return nil, fmt.Errorf("connect to postgres: %w", err)
	}

	return store, nil

}
