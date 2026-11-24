// Command tmdb-upcoming writes a JSON Feed of movies and TV shows in
// production for a list of TMDB people and companies.
package main

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

const version = "0.0.0"

const (
	dateLayout      = "2006-01-02"
	timestampLayout = "2006-01-02T15:04:05-07:00"
	monthLayout     = "January 2006"
	cacheMaxItems   = 1000
)

// now is replaced in tests.
var now = time.Now

func main() {
	var opts options
	flag.StringVar(&opts.peopleFile, "people-file", "", "file of TMDB person IDs")
	flag.StringVar(&opts.companiesFile, "companies-file", "", "file of TMDB company IDs")
	flag.StringVar(&opts.outputFile, "output-file", "-", "feed output path, or - for stdout")
	flag.StringVar(&opts.apiKey, "api-key", os.Getenv("TMDB_API_KEY"), "TMDB API key (default $TMDB_API_KEY)")
	flag.StringVar(&opts.apiURL, "api-url", os.Getenv("TMDB_API_URL"), "TMDB API base URL, e.g. an authenticating proxy (default $TMDB_API_URL or "+defaultAPIURL+")")
	flag.StringVar(&opts.cacheFile, "cache-file", os.Getenv("CACHE_FILE"), "cache path (default $CACHE_FILE)")
	flag.BoolVar(&opts.verbose, "verbose", false, "enable debug logging")
	flag.BoolVar(&opts.verbose, "v", false, "shorthand for --verbose")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		return
	}

	if opts.apiKey == "" && opts.apiURL == "" {
		fmt.Fprintln(os.Stderr, "TMDB_API_URL or TMDB_API_KEY must be set")
		flag.Usage()
		os.Exit(2)
	}

	level := slog.LevelInfo
	if opts.verbose {
		level = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))

	if err := run(opts); err != nil {
		slog.Error(err.Error())
		os.Exit(1)
	}
}

type options struct {
	peopleFile    string
	companiesFile string
	outputFile    string
	apiKey        string
	apiURL        string
	cacheFile     string
	verbose       bool
}

type feed struct {
	Version string `json:"version"`
	Title   string `json:"title"`
	Icon    string `json:"icon"`
	Items   []item `json:"items"`
}

type item struct {
	ID            string `json:"id"`
	URL           string `json:"url"`
	Title         string `json:"title"`
	ContentText   string `json:"content_text"`
	DatePublished string `json:"date_published"`
}

func run(opts options) error {
	c, err := loadCache(opts.cacheFile, cacheMaxItems)
	if err != nil {
		return err
	}
	peopleIDs, err := readIDsFile(opts.peopleFile)
	if err != nil {
		return err
	}
	companyIDs, err := readIDsFile(opts.companiesFile)
	if err != nil {
		return err
	}
	client := newTMDBClient(opts.apiURL, opts.apiKey)

	f := feed{
		Version: "https://jsonfeed.org/version/1.1",
		Title:   "Movies in Production",
		Icon:    "https://m.media-amazon.com/images/G/01/imdb/images-ANDW73HA/favicon_iPhone_retina_180x180._CB1582158069_.png",
		Items:   []item{},
	}

	refs, err := discoverCredits(client, peopleIDs, companyIDs)
	if err != nil {
		return err
	}
	for _, ref := range refs {
		m, err := client.media(ref.mediaType, ref.id)
		if isNotFound(err) {
			// Deleted from TMDB since the credit was listed.
			slog.Warn(fmt.Sprintf("Skip '%s/%d' not found", ref.mediaType, ref.id))
			continue
		} else if err != nil {
			return err
		}
		if it, ok := buildItem(m, peopleIDs, c); ok {
			f.Items = append(f.Items, it)
		}
	}

	sort.SliceStable(f.Items, func(i, j int) bool { return f.Items[i].ID < f.Items[j].ID })

	if err := c.close(); err != nil {
		return err
	}
	return writeFeed(opts.outputFile, f)
}

