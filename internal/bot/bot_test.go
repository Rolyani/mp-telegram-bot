package bot_test

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/Rolyani/mp-telegram-bot/internal/bot"
)

// fakeSource is an in-memory ActivitySource: canned activity items keyed by MP, so the
// poll slice stays offline (real HTTP feeds arrive in Phase C).
type fakeSource struct {
	items map[string][]bot.Activity
}

func (f fakeSource) Activity(mp string) []bot.Activity {
	return f.items[mp]
}

// fakeResolver is an in-memory name resolver: canned matches keyed by the query. Handler
// tests use it instead of a *Resolver + httptest.Server so they never encode Parliament's
// wire format — the resolver's own tests already own that, and a handler test that decodes
// JSON would break whenever the API's shape changed for reasons /find does not care about.
// A query with no canned entry yields no matches, which is what the real API does too.
//
// It also records every query it is asked for, so a test can assert the resolver was NOT
// consulted — "no request was made" is a real, observable behavior, and the only way to
// pin that a guard runs BEFORE the network call rather than after it. Recording needs a
// POINTER receiver: a value receiver gets a copy, so the append would be written to the
// copy and thrown away, leaving calls empty and the assertion passing vacuously.
// A non-nil err makes every lookup fail with it, standing in for an unreachable or
// misbehaving Members API. It returns no members alongside it, exactly as the real
// resolver does on all three of its failure paths — so no test can quietly depend on
// half an answer riding along with a failure.
type fakeResolver struct {
	matches map[string][]bot.Member
	calls   []string
	err     error
}

func (f *fakeResolver) ResolveName(name string) ([]bot.Member, error) {
	f.calls = append(f.calls, name)
	if f.err != nil {
		return nil, f.err
	}
	return f.matches[name], nil
}

// knownMPs builds a resolver that recognises each of names, each resolving to exactly one
// member with a distinct, stable ID.
//
// Since /follow started resolving names (slice C0), every test that uses /follow merely as
// SETUP needs a resolver that knows the names it types — while caring only that the follow
// landed, not which ID it landed under. This keeps that scaffolding to a single line and
// out of the assertions. Tests that are ABOUT resolution build their fakeResolver directly,
// so the IDs and match counts stay visible where they matter.
//
// A name it was not given still resolves to no matches, exactly as the real API does.
func knownMPs(names ...string) *fakeResolver {
	matches := make(map[string][]bot.Member, len(names))
	for i, name := range names {
		matches[name] = []bot.Member{{ID: 100 + i, Name: name}}
	}
	return &fakeResolver{matches: matches}
}

// Slice B1 (Phase B, the HTTP seam): given the Members API's search JSON for a name, the
// resolver returns that MP's durable member ID. This is the first network slice, and the
// behavior forces the whole seam into existence: a Resolver with an INJECTABLE base URL
// (pointed at an httptest.Server here, the real members-api.parliament.uk in production),
// a real *http.Client making the request, and tolerant JSON parsing of the nested shape
// the API actually returns — the id lives at items[].value.id, so this drives explicit
// json:"" tags on per-level structs. Seam decision (httptest over a mocked client) is
// documented in docs/ARCHITECTURE.md. We assert BOTH facets of "resolve BY name": the
// server was queried with the name (proving URL/query construction, the reason we chose
// httptest — a faked client would let a wrong URL pass), and the parsed id comes back.
func TestResolver_ResolveName_returnsMemberID(t *testing.T) {
	// Fake Members API: capture the outgoing Name query, then return canned search JSON
	// in the real API's nested shape. The handler runs during ResolveName's blocking HTTP
	// call and completes before it returns, so reading gotName afterwards is safe.
	var gotName string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotName = r.URL.Query().Get("Name")
		fmt.Fprint(w, `{"items":[{"value":{"id":4514,"nameDisplayAs":"Keir Starmer"}}]}`)
	}))
	defer srv.Close()

	resolver := bot.NewResolver(srv.URL)
	members, err := resolver.ResolveName("Keir Starmer")
	if err != nil {
		t.Fatalf("ResolveName returned error: %v", err)
	}

	// Queried by name — proves the request was built from the argument, not a constant.
	if gotName != "Keir Starmer" {
		t.Errorf("resolver queried Name=%q, want %q", gotName, "Keir Starmer")
	}
	// One match in, one member out.
	if len(members) != 1 {
		t.Fatalf("ResolveName returned %d members, want 1", len(members))
	}
	// Parsed the member ID out of the nested items[].value.id shape.
	if members[0].ID != 4514 {
		t.Errorf("member ID = %d, want 4514", members[0].ID)
	}
}

// Slice B1b (Phase B, the HTTP seam — the sad path B1 deliberately deferred): a name that
// matches no MP comes back from the real Members API as a well-formed 200 with an EMPTY
// items array. The original guarantee stands unchanged — the resolver must not reach for
// items[0] and panic, which in the poll loop would take the whole bot down over one typo'd
// /follow. What changed in B4 is how "none" is REPORTED: now that the resolver returns a
// slice, zero matches is simply an empty one, not an error. A well-formed 200 saying "nobody
// is called that" is a successful answer to a reasonable question, so errors are reserved for
// requests that genuinely failed (transport, non-200, unparseable body). The user-facing
// "no MP found" wording moves up to the caller, which is where phrasing belongs.
func TestResolver_ResolveName_unknownName_returnsNoMembersNotError(t *testing.T) {
	// Fake Members API answering the way it really does for a no-match search: 200 OK,
	// valid JSON, zero items. Nothing is malformed — the emptiness IS the response.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"items":[]}`)
	}))
	defer srv.Close()

	resolver := bot.NewResolver(srv.URL)
	members, err := resolver.ResolveName("Nobody McNobody")

	// A successful request that found nobody is not a failure.
	if err != nil {
		t.Fatalf("ResolveName of an unknown name returned error %v, want nil", err)
	}
	// The original point of the slice: no members, and critically no panic on items[0].
	if len(members) != 0 {
		t.Errorf("ResolveName returned %d members, want 0", len(members))
	}
}

// Slice B2 (Phase B, the HTTP seam — transport failures): when the Members API is down or
// rate-limiting us it answers with a non-200 and, typically, an HTML error page rather than
// JSON. Today the resolver ignores resp.StatusCode entirely and feeds that HTML straight to
// the JSON decoder, so an outage surfaces as `invalid character '<' looking for beginning of
// value` — an error that describes our parser's confusion instead of the actual fault. The
// status IS the diagnosis and it must reach the caller, so the error has to name it.
//
// Note this slice cannot assert merely "err != nil": the decode already fails today. What is
// missing is a USEFUL error, so the assertion pins the actionable part — the status code —
// while leaving the surrounding wording free.
func TestResolver_ResolveName_apiReturnsNon200_errorNamesTheStatus(t *testing.T) {
	// Fake Members API mid-outage: a 500 whose body is an HTML error page, exactly the shape
	// that makes the current failure so baffling.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `<html><body>500 Internal Server Error</body></html>`)
	}))
	defer srv.Close()

	resolver := bot.NewResolver(srv.URL)
	members, err := resolver.ResolveName("Keir Starmer")

	if err == nil {
		t.Fatalf("ResolveName against a 500 returned no error (%d members), want an error", len(members))
	}
	// The diagnosis must be in the message: someone reading a log needs to see that the API
	// rejected us, not that some JSON looked odd.
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("ResolveName error = %q, want it to mention the 500 status", err)
	}
	// No half-answer rides along with a failure — this is the distinction B1b now rests on,
	// so a failed request must be empty, not merely "probably empty".
	if len(members) != 0 {
		t.Errorf("ResolveName returned %d members alongside error %v, want 0", len(members), err)
	}
}

