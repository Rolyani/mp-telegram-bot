# mp-telegram-bot

A Telegram bot that lets anyone follow a UK Member of Parliament and get pushed a
message whenever that MP does something in Parliament — votes in a division, tables a
written question, or speaks in the chamber. Written in **Go**.

> Contains Parliamentary information licensed under the Open Parliament Licence v3.0.

## What it does

- A user finds the bot on Telegram, taps **Start**, and picks an MP by postcode or name.
- The bot stores which MP(s) that user follows.
- A background loop polls UK Parliament every ~15 minutes and, when a followed MP has new
  activity, messages everyone following that MP.
- The first time an MP is followed, the bot records their current activity **silently**,
  so a new follower isn't blasted with a backlog — only genuinely new items get pushed.

There is no allow-list: any Telegram user who finds the bot can use it, and each user's
subscriptions are private to their own chat.

## Why Go

This is an I/O-bound service (HTTP polling + a small database), so raw performance is
irrelevant at any realistic scale. Go is chosen for the **operations** story rather than
speed:

- Compiles to a single static binary — trivial to put in a small container image and run
  as a long-lived daemon.
- Small memory footprint (comfortably well under 100 MB).
- Goroutines make concurrent polling of many MPs natural, so it ages well if it grows to
  many followed MPs/users.

The one area that needs care in Go: the Parliament APIs are three separate services with
**inconsistent JSON field casing** (PascalCase in one, camelCase in another). The parsing
layer must be deliberately tolerant — per-feed structs with explicit JSON tags, and one
feed being down or changing shape must not crash the others.

## Data sources (UK Parliament, key-less, open)

All are open JSON REST APIs under the Open Parliament Licence — no API key required:

| Purpose | API |
| --- | --- |
| Look up an MP by postcode or name | Members API — `members-api.parliament.uk` |
| How they voted | Commons Votes API |
| Written questions they tabled | Written Questions API |
| What they said in the chamber | Hansard |

`fetch_votes` / `fetch_written_questions` / `fetch_contributions` (or their Go
equivalents) are the isolated places to adjust query-parameter names if a feed changes.

## User experience

A Telegram bot is live the moment it's created — no app-store listing. Users reach it via
its link `https://t.me/<BotName>`, a QR code, or exact-username search. Discovery is driven
by sharing the link, not by in-app search. On open they see the profile picture, about
line, and description, then tap **Start** (which is the opt-in and the first time the bot
sees their chat).

### Commands

| Command | Description |
| --- | --- |
| `/start` | Opt in; bot records the chat |
| `/find` | Find an MP by postcode or name |
| `/list` | See who you follow |
| `/latest` | Show recent activity now |
| `/privacy` | What we store and why (UK GDPR transparency) |
| `/forgetme` | Delete all your data (and stop) |
| `/help` | How the bot works |

## Architecture & operations

- **Telegram integration:** long-polling (`getUpdates`), so the bot makes **outbound
  connections only** — no public IP, inbound ports, domain, or TLS cert needed.
- **Persistence:** PostgreSQL via CloudNativePG (CNPG) in a Kubernetes homelab. Stores
  only each `chat_id` → followed MP(s), plus which activity items have already been sent.
- **Deployment:** GitOps with Flux from a **public** repo. Bot token is kept out of git
  via a SOPS-encrypted Secret; CNPG credentials live entirely in-cluster.

Two critical deployment gotchas follow from the design: **run exactly one replica**
(Telegram allows only one active `getUpdates` poll per token — a second gets
`409 Conflict`), and use the **`Recreate`** update strategy so rollouts don't briefly run
two pollers.

## Legal / compliance

The bot is a UK GDPR **data controller** (a Telegram `chat_id` is personal data; which MP
someone follows is arguably special-category political data). The compliance basics are
mostly good engineering: consent as the lawful basis, a `/privacy` notice, a `/forgetme`
deletion command, data minimisation, and pruning users who block the bot.

The **ICO fee** is not owed for this project (non-commercial, no income — confirmed via the
ICO self-assessment). That status is fragile: **affiliate links** or paid promotion on an
associated blog would trigger the fee. An unpaid, genuine "I like tool X" mention or a
plain "buy me a coffee" tip jar does not.

*Not legal advice.*

## Licence

**GNU AGPLv3** — see [LICENSE](LICENSE). The bot is a hosted network service and the
project's ethos is public-interest transparency, so AGPL's requirement that hosted forks
also make their source available to users (closing the GPL "run it as a service without
distributing" gap) is thematically aligned.

The relayed Parliament data remains under the **Open Parliament Licence v3.0**
(attribution), independent of the code licence — keep the attribution line ("Contains
Parliamentary information licensed under the Open Parliament Licence v3.0") in the bot's
output/docs.

Per the AGPL's own recommendation for network services, the bot should provide a way for
users to get its source (e.g. a `/source` command or a link in `/help` pointing to this
repo).

## Status

Early. Go module initialised (`github.com/Rolyani/mp-telegram-bot`, Go 1.26). No
application code yet — development will proceed test-first, in vertical slices.
