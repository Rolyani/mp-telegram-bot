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
	endpoint := r.baseURL + "/api/Members/Search?Name=" + url.QueryEscape(name)

	resp, err := r.client.Get(endpoint)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

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
