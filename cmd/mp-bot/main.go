// Command mp-bot runs the bot against a terminal: a line typed on stdin is treated as an
// incoming message, and the reply is printed on stdout. There is no Telegram here yet, and
// no poll loop — this exists so the code can be run at all, having been a tested library
// with no way to execute it.
package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/Rolyani/mp-telegram-bot/internal/bot"
)

// main is deliberately three lines long. It cannot be tested — the test binary supplies its
// own main, and main takes no arguments and returns nothing, so there is no seam to push a
// fixture through. Everything worth getting wrong therefore lives in run, which takes its
// input and output as interfaces and so can be driven by a strings.Reader and a
// bytes.Buffer just as well as by the terminal.
func main() {
	if err := run(os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// run reads one message per line from in and writes each reply to out, stopping at end of
// input or at the first failure.
func run(in io.Reader, out io.Writer) error {
	// The resolver is real: /find and /follow hit the live Members API from here. The
	// activity source is still nil, which is a statement about how far this entrypoint has
	// got rather than an oversight — but note it is no longer harmless. /latest calls
	// through it the moment a chat follows anybody, so following an MP and asking for
	// /latest panics until E0b's second half lands.
	b := bot.New(bot.NewMemoryStore(), bot.NewResolver("https://members-api.parliament.uk"), bot.NewVotesSource("https://commonsvotes-api.parliament.uk"))

	scanner := bufio.NewScanner(in)
	for scanner.Scan() {
		// A terminal session is exactly one conversation, so the chat ID is a constant
		// rather than something to invent. It matters only once several chats share a
		// process, which is Telegram's problem and not stdin's.
		update := bot.Update{ChatID: 0, Text: scanner.Text()}

		// The reply is printed before the error is acted on, and that order is required:
		// HandleUpdate's two return values are not either/or, and a failed lookup still
		// carries a sentence the user is owed. Returning early here would answer them with
		// silence. See the doc comment on HandleUpdate.
		resp, err := b.HandleUpdate(update)
		fmt.Fprintln(out, resp.Text)
		if err != nil {
			return err
		}
	}

	// Scan returns false at clean end of input and on a read error alike, so the two are
	// told apart afterwards rather than inside the loop. A nil here means the input simply
	// ran out.
	return scanner.Err()
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
