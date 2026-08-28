package bot_test

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Rolyani/mp-telegram-bot/internal/bot"
)

// fakeSource is an in-memory ActivitySource: canned activity items keyed by member ID, so
// the poll slice stays offline (real HTTP feeds arrive in Phase C).
//
// It is keyed by ID and knows no names at all, which is deliberate: the display name a
// follow record holds is a snapshot taken when the user followed, and nothing keeps it in
// sync with Parliament. Two sitting MPs can also share one display name outright. A fake
// that could be asked for activity by name would let that mistake back in unnoticed.
type fakeSource struct {
	items map[int][]bot.Activity
}

func (f fakeSource) Activity(memberID int) []bot.Activity {
	return f.items[memberID]
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

// Slice C6 (Phase C): /latest is the read-now command — it answers with the recent activity
// of every MP the chat follows, aggregated into one reply and fetched live rather than read
// back out of the store.
//
// This is the first command that needs an ActivitySource, and so the slice that turns the
// source into a field on Bot. There was never a per-call alternative: HandleUpdate takes an
// Update and nothing else, so a command cannot be handed a collaborator at call time. Bot's
// doc comment has predicted exactly this since B5.
//
// ⭐ RATCHET — the last assertion passes the moment the command works, and is the point of
// it. /latest is a PURE READ: the already-sent set exists to stop the poll loop repeating
// itself, and a user who explicitly asked was never being spammed. Without this assertion
// the slice could be "passed" by calling MarkSent on the way out, which would quietly make
// asking a question suppress the answer being delivered — and in Phase E, a reply that never
// reached Telegram would have been marked delivered to nobody.
func TestHandleUpdate_latest_repliesWithActivityForEveryFollowedMP(t *testing.T) {
	store := bot.NewMemoryStore()
	store.AddChat(1)
	store.FollowMP(1, bot.Member{ID: 101, Name: "Cat Smith"})
	store.FollowMP(1, bot.Member{ID: 102, Name: "Greg Smith"})

	source := fakeSource{items: map[int][]bot.Activity{
		101: {{ID: "s7", Text: "spoke on housing"}},
		102: {{ID: "v42", Text: "voted on Bill 42"}},
	}}

	// nil resolver: /latest looks up no names, and the nil enforces that it never tries.
	b := bot.New(store, nil, source)

	reply, err := b.HandleUpdate(bot.Update{ChatID: 1, Text: "/latest"})
	if err != nil {
		t.Fatalf("/latest returned error: %v", err)
	}
	if reply.ChatID != 1 {
		t.Errorf("reply addressed to chat %d, want 1", reply.ChatID)
	}

	// Both MPs' activity, in one reply, and each item ATTRIBUTED to the MP it belongs to.
	// ⭐ Attribution is asserted per LINE, and that is the whole point: checking that
	// "Cat Smith" and "voted on Bill 42" both appear somewhere in one blob would pass even
	// if every item were credited to the wrong MP. Following several MPs is the normal
	// case, so an unattributed feed cannot be read.
	//
	// The cost is that this pins one item per line — the minimum structure attribution
	// needs to be observable at all. Wording, order and separator stay free.
	attributed := map[string]string{
		"spoke on housing": "Cat Smith",
		"voted on Bill 42": "Greg Smith",
	}
	for text, name := range attributed {
		found := false
		for line := range strings.SplitSeq(reply.Text, "\n") {
			if !strings.Contains(line, text) {
				continue
			}
			found = true
			if !strings.Contains(line, name) {
				t.Errorf("line %q reports %q without naming %s", line, text, name)
			}
			// And nobody ELSE'S name on that line. Without this the per-line check is
			// vacuous when everything lands on one line: a single line holding every item
			// and every name trivially "contains" the right name for all of them.
			for _, other := range attributed {
				if other != name && strings.Contains(line, other) {
					t.Errorf("line %q credits %q to both %s and %s", line, text, name, other)
				}
			}
		}
		if !found {
			t.Errorf("reply %q does not mention %q at all", reply.Text, text)
		}
	}

	// RATCHET: reading is not delivering — the poll loop still owes the user both items.
	if pushed := b.CheckActivity(); len(pushed) != 2 {
		t.Errorf("poll delivered %d items after /latest, want 2: /latest must not mark items sent", len(pushed))
	}
}

// Slice C7 (Phase C): /latest has TWO ways to come back empty, and they are different
// situations, so they must not say the same thing. A chat that follows nobody should be
// told to go and follow someone; a chat whose MPs have simply been quiet should be told
// there is nothing new. Today both produce strings.Join(nil, "\n") — a blank reply — the
// dangling-string shape slice 11 fixed for /list and B6 for /find.
//
// This is the same principle already stated at bot.go:223 for /find: "asked and nobody
// matched" and "could not ask" are distinct answers the user can act on differently.
// Collapsing these two would tell someone following three MPs that they follow nobody.
//
// Wording is not pinned — only that each reply is non-empty and that the two differ.
func TestHandleUpdate_latest_emptyCases_saySomethingAndDiffer(t *testing.T) {
	// Case 1: the chat follows nobody.
	//
	// nil source, deliberately: with no follows there is nothing to fetch, so /latest
	// must never reach for activity here. A panic is the assertion.
	noFollows := bot.NewMemoryStore()
	noFollows.AddChat(1)

	nobody, err := bot.New(noFollows, nil, nil).HandleUpdate(bot.Update{ChatID: 1, Text: "/latest"})
	if err != nil {
		t.Fatalf("/latest with no follows returned error: %v", err)
	}
	if nobody.ChatID != 1 {
		t.Errorf("reply addressed to chat %d, want 1", nobody.ChatID)
	}
	if strings.TrimSpace(nobody.Text) == "" {
		t.Error("/latest with no follows replied with blank text, want a message saying you follow nobody")
	}

	// Case 2: the chat follows two MPs and neither has done anything. The source knows
	// both members and answers for both — it just has nothing to report, which is a
	// normal, successful answer and not a failure.
	quiet := bot.NewMemoryStore()
	quiet.AddChat(2)
	quiet.FollowMP(2, bot.Member{ID: 101, Name: "Cat Smith"})
	quiet.FollowMP(2, bot.Member{ID: 102, Name: "Greg Smith"})

	silent := fakeSource{items: map[int][]bot.Activity{
		101: {},
		102: {},
	}}

	nothingNew, err := bot.New(quiet, nil, silent).HandleUpdate(bot.Update{ChatID: 2, Text: "/latest"})
	if err != nil {
		t.Fatalf("/latest with quiet MPs returned error: %v", err)
	}
	if nothingNew.ChatID != 2 {
		t.Errorf("reply addressed to chat %d, want 2", nothingNew.ChatID)
	}
	if strings.TrimSpace(nothingNew.Text) == "" {
		t.Error("/latest with quiet MPs replied with blank text, want a message saying there is nothing new")
	}

	// The two must be told apart. This is what stops the single len(texts) == 0 guard:
	// that one guard cannot distinguish "you follow nobody" from "nobody did anything",
	// so it would tell a chat following two MPs that it follows none.
	if nothingNew.Text == nobody.Text {
		t.Errorf("both empty cases replied %q; a chat following 2 MPs must not be told it follows nobody", nobody.Text)
	}

	// Case 3: one quiet MP and one active one. The quiet MP must not suppress the other.
	//
	// This case exists because cases 1 and 2 do not constrain WHERE the second guard
	// goes: with every followed MP quiet, a guard inside the follows loop returns the
	// same right answer as one placed after it. Only a mixed follow list tells them
	// apart — and only when the quiet MP is reached FIRST, which is why Cat (quiet)
	// sorts before Greg (active) in the follow order below.
	mixed := bot.NewMemoryStore()
	mixed.AddChat(3)
	mixed.FollowMP(3, bot.Member{ID: 101, Name: "Cat Smith"})
	mixed.FollowMP(3, bot.Member{ID: 102, Name: "Greg Smith"})

	partial := fakeSource{items: map[int][]bot.Activity{
		101: {},
		102: {{ID: "v42", Text: "voted on Bill 42"}},
	}}

	some, err := bot.New(mixed, nil, partial).HandleUpdate(bot.Update{ChatID: 3, Text: "/latest"})
	if err != nil {
		t.Fatalf("/latest with one quiet MP returned error: %v", err)
	}
	if !strings.Contains(some.Text, "voted on Bill 42") {
		t.Errorf("/latest replied %q, losing the active MP's item; a followed MP with no activity must not cut the walk short", some.Text)
	}
}

// Slice C8a (Phase C): /latest says WHEN each item happened.
//
// This is the slice that puts a timestamp on Activity, and RENDERING it is deliberately the
// smallest thing that forces the field to exist. Ordering by it is a separate behavior and
// comes next: a feed can be dated without being sorted, and sorted without being dated, so
// proving the two apart keeps each red pointing at exactly one fault.
//
// Layout is NOT pinned. The assertion is that the date reaches the item's line — the one new
// fact this slice adds — not that it sits in parentheses, or before the colon, or after the
// name. Wording and separator stayed free in C6 and stay free here. The one thing it does fix
// is the SHAPE of the date: day then abbreviated month, which is Go's "2 Jan" layout.
func TestHandleUpdate_latest_datesEachItem(t *testing.T) {
	store := bot.NewMemoryStore()
	store.AddChat(1)
	store.FollowMP(1, bot.Member{ID: 101, Name: "Cat Smith"})

	// A FIXED date, never time.Now(): a test that builds its expectation from the clock
	// agrees with any implementation that also reads the clock, including one that ignores
	// the item entirely and stamps every line with today. Six months away from today makes
	// that particular wrong answer impossible to mistake for a right one.
	spokeOn := time.Date(2026, time.February, 3, 14, 30, 0, 0, time.UTC)

	source := fakeSource{items: map[int][]bot.Activity{
		101: {{ID: "s7", Text: "spoke on housing", When: spokeOn}},
	}}

	// nil resolver: /latest looks up no names, and the nil enforces that it never tries.
	reply, err := bot.New(store, nil, source).HandleUpdate(bot.Update{ChatID: 1, Text: "/latest"})
	if err != nil {
		t.Fatalf("/latest returned error: %v", err)
	}

	// What C6 established must survive: the date is ADDED to the line, not swapped in for the
	// attribution or the item text. A slice that adds a fact should not quietly drop one.
	if !strings.Contains(reply.Text, "Cat Smith") || !strings.Contains(reply.Text, "spoke on housing") {
		t.Fatalf("/latest replied %q, want it to still name the MP and describe the item", reply.Text)
	}

	if !strings.Contains(reply.Text, "3 Feb") {
		t.Errorf("/latest replied %q, want the item dated 3 Feb alongside it", reply.Text)
	}
}

// Slice C8b (Phase C): /latest lists one MP's items newest-first. C8a put a date on every
// line; a date the reader has to scan for the biggest number is only half the job, and
// "latest" is a promise about ORDER, not just about content.
//
// The fixture's three items are deliberately SCRAMBLED rather than merely reversed, and
// that choice is the whole strength of this test. Source order here is middle, oldest,
// newest — so every cheap wrong answer produces a visibly different list:
//
//	as fetched (no sort at all)  -> 3 Feb, 1 Feb, 5 Feb
//	reverse the slice           -> 5 Feb, 1 Feb, 3 Feb
//	sort OLDEST-first           -> 1 Feb, 3 Feb, 5 Feb
//	sort NEWEST-first           -> 5 Feb, 3 Feb, 1 Feb   <- the only one that passes
//
// A fixture already in oldest-first order would have let a plain reversal pass, and one
// already newest-first would have passed with no implementation at all. Both are the
// failure mode C7 caught and C8a's own demo tripped over: something that agrees with the
// right answer by coincidence tells you nothing.
//
// The assertions read the reply LINE BY LINE and only require each line to CONTAIN its
// item, rather than pinning the finished "Name: date, text" string. Order is what this
// slice is about; the wording is C8a's and is already ratcheted there, and a test that
// re-froze it would turn any later rewording into two failures instead of one.
func TestHandleUpdate_latest_ordersOneMPsItemsNewestFirst(t *testing.T) {
	store := bot.NewMemoryStore()
	store.AddChat(1)
	store.FollowMP(1, bot.Member{ID: 101, Name: "Cat Smith"})

	// Fixed dates, never time.Now() — same reasoning as C8a: an expectation built from the
	// clock agrees with an implementation that also reads the clock.
	var (
		newest = time.Date(2026, time.February, 5, 9, 0, 0, 0, time.UTC)
		middle = time.Date(2026, time.February, 3, 14, 30, 0, 0, time.UTC)
		oldest = time.Date(2026, time.February, 1, 11, 15, 0, 0, time.UTC)
	)

	source := fakeSource{items: map[int][]bot.Activity{
		101: {
			{ID: "s7", Text: "spoke on housing", When: middle},
			{ID: "q3", Text: "asked about rail fares", When: oldest},
			{ID: "v9", Text: "voted on the finance bill", When: newest},
		},
	}}

	// nil resolver: /latest looks up no names, and the nil enforces that it never tries.
	reply, err := bot.New(store, nil, source).HandleUpdate(bot.Update{ChatID: 1, Text: "/latest"})
	if err != nil {
		t.Fatalf("/latest returned error: %v", err)
	}

	want := []string{
		"voted on the finance bill",
		"spoke on housing",
		"asked about rail fares",
	}

	lines := strings.Split(reply.Text, "\n")
	if len(lines) != len(want) {
		t.Fatalf("/latest replied %q, want %d lines, got %d", reply.Text, len(want), len(lines))
	}

	// Errorf, not Fatalf: if the order is wrong, seeing every misplaced line at once says
	// which rearrangement happened, where stopping at the first only says "not this".
	for i, item := range want {
		if !strings.Contains(lines[i], item) {
			t.Errorf("/latest line %d is %q, want it to be the item %q (full reply %q)", i+1, lines[i], item, reply.Text)
		}
	}
}

// Slice C8b, second half: an item with no date says so, and sinks to the bottom.
//
// This is the hole C8a opened and deferred to here. time.Time's zero value is not a null
// and not an error — it is midnight on 1 January, year 1, a perfectly valid instant that
// Format renders without complaint. So an Activity whose When was never set does not
// crash and does not blank out; it produces "1 January 0001" and looks for all the world
// like an item we successfully dated. That is worse than a failure, because nothing
// anywhere reports it. Go has no null for a struct field, so a test is the only thing
// that can hold this line.
//
// THREE assertions, and each one kills a different cheap implementation:
//
//   - the undated line says "date unknown"  — kills doing nothing at all
//   - the DATED line still shows 3 February — kills stamping "date unknown" on everything,
//     which a test that only looked at the undated item would happily pass
//   - no "0001" survives anywhere           — names the actual hazard, so a future render
//     that formats the zero value differently but still leaks year 1 is caught
//
// The undated item is deliberately placed FIRST in the fixture. It has to end up LAST, so
// leaving the slice untouched cannot satisfy the ordering half by accident — the same
// discipline as the scrambled fixture above. Sorting last already works today, for free,
// because year 1 is older than everything; asserting it anyway is what stops a later
// change to the comparator from quietly undoing it.
func TestHandleUpdate_latest_undatedItem_saysDateUnknownAndSortsLast(t *testing.T) {
	store := bot.NewMemoryStore()
	store.AddChat(1)
	store.FollowMP(1, bot.Member{ID: 101, Name: "Cat Smith"})

	spokeOn := time.Date(2026, time.February, 3, 14, 30, 0, 0, time.UTC)

	source := fakeSource{items: map[int][]bot.Activity{
		101: {
			// When omitted entirely — the zero value, exactly as a source that forgot to
			// set it would leave things. Written as a bare missing field rather than an
			// explicit time.Time{} because that is how it will really happen.
			{ID: "q3", Text: "asked about rail fares"},
			{ID: "s7", Text: "spoke on housing", When: spokeOn},
		},
	}}

	// nil resolver: /latest looks up no names, and the nil enforces that it never tries.
	reply, err := bot.New(store, nil, source).HandleUpdate(bot.Update{ChatID: 1, Text: "/latest"})
	if err != nil {
		t.Fatalf("/latest returned error: %v", err)
	}

	lines := strings.Split(reply.Text, "\n")
	if len(lines) != 2 {
		t.Fatalf("/latest replied %q, want 2 lines, got %d — an undated item is still an item and must not be dropped", reply.Text, len(lines))
	}

	// The dated item, unchanged and still first.
	if !strings.Contains(lines[0], "spoke on housing") || !strings.Contains(lines[0], "3 February 2026") {
		t.Errorf("/latest line 1 is %q, want the dated item still reading 3 February 2026", lines[0])
	}

	// The undated item: last, honest about the gap, and still carrying its text.
	if !strings.Contains(lines[1], "asked about rail fares") {
		t.Errorf("/latest line 2 is %q, want the undated item last and still described", lines[1])
	}
	if !strings.Contains(lines[1], "date unknown") {
		t.Errorf("/latest line 2 is %q, want it to say the date is unknown rather than invent one", lines[1])
	}

	if strings.Contains(reply.Text, "0001") {
		t.Errorf("/latest replied %q, want no year-1 date anywhere — that is the zero value leaking, not a real timestamp", reply.Text)
	}
}

// Slice C8c (Phase C): two MPs' items INTERLEAVE by recency. This is the slice the whole
// C8 split was arranged around, and the one C8b was deliberately built to fail.
//
// C8b sorts each MP's items among themselves, INSIDE the per-MP loop. That satisfies every
// test written so far and is still wrong for a command called /latest: with two MPs
// followed, the reply is a run of one MP's items followed by a run of the other's, so an
// item from three weeks ago can sit above one from this morning purely because of the order
// the follows happen to be stored in. "Latest" has to mean latest across the whole reply.
//
// The fixture makes every cheap implementation produce a visibly different list. Each MP's
// items arrive OLDEST-first, and the two MPs' dates interleave (5th, 4th, 2nd, 1st alternates
// between them), so grouping and ordering are independent failures:
//
//	no sort at all         -> Cat 1 Feb, Cat 5 Feb, Ada 2 Feb, Ada 4 Feb
//	C8b as it stands today -> Cat 5 Feb, Cat 1 Feb, Ada 4 Feb, Ada 2 Feb
//	sorted OLDEST-first    -> Cat 1 Feb, Ada 2 Feb, Ada 4 Feb, Cat 5 Feb
//	interleaved, newest    -> Cat 5 Feb, Ada 4 Feb, Ada 2 Feb, Cat 1 Feb   <- the only pass
//
// ⭐ Every line is checked for its MP's NAME as well as its item text, and that is not
// belt-and-braces. Sorting across MPs means the sort can no longer live where `mp` and `a`
// are both in scope, so whatever carries an item past that point has to carry its MP with
// it. Attribution is therefore the thing most likely to break in this slice — an
// implementation that gets the ORDER right while pairing items with the wrong MP would
// otherwise pass, and would be a worse bug than the one being fixed.
func TestHandleUpdate_latest_twoMPs_itemsInterleaveByRecency(t *testing.T) {
	store := bot.NewMemoryStore()
	store.AddChat(1)
	store.FollowMP(1, bot.Member{ID: 101, Name: "Cat Smith"})
	store.FollowMP(1, bot.Member{ID: 102, Name: "Ada Clark"})

	day := func(d int) time.Time {
		return time.Date(2026, time.February, d, 9, 0, 0, 0, time.UTC)
	}

	source := fakeSource{items: map[int][]bot.Activity{
		101: {
			{ID: "q3", Text: "asked about rail fares", When: day(1)},
			{ID: "v9", Text: "voted on the finance bill", When: day(5)},
		},
		102: {
			{ID: "w2", Text: "tabled a question on ferries", When: day(2)},
			{ID: "s7", Text: "spoke on housing", When: day(4)},
		},
	}}

	// nil resolver: /latest looks up no names, and the nil enforces that it never tries.
	reply, err := bot.New(store, nil, source).HandleUpdate(bot.Update{ChatID: 1, Text: "/latest"})
	if err != nil {
		t.Fatalf("/latest returned error: %v", err)
	}

	want := []struct{ mp, item string }{
		{"Cat Smith", "voted on the finance bill"},    // 5 Feb
		{"Ada Clark", "spoke on housing"},             // 4 Feb
		{"Ada Clark", "tabled a question on ferries"}, // 2 Feb
		{"Cat Smith", "asked about rail fares"},       // 1 Feb
	}

	lines := strings.Split(reply.Text, "\n")
	if len(lines) != len(want) {
		t.Fatalf("/latest replied %q, want %d lines, got %d", reply.Text, len(want), len(lines))
	}

	for i, w := range want {
		if !strings.Contains(lines[i], w.item) {
			t.Errorf("/latest line %d is %q, want the item %q (full reply %q)", i+1, lines[i], w.item, reply.Text)
		}
		if !strings.Contains(lines[i], w.mp) {
			t.Errorf("/latest line %d is %q, want it attributed to %q — the item must keep its MP when the sort crosses MPs", i+1, lines[i], w.mp)
		}
	}
}

// Slice C8d (Phase C): /latest keeps only each MP's three most recent items.
//
// A per-MP cap rather than one cap over the whole reply, so that every followed MP is
// represented: with a single overall limit, two busy MPs could fill the reply and someone
// following twenty would never hear about most of them. The cost is that the reply still
// grows with the follow count, which is a real weakness of this shape and is why the
// message-length guard is only partial.
//
// ⭐ The fixture is built so that FOUR different wrong implementations each produce a
// visibly different reply. Cat Smith has five items, arriving in scrambled date order;
// Ada Clark has two, which is under the cap and so must survive untouched:
//
//	Cat, as fetched:  1 Feb, 9 Feb, 3 Feb, 7 Feb, 5 Feb
//
//	take the first 3 as fetched -> 1, 9, 3   (wrong: keeps the two oldest)
//	take the last 3 as fetched  -> 3, 7, 5   (wrong: drops the newest)
//	keep the OLDEST 3           -> 1, 3, 5   (wrong: exactly backwards)
//	keep the NEWEST 3           -> 9, 7, 5   <- the only right answer
//
// Scrambling matters as much here as it did in C8b: had the items arrived newest-first,
// "take the first three" would have passed while being the wrong rule entirely.
//
// ⭐ The two DROPPED items are asserted absent from the whole reply, not merely missing
// from their expected positions. That is what distinguishes a cap from a reordering — an
// implementation that merely sorted differently would leave them in the text somewhere.
//
// ⚠️ This test asserts the first five lines and a MINIMUM line count, deliberately not an
// exact one. The next slice adds a notice saying items were withheld, which will append a
// line; pinning the total here would mean rewriting this test to accommodate a behavior it
// is not about. Order and membership are what this slice owns.
func TestHandleUpdate_latest_keepsOnlyEachMPsThreeNewestItems(t *testing.T) {
	store := bot.NewMemoryStore()
	store.AddChat(1)
	store.FollowMP(1, bot.Member{ID: 101, Name: "Cat Smith"})
	store.FollowMP(1, bot.Member{ID: 102, Name: "Ada Clark"})

	day := func(d int) time.Time {
		return time.Date(2026, time.February, d, 9, 0, 0, 0, time.UTC)
	}

	source := fakeSource{items: map[int][]bot.Activity{
		// Five items, out of order, so the newest three are neither the first three
		// nor the last three as they arrive.
		101: {
			{ID: "c1", Text: "asked about rail fares", When: day(1)},
			{ID: "c2", Text: "voted on the finance bill", When: day(9)},
			{ID: "c3", Text: "spoke on housing", When: day(3)},
			{ID: "c4", Text: "tabled a question on ferries", When: day(7)},
			{ID: "c5", Text: "met the transport minister", When: day(5)},
		},
		// Two items — under the cap, so both must survive.
		102: {
			{ID: "a1", Text: "opposed the housing bill", When: day(8)},
			{ID: "a2", Text: "asked about school funding", When: day(2)},
		},
	}}

	// nil resolver: /latest looks up no names, and the nil enforces that it never tries.
	reply, err := bot.New(store, nil, source).HandleUpdate(bot.Update{ChatID: 1, Text: "/latest"})
	if err != nil {
		t.Fatalf("/latest returned error: %v", err)
	}

	// Cat's newest three and both of Ada's, still interleaved by recency across the two.
	want := []struct{ mp, item string }{
		{"Cat Smith", "voted on the finance bill"},    // 9 Feb
		{"Ada Clark", "opposed the housing bill"},     // 8 Feb
		{"Cat Smith", "tabled a question on ferries"}, // 7 Feb
		{"Cat Smith", "met the transport minister"},   // 5 Feb
		{"Ada Clark", "asked about school funding"},   // 2 Feb
	}

	lines := strings.Split(reply.Text, "\n")
	if len(lines) < len(want) {
		t.Fatalf("/latest replied %q, want at least %d lines, got %d", reply.Text, len(want), len(lines))
	}

	for i, w := range want {
		if !strings.Contains(lines[i], w.item) {
			t.Errorf("/latest line %d is %q, want the item %q (full reply %q)", i+1, lines[i], w.item, reply.Text)
		}
		if !strings.Contains(lines[i], w.mp) {
			t.Errorf("/latest line %d is %q, want it attributed to %q", i+1, lines[i], w.mp)
		}
	}

	// Cat's two oldest are over the cap and must be gone entirely, not merely demoted.
	for _, dropped := range []string{"spoke on housing", "asked about rail fares"} {
		if strings.Contains(reply.Text, dropped) {
			t.Errorf("/latest replied %q, want %q dropped — it is Cat Smith's 4th or 5th newest item", reply.Text, dropped)
		}
	}
}

// Slice C8d, second half: the reply says so when items were withheld.
//
// Without this, three items from an MP are indistinguishable from that MP having had a
// quiet fortnight. The cap is invisible to the person reading, which is the same objection
// C8b raised against rendering an unset date as "1 January 0001": a silent gap reads as a
// complete answer. The sentence chosen is "Showing each MP's 3 most recent items."
//
// ⭐ THREE cases, because the interesting behavior is when the notice is ABSENT. A test
// with only the over-cap case would pass an implementation that appends the sentence to
// every reply — including one listing a single item, where it would be simply false.
//
// ⭐ The middle case is the boundary and is the point of the table. An MP with EXACTLY
// three items has had nothing withheld, so there must be no notice. That is what separates
// `> maxItemsPerMP` from `>= maxItemsPerMP`, an off-by-one that no other case here catches
// and that would otherwise ship a reply claiming to have hidden something it did not.
//
// ⭐ The assertions match FRAGMENTS — "each MP" and "3 most recent" — rather than the whole
// sentence. Both halves of the meaning are pinned (per-MP, and the newest three) while
// punctuation and phrasing stay free, exactly as C8a pinned "3 Feb" rather than a layout.
// Checking the LAST line, not the whole reply, keeps the notice out of the item list; a
// blank spacer line before it would not break this.
func TestHandleUpdate_latest_saysWhenItemsWereWithheld(t *testing.T) {
	day := func(d int) time.Time {
		return time.Date(2026, time.February, d, 9, 0, 0, 0, time.UTC)
	}

	tests := []struct {
		name       string
		items      map[int][]bot.Activity
		wantNotice bool
	}{
		{
			name: "an MP has more than three items",
			items: map[int][]bot.Activity{
				101: {
					{ID: "c1", Text: "asked about rail fares", When: day(1)},
					{ID: "c2", Text: "voted on the finance bill", When: day(9)},
					{ID: "c3", Text: "spoke on housing", When: day(3)},
					{ID: "c4", Text: "tabled a question on ferries", When: day(7)},
				},
				102: {{ID: "a1", Text: "opposed the housing bill", When: day(8)}},
			},
			wantNotice: true,
		},
		{
			// The boundary: three is not "more than three". Nothing was withheld.
			name: "an MP has exactly three items",
			items: map[int][]bot.Activity{
				101: {
					{ID: "c1", Text: "asked about rail fares", When: day(1)},
					{ID: "c2", Text: "voted on the finance bill", When: day(9)},
					{ID: "c3", Text: "spoke on housing", When: day(3)},
				},
				102: {{ID: "a1", Text: "opposed the housing bill", When: day(8)}},
			},
			wantNotice: false,
		},
		{
			name: "every MP has fewer than three items",
			items: map[int][]bot.Activity{
				101: {{ID: "c2", Text: "voted on the finance bill", When: day(9)}},
				102: {{ID: "a1", Text: "opposed the housing bill", When: day(8)}},
			},
			wantNotice: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := bot.NewMemoryStore()
			store.AddChat(1)
			store.FollowMP(1, bot.Member{ID: 101, Name: "Cat Smith"})
			store.FollowMP(1, bot.Member{ID: 102, Name: "Ada Clark"})

			// nil resolver: /latest looks up no names, and the nil enforces it never tries.
			reply, err := bot.New(store, nil, fakeSource{items: tt.items}).
				HandleUpdate(bot.Update{ChatID: 1, Text: "/latest"})
			if err != nil {
				t.Fatalf("/latest returned error: %v", err)
			}

			lines := strings.Split(reply.Text, "\n")
			last := lines[len(lines)-1]

			if tt.wantNotice {
				if !strings.Contains(last, "each MP") || !strings.Contains(last, "3 most recent") {
					t.Errorf("/latest replied %q, want the last line to say each MP's 3 most recent items are shown", reply.Text)
				}
				// The notice is an addition, not a replacement: the newest item still leads.
				if !strings.Contains(lines[0], "voted on the finance bill") {
					t.Errorf("/latest line 1 is %q, want the newest item still first", lines[0])
				}
				return
			}

			if strings.Contains(reply.Text, "each MP") || strings.Contains(reply.Text, "3 most recent") {
				t.Errorf("/latest replied %q, want no withholding notice — nothing was withheld", reply.Text)
			}
		})
	}
}

