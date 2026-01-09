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

const (
	SheetRange = "Sheet1!A:A" // Checks Column A for duplicates
)

// -- EMBEDDED FRONTEND --
//
//go:embed index.html
var indexHTML []byte

var TmdbApiKey string
var SpreadsheetID string

func main() {
	TmdbApiKey = os.Getenv("TMDB_API_KEY")
	SpreadsheetID = os.Getenv("SPREADSHEET_ID")
	// 1. Define Handlers
	mux := http.NewServeMux()

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write(indexHTML)
	})

	mux.HandleFunc("/api/scan", handleScan)

	// 2. Wrap with Logging Middleware
	loggedMux := loggingMiddleware(mux)

	// 3. Start Server
	log.Println("[INFO] Server started at http://localhost:8080")
	if err := http.ListenAndServe(":8080", loggedMux); err != nil {
		log.Fatalf("[FATAL] Server crashed: %v", err)
	}
}

// -- MIDDLEWARE --
func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		log.Printf("[REQ] %s %s from %s", r.Method, r.URL.Path, r.RemoteAddr)

		next.ServeHTTP(w, r)

		log.Printf("[RES] %s %s completed in %v", r.Method, r.URL.Path, time.Since(start))
	})
}

// -- HANDLERS --

func handleScan(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// 1. Read Image
	log.Println("[INFO] Reading image from request...")
	file, header, err := r.FormFile("image")
	if err != nil {
		jsonError(w, "Failed to read image", 500, err)
		return
	}
	defer file.Close()

	imgBytes, _ := io.ReadAll(file)
	log.Printf("[INFO] Image received. Size: %d bytes, Filename: %s", len(imgBytes), header.Filename)

	// Encode to Base64
	base64Image := base64.StdEncoding.EncodeToString(imgBytes)

	// 2. Google Vision Request
	log.Println("[INFO] Sending image to Google Cloud Vision API...")
	ctx := context.Background()
	visionService, err := vision.NewService(ctx)
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
		log.Println("[WARN] Vision API returned no text.")
		jsonError(w, "No text found in image", 400, nil)
		return
	}

	// 3. Logic: Find Largest Text & Parse Format
	annotation := res.Responses[0].FullTextAnnotation
	combinedTitle, anchorTitle := findLargestTitle(annotation)
	detectedFormat := detectFormat(annotation.Text)

	log.Printf("[INFO] OCR Result -> Combined: '%s' | Anchor: '%s' | Format: '%s'", combinedTitle, anchorTitle, detectedFormat)

	if anchorTitle == "" {
		jsonError(w, "Could not identify a title (text too small or unclear)", 400, nil)
		return
	}

	// 4. Search TMDB (Fallback Strategy)
	var tmdbData TMDBResult
	var errSearch error

	// Attempt 1: Combined Title ("Leonardo DiCaprio Shutter Island")
	log.Printf("[INFO] Attempt 1 - Searching Combined: '%s'", combinedTitle)
	tmdbData, errSearch = searchTMDB(combinedTitle)

	// Attempt 2: Anchor Only ("Shutter Island") - if Attempt 1 failed
	if errSearch != nil && combinedTitle != anchorTitle {
		log.Printf("[INFO] Attempt 1 failed. Attempt 2 - Searching Anchor: '%s'", anchorTitle)
		tmdbData, errSearch = searchTMDB(anchorTitle)
	}

	// Attempt 3: Cleaned Anchor (Remove ":" or "-") - if Attempt 2 failed
	if errSearch != nil {
		cleanedTitle := strings.ReplaceAll(anchorTitle, ":", "")
		cleanedTitle = strings.ReplaceAll(cleanedTitle, "-", " ")
		log.Printf("[INFO] Attempt 2 failed. Attempt 3 - Searching Cleaned Anchor: '%s'", cleanedTitle)
		tmdbData, errSearch = searchTMDB(cleanedTitle)
	}

	if errSearch != nil {
		jsonError(w, "Movie not found in TMDB after multiple attempts", 404, errSearch)
		return
	}

	log.Printf("[INFO] TMDB Match -> Title: '%s', Date: %s, Rating: %.1f", tmdbData.Title, tmdbData.ReleaseDate, tmdbData.VoteAverage)

	// 5. Check Duplicates in Sheets
	log.Println("[INFO] Checking Google Sheets for duplicates...")
	sheetsService, err := sheets.NewService(ctx, option.WithCredentialsFile("credentials.json"))
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
			log.Printf("[INFO] Duplicate found for '%s'. Skipping add.", tmdbData.Title)
			jsonError(w, "Movie already exists in database", 409, nil)
			return
		}
	}

	// 6. Append to Sheet
	log.Println("[INFO] Appending new row to Sheet...")
	values := []interface{}{
		tmdbData.Title,
		detectedFormat,
		tmdbData.ReleaseDate,
		tmdbData.Genres,
		tmdbData.VoteAverage,
		tmdbData.VoteCount,
		"", // User rating blank
	}

	vr := &sheets.ValueRange{Values: [][]interface{}{values}}
	_, err = sheetsService.Spreadsheets.Values.Append(SpreadsheetID, "Sheet1!A1", vr).ValueInputOption("RAW").Do()
	if err != nil {
		jsonError(w, "Failed to write to Sheet", 500, err)
		return
	}

	log.Printf("[SUCCESS] Movie '%s' added successfully.", tmdbData.Title)

	// Success Response
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"status": "success",
		"title":  tmdbData.Title,
		"format": detectedFormat,
	})
}

