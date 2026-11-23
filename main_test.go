package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestNamesToSentence(t *testing.T) {
	tests := []struct {
		names []string
		want  string
	}{
		{[]string{"Alice"}, "Alice"},
		{[]string{"Bob", "Alice"}, "Alice and Bob"},
		{[]string{"Carol", "Alice", "Bob"}, "Alice, Bob, and Carol"},
	}
	for _, tt := range tests {
		set := map[string]bool{}
		for _, n := range tt.names {
			set[n] = true
		}
		if got := namesToSentence(set); got != tt.want {
			t.Errorf("namesToSentence(%v) = %q, want %q", tt.names, got, tt.want)
		}
	}
}

func TestSelfCharacter(t *testing.T) {
	for character, want := range map[string]bool{
		"Himself":                 true,
		"Self - Host":             true,
		"Narrator (uncredited)":   true,
		"Archive Footage":         true,
		"Interviewee":             true,
		"Captain Miller":          false,
		"":                        false,
		"Herself (archive)":       true,
		"Shelf Stocker":           false,
		"Dr. Selfridge (voice)":   true, // substring match, as in Python
		"Ellen Ripley":            false,
		"Mission Control (voice)": false,
	} {
		if got := selfCharacter(character); got != want {
			t.Errorf("selfCharacter(%q) = %v, want %v", character, got, want)
		}
	}
}

func TestReadIDs(t *testing.T) {
	ids, err := readIDs(strings.NewReader("31-tom-hanks\n\n488-steven-spielberg\n31-tom-hanks\n13184\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := sortedIDs(ids); len(got) != 3 || got[0] != 31 || got[1] != 488 || got[2] != 13184 {
		t.Errorf("readIDs = %v", got)
	}
	if _, err := readIDs(strings.NewReader("abc-def\n")); err == nil {
		t.Error("readIDs accepted a non-numeric ID")
	}
}

func TestContentText(t *testing.T) {
	var m media
	mustUnmarshal(t, `{
		"title": "Café Night", "release_date": "2027-03-14",
		"credits": {
			"cast": [{"id": 1, "name": "Pat Person"}, {"id": 2, "name": "Other Actor"}],
			"crew": [{"id": 5, "name": "Zed Director", "job": "Director"}, {"id": 5, "name": "Zed Director", "job": "Writer"}]
		}
	}`, &m)
	people := map[int]bool{1: true, 5: true}
	if got, want := movieContentText(&m, people), `"Café Night" directed by Zed Director along with Pat Person. Coming March 2027.`; got != want {
		t.Errorf("movieContentText = %q, want %q", got, want)
	}

	m = media{Title: "Untitled"}
	if got, want := movieContentText(&m, people), `"Untitled".`; got != want {
		t.Errorf("movieContentText = %q, want %q", got, want)
	}

	var tv media
	mustUnmarshal(t, `{
		"name": "Show", "first_air_date": "2027-01-02",
		"credits": {"cast": [{"id": 1, "name": "Pat Person"}], "crew": [{"id": 3, "name": "Ann Writer", "job": "Writer"}]}
	}`, &tv)
	if got, want := tvContentText(&tv, map[int]bool{1: true, 3: true}), `"Show" with Ann Writer and Pat Person. Coming January 2027.`; got != want {
		t.Errorf("tvContentText = %q, want %q", got, want)
	}
}

func TestCache(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "cache.json")
	// Written by the Python implementation.
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`[["a", "1"], ["b", "2"], ["c", "3"]]`), 0o644); err != nil {
		t.Fatal(err)
	}

	c, err := loadCache(path, 3)
	if err != nil {
		t.Fatal(err)
	}
	if got := c.getOrLoad("a", func() string { t.Fatal("loaded cached key"); return "" }); got != "1" {
		t.Errorf("cached a = %q", got)
	}
	if got := c.getOrLoad("d", func() string { return "4" }); got != "4" {
		t.Errorf("loaded d = %q", got)
	}
	if err := c.close(); err != nil {
		t.Fatal(err)
	}

	// "b" was least recently used, so it is evicted; "a" moved to the end.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(data), `[["c","3"],["a","1"],["d","4"]]`; got != want {
		t.Errorf("cache file = %s, want %s", got, want)
	}

	if c, err := loadCache(filepath.Join(t.TempDir(), "missing.json"), 3); err != nil || len(c.entries) != 0 {
		t.Errorf("loadCache(missing) = %v, %v", c, err)
	}
}

func TestNewUUID(t *testing.T) {
	re := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	if id := newUUID(); !re.MatchString(id) {
		t.Errorf("newUUID() = %q", id)
	}
}