// Slice B3 (Phase B, the HTTP seam — asking the right question): the Members API searches
// BOTH houses and ALL time, so an unfiltered ?Name= search returns peers and long-dead
// members alongside sitting MPs. Probing the live API for "Smith" returns 52 matches, led by
// Lord Booth-Smith (a peer) and Alick Buchanan-Smith (an MP who died in 1991). This bot
// follows PARLIAMENTARY ACTIVITY, which neither of those can ever produce, so resolving to
// one silently subscribes a user to permanent silence. Constraining the search to sitting
// Commons members is therefore part of asking the question correctly, not an optimisation —
// and it cuts "Smith" from 52 matches to 11 before the ambiguity slice has to deal with it.
//
// House=1 is the Commons (2 is the Lords). Both parameter names are case-sensitive.
func TestResolver_ResolveName_queriesOnlySittingCommonsMembers(t *testing.T) {
	// Fake Members API capturing the whole outgoing query, so the test can assert on the
	// filters as well as the name. Body is a valid single-match response: this slice is about
	// the REQUEST, so the response stays boring on purpose.
	var gotQuery url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query()
		fmt.Fprint(w, `{"items":[{"value":{"id":4514,"nameDisplayAs":"Keir Starmer"}}]}`)
	}))
	defer srv.Close()

	resolver := bot.NewResolver(srv.URL)
	if _, err := resolver.ResolveName("Keir Starmer"); err != nil {
		t.Fatalf("ResolveName returned error: %v", err)
	}

	// Commons only — excludes peers, who have no Commons activity to report.
	if got := gotQuery.Get("House"); got != "1" {
		t.Errorf("resolver queried House=%q, want %q (the Commons)", got, "1")
	}
	// Sitting members only — excludes former and deceased members.
	if got := gotQuery.Get("IsCurrentMember"); got != "true" {
		t.Errorf("resolver queried IsCurrentMember=%q, want %q", got, "true")
	}
}

