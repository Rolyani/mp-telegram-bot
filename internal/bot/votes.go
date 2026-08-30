package bot

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"time"
)

// parliamentTime is the layout the Commons Votes API dates arrive in: "2026-07-14T17:57:00".
//
// ⚠️ It is NOT RFC3339 — there is no timezone offset — so time.Parse(time.RFC3339, …) fails
// on it, and fails in the worst possible way: it returns the ZERO time alongside its error.
// Ignore the error and every division is dated 1 January year 1, which /latest faithfully
// renders as "date unknown" for everything, forever, with the whole suite still green.
const parliamentTime = "2006-01-02T15:04:05"

// VotesSource fetches an MP's Commons division votes. It satisfies ActivitySource.
type VotesSource struct {
	baseURL string
	client  *http.Client
}

func NewVotesSource(baseURL string) *VotesSource {
	return &VotesSource{
		baseURL: baseURL,
		client:  &http.Client{},
	}
}

// Activity returns the MP's recent divisions, newest first as the API supplies them.
//
// ⚠️ It reports no error, because ActivitySource gives it nowhere to put one — so every
// failure here is logged to stderr and answered with a nil slice, which the caller cannot
// tell from "this MP has not voted recently". That is a known and deliberate gap: /latest
// went to the trouble of telling its two empty cases apart (slice C7) and this undoes that
// distinction. Giving ActivitySource an error return is its own slice.
func (v *VotesSource) Activity(memberID int) []Activity {
	values := url.Values{}
	values.Set("queryParameters.memberId", strconv.Itoa(memberID))
	endpoint := v.baseURL + "/data/divisions.json/membervoting?" + values.Encode()

	resp, err := v.client.Get(endpoint)
	if err != nil {
		fmt.Fprintln(os.Stderr, "votes API:", err)
		return nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		fmt.Fprintln(os.Stderr, "votes API returned", resp.Status)
		return nil
	}

	// A BARE ARRAY, unlike the Members API's {"items": […]} — hence []struct and not struct.
	// No json tags: the wire names already match these field names, and Go matches them
	// case-insensitively. resolver.go needs tags only because its wire names genuinely differ.
	var payload []struct {
		MemberVotedNo     bool
		PublishedDivision struct {
			DivisionId int
			Date       string
			Title      string
		}
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		fmt.Fprintln(os.Stderr, "votes API:", err)
		return nil
	}

	// Translate the API's wire shape into ours, one Activity per division.
	var items []Activity
	for _, voted := range payload {
		division := voted.PublishedDivision

		// An unparsable date leaves when as the zero time, which is exactly what /latest
		// already knows how to render ("date unknown"), so the item is still worth
		// returning — a division we cannot date is better than a division we drop. The
		// log is there so a format change is noticed rather than silently absorbed.
		when, err := time.Parse(parliamentTime, division.Date)
		if err != nil {
			fmt.Fprintln(os.Stderr, "votes API: undated division", division.DivisionId, err)
		}

		// How they voted, then what it was.
		position := "Voted Aye on: "
		if voted.MemberVotedNo {
			position = "Voted No on: "
		}

		items = append(items, Activity{
			ID:   strconv.Itoa(division.DivisionId),
			Text: position + division.Title,
			When: when,
		})
	}

	return items
}
