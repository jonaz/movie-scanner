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
	"sort"
	"strings"
	"time"

	"google.golang.org/api/option"
	"google.golang.org/api/sheets/v4"
	"google.golang.org/api/vision/v1"
)

// -- CONFIGURATION --
const (
	SheetRange = "Sheet1!A:A"
	// Default secret if env var is missing. Change this!
)

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
	// 1. Define Handlers
	mux := http.NewServeMux()

	// Public: Serve HTML
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write(indexHTML)
	})

	// Protected: API Endpoints
	mux.HandleFunc("/api/scan", handleScan)
	mux.HandleFunc("/api/search", handleSearch) // New Manual Endpoint

	// 2. Wrap with Middleware (Logging + Auth)
	// We only apply Auth to /api/ routes
	finalHandler := loggingMiddleware(authMiddleware(mux))

	// 3. Start Server
	log.Println("[INFO] Server started at http://localhost:8080")
	if err := http.ListenAndServe(":8080", finalHandler); err != nil {
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
		// Only check auth for API routes
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

// 1. Manual Text Search
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

	// Reuse the shared processing logic
	processAndSave(w, req.Query, req.Format)
}

// 2. Image Scan
func handleScan(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", 405)
		return
	}

	// Read Image
	file, _, err := r.FormFile("image")
	if err != nil {
		jsonError(w, "Failed to read image", 500, err)
		return
	}
	defer file.Close()
	imgBytes, _ := io.ReadAll(file)
	base64Image := base64.StdEncoding.EncodeToString(imgBytes)

	// Google Vision
	ctx := context.Background()

	// Creds
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

	// Extract Title & Format
	annotation := res.Responses[0].FullTextAnnotation
	combinedTitle, anchorTitle := findLargestTitle(annotation)
	detectedFormat := detectFormat(annotation.Text)

	log.Printf("[OCR] Combined: '%s' | Anchor: '%s'", combinedTitle, anchorTitle)

	// Try finding movie in TMDB (Fallback logic)
	finalTitle := ""

	// Check Combined
	_, err = searchTMDB(combinedTitle)
	if err == nil {
		finalTitle = combinedTitle
	} else {
		// Check Anchor
		_, err = searchTMDB(anchorTitle)
		if err == nil {
			finalTitle = anchorTitle
		} else {
			// Check Cleaned Anchor
			cleaned := strings.ReplaceAll(anchorTitle, ":", "")
			cleaned = strings.ReplaceAll(cleaned, "-", " ")
			_, err = searchTMDB(cleaned)
			if err == nil {
				finalTitle = cleaned
			}
		}
	}

	if finalTitle == "" {
		jsonError(w, "Movie not found in TMDB (OCR read: "+anchorTitle+")", 404, nil)
		return
	}

	// Proceed to save
	processAndSave(w, finalTitle, detectedFormat)
}

// -- CORE LOGIC (Shared by Scan and Manual) --
func processAndSave(w http.ResponseWriter, query string, format string) {
	// 1. Search TMDB
	tmdbData, err := searchTMDB(query)
	if err != nil {
		jsonError(w, "Movie not found: "+query, 404, err)
		return
	}

	// 2. Check Duplicates in Sheets
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

	// 3. Append
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

	log.Printf("[SUCCESS] Added '%s'", tmdbData.Title)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"status": "success",
		"title":  tmdbData.Title,
		"format": format,
	})
}

// -- HELPERS -- (Same as previous: findLargestTitle, isNoise, detectFormat, searchTMDB, etc)
// Paste the helper functions (findLargestTitle, isNoise, detectFormat, searchTMDB, jsonError) from the previous answer here.
// Make sure 'searchTMDB' returns TMDBResult just like before.

// ... [Paste Helpers Here] ...

// -- HELPER FUNCTIONS START --

type TextLine struct {
	Text   string
	Height int
	Top    int
	Bottom int
}