var fixtures = map[string]string{
	"/person/1/combined_credits": `{
		"cast": [
			{"media_type": "movie", "id": 100, "release_date": "2027-05-01", "character": "Hero", "order": 0},
			{"media_type": "movie", "id": 101, "release_date": "2027-05-01", "character": "Hero", "order": 0, "video": true},
			{"media_type": "movie", "id": 102, "release_date": "2026-11-23", "character": "Hero", "order": 0},
			{"media_type": "movie", "id": 103, "release_date": "", "character": "Extra", "order": 20},
			{"media_type": "tv", "id": 200, "first_air_date": "", "character": "Himself"},
			{"media_type": "tv", "id": 201, "first_air_date": "", "character": "Lead"}
		],
		"crew": [
			{"media_type": "movie", "id": 104, "release_date": "", "department": "Sound"},
			{"media_type": "movie", "id": 100, "release_date": "2027-05-01", "department": "Directing"},
			{"media_type": "tv", "id": 202, "first_air_date": "2027-02-01", "department": "Writing"}
		]
	}`,
	"/person/2/combined_credits": `not json`,
	"/discover/movie?page=1":     `{"page": 1, "total_pages": 2, "results": [{"id": 105, "release_date": "2027-03-01"}, {"id": 102, "release_date": "2026-11-01"}]}`,
	"/discover/movie?page=2":     `{"page": 2, "total_pages": 2, "results": [{"id": 106, "release_date": ""}]}`,
	"/movie/100": `{"id": 100, "title": "Big Movie", "status": "In Production", "release_date": "2027-05-01",
		"external_ids": {"imdb_id": "tt100"},
		"credits": {"cast": [{"id": 1, "name": "Pat Person"}], "crew": [{"id": 1, "name": "Pat Person", "job": "Director"}]}}`,
	"/movie/105": `{"id": 105, "title": "Studio <Film> & Co", "status": "Post Production", "release_date": "2027-03-01",
		"external_ids": {"imdb_id": "tt105"}, "credits": {"cast": [], "crew": []}}`,
	"/movie/106": `{"id": 106, "title": "Planned", "status": "Planned", "external_ids": {"imdb_id": "tt106"}, "credits": {}}`,
	"/tv/201":    `{"id": 201, "name": "No IMDb", "status": "In Production", "external_ids": {"imdb_id": null}, "credits": {}}`,
	"/tv/202": `{"id": 202, "name": "Série", "status": "In Production", "first_air_date": "2027-02-01",
		"external_ids": {"imdb_id": "tt202"}, "credits": {"cast": [], "crew": [{"id": 1, "name": "Pat Person", "job": "Writer"}]}}`,
}

func TestRun(t *testing.T) {
	var requested []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("api_key") != "secret" {
			t.Errorf("missing api_key on %s", r.URL)
		}
		key := r.URL.Path
		if r.URL.Path == "/discover/movie" {
			if r.URL.Query().Get("with_companies") != "9" || r.URL.Query().Get("release_date.gte") != todayUTC().Format(dateLayout) {
				t.Errorf("unexpected discover query %s", r.URL.RawQuery)
			}
			key += "?page=" + r.URL.Query().Get("page")
		}
		requested = append(requested, key)
		body, ok := fixtures[key]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(body))
	}))
	defer srv.Close()

	oldBase, oldNow := apiBase, now
	apiBase = srv.URL
	now = func() time.Time { return time.Date(2026, 11, 23, 13, 0, 0, 500, time.FixedZone("PST", -8*3600)) }
	defer func() { apiBase, now = oldBase, oldNow }()

	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "people.csv"), "1-pat-person\n2-broken\n")
	writeFile(t, filepath.Join(dir, "companies.csv"), "9-studio\n")
	opts := options{
		peopleFile:    filepath.Join(dir, "people.csv"),
		companiesFile: filepath.Join(dir, "companies.csv"),
		outputFile:    filepath.Join(dir, "out.json"),
		cacheFile:     filepath.Join(dir, "cache.json"),
		apiKey:        "secret",
	}
	if err := run(opts); err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{"/movie/101", "/movie/102", "/movie/103", "/movie/104", "/tv/200"} {
		for _, r := range requested {
			if r == path {
				t.Errorf("filtered credit %s was fetched", path)
			}
		}
	}

	raw, err := os.ReadFile(opts.outputFile)
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasSuffix(string(raw), "\n") {
		t.Error("output has a trailing newline")
	}
	if !strings.Contains(string(raw), "\n    \"items\": [\n        {\n            \"id\": ") {
		t.Errorf("output is not indented with four spaces:\n%s", raw)
	}
	if !strings.Contains(string(raw), `Studio <Film> & Co`) || !strings.Contains(string(raw), `Série`) {
		t.Errorf("output escapes HTML or non-ASCII:\n%s", raw)
	}

	var got feed
	mustUnmarshal(t, string(raw), &got)
	if got.Version != "https://jsonfeed.org/version/1.1" || got.Title != "Movies in Production" {
		t.Errorf("feed header = %+v", got)
	}
	byURL := map[string]item{}
	for i, it := range got.Items {
		if i > 0 && got.Items[i-1].ID > it.ID {
			t.Error("items not sorted by id")
		}
		if it.DatePublished != "2026-11-23T21:00:00+00:00" {
			t.Errorf("date_published = %q", it.DatePublished)
		}
		byURL[it.URL] = it
	}
	want := map[string]string{
		"https://www.imdb.com/title/tt100/": `"Big Movie" directed by Pat Person. Coming May 2027.`,
		"https://www.imdb.com/title/tt105/": `"Studio <Film> & Co". Coming March 2027.`,
		"https://www.imdb.com/title/tt202/": `"Série" with Pat Person. Coming February 2027.`,
	}
	if len(byURL) != len(want) || len(got.Items) != len(want) {
		t.Errorf("got items %+v", got.Items)
	}
	for url, text := range want {
		if byURL[url].ContentText != text {
			t.Errorf("%s content_text = %q, want %q", url, byURL[url].ContentText, text)
		}
	}

	// A second run reuses cached ids and dates.
	now = func() time.Time { return time.Date(2026, 11, 24, 13, 0, 0, 0, time.UTC) }
	if err := run(opts); err != nil {
		t.Fatal(err)
	}
	raw2, err := os.ReadFile(opts.outputFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != string(raw2) {
		t.Errorf("second run changed output:\n%s\n---\n%s", raw, raw2)
	}
}

func mustUnmarshal(t *testing.T, s string, v any) {
	t.Helper()
	if err := json.Unmarshal([]byte(s), v); err != nil {
		t.Fatal(err)
	}
}

func writeFile(t *testing.T, path, s string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}
