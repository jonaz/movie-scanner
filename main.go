package main

import (
	"context"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"google.golang.org/api/option"
	"google.golang.org/api/sheets/v4"
	"google.golang.org/api/vision/v1"
)

// -- CONFIGURATION --
const SheetRange = "Sheet1!A:A"

var TmdbApiKey string
var AppSecret string
var SpreadsheetID string

// -- EMBEDDED FRONTEND --
//
//go:embed index.html
var indexHTML []byte

func main() {
	TmdbApiKey = os.Getenv("TMDB_API_KEY")
	SpreadsheetID = os.Getenv("SPREADSHEET_ID")
	AppSecret = os.Getenv("APP_SECRET")

	if AppSecret == "" {
		log.Println("APP_SECRET must be defined")
		return
	}

	port := os.Getenv("HTTP_PORT")
	if port == "" {
		port = "8080"
	}

	mux := http.NewServeMux()

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write(indexHTML)
	})

	mux.HandleFunc("/api/scan", handleScan)
	mux.HandleFunc("/api/search", handleSearch)

	finalHandler := loggingMiddleware(authMiddleware(mux))

	log.Println("[INFO] Server started at http://localhost:" + port)
	if err := http.ListenAndServe(":"+port, finalHandler); err != nil {
		log.Fatalf("[FATAL] Server crashed: %v", err)
	}
}

// -- MIDDLEWARE --

func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		log.Printf("[REQ] %s %s", r.Method, r.URL.Path)
		next.ServeHTTP(w, r)
		log.Printf("[RES] %s %s (%v)", r.Method, r.URL.Path, time.Since(start))
	})
}

func authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			clientSecret := r.Header.Get("X-App-Secret")
			if clientSecret != AppSecret {
				log.Printf("[AUTH] Failed attempt from %s", r.RemoteAddr)
				jsonError(w, "Unauthorized: Invalid App Secret", http.StatusUnauthorized, nil)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// -- HANDLERS --

type SearchRequest struct {
	Query  string `json:"query"`
	Format string `json:"format"`
}

func handleSearch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", 405)
		return
	}

	var req SearchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, "Invalid JSON", 400, err)
		return
	}

	if req.Query == "" {
		jsonError(w, "Query cannot be empty", 400, nil)
		return
	}

	log.Printf("[INFO] Manual Search: '%s' (%s)", req.Query, req.Format)

	results, err := searchTMDBList(req.Query)
	if err != nil || len(results) == 0 {
		jsonError(w, "Movie not found: "+req.Query, 404, err)
		return
	}

	processAndSave(w, results[0], req.Format)
}

func handleScan(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", 405)
		return
	}

	file, _, err := r.FormFile("image")
	if err != nil {
		jsonError(w, "Failed to read image", 500, err)
		return
	}
	defer file.Close()
	imgBytes, _ := io.ReadAll(file)
	base64Image := base64.StdEncoding.EncodeToString(imgBytes)

	ctx := context.Background()
	creds, err := os.ReadFile("credentials.json")
	if err != nil {
		jsonError(w, "Missing credentials.json", 500, err)
		return
	}

	visionService, err := vision.NewService(ctx, option.WithCredentialsJSON(creds))
	if err != nil {
		jsonError(w, "Failed to connect to Vision API", 500, err)
		return
	}

	req := &vision.AnnotateImageRequest{
		Image:    &vision.Image{Content: base64Image},
		Features: []*vision.Feature{{Type: "TEXT_DETECTION"}},
	}

	batch := &vision.BatchAnnotateImagesRequest{Requests: []*vision.AnnotateImageRequest{req}}
	res, err := visionService.Images.Annotate(batch).Do()
	if err != nil {
		jsonError(w, fmt.Sprintf("Vision API Error: %v", err), 500, err)
		return
	}

	if len(res.Responses) == 0 || res.Responses[0].FullTextAnnotation == nil {
		jsonError(w, "No text found in image", 400, nil)
		return
	}

	annotation := res.Responses[0].FullTextAnnotation
	fullOCRText := annotation.Text
	detectedFormat := detectFormat(fullOCRText)

	// NLP PATH 2 LOGIC
	seeds := getSeedQueries(annotation)
	log.Printf("[NLP] Generated Search Seeds: %v", seeds)

	var bestMatch TMDBResult
	bestScore := 0.0

	searchedMovies := make(map[string]bool)

	for _, seed := range seeds {
		tmdbResults, err := searchTMDBList(seed)
		if err != nil {
			continue
		}

		for _, movie := range tmdbResults {
			if searchedMovies[movie.Title] {
				continue
			}
			searchedMovies[movie.Title] = true

			score := scoreMovieTitle(movie.Title, fullOCRText)
			log.Printf("[NLP] Evaluated '%s' -> Score: %.2f", movie.Title, score)

			if score > bestScore {
				bestScore = score
				bestMatch = movie
			}
		}
	}

	if bestScore == 0 {
		jsonError(w, "Could not confidently match a movie from the cover text.", 404, nil)
		return
	}

	log.Printf("[SUCCESS] Best Match: '%s' (Score: %.2f)", bestMatch.Title, bestScore)
	processAndSave(w, bestMatch, detectedFormat)
}

