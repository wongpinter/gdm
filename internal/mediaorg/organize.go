package mediaorg

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	TV    = "tv"
	Movie = "movie"
)

var (
	videoExts = map[string]bool{
		".avi": true, ".m4v": true, ".mkv": true, ".mov": true,
		".mp4": true, ".mpeg": true, ".mpg": true, ".ts": true,
		".webm": true, ".wmv": true,
	}
	episodePattern = regexp.MustCompile(`(?i)^(.*?)\bS(\d{1,2})E(\d{1,3})\b`)
	seasonPattern  = regexp.MustCompile(`(?i)^(.+?)[ ._-]+season[ ._-]*(\d{1,2})$`)
	seasonOnly     = regexp.MustCompile(`(?i)^season[ ._-]*(\d{1,2})$`)
	yearPattern    = regexp.MustCompile(`(?:^|[ ._(-])((?:18|19|20)\d{2})(?:$|[ ._) -])`)
	spacePattern   = regexp.MustCompile(`[._]+`)
)

type Options struct {
	Kind, Input, Output, Query string
	Apply                      bool
	Client                     *http.Client
	TVMazeURL, TMDBURL         string
}

type Item struct {
	Source, Destination string
	Err                 error
}

type organizer struct {
	client   *http.Client
	tvmaze   string
	tmdb     string
	movieKey string
	movieTok string
	shows    map[string]tvShow
	episodes map[int]map[[2]int]string
	movies   map[string]movie
}

type tvShow struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

type tvSearchResult struct {
	Score float64 `json:"score"`
	Show  tvShow  `json:"show"`
}

type tvEpisode struct {
	Season int    `json:"season"`
	Number int    `json:"number"`
	Name   string `json:"name"`
}

type movie struct {
	Title       string `json:"title"`
	ReleaseDate string `json:"release_date"`
}

type movieSearchResult struct {
	Results []movie `json:"results"`
}

func Plan(ctx context.Context, opts Options) ([]Item, error) {
	if opts.Kind != TV && opts.Kind != Movie {
		return nil, fmt.Errorf("unsupported media type %q (use tv or movie)", opts.Kind)
	}
	if strings.TrimSpace(opts.Input) == "" || strings.TrimSpace(opts.Output) == "" {
		return nil, errors.New("input and output paths are required")
	}
	input, err := filepath.Abs(opts.Input)
	if err != nil {
		return nil, fmt.Errorf("resolving input path: %w", err)
	}
	output, err := filepath.Abs(opts.Output)
	if err != nil {
		return nil, fmt.Errorf("resolving output path: %w", err)
	}
	if filepath.Clean(input) == filepath.Clean(output) {
		return nil, errors.New("input and output directories must differ")
	}
	files, err := discover(input, output)
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no supported video files found in %s", input)
	}

	client := opts.Client
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	o := &organizer{
		client: client, tvmaze: strings.TrimRight(opts.TVMazeURL, "/"),
		tmdb:     strings.TrimRight(opts.TMDBURL, "/"),
		movieKey: os.Getenv("TMDB_API_KEY"), movieTok: os.Getenv("TMDB_READ_ACCESS_TOKEN"),
		shows: make(map[string]tvShow), episodes: make(map[int]map[[2]int]string), movies: make(map[string]movie),
	}
	if o.tvmaze == "" {
		o.tvmaze = "https://api.tvmaze.com"
	}
	if o.tmdb == "" {
		o.tmdb = "https://api.themoviedb.org/3"
	}

	items := make([]Item, 0, len(files))
	for _, file := range files {
		item := Item{Source: file}
		var rel string
		if opts.Kind == TV {
			rel, err = o.tvPath(ctx, input, file, opts.Query)
		} else {
			rel, err = o.moviePath(ctx, file, opts.Query)
		}
		if err != nil {
			item.Err = err
		} else {
			item.Destination = filepath.Join(output, rel)
		}
		items = append(items, item)
	}
	markCollisions(items)
	return items, nil
}