// Slice C5 (Phase C): activity is fetched by member ID, not by display name. Two sitting
// MPs can share one display name — B3's live probing of the Members API found "Smith"
// matching 11 sitting members, and matching is substring, so ambiguity cannot be filtered
// away. Polling by name therefore cannot tell two such MPs apart, and every follower of
// either one would be sent both MPs' activity.
//
// The two followers are in SEPARATE chats deliberately. Already-sent suppression is
// per-chat, so it cannot quietly absorb the duplicate the way it would if one chat
// followed both: each chat's extra item would be a first sighting for that chat and
// arrive. That makes the misrouting visible rather than masked.
//
// This is also what the durable ID stored since C0 is ultimately for. A follow record's
// name is a snapshot from the moment the user followed and nothing keeps it in sync;
// the ID is the only part of it that Parliament will still recognise later.
func TestCheckActivity_twoMPsShareADisplayName_eachChatGetsOnlyItsOwn(t *testing.T) {
	store := bot.NewMemoryStore()
	store.AddChat(1)
	store.AddChat(2)

	// Same display name, different people.
	store.FollowMP(1, bot.Member{ID: 101, Name: "John Smith"})
	store.FollowMP(2, bot.Member{ID: 102, Name: "John Smith"})

	source := fakeSource{items: map[int][]bot.Activity{
		101: {{ID: "s7", Text: "spoke on housing"}},
		102: {{ID: "v42", Text: "voted on Bill 42"}},
	}}

	replies := bot.New(store, nil, source).CheckActivity()

	// One item each. Under name-based polling both chats would receive both items.
	if len(replies) != 2 {
		t.Fatalf("got %d replies, want 2 (one per chat)", len(replies))
	}

	// Assert which chat got WHICH item, not just how many arrived. A count alone would
	// pass if the two items were delivered to the same chat, or swapped between them.
	want := map[int64]string{
		1: "spoke on housing",
		2: "voted on Bill 42",
	}
	got := make(map[int64]string, len(replies))
	for _, r := range replies {
		if _, dup := got[r.ChatID]; dup {
			t.Fatalf("chat %d received more than one item", r.ChatID)
		}
		got[r.ChatID] = r.Text
	}
	for chatID, text := range want {
		if !strings.Contains(got[chatID], text) {
			t.Errorf("chat %d got %q, want it to mention %q", chatID, got[chatID], text)
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

	source := fakeSource{items: map[int][]bot.Activity{
		4514: {{ID: "v42", Text: "voted on Bill 42"}},
	}}

	b := bot.New(store, nil, source)

	first := b.CheckActivity()
	if len(first) != 1 {
		t.Fatalf("first poll: got %d replies, want 1", len(first))
	}

	second := b.CheckActivity()
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

	source := fakeSource{items: map[int][]bot.Activity{
		4514: {{ID: "v42", Text: "voted on Bill 42"}},
	}}

	replies := bot.New(store, nil, source).CheckActivity()

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
	b := bot.New(store, knownMPs(mps...), nil)

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
	b := bot.New(store, knownMPs("Keir Starmer"), nil)

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
	confirm, err := bot.New(bot.NewMemoryStore(), knownMPs("Keir Starmer"), nil).HandleUpdate(bot.Update{ChatID: 1, Text: "/follow Keir Starmer"})
	if err != nil {
		t.Fatalf("HandleUpdate(/follow <name>) returned error: %v", err)
	}

	for _, text := range []string{"/follow", "/follow   "} {
		t.Run(text, func(t *testing.T) {
			store := bot.NewMemoryStore()
			b := bot.New(store, nil, nil)

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
	b := bot.New(store, knownMPs(removed, kept), nil)

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
	confirmBot := bot.New(confirmStore, knownMPs(realName), nil)
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
			b := bot.New(store, knownMPs(followed), nil)
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
	followingBot := bot.New(following, knownMPs(mp), nil)
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
	b := bot.New(store, knownMPs(other), nil)
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
	soloBot := bot.New(solo, knownMPs(target), nil)
	if _, err := soloBot.HandleUpdate(bot.Update{ChatID: 1, Text: "/follow " + target}); err != nil {
		t.Fatalf("solo follow setup failed: %v", err)
	}
	success, err := soloBot.HandleUpdate(bot.Update{ChatID: 1, Text: "/unfollow " + target})
	if err != nil {
		t.Fatalf("solo unfollow failed: %v", err)
	}

	// The chat under test follows the target AND someone else, then unfollows the target.
	store := bot.NewMemoryStore()
	b := bot.New(store, knownMPs(target, other), nil)
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
	b := bot.New(store, knownMPs("Keir Starmer", "Rishi Sunak"), nil)

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
	b := bot.New(store, &fakeResolver{matches: map[string][]bot.Member{"Smith": smiths}}, nil)

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
	b := bot.New(store, resolver, nil)

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
	noMatches, err := bot.New(store, &fakeResolver{matches: canned}, nil).
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
			b := bot.New(store, resolver, nil)

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
	b := bot.New(store, resolver, nil)

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
	noMatches, err := bot.New(store, &fakeResolver{matches: canned}, nil).
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
	b := bot.New(store, resolver, nil)

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
	b := bot.New(store, resolver, nil)

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
	b := bot.New(store, nil, nil)
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
	b := bot.New(store, nil, nil)

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
	b := bot.New(store, nil, nil)

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
	welcome, err := bot.New(bot.NewMemoryStore(), nil, nil).HandleUpdate(bot.Update{ChatID: 1, Text: "/start"})
	if err != nil {
		t.Fatalf("HandleUpdate(/start) returned error: %v", err)
	}

	store := bot.NewMemoryStore()
	b := bot.New(store, nil, nil)
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
	fallback, err := bot.New(bot.NewMemoryStore(), nil, nil).HandleUpdate(bot.Update{ChatID: 1, Text: "not-a-command"})
	if err != nil {
		t.Fatalf("HandleUpdate(unknown) returned error: %v", err)
	}

	store := bot.NewMemoryStore()
	b := bot.New(store, nil, nil)
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
	fallback, err := bot.New(bot.NewMemoryStore(), nil, nil).HandleUpdate(bot.Update{ChatID: 1, Text: "not-a-command"})
	if err != nil {
		t.Fatalf("HandleUpdate(unknown) returned error: %v", err)
	}

	store := bot.NewMemoryStore()
	b := bot.New(store, nil, nil)
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
	fallback, err := bot.New(bot.NewMemoryStore(), nil, nil).HandleUpdate(bot.Update{ChatID: 1, Text: "not-a-command"})
	if err != nil {
		t.Fatalf("HandleUpdate(unknown) returned error: %v", err)
	}

	store := bot.NewMemoryStore()
	b := bot.New(store, nil, nil)
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
			b := bot.New(store, nil, nil)

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
	confirm, err := bot.New(confirmStore, knownMPs("Keir Starmer"), nil).HandleUpdate(bot.Update{ChatID: 9, Text: "/follow Keir Starmer"})
	if err != nil {
		t.Fatalf("HandleUpdate(/follow Keir Starmer) returned error: %v", err)
	}

	store := bot.NewMemoryStore()
	// The resolver knows Keir Starmer and nobody else, so any other name resolves to no
	// matches — exactly what the live API returns for a name nobody has.
	b := bot.New(store, knownMPs("Keir Starmer"), nil)

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
	b := bot.New(store, &fakeResolver{matches: matches}, nil)

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
			chosen := bot.New(chosenStore, &fakeResolver{matches: matches}, nil)

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
	b := bot.New(store, knownMPs("Keir Starmer"), nil)

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
	missBot := bot.New(missStore, knownMPs(unrelated), nil)
	if _, err := missBot.HandleUpdate(bot.Update{ChatID: 1, Text: "/follow " + unrelated}); err != nil {
		t.Fatalf("reference follow setup failed: %v", err)
	}
	notFollowing, err := missBot.HandleUpdate(bot.Update{ChatID: 1, Text: "/unfollow " + typed})
	if err != nil {
		t.Fatalf("reference unfollow failed: %v", err)
	}

	// The chat under test follows exactly one MP, whose name CONTAINS the typed fragment.
	store := bot.NewMemoryStore()
	b := bot.New(store, knownMPs(followed), nil)
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
		b := bot.New(store, knownMPs(names...), nil)
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