// -- CORE LOGIC --
func processAndSave(w http.ResponseWriter, tmdbData TMDBResult, format string) {
	ctx := context.Background()
	creds, err := os.ReadFile("credentials.json")
	if err != nil {
		jsonError(w, "Missing credentials.json", 500, err)
		return
	}

	sheetsService, err := sheets.NewService(ctx, option.WithCredentialsJSON(creds))
	if err != nil {
		jsonError(w, "Failed to connect to Sheets API", 500, err)
		return
	}

	readRange, err := sheetsService.Spreadsheets.Values.Get(SpreadsheetID, SheetRange).Do()
	if err != nil {
		jsonError(w, "Failed to read Sheet", 500, err)
		return
	}

	for _, row := range readRange.Values {
		if len(row) > 0 && strings.EqualFold(fmt.Sprintf("%v", row[0]), tmdbData.Title) {
			jsonError(w, "Movie already exists: "+tmdbData.Title, 409, nil)
			return
		}
	}

	values := []interface{}{
		tmdbData.Title,
		format,
		tmdbData.ReleaseDate,
		tmdbData.Genres,
		tmdbData.VoteAverage,
		tmdbData.VoteCount,
		"",
	}

	vr := &sheets.ValueRange{Values: [][]interface{}{values}}
	_, err = sheetsService.Spreadsheets.Values.Append(SpreadsheetID, "Sheet1!A1", vr).ValueInputOption("RAW").Do()
	if err != nil {
		jsonError(w, "Failed to write to Sheet", 500, err)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":      "success",
		"title":       tmdbData.Title,
		"format":      format,
		"releaseDate": tmdbData.ReleaseDate,
	})
}

// -- NLP PATH 2 HELPERS --

type TextBlock struct {
	Text string
	Area int
}

func getSeedQueries(annotation *vision.TextAnnotation) []string {
	var blocks []TextBlock

	for _, page := range annotation.Pages {
		for _, block := range page.Blocks {
			for _, paragraph := range block.Paragraphs {
				if len(paragraph.BoundingBox.Vertices) < 4 {
					continue
				}
				v := paragraph.BoundingBox.Vertices
				minX, maxX := v[0].X, v[0].X
				minY, maxY := v[0].Y, v[0].Y
				for _, pt := range v {
					if pt.X < minX {
						minX = pt.X
					}
					if pt.X > maxX {
						maxX = pt.X
					}
					if pt.Y < minY {
						minY = pt.Y
					}
					if pt.Y > maxY {
						maxY = pt.Y
					}
				}
				area := int((maxX - minX) * (maxY - minY))

				var words []string
				for _, word := range paragraph.Words {
					var symbols []string
					for _, s := range word.Symbols {
						symbols = append(symbols, s.Text)
					}
					words = append(words, strings.Join(symbols, ""))
				}

				fullText := strings.TrimSpace(strings.Join(words, " "))
				cleanText := cleanMovieText(fullText)
				cleanText = strings.TrimSpace(cleanText)

				// Discard pure numbers (handles "11", but also spaced groups like "9 11 12")
				isPureNumber, _ := regexp.MatchString(`^[\d\s]+$`, cleanText)

				if len(cleanText) > 1 && !isPureNumber {
					blocks = append(blocks, TextBlock{Text: cleanText, Area: area})
				}
			}
		}
	}

	sort.Slice(blocks, func(i, j int) bool {
		return blocks[i].Area > blocks[j].Area
	})

	var seeds []string
	seen := make(map[string]bool)

	for i := 0; i < len(blocks) && len(seeds) < 3; i++ {
		if blocks[i].Text != "" && !seen[blocks[i].Text] {
			seen[blocks[i].Text] = true
			seeds = append(seeds, blocks[i].Text)
		}
	}
	return seeds
}