func Run(ctx context.Context, opts Options, out io.Writer) error {
	items, err := Plan(ctx, opts)
	if err != nil {
		return err
	}
	failed := 0
	for _, item := range items {
		if item.Err != nil {
			failed++
			fmt.Fprintf(out, "[NO MATCH] %s: %v\n", item.Source, item.Err)
			continue
		}
		if opts.Apply {
			if err := copyNew(item.Source, item.Destination); err != nil {
				failed++
				fmt.Fprintf(out, "[FAILED] %s: %v\n", item.Source, err)
				continue
			}
			fmt.Fprintf(out, "[COPIED] %s -> %s\n", item.Source, item.Destination)
		} else {
			fmt.Fprintf(out, "[DRY-RUN] %s -> %s\n", item.Source, item.Destination)
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d files could not be organized", failed, len(items))
	}
	if opts.Apply {
		fmt.Fprintf(out, "Copied %d file(s).\n", len(items))
	} else {
		fmt.Fprintf(out, "Previewed %d file(s); pass --apply to copy.\n", len(items))
	}
	return nil
}

func discover(input, output string) ([]string, error) {
	info, err := os.Stat(input)
	if err != nil {
		return nil, fmt.Errorf("reading input: %w", err)
	}
	if !info.IsDir() {
		if info.Mode().IsRegular() && videoExts[strings.ToLower(filepath.Ext(input))] {
			return []string{input}, nil
		}
		return nil, fmt.Errorf("input must be a directory or supported video file: %s", input)
	}
	var files []string
	err = filepath.WalkDir(input, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() && path != input && (path == output || within(output, path)) {
			return filepath.SkipDir
		}
		if !entry.Type().IsRegular() || !videoExts[strings.ToLower(filepath.Ext(path))] {
			return nil
		}
		files = append(files, path)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("scanning input: %w", err)
	}
	return files, nil
}

func within(parent, path string) bool {
	rel, err := filepath.Rel(parent, path)
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func (o *organizer) tvPath(ctx context.Context, input, file, override string) (string, error) {
	base := strings.TrimSuffix(filepath.Base(file), filepath.Ext(file))
	match := episodePattern.FindStringSubmatch(base)
	if match == nil {
		return "", errors.New("filename has no SxxEyy episode marker")
	}
	season, _ := strconv.Atoi(match[2])
	episode, _ := strconv.Atoi(match[3])
	query := strings.TrimSpace(override)
	if query == "" {
		query = strings.TrimSpace(spacePattern.ReplaceAllString(match[1], " "))
	}
	if query == "" {
		rel, _ := filepath.Rel(input, filepath.Dir(file))
		parts := strings.Split(rel, string(filepath.Separator))
		for i, part := range parts {
			if seasonMatch := seasonPattern.FindStringSubmatch(part); seasonMatch != nil {
				query = strings.TrimSpace(spacePattern.ReplaceAllString(seasonMatch[1], " "))
				break
			}
			if seasonOnly.MatchString(part) && i > 0 {
				query = strings.TrimSpace(spacePattern.ReplaceAllString(parts[i-1], " "))
				break
			}
		}
	}
	if query == "" {
		return "", errors.New("cannot infer show name; pass --query")
	}
	show, err := o.show(ctx, query)
	if err != nil {
		return "", err
	}
	episodes, err := o.showEpisodes(ctx, show)
	if err != nil {
		return "", err
	}
	title, ok := episodes[[2]int{season, episode}]
	if !ok {
		return "", fmt.Errorf("%s has no S%02dE%02d episode", show.Name, season, episode)
	}
	showName := cleanName(show.Name)
	ext := filepath.Ext(file)
	filename := fmt.Sprintf("%s - S%02dE%02d - %s%s", showName, season, episode, cleanName(title), ext)
	return filepath.Join(showName, fmt.Sprintf("Season %02d", season), filename), nil
}

func (o *organizer) show(ctx context.Context, query string) (tvShow, error) {
	key := strings.ToLower(strings.TrimSpace(query))
	if show, ok := o.shows[key]; ok {
		return show, nil
	}
	var results []tvSearchResult
	if err := o.getJSON(ctx, o.tvmaze+"/search/shows?q="+url.QueryEscape(query), &results, ""); err != nil {
		return tvShow{}, fmt.Errorf("TVMaze search %q: %w", query, err)
	}
	for _, result := range results {
		if result.Show.ID != 0 && result.Show.Name != "" {
			o.shows[key] = result.Show
			return result.Show, nil
		}
	}
	return tvShow{}, fmt.Errorf("TVMaze found no show matching %q", query)
}

func (o *organizer) showEpisodes(ctx context.Context, show tvShow) (map[[2]int]string, error) {
	if episodes, ok := o.episodes[show.ID]; ok {
		return episodes, nil
	}
	var list []tvEpisode
	if err := o.getJSON(ctx, fmt.Sprintf("%s/shows/%d/episodes", o.tvmaze, show.ID), &list, ""); err != nil {
		return nil, fmt.Errorf("TVMaze episodes for %q: %w", show.Name, err)
	}
	episodes := make(map[[2]int]string, len(list))
	for _, episode := range list {
		episodes[[2]int{episode.Season, episode.Number}] = episode.Name
	}
	o.episodes[show.ID] = episodes
	return episodes, nil
}

func (o *organizer) moviePath(ctx context.Context, file, override string) (string, error) {
	query := strings.TrimSpace(override)
	year := ""
	if query == "" {
		stem := strings.TrimSuffix(filepath.Base(file), filepath.Ext(file))
		loc := yearPattern.FindStringSubmatchIndex(stem)
		if loc != nil {
			year = stem[loc[2]:loc[3]]
			query = strings.Trim(stem[:loc[0]], " ._-(")
		} else {
			query = stem
		}
		query = strings.TrimSpace(spacePattern.ReplaceAllString(query, " "))
	}
	if query == "" {
		return "", errors.New("cannot infer movie title; pass --query")
	}
	key := strings.ToLower(query + "|" + year)
	found, ok := o.movies[key]
	if !ok {
		if o.movieKey == "" && o.movieTok == "" {
			return "", errors.New("set TMDB_API_KEY or TMDB_READ_ACCESS_TOKEN to enable movie matching")
		}
		endpoint := o.tmdb + "/search/movie?query=" + url.QueryEscape(query)
		if year != "" {
			endpoint += "&year=" + year
		}
		var results movieSearchResult
		if err := o.getJSON(ctx, endpoint, &results, o.movieTok); err != nil {
			return "", fmt.Errorf("TMDb search %q: %w", query, err)
		}
		if len(results.Results) == 0 {
			return "", fmt.Errorf("TMDb found no movie matching %q", query)
		}
		found, ok = results.Results[0], true
		if found.Title == "" || len(found.ReleaseDate) < 4 {
			return "", fmt.Errorf("TMDb result for %q has incomplete title or release date", query)
		}
		o.movies[key] = found
	}
	label := cleanName(found.Title) + " (" + found.ReleaseDate[:4] + ")"
	return filepath.Join(label, label+filepath.Ext(file)), nil
}

func (o *organizer) getJSON(ctx context.Context, endpoint string, dst any, bearer string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	} else if o.movieKey != "" && strings.HasPrefix(endpoint, o.tmdb) {
		parsed, err := url.Parse(endpoint)
		if err != nil {
			return err
		}
		q := parsed.Query()
		q.Set("api_key", o.movieKey)
		parsed.RawQuery = q.Encode()
		req.URL = parsed
	}
	resp, err := o.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("HTTP %s", resp.Status)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(dst); err != nil {
		return fmt.Errorf("decoding response: %w", err)
	}
	return nil
}

func cleanName(value string) string {
	value = strings.TrimSpace(value)
	var b strings.Builder
	for _, r := range value {
		if r < 32 || strings.ContainsRune(`<>:"/\\|?*`, r) {
			b.WriteRune('-')
		} else {
			b.WriteRune(r)
		}
	}
	return strings.TrimSpace(strings.Trim(b.String(), ". "))
}

func markCollisions(items []Item) {
	seen := make(map[string]int, len(items))
	for i := range items {
		if items[i].Err != nil {
			continue
		}
		key := strings.ToLower(filepath.Clean(items[i].Destination))
		if previous, ok := seen[key]; ok {
			items[i].Err = fmt.Errorf("destination collides with %s", items[previous].Source)
			continue
		}
		seen[key] = i
		if _, err := os.Lstat(items[i].Destination); err == nil {
			items[i].Err = errors.New("destination already exists; refusing to overwrite")
		} else if !os.IsNotExist(err) {
			items[i].Err = fmt.Errorf("checking destination: %w", err)
		}
	}
}

func copyNew(source, destination string) error {
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return fmt.Errorf("creating destination directory: %w", err)
	}
	in, err := os.Open(source)
	if err != nil {
		return fmt.Errorf("opening source: %w", err)
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return fmt.Errorf("reading source metadata: %w", err)
	}
	out, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, info.Mode().Perm())
	if err != nil {
		return fmt.Errorf("creating destination without overwrite: %w", err)
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(destination)
		return fmt.Errorf("copying file: %w", err)
	}
	if err := out.Close(); err != nil {
		os.Remove(destination)
		return fmt.Errorf("closing destination: %w", err)
	}
	return nil
}
