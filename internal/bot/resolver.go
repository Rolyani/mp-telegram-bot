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

func NewResolver(baseURL string) *Resolver {
	return &Resolver{
		baseURL: baseURL,
		client:  &http.Client{},
	}
}

func (r *Resolver) ResolveName(name string) (int, error) {
	// Constrain the search to sitting Commons members: the API otherwise searches both
	// houses and all time, returning peers and former members who have no activity to follow.
	values := url.Values{}
	values.Set("Name", name)
	values.Set("House", "1") // 1 = Commons, 2 = Lords
	values.Set("IsCurrentMember", "true")

	endpoint := r.baseURL + "/api/Members/Search?" + values.Encode()

	resp, err := r.client.Get(endpoint)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("members API returned %s", resp.Status)
	}

	var payload struct {
		Items []struct {
			Value struct {
				ID int `json:"id"`
			} `json:"value"`
		} `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return 0, err
	}

	if len(payload.Items) == 0 {
		return 0, fmt.Errorf("no MP found for %q", name)
	}

	return payload.Items[0].Value.ID, nil
}