func buildItem(m *media, peopleIDs map[int]bool, c *cache) (item, bool) {
	title := m.displayTitle()
	if title == "" {
		slog.Error(fmt.Sprintf("Skip '%s/%d' missing title", m.MediaType, m.ID))
		return item{}, false
	}

	imdbID := m.ExternalIDs.IMDbID
	if imdbID == "" {
		slog.Debug(fmt.Sprintf("Skip '%s' missing IMDb ID", title))
		return item{}, false
	}

	if m.Status != "In Production" && m.Status != "Post Production" {
		slog.Debug(fmt.Sprintf("Skip '%s', not in production", title))
		return item{}, false
	}

	releaseEstimate := ""
	if m.MediaType == "movie" {
		if d, ok := parseDate(m.ReleaseDate); ok {
			releaseEstimate = d.Format(monthLayout)
		}
	}

	var contentText string
	switch m.MediaType {
	case "movie":
		contentText = movieContentText(m, peopleIDs)
	case "tv":
		contentText = tvContentText(m, peopleIDs)
	}

	// Generate a unique key for any major changes to the item
	itemKey := fmt.Sprintf("%s:%s:%s:%s", imdbID, title, m.Status, releaseEstimate)
	id := c.getOrLoad("item_id:"+itemKey, newUUID)
	updated := c.getOrLoad("content_updated:"+itemKey, func() string {
		return now().UTC().Truncate(time.Second).Format(timestampLayout)
	})

	return item{
		ID:            id,
		URL:           "https://www.imdb.com/title/" + imdbID + "/",
		Title:         title,
		ContentText:   contentText,
		DatePublished: updated,
	}, true
}

// writeFeed matches Python's json.dump(indent=4, ensure_ascii=False): four
// space indent, no HTML escaping and no trailing newline.
func writeFeed(path string, f feed) error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "    ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(f); err != nil {
		return err
	}
	out := bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
	if path == "" || path == "-" {
		_, err := os.Stdout.Write(out)
		return err
	}
	return writeFileAtomic(path, out)
}

type mediaRef struct {
	mediaType string
	id        int
}

var relevantDepartments = map[string]bool{"Directing": true, "Writing": true}

func discoverCredits(client *tmdbClient, peopleIDs, companyIDs map[int]bool) ([]mediaRef, error) {
	today := todayUTC()
	var refs []mediaRef
	seen := map[mediaRef]bool{}
	add := func(r mediaRef) {
		if !seen[r] {
			seen[r] = true
			refs = append(refs, r)
		}
	}

	for _, personID := range sortedIDs(peopleIDs) {
		credits, err := client.personCredits(personID)
		if err != nil {
			return nil, err
		}
		for _, credit := range credits {
			if keepCredit(credit, today) {
				add(mediaRef{credit.MediaType, credit.ID})
			}
		}
	}

	for _, companyID := range sortedIDs(companyIDs) {
		movies, err := client.discoverCompanyMovies(companyID, today)
		if err != nil {
			return nil, err
		}
		for _, movie := range movies {
			if d, ok := parseDate(movie.ReleaseDate); ok && !d.After(today) {
				slog.Debug(fmt.Sprintf("Skip already released: %d", movie.ID))
				continue
			}
			add(mediaRef{"movie", movie.ID})
		}
	}

	return refs, nil
}

func keepCredit(c personCredit, today time.Time) bool {
	if c.MediaType == "movie" && c.Video {
		slog.Debug(fmt.Sprintf("Skip video: %d", c.ID))
		return false
	}

	releaseDate := ""
	switch c.MediaType {
	case "movie":
		releaseDate = c.ReleaseDate
	case "tv":
		releaseDate = c.FirstAirDate
	}
	if d, ok := parseDate(releaseDate); ok && !d.After(today) {
		slog.Debug(fmt.Sprintf("Skip already released: %d", c.ID))
		return false
	}

	if c.MediaType == "movie" && c.CreditType == "cast" && c.Order > 10 {
		slog.Debug(fmt.Sprintf("Skip non-top billed credit: %d", c.ID))
		return false
	}

	if c.CreditType == "cast" && selfCharacter(c.Character) {
		slog.Debug(fmt.Sprintf("Skip self credit: %d", c.ID))
		return false
	}

	if c.CreditType == "crew" && !relevantDepartments[c.Department] {
		slog.Debug(fmt.Sprintf("Skip not relevant crew department: %d", c.ID))
		return false
	}

	if c.MediaType != "movie" && c.MediaType != "tv" {
		slog.Warn(fmt.Sprintf("Unknown credit media type: %s", c.MediaType))
		return false
	}
	return true
}

