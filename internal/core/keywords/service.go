package keywords

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

type Service struct {
	httpClient *http.Client
	apiKey     string
	baseURL    string
}

func NewService(apiKey string) *Service {
	return &Service{
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
		apiKey:  apiKey,
		baseURL: "https://api.dataforseo.com/v3/keywords_data/google_ads/keywords_for_site/live",
	}
}

// Request types
type KeywordsRequest struct {
	TargetType string `json:"target_type"`
	Limit      int    `json:"limit"`
	Target     string `json:"target"`
	SortBy     string `json:"sort_by"`
}

// Response types
type KeywordsResponse struct {
	Version       string  `json:"version"`
	StatusCode    int     `json:"status_code"`
	StatusMessage string  `json:"status_message"`
	Time          string  `json:"time"`
	Cost          float64 `json:"cost"`
	TasksCount    int     `json:"tasks_count"`
	TasksError    int     `json:"tasks_error"`
	Tasks         []Task  `json:"tasks"`
}

type Task struct {
	ID            string          `json:"id"`
	StatusCode    int             `json:"status_code"`
	StatusMessage string          `json:"status_message"`
	Time          string          `json:"time"`
	Cost          float64         `json:"cost"`
	ResultCount   int             `json:"result_count"`
	Path          []string        `json:"path"`
	Data          interface{}     `json:"data"`
	Result        []KeywordResult `json:"result"`
}

type KeywordResult struct {
	Keyword            string              `json:"keyword"`
	LocationCode       *int                `json:"location_code"`
	LanguageCode       *string             `json:"language_code"`
	SearchPartners     bool                `json:"search_partners"`
	Competition        *string             `json:"competition"`
	CompetitionIndex   *int                `json:"competition_index"`
	SearchVolume       *int                `json:"search_volume"`
	LowTopOfPageBid    *float64            `json:"low_top_of_page_bid"`
	HighTopOfPageBid   *float64            `json:"high_top_of_page_bid"`
	CPC                *float64            `json:"cpc"`
	MonthlySearches    []MonthlySearch     `json:"monthly_searches"`
	KeywordAnnotations *KeywordAnnotations `json:"keyword_annotations"`
}

type MonthlySearch struct {
	Year         int `json:"year"`
	Month        int `json:"month"`
	SearchVolume int `json:"search_volume"`
}

type KeywordAnnotations struct {
	Concepts []Concept `json:"concepts"`
}

type Concept struct {
	Name         string       `json:"name"`
	ConceptGroup ConceptGroup `json:"concept_group"`
}

type ConceptGroup struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

// GetKeywordsForSite makes a request to DataForSEO API and returns filtered results
func (s *Service) GetKeywordsForSite(req KeywordsRequest) (*KeywordsResponse, error) {
	// Prepare request body
	requestBody := []KeywordsRequest{req}
	jsonData, err := json.Marshal(requestBody)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	// Create HTTP request
	httpReq, err := http.NewRequest("POST", s.baseURL, bytes.NewBuffer(jsonData))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	// Set headers
	httpReq.Header.Set("Authorization", "Basic "+s.apiKey)
	httpReq.Header.Set("Content-Type", "application/json")

	// Make request
	resp, err := s.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("failed to make request: %w", err)
	}
	defer resp.Body.Close()

	// Read response
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	// Parse response
	var keywordsResp KeywordsResponse
	if err := json.Unmarshal(body, &keywordsResp); err != nil {
		return nil, fmt.Errorf("failed to unmarshal response: %w", err)
	}

	// Limit results to top 150 most relevant keywords (API already sorts by relevance)
	s.limitResults(&keywordsResp)

	return &keywordsResp, nil
}

// limitResults limits results to top 150 most relevant keywords
// The API already sorts by relevance when sort_by is set to "relevance"
func (s *Service) limitResults(resp *KeywordsResponse) {
	for i := range resp.Tasks {
		if len(resp.Tasks[i].Result) == 0 {
			continue
		}

		// Keep only top 150 results (API already sorted by relevance)
		if len(resp.Tasks[i].Result) > 150 {
			resp.Tasks[i].Result = resp.Tasks[i].Result[:150]
		}

		// Update result count
		resp.Tasks[i].ResultCount = len(resp.Tasks[i].Result)
	}
}
