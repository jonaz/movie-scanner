package main

type SearchRequest struct {
	Query  string `json:"query"`
	Format string `json:"format"`
}

type BarcodeRequest struct {
	UPC string `json:"upc"`
}

type SaveRequest struct {
	Title       string  `json:"title"`
	Format      string  `json:"format"`
	ReleaseDate string  `json:"releaseDate"`
	Genres      string  `json:"genres"`
	VoteAverage float64 `json:"voteAverage"`
	VoteCount   int     `json:"voteCount"`
	Notes       string  `json:"notes"`
}

type CandidatesResponse struct {
	Candidates []TMDBResult `json:"candidates"`
	Format     string       `json:"format"`
}

type UPCItemDBResponse struct {
	Items []struct {
		Title string `json:"title"`
	} `json:"items"`
}

type GinzaResponse struct {
	Result []struct {
		Text string `json:"Text"`
	} `json:"Result"`
}

type KvarnvideoSearchResponse struct {
	Result []int `json:"result"`
}

type KvarnvideoArticle struct {
	Name map[string]string `json:"name"`
}

type KvarnvideoListResponse struct {
	Result []KvarnvideoArticle `json:"result"`
}

type TextBlock struct {
	Text string
	Area int
}

type TMDBResult struct {
	ID          int     `json:"id"`
	Title       string  `json:"title"`
	ReleaseDate string  `json:"releaseDate"`
	Genres      string  `json:"genres"`
	VoteAverage float64 `json:"voteAverage"`
	VoteCount   int     `json:"voteCount"`
	PosterPath  string  `json:"posterPath,omitempty"`
	Overview    string  `json:"overview,omitempty"`
	Score       float64 `json:"score"`
	Exists      bool    `json:"exists"`
}

type TMDBResponse struct {
	Results []struct {
		ID          int     `json:"id"`
		Title       string  `json:"title"`
		ReleaseDate string  `json:"release_date"`
		GenreIDs    []int   `json:"genre_ids"`
		VoteAverage float64 `json:"vote_average"`
		VoteCount   int     `json:"vote_count"`
		PosterPath  string  `json:"poster_path"`
		Overview    string  `json:"overview"`
	} `json:"results"`
}
