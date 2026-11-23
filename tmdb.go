package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

const defaultAPIURL = "https://api.themoviedb.org"

var httpClient = &http.Client{Timeout: 10 * time.Second}

// tmdbClient talks to the TMDB v3 API at baseURL, a host root such as
// defaultAPIURL or an authenticating proxy for one. apiKey is optional when
// the proxy adds credentials itself.
type tmdbClient struct {
	baseURL string
	apiKey  string
}

func newTMDBClient(baseURL, apiKey string) *tmdbClient {
	if baseURL == "" {
		baseURL = defaultAPIURL
	}
	return &tmdbClient{baseURL: strings.TrimRight(baseURL, "/") + "/3", apiKey: apiKey}
}

type personCredit struct {
	CreditType   string `json:"-"` // "cast" or "crew"
	MediaType    string `json:"media_type"`
	ID           int    `json:"id"`
	ReleaseDate  string `json:"release_date"`
	FirstAirDate string `json:"first_air_date"`
	Video        bool   `json:"video"`
	Character    string `json:"character"`
	Order        int    `json:"order"`
	Department   string `json:"department"`
}

type media struct {
	MediaType    string       `json:"-"` // "movie" or "tv"
	ID           int          `json:"id"`
	Title        string       `json:"title"`
	Name         string       `json:"name"`
	Status       string       `json:"status"`
	ReleaseDate  string       `json:"release_date"`
	FirstAirDate string       `json:"first_air_date"`
	Credits      mediaCredits `json:"credits"`
	ExternalIDs  struct {
		IMDbID string `json:"imdb_id"`
	} `json:"external_ids"`
}

func (m *media) displayTitle() string {
	if m.MediaType == "tv" {
		return m.Name
	}
	return m.Title
}

type mediaCredits struct {
	Cast []struct {
		ID   int    `json:"id"`
		Name string `json:"name"`
	} `json:"cast"`
	Crew []struct {
		ID   int    `json:"id"`
		Name string `json:"name"`
		Job  string `json:"job"`
	} `json:"crew"`
}

type discoverResult struct {
	ID          int    `json:"id"`
	ReleaseDate string `json:"release_date"`
}

// personCredits returns a person's cast and crew credits. Any fetch failure
// is logged and skips this person rather than aborting the whole run.
func (c *tmdbClient) personCredits(personID int) []personCredit {
	var resp struct {
		Cast []personCredit `json:"cast"`
		Crew []personCredit `json:"crew"`
	}
	if err := c.getJSON(fmt.Sprintf("%s/person/%d/combined_credits", c.baseURL, personID), &resp); err != nil {
		slog.Error(fmt.Sprintf("Error fetching person '%d' credits: %v", personID, err))
		return nil
	}
	credits := make([]personCredit, 0, len(resp.Cast)+len(resp.Crew))
	for _, credit := range resp.Cast {
		credit.CreditType = "cast"
		credits = append(credits, credit)
	}
	for _, credit := range resp.Crew {
		credit.CreditType = "crew"
		credits = append(credits, credit)
	}
	return credits
}

// discoverCompanyMovies returns upcoming movies from a company. A fetch
// failure is logged and returns the pages fetched so far.
func (c *tmdbClient) discoverCompanyMovies(companyID int, today time.Time) []discoverResult {
	url := fmt.Sprintf("%s/discover/movie?&release_date.gte=%s&sort_by=primary_release_date.asc&with_companies=%d",
		c.baseURL, today.Format(dateLayout), companyID)
	var results []discoverResult
	for page := 1; ; page++ {
		var resp struct {
			Page       int              `json:"page"`
			Results    []discoverResult `json:"results"`
			TotalPages int              `json:"total_pages"`
		}
		if err := c.getJSON(fmt.Sprintf("%s&page=%d", url, page), &resp); err != nil {
			slog.Error(fmt.Sprintf("Error discovering company '%d' media: %v", companyID, err))
			return results
		}
		results = append(results, resp.Results...)
		if resp.Page >= resp.TotalPages {
			return results
		}
	}
}

func (c *tmdbClient) media(mediaType string, id int) (*media, error) {
	var m media
	url := fmt.Sprintf("%s/%s/%d?append_to_response=credits,external_ids", c.baseURL, mediaType, id)
	if err := c.getJSON(url, &m); err != nil {
		return nil, err
	}
	m.MediaType = mediaType
	return &m, nil
}

func (c *tmdbClient) getJSON(url string, v any) error {
	slog.Info("Fetch " + url)
	if c.apiKey != "" {
		sep := "?"
		if strings.Contains(url, "?") {
			sep = "&"
		}
		url += sep + "api_key=" + c.apiKey
	}
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("HTTP Error %d: %s", resp.StatusCode, http.StatusText(resp.StatusCode))
	}
	return json.NewDecoder(resp.Body).Decode(v)
}