func findLargestTitle(annotation *vision.TextAnnotation) (string, string) {
	var candidates []TextLine

	for _, page := range annotation.Pages {
		for _, block := range page.Blocks {
			for _, paragraph := range block.Paragraphs {
				if len(paragraph.BoundingBox.Vertices) < 4 {
					continue
				}
				v := paragraph.BoundingBox.Vertices
				height := v[3].Y - v[0].Y

				var words []string
				for _, word := range paragraph.Words {
					var symbols []string
					for _, s := range word.Symbols {
						symbols = append(symbols, s.Text)
					}
					words = append(words, strings.Join(symbols, ""))
				}
				fullText := strings.Join(words, " ")

				if isNoise(fullText) {
					continue
				}

				candidates = append(candidates, TextLine{
					Text:   fullText,
					Height: int(height),
					Top:    int(v[0].Y),
					Bottom: int(v[3].Y),
				})
			}
		}
	}

	if len(candidates) == 0 {
		return "", ""
	}

	var anchor TextLine
	maxHeight := 0
	for _, line := range candidates {
		if line.Height > maxHeight {
			maxHeight = line.Height
			anchor = line
		}
	}

	spanTop := anchor.Top
	spanBottom := anchor.Bottom

	for _, line := range candidates {
		dist := 0
		if line.Top > anchor.Bottom {
			dist = line.Top - anchor.Bottom
		}
		if line.Bottom < anchor.Top {
			dist = anchor.Top - line.Bottom
		}

		if line.Height > int(float64(maxHeight)*0.50) && dist < int(float64(maxHeight)*2.0) {
			if line.Top < spanTop {
				spanTop = line.Top
			}
			if line.Bottom > spanBottom {
				spanBottom = line.Bottom
			}
		}
	}

	var titleParts []TextLine
	for _, line := range candidates {
		if line.Top >= spanTop && line.Bottom <= spanBottom {
			if line.Height > int(float64(maxHeight)*0.10) {
				titleParts = append(titleParts, line)
			}
			continue
		}
		distToSpan := 0
		if line.Top > spanBottom {
			distToSpan = line.Top - spanBottom
		} else if line.Bottom < spanTop {
			distToSpan = spanTop - line.Bottom
		}
		if distToSpan < int(float64(maxHeight)*0.5) && line.Height > int(float64(maxHeight)*0.15) {
			titleParts = append(titleParts, line)
		}
	}

	sort.Slice(titleParts, func(i, j int) bool {
		return titleParts[i].Top < titleParts[j].Top
	})

	var finalTitle []string
	seen := make(map[string]bool)
	for _, part := range titleParts {
		if !seen[part.Text] {
			finalTitle = append(finalTitle, part.Text)
			seen[part.Text] = true
		}
	}

	return strings.Join(finalTitle, " "), anchor.Text
}

func isNoise(text string) bool {
	t := strings.ToLower(text)
	if len(t) < 2 {
		return true
	}
	noiseKeywords := []string{
		"dvd",
		"bluray",
		"blu-ray",
		"video",
		"4k",
		"ultra hd",
		"uhd",
		"digital",
		"hdr",
	}
	for _, keyword := range noiseKeywords {
		if strings.Contains(t, keyword) {
			return true
		}
	}
	return false
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

func searchTMDB(query string) (TMDBResult, error) {
	q := url.QueryEscape(query)
	resp, err := http.Get(fmt.Sprintf("https://api.themoviedb.org/3/search/movie?api_key=%s&query=%s", TmdbApiKey, q))
	if err != nil {
		return TMDBResult{}, err
	}
	defer resp.Body.Close()

	var data TMDBResponse
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return TMDBResult{}, err
	}
	if len(data.Results) == 0 {
		return TMDBResult{}, fmt.Errorf("no results")
	}

	first := data.Results[0]
	genreMap := map[int]string{
		28:    "Action",
		12:    "Adventure",
		16:    "Animation",
		35:    "Comedy",
		80:    "Crime",
		99:    "Documentary",
		18:    "Drama",
		10751: "Family",
		14:    "Fantasy",
		36:    "History",
		27:    "Horror",
		10402: "Music",
		9648:  "Mystery",
		10749: "Romance",
		878:   "Sci-Fi",
		10770: "TV Movie",
		53:    "Thriller",
		10752: "War",
		37:    "Western",
	}
	var genreNames []string
	for _, id := range first.GenreIDs {
		if name, ok := genreMap[id]; ok {
			genreNames = append(genreNames, name)
		}
	}
	return TMDBResult{
		Title:       first.Title,
		ReleaseDate: first.ReleaseDate,
		Genres:      strings.Join(genreNames, ", "),
		VoteAverage: first.VoteAverage,
		VoteCount:   first.VoteCount,
	}, nil
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

// -- HELPER FUNCTIONS END --