func scoreMovieTitle(tmdbTitle string, coverText string) float64 {
	cleanTitle := cleanMovieText(tmdbTitle)
	cleanCover := cleanMovieText(coverText)

	titleWords := strings.Fields(cleanTitle)
	coverWords := strings.Fields(cleanCover)

	if len(titleWords) == 0 || len(coverWords) == 0 {
		return 0
	}

	matchCount := 0
	hasSignificantMatch := false

	for _, w := range titleWords {
		matched := false
		for _, cw := range coverWords {
			if w == cw {
				matched = true
				break
			}
			// Typo tolerance: If word is >= 5 letters, allow a 1 letter typo
			if len(w) >= 5 && levenshtein(w, cw) <= 1 {
				matched = true
				break
			}
		}

		if matched {
			matchCount++
			// Check if the matched word is actually a word and not just a digit
			isNum, _ := regexp.MatchString(`^\d+$`, w)
			if !isNum && len(w) > 1 {
				hasSignificantMatch = true
			}
		}
	}

	if matchCount == 0 {
		return 0
	}

	// PREVENT NUMBER HIJACKING:
	// If we ONLY matched numbers (like "9", "11", "12"), throw the result out.
	// Exception: The movie is literally just a number and we matched 100% of it (like the movie "1917").
	if !hasSignificantMatch && matchCount < len(titleWords) {
		return 0
	}

	ratio := float64(matchCount) / float64(len(titleWords))
	bonus := float64(matchCount) * 0.1

	return ratio + bonus
}
func cleanMovieText(s string) string {
	lower := strings.ToLower(s)

	// 1. Strip punctuation FIRST.
	// This turns "blu-ray" into "blu ray" and "director's cut" into "directors cut"
	reg := regexp.MustCompile(`[^a-zA-Z0-9\s]`)
	lower = reg.ReplaceAllString(lower, " ")

	// 2. Normalize spaces so things like "blu   ray" become a predictable "blu ray"
	lower = strings.Join(strings.Fields(lower), " ")

	// 3. NOW remove the noise phrases
	noisePhrases := []string{
		"4k", "ultra hd", "ultrahd", "blu ray", "bluray", "dvd", "hdr 10", "hdr10", "hdr",
		"directors cut", "extended edition",
		"special edition", "collectors edition", "digital copy",
		"includes digital", "bonus features", "combo pack", "video calibration",
	}

	for _, phrase := range noisePhrases {
		// Replace the phrase with a space to avoid squishing words together
		lower = strings.ReplaceAll(lower, phrase, " ")
	}

	// 4. Clean up any leftover awkward spaces caused by the deletions
	return strings.Join(strings.Fields(lower), " ")
}

func detectFormat(fullText string) string {
	lower := strings.ToLower(fullText)
	if strings.Contains(lower, "4k") || strings.Contains(lower, "ultra hd") {
		return "4K"
	}
	if strings.Contains(lower, "blu-ray") || strings.Contains(lower, "bluray") {
		return "Bluray"
	}
	if strings.Contains(lower, "dvd") {
		return "DVD"
	}
	return "Unknown"
}

// levenshtein calculates the distance between two strings for fuzzy typo matching
func levenshtein(s, t string) int {
	d := make([][]int, len(s)+1)
	for i := range d {
		d[i] = make([]int, len(t)+1)
		d[i][0] = i
	}
	for j := range t {
		d[0][j+1] = j + 1
	}
	for i := 0; i < len(s); i++ {
		for j := 0; j < len(t); j++ {
			cost := 1
			if s[i] == t[j] {
				cost = 0
			}
			min := d[i][j+1] + 1
			if d[i+1][j]+1 < min {
				min = d[i+1][j] + 1
			}
			if d[i][j]+cost < min {
				min = d[i][j] + cost
			}
			d[i+1][j+1] = min
		}
	}
	return d[len(s)][len(t)]
}

// -- TMDB API --

type TMDBResult struct {
	Title, ReleaseDate, Genres string
	VoteAverage                float64
	VoteCount                  int
}

type TMDBResponse struct {
	Results []struct {
		Title       string
		ReleaseDate string  `json:"release_date"`
		GenreIDs    []int   `json:"genre_ids"`
		VoteAverage float64 `json:"vote_average"`
		VoteCount   int     `json:"vote_count"`
	}
}

func searchTMDBList(query string) ([]TMDBResult, error) {
	q := url.QueryEscape(query)
	resp, err := http.Get(fmt.Sprintf("https://api.themoviedb.org/3/search/movie?api_key=%s&query=%s", TmdbApiKey, q))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var data TMDBResponse
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, err
	}
	if len(data.Results) == 0 {
		return nil, fmt.Errorf("no results")
	}

	genreMap := map[int]string{
		28: "Action", 12: "Adventure", 16: "Animation", 35: "Comedy",
		80: "Crime", 99: "Documentary", 18: "Drama", 10751: "Family",
		14: "Fantasy", 36: "History", 27: "Horror", 10402: "Music",
		9648: "Mystery", 10749: "Romance", 878: "Sci-Fi", 10770: "TV Movie",
		53: "Thriller", 10752: "War", 37: "Western",
	}

	var results []TMDBResult
	limit := 5
	if len(data.Results) < 5 {
		limit = len(data.Results)
	}

	for i := 0; i < limit; i++ {
		res := data.Results[i]
		var genreNames []string
		for _, id := range res.GenreIDs {
			if name, ok := genreMap[id]; ok {
				genreNames = append(genreNames, name)
			}
		}
		results = append(results, TMDBResult{
			Title:       res.Title,
			ReleaseDate: res.ReleaseDate,
			Genres:      strings.Join(genreNames, ", "),
			VoteAverage: res.VoteAverage,
			VoteCount:   res.VoteCount,
		})
	}

	return results, nil
}

func jsonError(w http.ResponseWriter, msg string, code int, err error) {
	if err != nil {
		log.Printf("[ERROR] %s: %v", msg, err)
	} else {
		log.Printf("[ERROR] %s", msg)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