var selfCharacterPattern = regexp.MustCompile(`(?i)self|himself|herself|uncredited|interviewee|archive footage`)

func selfCharacter(character string) bool {
	return selfCharacterPattern.MatchString(character)
}

func movieContentText(m *media, peopleIDs map[int]bool) string {
	directors := map[string]bool{}
	for _, crew := range m.Credits.Crew {
		if crew.Job == "Director" {
			directors[crew.Name] = true
		}
	}
	people := relevantPeopleNames(m.Credits, peopleIDs)
	for name := range directors {
		delete(people, name)
	}

	var b strings.Builder
	b.WriteString(`"` + m.Title + `"`)
	if len(directors) > 0 {
		b.WriteString(" directed by " + namesToSentence(directors))
	}
	if len(people) > 0 {
		b.WriteString(" along with " + namesToSentence(people))
	}
	b.WriteString(".")
	if d, ok := parseDate(m.ReleaseDate); ok {
		b.WriteString(" Coming " + d.Format(monthLayout) + ".")
	}
	return b.String()
}

func tvContentText(m *media, peopleIDs map[int]bool) string {
	var b strings.Builder
	b.WriteString(`"` + m.Name + `"`)
	if people := relevantPeopleNames(m.Credits, peopleIDs); len(people) > 0 {
		b.WriteString(" with " + namesToSentence(people))
	}
	b.WriteString(".")
	if d, ok := parseDate(m.FirstAirDate); ok {
		b.WriteString(" Coming " + d.Format(monthLayout) + ".")
	}
	return b.String()
}

func namesToSentence(names map[string]bool) string {
	ns := make([]string, 0, len(names))
	for n := range names {
		ns = append(ns, n)
	}
	slices.Sort(ns)
	switch len(ns) {
	case 0:
		return ""
	case 1:
		return ns[0]
	case 2:
		return ns[0] + " and " + ns[1]
	default:
		return strings.Join(ns[:len(ns)-1], ", ") + ", and " + ns[len(ns)-1]
	}
}

func relevantPeopleNames(credits mediaCredits, peopleIDs map[int]bool) map[string]bool {
	names := map[string]bool{}
	for _, c := range credits.Cast {
		if peopleIDs[c.ID] {
			names[c.Name] = true
		}
	}
	for _, c := range credits.Crew {
		if peopleIDs[c.ID] {
			names[c.Name] = true
		}
	}
	return names
}

func readIDsFile(path string) (map[int]bool, error) {
	if path == "" {
		return map[int]bool{}, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return readIDs(f)
}

// readIDs parses lines like "31-tom-hanks" into a set of IDs.
func readIDs(r io.Reader) (map[int]bool, error) {
	ids := map[int]bool{}
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		idStr, _, _ := strings.Cut(line, "-")
		id, err := strconv.Atoi(idStr)
		if err != nil {
			return nil, fmt.Errorf("invalid ID line %q: %w", line, err)
		}
		ids[id] = true
	}
	return ids, scanner.Err()
}

func sortedIDs(ids map[int]bool) []int {
	out := make([]int, 0, len(ids))
	for id := range ids {
		out = append(out, id)
	}
	slices.Sort(out)
	return out
}

func todayUTC() time.Time {
	y, m, d := now().UTC().Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

func parseDate(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(dateLayout, s)
	if err != nil {
		slog.Warn(fmt.Sprintf("Invalid date %q", s))
		return time.Time{}, false
	}
	return t, true
}

func newUUID() string {
	var b [16]byte
	rand.Read(b[:]) // never returns an error since Go 1.24
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