// -- INTELLIGENT PARSING --

type TextLine struct {
	Text   string
	Height int
	Top    int
	Bottom int
}

// Returns: (combinedTitle, anchorText)
func findLargestTitle(annotation *vision.TextAnnotation) (string, string) {
	var candidates []TextLine

	// 1. Flatten all paragraphs
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

	// 2. Find the Anchor (Tallest line)
	var anchor TextLine
	maxHeight := 0
	for _, line := range candidates {
		if line.Height > maxHeight {
			maxHeight = line.Height
			anchor = line
		}
	}

	// 3. Define the "Sandwich" Span
	// Expand span to include any other "Huge" lines (like "WARS" in Star Wars)
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

		// If a line is Huge (>50%) and Close (<2x height), it extends the main title block
		if line.Height > int(float64(maxHeight)*0.50) && dist < int(float64(maxHeight)*2.0) {
			if line.Top < spanTop {
				spanTop = line.Top
			}
			if line.Bottom > spanBottom {
				spanBottom = line.Bottom
			}
		}
	}

	// 4. Collect Text
	var titleParts []TextLine

	for _, line := range candidates {
		// A. Inside the Span (Sandwich Filling)
		// Very permissive: capture almost anything inside the main block
		if line.Top >= spanTop && line.Bottom <= spanBottom {
			if line.Height > int(float64(maxHeight)*0.10) { // 10% threshold inside sandwich
				titleParts = append(titleParts, line)
			}
			continue
		}

		// B. Outside the Span (Subtitles/Prequels)
		distToSpan := 0
		if line.Top > spanBottom {
			distToSpan = line.Top - spanBottom // Below
		} else if line.Bottom < spanTop {
			distToSpan = spanTop - line.Bottom // Above
		}

		// LOGIC CHANGE HERE:
		// We lowered the threshold from 0.35 to 0.15 (15%)
		// This catches "The Way of Water" (Medium) but skips "Directed by..." (Tiny)
		if distToSpan < int(float64(maxHeight)*0.5) && line.Height > int(float64(maxHeight)*0.15) {
			titleParts = append(titleParts, line)
		}
	}

	// 5. Sort & Join
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

	// 1. Too short to be a meaningful title
	if len(t) < 3 {
		return true
	}

	// 2. Specific Format Keywords to ignore
	// If the text line contains any of these, we assume it is NOT the movie title.
	noiseKeywords := []string{
		"dvd",
		"bluray", "blu-ray",
		"video",
		"4k",
		"ultra hd",
		"uhd",
		"hdr",
		"digital", // Often appears as "Digital Copy"
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

// -- TMDB API --

type TMDBResult struct {
	Title       string
	ReleaseDate string
	Genres      string
	VoteAverage float64
	VoteCount   int
}

type TMDBResponse struct {
	Results []struct {
		Title       string  `json:"title"`
		ReleaseDate string  `json:"release_date"`
		GenreIDs    []int   `json:"genre_ids"`
		VoteAverage float64 `json:"vote_average"`
		VoteCount   int     `json:"vote_count"`
	} `json:"results"`
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
		28: "Action", 12: "Adventure", 16: "Animation", 35: "Comedy", 80: "Crime",
		99: "Documentary", 18: "Drama", 10751: "Family", 14: "Fantasy", 36: "History",
		27: "Horror", 10402: "Music", 9648: "Mystery", 10749: "Romance", 878: "Sci-Fi",
		10770: "TV Movie", 53: "Thriller", 10752: "War", 37: "Western",
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

// -- UTILS --

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
