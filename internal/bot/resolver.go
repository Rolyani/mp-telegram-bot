package bot

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
)

type Resolver struct {
	baseURL string
	client  *http.Client
}

// Member is one MP as the bot cares about them: a durable ID to follow activity by, and the
// display name a user needs in order to tell two similarly-named MPs apart.
type Member struct {
	ID   int
	Name string
}

func NewResolver(baseURL string) *Resolver {
	return &Resolver{
		baseURL: baseURL,
		client:  &http.Client{},
	}
}

func (r *Resolver) ResolveName(name string) ([]Member, error) {
	// Constrain the search to sitting Commons members: the API otherwise searches both
	// houses and all time, returning peers and former members who have no activity to follow.
	values := url.Values{}
	values.Set("Name", name)
	values.Set("House", "1") // 1 = Commons, 2 = Lords
	values.Set("IsCurrentMember", "true")

	endpoint := r.baseURL + "/api/Members/Search?" + values.Encode()

	resp, err := r.client.Get(endpoint)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("members API returned %s", resp.Status)
	}

	var payload struct {
		Items []struct {
			Value struct {
				ID            int    `json:"id"`
				NameDisplayAs string `json:"nameDisplayAs"`
			} `json:"value"`
		} `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, err
	}

	// Translate the API's wire shape into our own, one match per item. Every match is kept:
	// the API matches a name as a substring anywhere in the full name, so a common surname
	// legitimately returns several sitting MPs and the resolver has no better rule than the
	// API's for picking between them. Choosing is the caller's job, not ours.
	var members []Member
	for _, item := range payload.Items {
		members = append(members, Member{
			ID:   item.Value.ID,
			Name: item.Value.NameDisplayAs,
		})
	}

	return members, nil
}