// Slice B4 (Phase B, the HTTP seam — ambiguity): the Members API matches a name as a
// SUBSTRING anywhere in the full name, so common surnames match many sitting MPs at once —
// the live API returns 11 for "Smith", including Connor Naismith, whose surname merely
// CONTAINS it. Ambiguity therefore cannot be filtered away (B3 already narrowed this from 52),
// and silently taking items[0] would attach a user to an arbitrary Smith with no way of
// knowing. So the resolver stops deciding: it returns EVERY match and lets the caller present
// them for disambiguation. That is what turns ResolveName plural, and it also drives a named
// Member type — a bare []int could not carry the names a chooser has to display.
func TestResolver_ResolveName_multipleMatches_returnsThemAll(t *testing.T) {
	// Fake Members API returning three sitting Smiths, in the real nested shape. The third is
	// a Naismith: proof the resolver must not try to second-guess the API's matching by
	// filtering results itself — it has no better rule available than the API's own.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"items":[
			{"value":{"id":4471,"nameDisplayAs":"Cat Smith"}},
			{"value":{"id":5090,"nameDisplayAs":"Greg Smith"}},
			{"value":{"id":5399,"nameDisplayAs":"Connor Naismith"}}
		]}`)
	}))
	defer srv.Close()

	resolver := bot.NewResolver(srv.URL)
	members, err := resolver.ResolveName("Smith")
	if err != nil {
		t.Fatalf("ResolveName returned error: %v", err)
	}

	// Every match survives — none dropped, none invented.
	if len(members) != 3 {
		t.Fatalf("ResolveName returned %d members, want all 3", len(members))
	}

	// Each match keeps BOTH its ID and its display name, in the order the API gave them.
	// The name is not decoration: it is the only thing that lets a user tell these apart.
	want := []bot.Member{
		{ID: 4471, Name: "Cat Smith"},
		{ID: 5090, Name: "Greg Smith"},
		{ID: 5399, Name: "Connor Naismith"},
	}
	for i, w := range want {
		if members[i] != w {
			t.Errorf("members[%d] = %+v, want %+v", i, members[i], w)
		}
	}
}

// Slice 10 (Phase D, poll loop — newness half): polling twice must not re-push an item
// already sent to a chat. The first CheckActivity delivers the item; the second, over the
// same unchanged source, delivers nothing. This drives a per-chat "already sent" high-water
// mark in the store, keyed on Activity.ID — "sent" is tracked per chat, so each follower is
// notified of a given item exactly once. (First-follow baseline — suppressing backlog for a
// brand-new follower — is the next slice and reuses this same machinery.)
func TestCheckActivity_itemAlreadySent_notPushedAgain(t *testing.T) {
	store := bot.NewMemoryStore()
	store.AddChat(1)
	store.FollowMP(1, bot.Member{ID: 4514, Name: "Keir Starmer"})

	source := fakeSource{items: map[string][]bot.Activity{
		"Keir Starmer": {{ID: "v42", Text: "voted on Bill 42"}},
	}}

	first := bot.CheckActivity(source, store)
	if len(first) != 1 {
		t.Fatalf("first poll: got %d replies, want 1", len(first))
	}

	second := bot.CheckActivity(source, store)
	if len(second) != 0 {
		t.Errorf("second poll re-pushed %d already-sent item(s), want 0", len(second))
	}
}

// Slice 9 (Phase D, poll loop — fan-out half): a poll over the store turns an MP's
// activity into a reply for each chat that follows that MP. One follower, one MP, one
// item -> one reply addressed to that chat, mentioning the activity. Detecting *new*
// activity (not re-pushing already-sent items) is a deliberately separate behavior — the
// next slice — so this one proves only source -> match subscribers -> broadcast.
func TestCheckActivity_itemForFollowedMP_repliesToSubscriber(t *testing.T) {
	store := bot.NewMemoryStore()
	store.AddChat(1)
	store.FollowMP(1, bot.Member{ID: 4514, Name: "Keir Starmer"})

	source := fakeSource{items: map[string][]bot.Activity{
		"Keir Starmer": {{ID: "v42", Text: "voted on Bill 42"}},
	}}

	replies := bot.CheckActivity(source, store)

	if len(replies) != 1 {
		t.Fatalf("got %d replies, want 1", len(replies))
	}
	if replies[0].ChatID != 1 {
		t.Errorf("reply addressed to chat %d, want 1", replies[0].ChatID)
	}
	if !strings.Contains(replies[0].Text, "voted on Bill 42") {
		t.Errorf("reply text %q does not mention the activity", replies[0].Text)
	}
}

// Slice 8: /list replies with the MPs the chat follows. After following two MPs, the
// reply is addressed back to the chat and mentions each by name. Substring checks keep
// the exact formatting and ordering free to change. (The "follows nobody" case is a
// distinct behavior — its own later slice, not this one.)
func TestHandleUpdate_list_repliesWithFollowedMPs(t *testing.T) {
	mps := []string{"Keir Starmer", "Rishi Sunak"}
	store := bot.NewMemoryStore()
	b := bot.New(store, knownMPs(mps...))

	for _, mp := range mps {
		if _, err := b.HandleUpdate(bot.Update{ChatID: 42, Text: "/follow " + mp}); err != nil {
			t.Fatalf("following %q: %v", mp, err)
		}
	}

	reply, err := b.HandleUpdate(bot.Update{ChatID: 42, Text: "/list"})
	if err != nil {
		t.Fatalf("HandleUpdate(/list) returned error: %v", err)
	}

	if reply.ChatID != 42 {
		t.Errorf("reply addressed to chat %d, want 42", reply.ChatID)
	}
	for _, mp := range mps {
		if !strings.Contains(reply.Text, mp) {
			t.Errorf("reply %q does not mention followed MP %q", reply.Text, mp)
		}
	}
}

// Slice 11 (Phase A): /list when the chat follows nobody must NOT emit the dangling
// "You follow: " reply (which is what strings.Join over an empty list produces today).
// It should send a non-empty message that reads distinctly from the populated list, so a
// brand-new user is told they follow no one rather than shown an empty list. Wording is
// not pinned — only non-empty, and distinct from the with-follows reply.
func TestHandleUpdate_list_whenFollowingNobody_distinctReply(t *testing.T) {
	store := bot.NewMemoryStore()
	b := bot.New(store, knownMPs("Keir Starmer"))

	empty, err := b.HandleUpdate(bot.Update{ChatID: 7, Text: "/list"})
	if err != nil {
		t.Fatalf("HandleUpdate(/list) returned error: %v", err)
	}

	if empty.ChatID != 7 {
		t.Errorf("reply addressed to chat %d, want 7", empty.ChatID)
	}
	if strings.TrimSpace(empty.Text) == "" {
		t.Errorf("empty-follows /list reply is blank, want a non-empty message")
	}

	// A chat that DOES follow someone gets the normal list reply. The no-follows reply
	// must not merely be that list with an empty body — i.e. not a prefix of the
	// populated reply ("You follow: " is a prefix of "You follow: Keir Starmer"). It has
	// to be its own message, so a user following nobody isn't shown a dangling list.
	if _, err := b.HandleUpdate(bot.Update{ChatID: 7, Text: "/follow Keir Starmer"}); err != nil {
		t.Fatalf("follow setup failed: %v", err)
	}
	populated, err := b.HandleUpdate(bot.Update{ChatID: 7, Text: "/list"})
	if err != nil {
		t.Fatalf("HandleUpdate(/list) after follow returned error: %v", err)
	}
	if strings.HasPrefix(populated.Text, empty.Text) {
		t.Errorf("no-follows reply %q is just a prefix of the with-follows reply %q (dangling empty list); want a distinct message", empty.Text, populated.Text)
	}
}

// Slice 7: /follow with no MP name must not record an empty follow, and must reply
// with a usage hint distinct from the success confirmation. Covers both a bare
// "/follow" (no argument) and "/follow   " (whitespace-only argument) — the latter
// pins that the guard trims before deciding, so spaces alone don't count as a name.
func TestHandleUpdate_followWithoutName_recordsNothingAndHints(t *testing.T) {
	// Capture the success confirmation behaviorally so we can assert the hint differs
	// from it without hardcoding either string.
	confirm, err := bot.New(bot.NewMemoryStore(), knownMPs("Keir Starmer")).HandleUpdate(bot.Update{ChatID: 1, Text: "/follow Keir Starmer"})
	if err != nil {
		t.Fatalf("HandleUpdate(/follow <name>) returned error: %v", err)
	}

	for _, text := range []string{"/follow", "/follow   "} {
		t.Run(text, func(t *testing.T) {
			store := bot.NewMemoryStore()
			b := bot.New(store, nil)

			reply, err := b.HandleUpdate(bot.Update{ChatID: 7, Text: text})
			if err != nil {
				t.Fatalf("HandleUpdate(%q) returned error: %v", text, err)
			}

			if got := store.Follows(7); len(got) != 0 {
				t.Errorf("store.Follows(7) = %v, want nothing recorded for %q", got, text)
			}
			if reply.ChatID != 7 {
				t.Errorf("reply addressed to chat %d, want 7", reply.ChatID)
			}
			if reply.Text == "" {
				t.Errorf("reply.Text is empty, want a usage hint")
			}
			if reply.Text == confirm.Text {
				t.Errorf("got the success confirmation %q, want a distinct usage hint", reply.Text)
			}
		})
	}
}

// Slice 12 (Phase A): /unfollow <name> removes just that MP from the chat's follow
// list, leaving any others intact. Mirror of /follow. The chat follows two MPs, then
// unfollows one; only the other remains. There's no built-in slice remove, so this
// drives an UnfollowMP store method (filter into a new slice). Confirmation is
// addressed back to the chat and non-empty; exact wording stays free.
func TestHandleUpdate_unfollow_removesNamedMPOnly(t *testing.T) {
	const kept = "Rishi Sunak"
	const removed = "Keir Starmer"

	store := bot.NewMemoryStore()
	b := bot.New(store, knownMPs(removed, kept))

	for _, mp := range []string{removed, kept} {
		if _, err := b.HandleUpdate(bot.Update{ChatID: 42, Text: "/follow " + mp}); err != nil {
			t.Fatalf("follow setup for %q failed: %v", mp, err)
		}
	}

	reply, err := b.HandleUpdate(bot.Update{ChatID: 42, Text: "/unfollow " + removed})
	if err != nil {
		t.Fatalf("HandleUpdate(/unfollow) returned error: %v", err)
	}

	// Assert: only the unfollowed MP is gone; the other remains.
	got := store.Follows(42)
	if len(got) != 1 || got[0].Name != kept {
		t.Fatalf("store.Follows(42) = %v, want exactly [%q]", got, kept)
	}

	// Assert: confirmation addressed back to the chat and non-empty.
	if reply.ChatID != 42 {
		t.Errorf("reply addressed to chat %d, want 42", reply.ChatID)
	}
	if reply.Text == "" {
		t.Errorf("reply.Text is empty, want an unfollow confirmation")
	}
}

// Slice 13 (Phase A): /unfollow with no MP name must not touch the chat's existing
// follows, and must reply with a usage hint — NOT the bogus "You have unfollowed ."
// success message it sends today. Mirror of the /follow guard (slice 7). Covers both a
// bare "/unfollow" and a whitespace-only "/unfollow   " so the guard is pinned to trim
// before deciding, just like /follow.
func TestHandleUpdate_unfollowWithoutName_changesNothingAndHints(t *testing.T) {
	// Capture a real success confirmation behaviorally, then blank out the name. The
	// confirmation embeds the name verbatim, so removing it collapses the template down
	// to exactly the reply the empty-name case wrongly produces today ("You have
	// unfollowed ."). Asserting the guard reply differs from that pins it as a genuine
	// usage hint rather than a name-less "success" — and avoids hardcoding any wording.
	const realName = "Keir Starmer"
	confirmStore := bot.NewMemoryStore()
	confirmBot := bot.New(confirmStore, knownMPs(realName))
	if _, err := confirmBot.HandleUpdate(bot.Update{ChatID: 1, Text: "/follow " + realName}); err != nil {
		t.Fatalf("follow setup for confirmation failed: %v", err)
	}
	confirm, err := confirmBot.HandleUpdate(bot.Update{ChatID: 1, Text: "/unfollow " + realName})
	if err != nil {
		t.Fatalf("HandleUpdate(/unfollow <name>) returned error: %v", err)
	}
	namelessConfirm := strings.Replace(confirm.Text, realName, "", 1)

	for _, text := range []string{"/unfollow", "/unfollow   "} {
		t.Run(text, func(t *testing.T) {
			const followed = "Rishi Sunak"
			store := bot.NewMemoryStore()
			b := bot.New(store, knownMPs(followed))
			if _, err := b.HandleUpdate(bot.Update{ChatID: 7, Text: "/follow " + followed}); err != nil {
				t.Fatalf("follow setup failed: %v", err)
			}

			reply, err := b.HandleUpdate(bot.Update{ChatID: 7, Text: text})
			if err != nil {
				t.Fatalf("HandleUpdate(%q) returned error: %v", text, err)
			}

			// The existing follow must be untouched — a nameless /unfollow removes nothing.
			if got := store.Follows(7); len(got) != 1 || got[0].Name != followed {
				t.Errorf("store.Follows(7) = %v, want unchanged [%q] for %q", got, followed, text)
			}
			if reply.ChatID != 7 {
				t.Errorf("reply addressed to chat %d, want 7", reply.ChatID)
			}
			if reply.Text == "" {
				t.Errorf("reply.Text is empty, want a usage hint")
			}
			if reply.Text == namelessConfirm {
				t.Errorf("got the name-less success confirmation %q, want a distinct usage hint", reply.Text)
			}
		})
	}
}

// Slice 14 (Phase A): /unfollow <name> for an MP the chat does NOT follow is a no-op
// and must not falsely claim a removal happened. The SAME name is used in both arms so
// the only variable is followed-vs-not, not the name (slice 13's lesson): a chat that
// really follows the MP gets the success confirmation; a chat that never followed them
// must get a DIFFERENT reply. This drives UnfollowMP to report whether it actually
// removed anything — today it silently filters and the handler always claims success.
func TestHandleUpdate_unfollowUnknownName_noOpAndDistinctReply(t *testing.T) {
	const mp = "Keir Starmer"

	// Arm A: chat genuinely follows mp, then unfollows — capture the real success reply.
	following := bot.NewMemoryStore()
	followingBot := bot.New(following, knownMPs(mp))
	if _, err := followingBot.HandleUpdate(bot.Update{ChatID: 1, Text: "/follow " + mp}); err != nil {
		t.Fatalf("follow setup failed: %v", err)
	}
	success, err := followingBot.HandleUpdate(bot.Update{ChatID: 1, Text: "/unfollow " + mp})
	if err != nil {
		t.Fatalf("HandleUpdate(/unfollow followed) returned error: %v", err)
	}

	// Arm B: chat follows someone ELSE, then unfollows mp it never followed.
	const other = "Rishi Sunak"
	store := bot.NewMemoryStore()
	b := bot.New(store, knownMPs(other))
	if _, err := b.HandleUpdate(bot.Update{ChatID: 7, Text: "/follow " + other}); err != nil {
		t.Fatalf("follow setup failed: %v", err)
	}
	reply, err := b.HandleUpdate(bot.Update{ChatID: 7, Text: "/unfollow " + mp})
	if err != nil {
		t.Fatalf("HandleUpdate(/unfollow unknown) returned error: %v", err)
	}

	// No-op: the unrelated follow is left untouched.
	if got := store.Follows(7); len(got) != 1 || got[0].Name != other {
		t.Errorf("store.Follows(7) = %v, want unchanged [%q]", got, other)
	}
	if reply.ChatID != 7 {
		t.Errorf("reply addressed to chat %d, want 7", reply.ChatID)
	}
	if reply.Text == "" {
		t.Errorf("reply.Text is empty, want a 'not following them' message")
	}
	// Same name in both arms, so an identical reply can only mean the handler claimed a
	// removal that never happened.
	if reply.Text == success.Text {
		t.Errorf("unknown-name /unfollow reply %q equals the real success confirmation; want a distinct 'you weren't following them' message", reply.Text)
	}
}

// Slice 14b (Phase A): /unfollow an MP the chat follows AMONG OTHERS must report
// success — it WAS removed — and reply with the success confirmation, not the "weren't
// following them" message. Pins UnfollowMP's bool to mean "did mp get removed?", not
// "did the list become empty / contain only mp". Reference success reply is captured
// from a chat that follows ONLY the target, so the two replies must match.
func TestHandleUpdate_unfollowFollowedAmongOthers_reportsSuccess(t *testing.T) {
	const target = "Keir Starmer"
	const other = "Rishi Sunak"

	// Reference: the success confirmation when the chat follows ONLY the target.
	solo := bot.NewMemoryStore()
	soloBot := bot.New(solo, knownMPs(target))
	if _, err := soloBot.HandleUpdate(bot.Update{ChatID: 1, Text: "/follow " + target}); err != nil {
		t.Fatalf("solo follow setup failed: %v", err)
	}
	success, err := soloBot.HandleUpdate(bot.Update{ChatID: 1, Text: "/unfollow " + target})
	if err != nil {
		t.Fatalf("solo unfollow failed: %v", err)
	}

	// The chat under test follows the target AND someone else, then unfollows the target.
	store := bot.NewMemoryStore()
	b := bot.New(store, knownMPs(target, other))
	for _, mp := range []string{target, other} {
		if _, err := b.HandleUpdate(bot.Update{ChatID: 7, Text: "/follow " + mp}); err != nil {
			t.Fatalf("follow setup for %q failed: %v", mp, err)
		}
	}
	reply, err := b.HandleUpdate(bot.Update{ChatID: 7, Text: "/unfollow " + target})
	if err != nil {
		t.Fatalf("HandleUpdate(/unfollow) returned error: %v", err)
	}

	// The target is gone, the other remains.
	if got := store.Follows(7); len(got) != 1 || got[0].Name != other {
		t.Fatalf("store.Follows(7) = %v, want exactly [%q]", got, other)
	}
	// The target WAS removed, so the reply must be the success confirmation — identical
	// to the solo case (same name, removal genuinely happened in both).
	if reply.Text != success.Text {
		t.Errorf("reply %q, want the success confirmation %q (the MP was actually removed)", reply.Text, success.Text)
	}
}

// Slice 15 (Phase A): /forgetme wipes EVERYTHING the bot holds for a chat — its
// subscription AND its entire follow list — in one command, then confirms. This is more
// than /stop (which only unsubscribes, leaving follows behind): the chat under test both
// subscribed and follows two MPs, so a minimal "just unsubscribe" implementation leaves
// the follows and fails. Asserting on both the chat record and the follow list forces a
// store method that clears the two together. Confirmation is addressed back to the chat
// and non-empty; wording stays free.
func TestHandleUpdate_forgetme_wipesSubscriptionAndFollows(t *testing.T) {
	store := bot.NewMemoryStore()
	b := bot.New(store, knownMPs("Keir Starmer", "Rishi Sunak"))

	// The chat is fully present: subscribed via /start and following two MPs.
	if _, err := b.HandleUpdate(bot.Update{ChatID: 42, Text: "/start"}); err != nil {
		t.Fatalf("/start setup failed: %v", err)
	}
	for _, mp := range []string{"Keir Starmer", "Rishi Sunak"} {
		if _, err := b.HandleUpdate(bot.Update{ChatID: 42, Text: "/follow " + mp}); err != nil {
			t.Fatalf("follow setup for %q failed: %v", mp, err)
		}
	}

	reply, err := b.HandleUpdate(bot.Update{ChatID: 42, Text: "/forgetme"})
	if err != nil {
		t.Fatalf("HandleUpdate(/forgetme) returned error: %v", err)
	}

	// Both halves must be gone: no longer a recorded chat, and no follows remain.
	if store.HasChat(42) {
		t.Errorf("chat 42 still recorded after /forgetme, want it removed")
	}
	if got := store.Follows(42); len(got) != 0 {
		t.Errorf("store.Follows(42) = %v after /forgetme, want nothing", got)
	}

	// Confirmation addressed back to the chat and non-empty.
	if reply.ChatID != 42 {
		t.Errorf("reply addressed to chat %d, want 42", reply.ChatID)
	}
	if reply.Text == "" {
		t.Errorf("reply.Text is empty, want a confirmation message")
	}
}

// Slice B5 (Phase B): /find <query> is the FIRST command that reaches outside the bot's own
// memory. Everything the resolver learned in B1–B4 has so far been unreachable from a chat:
// HandleUpdate could not name a real MP if it wanted to. This slice connects the two, which
// is why the resolver arrives as a Bot dependency rather than a parameter.
//
// The behavior: a search naming several sitting MPs must show the user ALL of them. B4 made
// the deliberate decision to return every match rather than guess, precisely so a caller
// could offer a choice — a /find that reported only the first would throw that away and
// silently mislead anyone searching a common surname. The reply is asserted by Contains per
// member so the layout (numbered list, one per line, however Ian likes) stays free.
//
// Note the fake is passed as a value of an unexported test type, so /find cannot be
// satisfied by depending on *Resolver: the Bot has to accept an INTERFACE that both the real
// resolver and this fake satisfy. That is the point of taking the dependency here.
//
// Deliberately NOT in this slice, each its own RED later: a bare /find with no query, a
// query matching nobody (B1b moved that wording up to exactly this caller), and a resolver
// returning an error because the API is down.
func TestHandleUpdate_find_repliesWithEveryMatchingMP(t *testing.T) {
	// Three sitting MPs the live API really does return for "Smith" — including Connor
	// Naismith, who matches because the API matches a substring anywhere in the name.
	smiths := []bot.Member{
		{ID: 4451, Name: "Cat Smith"},
		{ID: 4425, Name: "Jeff Smith"},
		{ID: 5266, Name: "Connor Naismith"},
	}
	store := bot.NewMemoryStore()
	b := bot.New(store, &fakeResolver{matches: map[string][]bot.Member{"Smith": smiths}})

	reply, err := b.HandleUpdate(bot.Update{ChatID: 7, Text: "/find Smith"})
	if err != nil {
		t.Fatalf("HandleUpdate(/find) returned error: %v", err)
	}

	// Assert: every match is offered to the user, none quietly dropped. Keying the fake by
	// query also pins that the search term came from the message, not a constant — a wrong
	// query finds no canned matches and no name appears.
	for _, m := range smiths {
		if !strings.Contains(reply.Text, m.Name) {
			t.Errorf("/find reply %q does not mention %q", reply.Text, m.Name)
		}
	}

	// Assert: addressed back to the chat that asked.
	if reply.ChatID != 7 {
		t.Errorf("reply addressed to chat %d, want 7", reply.ChatID)
	}
}

// Slice B6: /find for a name nobody has replies with something that actually says so.
// B4 made "nobody matched" a SUCCESSFUL answer (an empty slice, not an error) and moved
// the wording up to this caller — this is that caller, and it currently produces the
// dangling "Found: " that slice 11 fixed for /list. The assertion is slice 11's trick,
// and it is the whole point of the test: comparing the two replies for INEQUALITY would
// pass against the bug, because "Found: " already differs from "Found: Cat Smith, ...".
// Asserting the populated reply does not START WITH the empty one catches a reply that is
// merely the same template with its list missing, which is the failure we actually mean.
func TestHandleUpdate_find_noMatches_distinctReply(t *testing.T) {
	store := bot.NewMemoryStore()
	// The fake yields no matches for any query it has no canned entry for — exactly what
	// the live API does for a name nobody has.
	resolver := &fakeResolver{matches: map[string][]bot.Member{
		"Smith": {{ID: 4451, Name: "Cat Smith"}},
	}}
	b := bot.New(store, resolver)

	// Capture a populated reply behaviorally, so the test never pins either wording.
	populated, err := b.HandleUpdate(bot.Update{ChatID: 7, Text: "/find Smith"})
	if err != nil {
		t.Fatalf("HandleUpdate(/find Smith) returned error: %v", err)
	}

	empty, err := b.HandleUpdate(bot.Update{ChatID: 7, Text: "/find Wibblethorpe"})
	if err != nil {
		t.Fatalf("HandleUpdate(/find Wibblethorpe) returned error: %v", err)
	}

	if empty.Text == "" {
		t.Error("/find with no matches replied with empty text, want a message saying nobody matched")
	}

	if strings.HasPrefix(populated.Text, empty.Text) {
		t.Errorf("/find with no matches replied %q, which is just the start of the populated reply %q — want a distinct message, not a list with nothing in it", empty.Text, populated.Text)
	}

	// Assert: the "nobody matched" path is still addressed back to the asking chat.
	if empty.ChatID != 7 {
		t.Errorf("reply addressed to chat %d, want 7", empty.ChatID)
	}
}

// Slice B7: /find with no query is rejected WITHOUT asking the Members API anything.
// Mirror of the /follow guard (slice 7) and the /unfollow guard (slice 13) — but unlike
// those two this guard is not merely cosmetic, because /find has a collaborator behind it.
// Today a bare /find sends an EMPTY QUERY to the live API and then formats whatever comes
// back, so the guard has to short-circuit BEFORE the ResolveName call, not after it.
// That ordering is the whole point of the slice, and asserting the reply alone would not
// pin it: a guard placed after the call would produce the same words having already made
// a pointless request. So the load-bearing assertion is on the resolver's call log.
func TestHandleUpdate_findWithoutQuery_asksTheAPINothingAndHints(t *testing.T) {
	store := bot.NewMemoryStore()
	canned := map[string][]bot.Member{"Smith": {{ID: 4451, Name: "Cat Smith"}}}

	// Capture the "nobody matched" reply behaviorally, so no wording is pinned. A missing
	// query must not be answered with it: "no MP is called that" is a different statement
	// from "you didn't tell me who to look for", and only one of them is the user's fault.
	noMatches, err := bot.New(store, &fakeResolver{matches: canned}).
		HandleUpdate(bot.Update{ChatID: 7, Text: "/find Wibblethorpe"})
	if err != nil {
		t.Fatalf("HandleUpdate(/find Wibblethorpe) returned error: %v", err)
	}

	tests := []struct {
		name string
		text string
	}{
		{name: "bare command", text: "/find"},
		{name: "whitespace-only query", text: "/find   "},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// A fresh resolver per row, so each row's call log is only about that row.
			resolver := &fakeResolver{matches: canned}
			b := bot.New(store, resolver)

			reply, err := b.HandleUpdate(bot.Update{ChatID: 7, Text: tt.text})
			if err != nil {
				t.Fatalf("HandleUpdate(%q) returned error: %v", tt.text, err)
			}

			if len(resolver.calls) != 0 {
				t.Errorf("%q searched the API for %q; a missing query must be rejected before any request is made", tt.text, resolver.calls)
			}

			if reply.Text == "" {
				t.Errorf("%q replied with empty text, want a hint saying a name is needed", tt.text)
			}

			if reply.Text == noMatches.Text {
				t.Errorf("%q replied %q, the same thing said when a real name matches nobody — want a hint that a name is missing", tt.text, reply.Text)
			}

			if reply.ChatID != 7 {
				t.Errorf("reply addressed to chat %d, want 7", reply.ChatID)
			}
		})
	}
}

// Slice B7b: a padded query is searched for CLEANED. The guard above trims the argument to
// decide whether anything was typed, so the search must use that same trimmed value —
// otherwise the two drift apart and /find asks Parliament for "  Smith  ", padding and all.
// The live API matches on a substring and does no fuzzy matching (probed in B3), so stray
// spaces are not harmlessly ignored: they are part of what it looks for, and find nothing.
func TestHandleUpdate_findPaddedQuery_searchesForTheTrimmedName(t *testing.T) {
	store := bot.NewMemoryStore()
	resolver := &fakeResolver{matches: map[string][]bot.Member{
		"Smith": {{ID: 4451, Name: "Cat Smith"}},
	}}
	b := bot.New(store, resolver)

	if _, err := b.HandleUpdate(bot.Update{ChatID: 7, Text: "/find   Smith  "}); err != nil {
		t.Fatalf("HandleUpdate(/find   Smith  ) returned error: %v", err)
	}

	// Asserting the exact call log rather than the reply: the reply happens to be right
	// either way here, because a fake keyed by an unknown query returns no matches and the
	// B6 guard answers politely. What is wrong is the request itself.
	want := []string{"Smith"}
	if len(resolver.calls) != 1 || resolver.calls[0] != want[0] {
		t.Errorf("/find searched the API for %q, want %q — the padding should be trimmed before the query goes out", resolver.calls, want)
	}
}

// Slice B8: the last /find sad path — the Members API itself fails. Today the error is
// returned bare with a zero Reply, so the user is told nothing at all while the bot knows
// perfectly well what went wrong. This is a foreseeable failure of a service we do not
// control, not a bug, and silence is the one answer that is always wrong.
//
// Both halves of the return value are load-bearing here, which is the point of the slice:
// the reply assertions fail if the error is swallowed into words only, and the error
// assertion fails if the user is answered but the caller learns nothing. Neither alone
// pins the decision that a failed lookup has TWO audiences — the user, who needs a
// sentence, and Phase E's poll loop, which needs something to log and retry on.
func TestHandleUpdate_findWhenResolverFails_repliesAndReportsTheError(t *testing.T) {
	store := bot.NewMemoryStore()
	canned := map[string][]bot.Member{"Smith": {{ID: 4451, Name: "Cat Smith"}}}

	// Capture the "nobody matched" reply behaviorally, so no wording is pinned. An outage
	// must not be answered with it: "no MP is called that" is a settled fact about
	// Parliament, while "the lookup failed" is a temporary fact about us, and only one of
	// them is worth trying again in a minute.
	noMatches, err := bot.New(store, &fakeResolver{matches: canned}).
		HandleUpdate(bot.Update{ChatID: 7, Text: "/find Wibblethorpe"})
	if err != nil {
		t.Fatalf("HandleUpdate(/find Wibblethorpe) returned error: %v", err)
	}

	// A test-local error value, so the assertion below can check this exact failure came
	// back rather than merely that something did. This is not the sentinel-error work
	// deferred in B2: nothing in the bot package branches on the KIND of failure, and the
	// handler still treats every resolver error identically.
	errAPIDown := errors.New("members API returned 503 Service Unavailable")

	resolver := &fakeResolver{matches: canned, err: errAPIDown}
	b := bot.New(store, resolver)

	reply, err := b.HandleUpdate(bot.Update{ChatID: 7, Text: "/find Smith"})

	// The caller's half: the failure travels up intact, so the layer that owns logging and
	// retries can see what actually went wrong instead of a flattened "something failed".
	if !errors.Is(err, errAPIDown) {
		t.Errorf("HandleUpdate returned error %v, want the resolver's own %v — a failed lookup must reach the caller, not stop at the handler", err, errAPIDown)
	}

	// The user's half: they are told something, in this same call, despite the error.
	if reply.Text == "" {
		t.Errorf("/find replied with empty text when the API failed, want a sentence saying the lookup could not be done")
	}

	if reply.Text == noMatches.Text {
		t.Errorf("/find replied %q when the API failed, the same thing said when a real name matches nobody — want a reply that does not claim to have searched", reply.Text)
	}

	if reply.ChatID != 7 {
		t.Errorf("reply addressed to chat %d, want 7", reply.ChatID)
	}
}

// Slice C0 (was slice 6): /follow <name> resolves the name against the Members API and
// records the MP by DURABLE ID, not by the string the user typed.
//
// Slice 6 stored the raw name as an explicit placeholder, and this slice cashes that in.
// The name was never a usable identity: B3's live probing showed "Smith" matching 11
// sitting MPs, a peer, and a member who died in 1991, and Phase C cannot ask Parliament
// what a *string* has been up to — activity is fetched per member ID. So the ID has to be
// captured at the moment the user follows, which is the only moment we have a name to
// resolve and a user present to hear about it going wrong.
//
// What is asserted is that the stored value carries the resolver's ID. That ID exists
// nowhere in the update text, so it can only have arrived by consulting the resolver
// with the typed name — which is why this needs no separate call-log assertion.
//
// The reply assertions and the two-word name are INHERITED FROM SLICE 6 AND MUST SURVIVE:
// the name carries a space so the first-space-only split stays pinned, and a green reached
// by dropping the confirmation reply would be no green at all.
func TestHandleUpdate_follow_resolvesNameAndRecordsMemberID(t *testing.T) {
	const mp = "Keir Starmer"
	want := bot.Member{ID: 4514, Name: mp}
	resolver := &fakeResolver{matches: map[string][]bot.Member{
		mp: {want},
	}}
	store := bot.NewMemoryStore()
	b := bot.New(store, resolver)

	// Capture the welcome behaviorally so we can assert the confirmation differs
	// from it without hardcoding either string.
	welcome, err := b.HandleUpdate(bot.Update{ChatID: 42, Text: "/start"})
	if err != nil {
		t.Fatalf("HandleUpdate(/start) returned error: %v", err)
	}

	reply, err := b.HandleUpdate(bot.Update{ChatID: 42, Text: "/follow " + mp})
	if err != nil {
		t.Fatalf("HandleUpdate(/follow) returned error: %v", err)
	}

	// Assert: the MP is recorded as the resolver identified them — ID included.
	got := store.Follows(42)
	if len(got) != 1 || got[0] != want {
		t.Fatalf("store.Follows(42) = %v, want exactly [%v]", got, want)
	}

	// Assert: confirmation addressed back to the chat, non-empty, distinct from welcome.
	if reply.ChatID != 42 {
		t.Errorf("reply addressed to chat %d, want 42", reply.ChatID)
	}
	if reply.Text == "" {
		t.Errorf("reply.Text is empty, want a follow confirmation")
	}
	if reply.Text == welcome.Text {
		t.Errorf("/follow got the welcome reply %q, want a distinct confirmation", reply.Text)
	}
}

// Slice 5: Broadcast sends one reply per recorded subscriber, each addressed to
// that chat and carrying the same message. Chats() is unsorted, so we compare the
// replies as a set (chatID -> text), never by position.
func TestBroadcast_sendsMessageToEverySubscriber(t *testing.T) {
	store := bot.NewMemoryStore()
	b := bot.New(store, nil)
	for _, id := range []int64{1, 2} {
		if _, err := b.HandleUpdate(bot.Update{ChatID: id, Text: "/start"}); err != nil {
			t.Fatalf("subscribing chat %d: %v", id, err)
		}
	}

	const msg = "Division at 7pm"
	replies := bot.Broadcast(msg, store)

	got := make(map[int64]string)
	for _, r := range replies {
		got[r.ChatID] = r.Text
	}
	want := map[int64]string{1: msg, 2: msg}

	if len(replies) != len(want) {
		t.Fatalf("Broadcast returned %d replies, want %d: %+v", len(replies), len(want), replies)
	}
	for id, text := range want {
		if got[id] != text {
			t.Errorf("reply to chat %d = %q, want %q", id, got[id], text)
		}
	}
}

// Slice 4: /stop unsubscribes — a chat that previously /start-ed is removed
// from the store, and gets a goodbye reply addressed to it, distinct from the
// welcome. Drives a remove side-effect (the mirror of /start's record).
func TestHandleUpdate_stop_unsubscribesAndDistinctReply(t *testing.T) {
	store := bot.NewMemoryStore()
	b := bot.New(store, nil)

	// Arrange: chat 42 is subscribed. Capture the welcome behaviorally so we
	// can assert the goodbye differs from it without hardcoding either string.
	welcome, err := b.HandleUpdate(bot.Update{ChatID: 42, Text: "/start"})
	if err != nil {
		t.Fatalf("HandleUpdate(/start) returned error: %v", err)
	}
	if !store.HasChat(42) {
		t.Fatalf("precondition failed: chat 42 not recorded after /start")
	}

	// Act: chat 42 sends /stop.
	reply, err := b.HandleUpdate(bot.Update{ChatID: 42, Text: "/stop"})
	if err != nil {
		t.Fatalf("HandleUpdate(/stop) returned error: %v", err)
	}

	// Assert: removed from the store.
	if store.HasChat(42) {
		t.Errorf("chat 42 still recorded after /stop, want it removed")
	}
	// Assert: goodbye addressed back to the chat, non-empty, distinct from welcome.
	if reply.ChatID != 42 {
		t.Errorf("reply addressed to chat %d, want 42", reply.ChatID)
	}
	if reply.Text == "" {
		t.Errorf("reply.Text is empty, want a goodbye message")
	}
	if reply.Text == welcome.Text {
		t.Errorf("/stop got the welcome reply %q, want a distinct goodbye", reply.Text)
	}
}

// Slice 3: /start is idempotent — a repeated /start from the same chat must
// not duplicate the chat in the store. Pinning that requires enumerating the
// store via Chats(), the accessor broadcasting will need anyway.
func TestHandleUpdate_repeatedStart_recordsChatOnce(t *testing.T) {
	store := bot.NewMemoryStore()
	b := bot.New(store, nil)

	for i := range 2 {
		if _, err := b.HandleUpdate(bot.Update{ChatID: 7, Text: "/start"}); err != nil {
			t.Fatalf("HandleUpdate call %d returned error: %v", i+1, err)
		}
	}

	got := store.Chats()
	if len(got) != 1 || got[0] != 7 {
		t.Fatalf("store.Chats() = %v, want exactly [7]", got)
	}
}

// Slice 2: a non-/start message must not be recorded in the store, and must
// get a non-empty reply of its own — not the /start welcome.
func TestHandleUpdate_unknownMessage_notRecordedAndDistinctReply(t *testing.T) {
	// Establish what the welcome looks like, behaviorally, rather than
	// hardcoding the string here.
	welcome, err := bot.New(bot.NewMemoryStore(), nil).HandleUpdate(bot.Update{ChatID: 1, Text: "/start"})
	if err != nil {
		t.Fatalf("HandleUpdate(/start) returned error: %v", err)
	}

	store := bot.NewMemoryStore()
	b := bot.New(store, nil)
	reply, err := b.HandleUpdate(bot.Update{ChatID: 99, Text: "hello"})
	if err != nil {
		t.Fatalf("HandleUpdate returned error: %v", err)
	}

	if store.HasChat(99) {
		t.Errorf("store recorded chat 99 for %q, want only /start to record", "hello")
	}
	if reply.ChatID != 99 {
		t.Errorf("reply addressed to chat %d, want 99", reply.ChatID)
	}
	if reply.Text == "" {
		t.Errorf("reply.Text is empty, want a hint for unrecognized input")
	}
	if reply.Text == welcome.Text {
		t.Errorf("unknown message got the welcome reply %q, want a distinct reply", reply.Text)
	}
}

// Slice 16 (Phase A, static commands): /help returns a reply addressed to the chat with
// its own help text. The point of the slice is that /help is genuinely dispatched to its
// own case, not swallowed by the default fallback — so we capture the default reply
// behaviorally (via an unrecognized command) and assert /help differs from it, rather than
// hardcoding the fallback string.
func TestHandleUpdate_help_repliesWithHelpTextDistinctFromDefault(t *testing.T) {
	// Establish the default fallback behaviorally rather than hardcoding its text.
	fallback, err := bot.New(bot.NewMemoryStore(), nil).HandleUpdate(bot.Update{ChatID: 1, Text: "not-a-command"})
	if err != nil {
		t.Fatalf("HandleUpdate(unknown) returned error: %v", err)
	}

	store := bot.NewMemoryStore()
	b := bot.New(store, nil)
	reply, err := b.HandleUpdate(bot.Update{ChatID: 7, Text: "/help"})
	if err != nil {
		t.Fatalf("HandleUpdate(/help) returned error: %v", err)
	}

	if reply.ChatID != 7 {
		t.Errorf("reply addressed to chat %d, want 7", reply.ChatID)
	}
	if reply.Text == "" {
		t.Errorf("reply.Text is empty, want help text")
	}
	if reply.Text == fallback.Text {
		t.Errorf("/help got the default fallback reply %q, want distinct help text", reply.Text)
	}
}

// Slice 17 (Phase A, static commands): /privacy returns a reply addressed to the chat
// with its own privacy text. Like /help (slice 16), the point is that /privacy is
// genuinely dispatched to its OWN case, not swallowed by the default fallback — so we
// capture the default reply behaviorally (via an unrecognized command) and assert
// /privacy differs from it, rather than hardcoding the fallback string. Wording is free.
func TestHandleUpdate_privacy_repliesWithPrivacyTextDistinctFromDefault(t *testing.T) {
	// Establish the default fallback behaviorally rather than hardcoding its text.
	fallback, err := bot.New(bot.NewMemoryStore(), nil).HandleUpdate(bot.Update{ChatID: 1, Text: "not-a-command"})
	if err != nil {
		t.Fatalf("HandleUpdate(unknown) returned error: %v", err)
	}

	store := bot.NewMemoryStore()
	b := bot.New(store, nil)
	reply, err := b.HandleUpdate(bot.Update{ChatID: 7, Text: "/privacy"})
	if err != nil {
		t.Fatalf("HandleUpdate(/privacy) returned error: %v", err)
	}

	if reply.ChatID != 7 {
		t.Errorf("reply addressed to chat %d, want 7", reply.ChatID)
	}
	if reply.Text == "" {
		t.Errorf("reply.Text is empty, want privacy text")
	}
	if reply.Text == fallback.Text {
		t.Errorf("/privacy got the default fallback reply %q, want distinct privacy text", reply.Text)
	}
}

// Slice 18 (Phase A, static commands — last one): /source returns a reply addressed to
// the chat with its own text (e.g. a link to the project's source). Like /help (slice 16)
// and /privacy (slice 17), the point is that /source is genuinely dispatched to its OWN
// case, not swallowed by the default fallback — so we capture the default reply
// behaviorally and assert /source differs from it, rather than hardcoding wording.
func TestHandleUpdate_source_repliesWithSourceTextDistinctFromDefault(t *testing.T) {
	// Establish the default fallback behaviorally rather than hardcoding its text.
	fallback, err := bot.New(bot.NewMemoryStore(), nil).HandleUpdate(bot.Update{ChatID: 1, Text: "not-a-command"})
	if err != nil {
		t.Fatalf("HandleUpdate(unknown) returned error: %v", err)
	}

	store := bot.NewMemoryStore()
	b := bot.New(store, nil)
	reply, err := b.HandleUpdate(bot.Update{ChatID: 7, Text: "/source"})
	if err != nil {
		t.Fatalf("HandleUpdate(/source) returned error: %v", err)
	}

	if reply.ChatID != 7 {
		t.Errorf("reply addressed to chat %d, want 7", reply.ChatID)
	}
	if reply.Text == "" {
		t.Errorf("reply.Text is empty, want source text")
	}
	if reply.Text == fallback.Text {
		t.Errorf("/source got the default fallback reply %q, want distinct source text", reply.Text)
	}
}

// Slice 1: an incoming /start update should be recorded in the subscriber store
// and produce a welcome reply addressed back to the same chat.
func TestHandleUpdate(t *testing.T) {
	tests := []struct {
		name        string
		update      bot.Update
		wantReplyTo int64 // chat the reply is addressed to
		wantStored  int64 // chat expected to be recorded in the store
	}{
		{
			name:        "start command records chat and replies",
			update:      bot.Update{ChatID: 12345, Text: "/start"},
			wantReplyTo: 12345,
			wantStored:  12345,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := bot.NewMemoryStore()
			b := bot.New(store, nil)

			reply, err := b.HandleUpdate(tt.update)
			if err != nil {
				t.Fatalf("HandleUpdate returned error: %v", err)
			}
			if reply.ChatID != tt.wantReplyTo {
				t.Errorf("reply addressed to chat %d, want %d", reply.ChatID, tt.wantReplyTo)
			}
			if reply.Text == "" {
				t.Errorf("reply.Text is empty, want a welcome message")
			}
			if !store.HasChat(tt.wantStored) {
				t.Errorf("store did not record chat %d", tt.wantStored)
			}
		})
	}
}

// Slice C1: /follow a name NOBODY HAS. This is the deferred panic C0 shipped knowingly —
// `mp := members[0]` on an empty slice — and it is B1b's bug one layer up: B4 made zero
// matches a SUCCESSFUL answer (an empty slice, not an error), and C0 built a caller that
// never learned to read it. /find already handles this; /follow crashes the process.
//
// The three assertions do different jobs. Reaching HandleUpdate at all proves the panic is
// gone. Nothing recorded proves the guard runs BEFORE the store is touched — a green that
// replied politely while appending a zero-value Member would leave the user following an
// MP with ID 0 and no name. And err == nil pins the contract B4 chose and /find follows:
// "nobody is called that" is a real answer to a reasonable question, not a failure.
func TestHandleUpdate_follow_unknownName_recordsNothingAndSaysSo(t *testing.T) {
	// Capture the success confirmation behaviorally, from its own store so the follow it
	// records cannot leak into the assertions below. Neither wording is pinned; this also
	// doubles as a check that the happy path C0 built still works after the guard lands.
	confirmStore := bot.NewMemoryStore()
	confirm, err := bot.New(confirmStore, knownMPs("Keir Starmer")).HandleUpdate(bot.Update{ChatID: 9, Text: "/follow Keir Starmer"})
	if err != nil {
		t.Fatalf("HandleUpdate(/follow Keir Starmer) returned error: %v", err)
	}

	store := bot.NewMemoryStore()
	// The resolver knows Keir Starmer and nobody else, so any other name resolves to no
	// matches — exactly what the live API returns for a name nobody has.
	b := bot.New(store, knownMPs("Keir Starmer"))

	reply, err := b.HandleUpdate(bot.Update{ChatID: 9, Text: "/follow Wibblethorpe"})
	if err != nil {
		t.Fatalf("HandleUpdate(/follow Wibblethorpe) returned error: %v, want nil — no such MP is an answer, not a failure", err)
	}

	if got := store.Follows(9); len(got) != 0 {
		t.Errorf("store.Follows(9) = %v, want nothing recorded for a name nobody has", got)
	}

	if reply.Text == "" {
		t.Error("/follow with an unresolvable name replied with empty text, want a message saying nobody matched")
	}

	if reply.Text == confirm.Text {
		t.Errorf("/follow with an unresolvable name replied %q, the same as the success confirmation — want a distinct message", reply.Text)
	}

	if reply.ChatID != 9 {
		t.Errorf("reply addressed to chat %d, want 9", reply.ChatID)
	}
}

// Slice C2: /follow a name several MPs share follows NOBODY, and offers one choice per
// match instead. This is the last of C0's deliberate debt: "/follow Smith" silently picked
// one of eleven, so the user could end up following an MP they never asked for and had no
// way to notice.
//
// The design being pinned here is that the bot stays STATELESS. Each choice is the exact
// text that, sent back as an ordinary message, follows that one MP — so a Telegram reply
// keyboard can render them as buttons and tapping one simply sends it. The bot never has to
// remember that it asked a question, which is why no pending-question state appears in the
// store.
//
// That is also why the load-bearing assertion REPLAYS each choice through HandleUpdate
// rather than matching it against a string. A choice that reads "Cat Smith" looks perfectly
// reasonable in a test and does NOTHING when tapped, because it carries no command. Feeding
// it back is the only assertion that can tell the difference, and it leaves the exact
// wording free.
func TestHandleUpdate_follow_severalMatches_offersChoicesAndFollowsNobody(t *testing.T) {
	// Three real sitting Smiths, including Connor Nai*smith* — B3's live probing established
	// that the API matches SUBSTRING anywhere in the name, so ambiguity cannot be filtered
	// away. The fake mirrors the same API: a short query returns all three, each full name
	// returns one.
	smiths := []bot.Member{
		{ID: 4451, Name: "Cat Smith"},
		{ID: 4478, Name: "Greg Smith"},
		{ID: 4813, Name: "Connor Naismith"},
	}
	matches := map[string][]bot.Member{"Smith": smiths}
	for _, m := range smiths {
		matches[m.Name] = []bot.Member{m}
	}

	store := bot.NewMemoryStore()
	b := bot.New(store, &fakeResolver{matches: matches})

	reply, err := b.HandleUpdate(bot.Update{ChatID: 9, Text: "/follow Smith"})
	if err != nil {
		t.Fatalf("HandleUpdate(/follow Smith) returned error: %v", err)
	}

	// Assert: an ambiguous follow commits to nothing. Picking one silently is the bug.
	if got := store.Follows(9); len(got) != 0 {
		t.Errorf("store.Follows(9) = %v, want nothing followed until the user has chosen", got)
	}

	// Assert: Telegram will not send a message with no text, so a bare menu is unsendable.
	if reply.Text == "" {
		t.Error("/follow with several matches replied with empty text, want a sentence explaining the choice")
	}

	if reply.ChatID != 9 {
		t.Errorf("reply addressed to chat %d, want 9", reply.ChatID)
	}

	if len(reply.Choices) != len(smiths) {
		t.Fatalf("reply offered %d choices %q, want one per match (%d)", len(reply.Choices), reply.Choices, len(smiths))
	}

	// Assert: every choice actually works, and picks out the MP it was offered for. Order is
	// asserted because B4 already pins that the resolver preserves the API's order, so the
	// nth choice belongs to the nth match.
	for i, choice := range reply.Choices {
		t.Run(choice, func(t *testing.T) {
			chosenStore := bot.NewMemoryStore()
			chosen := bot.New(chosenStore, &fakeResolver{matches: matches})

			if _, err := chosen.HandleUpdate(bot.Update{ChatID: 9, Text: choice}); err != nil {
				t.Fatalf("replaying choice %q returned error: %v", choice, err)
			}

			got := chosenStore.Follows(9)
			if len(got) != 1 || got[0] != smiths[i] {
				t.Errorf("sending choice %q back recorded %v, want exactly %v — a choice must be a message that follows that one MP", choice, got, smiths[i])
			}
		})
	}
}

// Slice C2 (ratchet): an UNAMBIGUOUS /follow still just follows, offering no choices. This
// passes the moment Reply grows the field and is not what drives the slice — it stops the
// green being reached by attaching a menu to every reply, which would satisfy the test above
// while making the common case worse.
func TestHandleUpdate_follow_singleMatch_offersNoChoices(t *testing.T) {
	store := bot.NewMemoryStore()
	b := bot.New(store, knownMPs("Keir Starmer"))

	reply, err := b.HandleUpdate(bot.Update{ChatID: 9, Text: "/follow Keir Starmer"})
	if err != nil {
		t.Fatalf("HandleUpdate(/follow Keir Starmer) returned error: %v", err)
	}

	if len(store.Follows(9)) != 1 {
		t.Fatalf("store.Follows(9) = %v, want the single match followed outright", store.Follows(9))
	}
	if len(reply.Choices) != 0 {
		t.Errorf("reply offered choices %q for an unambiguous name, want none — there is nothing to choose", reply.Choices)
	}
}

// Slice C3: /unfollow matches the chat's OWN follow list the way /follow matches Parliament —
// by SUBSTRING, not by exact equality. A chat following "Cat Smith" who types "/unfollow Smith"
// is plainly asking to stop following her, but UnfollowMP compares f.Name != mp, so the typed
// fragment matches nothing and the bot insists she was never followed while /list is still
// listing her. One word, one user, two different matching rules.
//
// The matching stays against the STORE and never reaches the resolver. A follow list is local
// data: routing it through the Members API would make unfollowing fail during an outage, and
// because the resolver filters on IsCurrentMember=true, an MP who stood down would stop
// resolving and could never be unfollowed again.
//
// The "you weren't following them" reply is captured behaviorally from a chat that follows
// someone the fragment cannot match, so no wording is pinned here — and that reference is
// stable across this slice, since "Rishi Sunak" contains "Smith" under neither rule.
func TestHandleUpdate_unfollowPartialName_removesTheFollowItMatches(t *testing.T) {
	const followed = "Cat Smith"
	const typed = "Smith"

	// Reference: what the bot says when the fragment genuinely matches nothing followed.
	const unrelated = "Rishi Sunak"
	missStore := bot.NewMemoryStore()
	missBot := bot.New(missStore, knownMPs(unrelated))
	if _, err := missBot.HandleUpdate(bot.Update{ChatID: 1, Text: "/follow " + unrelated}); err != nil {
		t.Fatalf("reference follow setup failed: %v", err)
	}
	notFollowing, err := missBot.HandleUpdate(bot.Update{ChatID: 1, Text: "/unfollow " + typed})
	if err != nil {
		t.Fatalf("reference unfollow failed: %v", err)
	}

	// The chat under test follows exactly one MP, whose name CONTAINS the typed fragment.
	store := bot.NewMemoryStore()
	b := bot.New(store, knownMPs(followed))
	if _, err := b.HandleUpdate(bot.Update{ChatID: 42, Text: "/follow " + followed}); err != nil {
		t.Fatalf("follow setup failed: %v", err)
	}

	reply, err := b.HandleUpdate(bot.Update{ChatID: 42, Text: "/unfollow " + typed})
	if err != nil {
		t.Fatalf("HandleUpdate(/unfollow %q) returned error: %v", typed, err)
	}

	// The load-bearing assertion: she is actually gone.
	if got := store.Follows(42); len(got) != 0 {
		t.Fatalf("store.Follows(42) = %v, want empty — %q should have matched the followed %q", got, typed, followed)
	}
	// ...and the bot reports a removal, not a miss. Without this, a green that deleted her
	// while still saying "you were not following this MP" would pass.
	if reply.Text == notFollowing.Text {
		t.Errorf("reply %q is the not-following message, but %q was removed; want a removal confirmation", reply.Text, followed)
	}
	// Ratchet: passes today and must keep passing — the answer goes back to the asking chat.
	if reply.ChatID != 42 {
		t.Errorf("reply addressed to chat %d, want 42", reply.ChatID)
	}
	// Ratchet (added on C4's green): the confirmation names the MP actually removed, not the
	// fragment that was typed. "You have unfollowed Smith." is true of nobody in particular,
	// and once a fragment can match several follows it stops being a harmless shorthand.
	if !strings.Contains(reply.Text, followed) {
		t.Errorf("reply %q does not name %q; the confirmation should say who was removed, not what was typed", reply.Text, followed)
	}
	// Ratchet (added on C4's green): an UNAMBIGUOUS unfollow just removes, offering no menu.
	// Stops the ambiguous case being satisfied by attaching choices to every reply.
	if len(reply.Choices) != 0 {
		t.Errorf("reply offered choices %q for a fragment matching one follow, want none — there is nothing to choose", reply.Choices)
	}
}

// Slice C4: /unfollow a fragment SEVERAL of the chat's follows share removes NOBODY and
// offers one choice per match — C2's design applied to the other direction. C3 made
// "/unfollow Smith" match by substring, which immediately raises the question C2 already
// answered for /follow: if the fragment fits two of your follows, removing either one is a
// guess, and a wrong guess silently unsubscribes someone from an MP they wanted.
//
// The asymmetry with /follow is worth naming: /follow disambiguates against PARLIAMENT (11
// sitting Smiths), /unfollow disambiguates against YOUR OWN LIST (however many Smiths you
// happen to follow). So the matching here reads the store, never the resolver — an outage,
// or an MP who has since stood down and no longer resolves, must not strand a follow.
//
// As in C2 the load-bearing assertion REPLAYS each choice through HandleUpdate, because
// that is the only way to tell a working command from a plausible-looking label.
func TestHandleUpdate_unfollow_severalFollowsMatch_removesNoneAndOffersChoices(t *testing.T) {
	names := []string{"Cat Smith", "Greg Smith"}

	// A chat that follows both Smiths. Built fresh per arm: replaying a choice needs a
	// pristine list, and reading the members back out of the store rather than hardcoding
	// them keeps the test independent of how knownMPs assigns IDs.
	followingBoth := func(t *testing.T) (*bot.MemoryStore, *bot.Bot) {
		t.Helper()
		store := bot.NewMemoryStore()
		b := bot.New(store, knownMPs(names...))
		for _, n := range names {
			if _, err := b.HandleUpdate(bot.Update{ChatID: 42, Text: "/follow " + n}); err != nil {
				t.Fatalf("follow setup for %q failed: %v", n, err)
			}
		}
		return store, b
	}

	store, b := followingBoth(t)
	want := append([]bot.Member(nil), store.Follows(42)...)
	if len(want) != len(names) {
		t.Fatalf("setup recorded %v, want %d follows", want, len(names))
	}

	reply, err := b.HandleUpdate(bot.Update{ChatID: 42, Text: "/unfollow Smith"})
	if err != nil {
		t.Fatalf("HandleUpdate(/unfollow Smith) returned error: %v", err)
	}

	// Assert: an ambiguous unfollow commits to nothing. Dropping either one is the bug.
	got := store.Follows(42)
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("store.Follows(42) = %v, want %v unchanged until the user has chosen", got, want)
	}

	// Assert: Telegram will not send a message with no text, so a bare menu is unsendable.
	if reply.Text == "" {
		t.Error("/unfollow with several matches replied with empty text, want a sentence explaining the choice")
	}
	if reply.ChatID != 42 {
		t.Errorf("reply addressed to chat %d, want 42", reply.ChatID)
	}
	if len(reply.Choices) != len(want) {
		t.Fatalf("reply offered %d choices %q, want one per matching follow (%d)", len(reply.Choices), reply.Choices, len(want))
	}

	// Assert: every choice actually works, and unfollows the ONE MP it was offered for,
	// leaving the other in place. Order follows the stored list, so the nth choice is want[n].
	for i, choice := range reply.Choices {
		t.Run(choice, func(t *testing.T) {
			chosenStore, chosen := followingBoth(t)

			if _, err := chosen.HandleUpdate(bot.Update{ChatID: 42, Text: choice}); err != nil {
				t.Fatalf("replaying choice %q returned error: %v", choice, err)
			}

			left := chosenStore.Follows(42)
			if len(left) != 1 || left[0] != want[1-i] {
				t.Errorf("sending choice %q back left %v, want exactly %v — a choice must be a message that unfollows that one MP", choice, left, want[1-i])
			}
		})
	}
}
