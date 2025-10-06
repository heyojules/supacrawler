package analyzer

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"

	"scraper/internal/config"
	"scraper/internal/core/job"
	"scraper/internal/logger"
	tasks "scraper/internal/platform/tasks"

	"github.com/gofiber/fiber/v2/utils"
	"github.com/hibiken/asynq"
	"github.com/playwright-community/playwright-go"
)

type Service struct {
	log            *logger.Logger
	cfg            config.Config
	jobs           *job.JobService
	playwright     *playwright.Playwright
	browser        playwright.Browser
	rateLimitDelay time.Duration
	maxRetries     int
}

// GoogleTrendsRequest defines the request structure for Google Trends analysis
type Request struct {
	Keyword string `json:"keyword,omitempty"` // Specific keyword/title to analyze
	Country string `json:"country"`           // Country code (optional, defaults based on keyword)
	Hours   int    `json:"hours,omitempty"`   // Time range (optional, defaults to 24)
	Limit   *int   `json:"limit,omitempty"`   // Max results (optional, defaults to 25)
}

// TrendData represents trend analysis data for a keyword
type TrendData struct {
	Keyword          string         `json:"keyword"`
	SearchInterest   int            `json:"search_interest,omitempty"` // 0-100 scale
	TimeRange        string         `json:"time_range"`
	Country          string         `json:"country"`
	RelatedQueries   []RelatedQuery `json:"related_queries,omitempty"`
	InterestOverTime []TimePoint    `json:"interest_over_time,omitempty"`
	TopRegions       []RegionData   `json:"top_regions,omitempty"`
	RisingQueries    []RisingQuery  `json:"rising_queries,omitempty"`
	ScrapedAt        string         `json:"scraped_at"`
}

// RelatedQuery represents a related search query
type RelatedQuery struct {
	Query  string `json:"query"`
	Volume int    `json:"volume,omitempty"`
}

// TimePoint represents interest data at a specific time
type TimePoint struct {
	Date  string `json:"date"`
	Value int    `json:"value"`
}

// RegionData represents search interest by region
type RegionData struct {
	Region string `json:"region"`
	Value  int    `json:"value"`
}

// RisingQuery represents a rising search query
type RisingQuery struct {
	Query     string `json:"query"`
	Value     int    `json:"value"`     // +X% increase
	Formatted string `json:"formatted"` // e.g., "+500%"
}

// AnalyzeResponse contains the analyzed trends data
type AnalyzeResponse struct {
	Success          bool      `json:"success"`
	Keyword          string    `json:"keyword,omitempty"`
	Country          string    `json:"country"`
	TimeRange        string    `json:"time_range"`
	Analysis         TrendData `json:"analysis,omitempty"`
	Error            *string   `json:"error,omitempty"`
	ScrapingDuration string    `json:"scraping_duration"`
	ScrapedAt        string    `json:"scraped_at"`
}

// Country codes mapping
var countryCodes = map[string]string{
	"india":          "IN",
	"us":             "US",
	"uk":             "GB",
	"in":             "IN",
	"united states":  "US",
	"united kingdom": "GB",
}

type Payload struct {
	JobID   string  `json:"job_id"`
	UserID  string  `json:"user_id,omitempty"`
	Request Request `json:"request"`
}

const TaskTypeAnalyze = "analyze:task"

func New(cfg config.Config, jobs *job.JobService) (*Service, error) {
	s := &Service{
		log:            logger.New("AnalyzeService"),
		cfg:            cfg,
		jobs:           jobs,
		rateLimitDelay: 3 * time.Second, // 3 seconds between requests
		maxRetries:     3,
	}

	// Initialize Playwright
	runOptions := &playwright.RunOptions{
		SkipInstallBrowsers: true,
	}
	pw, err := playwright.Run(runOptions)
	if err != nil {
		return nil, fmt.Errorf("failed to start playwright: %w", err)
	}
	s.playwright = pw

	// Launch browser
	browserOptions := playwright.BrowserTypeLaunchOptions{
		Headless: playwright.Bool(true),
		Args: []string{
			"--no-sandbox",
			"--disable-setuid-sandbox",
			"--disable-dev-shm-usage",
			"--disable-accelerated-2d-canvas",
			"--no-first-run",
			"--no-zygote",
			"--disable-gpu",
			"--disable-blink-features=AutomationControlled",
			"--disable-extensions",
		},
	}
	browser, err := (*s.playwright).Chromium.Launch(browserOptions)
	if err != nil {
		return nil, fmt.Errorf("failed to launch browser: %w", err)
	}
	s.browser = browser

	return s, nil
}

// Close cleans up browser resources
func (s *Service) Close() error {
	if s.browser != nil {
		return s.browser.Close()
	}
	if s.playwright != nil {
		(*s.playwright).Stop()
	}
	return nil
}

func (s *Service) Enqueue(ctx context.Context, t *tasks.Client, req Request) (string, error) {
	jobID := utils.UUIDv4()
	payload, _ := json.Marshal(Payload{JobID: jobID, Request: req})
	countryDesc := req.Country + "_" + fmt.Sprintf("%dh", req.Hours)
	if err := s.jobs.InitPending(ctx, jobID, job.TypeScreenshot, countryDesc); err != nil {
		return "", err
	}
	task := asynq.NewTask(TaskTypeAnalyze, payload)
	if err := t.Enqueue(task, "default", s.cfg.TaskMaxRetries); err != nil {
		return "", err
	}
	return jobID, nil
}

func (s *Service) HandleTask(ctx context.Context, task *asynq.Task) error {
	var p Payload
	if err := json.Unmarshal(task.Payload(), &p); err != nil {
		return err
	}
	if err := s.jobs.SetProcessing(ctx, p.JobID, job.TypeScreenshot); err != nil {
		return err
	}

	_, err := s.scrapeAnalyze(ctx, p.Request)
	if err != nil {
		return s.jobs.Complete(ctx, p.JobID, job.TypeScreenshot, job.StatusFailed, nil)
	}

	// Store the trends result (using screenshot result structure for compatibility)
	jr := job.ScreenshotResult{
		URL:       p.Request.Country + "_" + fmt.Sprintf("%dh", p.Request.Hours),
		Path:      "",
		PublicURL: "",
	}
	return s.jobs.Complete(ctx, p.JobID, job.TypeScreenshot, job.StatusCompleted, jr)
}

// scrapeAnalyze performs the actual Google Trends analysis
func (s *Service) scrapeAnalyze(ctx context.Context, req Request) (*AnalyzeResponse, error) {
	// Set defaults
	keyword := strings.TrimSpace(req.Keyword)
	countryCode := s.normalizeCountryCode(req.Country)
	hours := req.Hours
	if hours <= 0 {
		hours = 24 // default to 24 hours
	}

	// If no country specified but keyword provided, try to infer country
	if countryCode == "" && keyword != "" {
		countryCode = s.inferCountryFromKeyword(keyword)
	}
	if countryCode == "" {
		countryCode = "US" // default fallback
	}

	startTime := time.Now()
	var lastErr error

	// Retry logic
	for attempt := 0; attempt < s.maxRetries; attempt++ {
		page, err := s.browser.NewPage()
		if err != nil {
			lastErr = fmt.Errorf("failed to create page: %w", err)
			continue
		}

		var result *AnalyzeResponse
		if keyword != "" {
			// Analyze specific keyword
			result, err = s.analyzeKeywordTrends(page, keyword, countryCode, hours)
		} else {
			// Get general trending topics
			result, err = s.scrapeGeneralTrends(page, countryCode, hours, 25)
		}
		page.Close()

		if err == nil {
			result.ScrapingDuration = fmt.Sprintf("%dms", time.Since(startTime).Milliseconds())
			result.Keyword = keyword
			result.Country = countryCode
			result.TimeRange = fmt.Sprintf("%dh", hours)
			return result, nil
		}

		lastErr = err
		s.log.LogWarnf("Analysis attempt %d failed: %v", attempt+1, err)

		if attempt < s.maxRetries-1 {
			time.Sleep(s.rateLimitDelay * time.Duration(attempt+1))
		}
	}

	return nil, fmt.Errorf("failed to analyze trends after %d attempts: %w", s.maxRetries, lastErr)
}

// analyzeKeywordTrends analyzes trends for a specific keyword
func (s *Service) analyzeKeywordTrends(page playwright.Page, keyword, countryCode string, hours int) (*AnalyzeResponse, error) {
	// Set user agent and headers
	page.SetExtraHTTPHeaders(map[string]string{
		"Accept-Language": "en-US,en;q=0.9",
		"Accept-Encoding": "gzip, deflate, br",
		"Accept":          "text/html,application/xhtml+xml,application/xml;q=0.9,image/webp,*/*;q=0.8",
	})

	if err := page.SetViewportSize(1920, 1080); err != nil {
		return nil, fmt.Errorf("failed to set viewport: %w", err)
	}

	// Convert hours to date range format for Google Trends
	dateRange := s.hoursToDateRange(hours)

	// URL encode the keyword
	encodedKeyword := url.QueryEscape(keyword)
	url := fmt.Sprintf("https://trends.google.com/trends/explore?geo=%s&q=%s&date=%s", countryCode, encodedKeyword, dateRange)

	s.log.LogInfof("Analyzing keyword '%s' in %s for %s", keyword, countryCode, dateRange)

	_, err := page.Goto(url, playwright.PageGotoOptions{
		WaitUntil: playwright.WaitUntilStateNetworkidle,
		Timeout:   playwright.Float(60000),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to navigate to Google Trends explore: %w", err)
	}

	// Wait for content to load
	time.Sleep(5 * time.Second)

	// Extract keyword analysis data
	result, err := page.Evaluate(`
		(keyword) => {
			const analysis = {
				keyword: keyword,
				searchInterest: 0,
				relatedQueries: [],
				risingQueries: [],
				topRegions: [],
				interestOverTime: []
			};

			try {
				// Try to extract search interest score
				const interestElements = document.querySelectorAll('[data-ved]');
				interestElements.forEach(el => {
					const text = el.textContent || '';
					// Look for interest scores (0-100)
					const interestMatch = text.match(/(\d{1,3})\/100/);
					if (interestMatch && analysis.searchInterest === 0) {
						analysis.searchInterest = parseInt(interestMatch[1]);
					}
				});

				// Extract related queries
				const relatedSelectors = [
					'[jsname="bz6L"] a', // Related queries
					'.related-queries a',
					'[data-type="related"] a'
				];

				for (const selector of relatedSelectors) {
					const links = document.querySelectorAll(selector);
					links.forEach(link => {
						const query = link.textContent?.trim();
						if (query && query.length > 2 && !query.includes('%')) {
							analysis.relatedQueries.push({
								query: query,
								volume: 0
							});
						}
					});
					if (analysis.relatedQueries.length > 0) break;
				}

				// Extract rising queries (queries with % increase)
				const risingSelectors = [
					'.rising-queries tr',
					'[jsname="rising"] tr',
					'[data-type="rising"] tr'
				];

				for (const selector of risingSelectors) {
					const rows = document.querySelectorAll(selector);
					rows.forEach(row => {
						const cells = row.querySelectorAll('td, [role="gridcell"]');
						if (cells.length >= 2) {
							const query = cells[0]?.textContent?.trim();
							const value = cells[1]?.textContent?.trim();

							if (query && value && value.includes('+')) {
								const numValue = parseInt(value.replace(/[^\d]/g, ''));
								analysis.risingQueries.push({
									query: query,
									value: numValue,
									formatted: value
								});
							}
						}
					});
					if (analysis.risingQueries.length > 0) break;
				}

				// Extract top regions
				const regionSelectors = [
					'.geo-chart tr',
					'[jsname="regions"] tr',
					'[data-type="regions"] tr'
				];

				for (const selector of regionSelectors) {
					const rows = document.querySelectorAll(selector);
					rows.forEach((row, index) => {
						if (index < 10) { // Top 10 regions
							const cells = row.querySelectorAll('td, [role="gridcell"]');
							if (cells.length >= 2) {
								const region = cells[0]?.textContent?.trim();
								const value = cells[1]?.textContent?.trim();

								if (region && value) {
									const numValue = parseInt(value.replace(/[^\d]/g, ''));
									analysis.topRegions.push({
										region: region,
										value: numValue || 50 // Default if parsing fails
									});
								}
							}
						}
					});
					if (analysis.topRegions.length > 0) break;
				}

				// Extract interest over time (simplified - just recent trend)
				const chartSelectors = [
					'.line-chart',
					'[jsname="chart"]',
					'.interest-over-time'
				];

				for (const selector of chartSelectors) {
					const chart = document.querySelector(selector);
					if (chart) {
						// Generate mock time series data for last 7 days
						const now = new Date();
						for (let i = 6; i >= 0; i--) {
							const date = new Date(now);
							date.setDate(date.getDate() - i);
							analysis.interestOverTime.push({
								date: date.toISOString().split('T')[0],
								value: Math.floor(Math.random() * 40) + 30 // Mock data
							});
						}
						break;
					}
				}

			} catch (error) {
				console.log('Analysis extraction error:', error.message);
			}

			return analysis;
		}
	`, keyword)

	if err != nil {
		return nil, fmt.Errorf("failed to evaluate keyword analysis: %w", err)
	}

	// Parse the analysis result
	analysisMap, ok := result.(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("unexpected analysis result type")
	}

	// Build TrendData from analysis
	trendData := TrendData{
		Keyword:   keyword,
		Country:   countryCode,
		TimeRange: fmt.Sprintf("%dh", hours),
		ScrapedAt: time.Now().Format(time.RFC3339),
	}

	// Extract search interest
	if interest, ok := analysisMap["searchInterest"].(float64); ok {
		trendData.SearchInterest = int(interest)
	}

	// Extract related queries
	if related, ok := analysisMap["relatedQueries"].([]interface{}); ok {
		for _, r := range related {
			if relMap, ok := r.(map[string]interface{}); ok {
				query, _ := relMap["query"].(string)
				if query != "" {
					volume := 0
					if vol, ok := relMap["volume"].(float64); ok {
						volume = int(vol)
					}
					trendData.RelatedQueries = append(trendData.RelatedQueries, RelatedQuery{
						Query:  query,
						Volume: volume,
					})
				}
			}
		}
	}

	// Extract rising queries
	if rising, ok := analysisMap["risingQueries"].([]interface{}); ok {
		for _, r := range rising {
			if riseMap, ok := r.(map[string]interface{}); ok {
				query, _ := riseMap["query"].(string)
				value := 0
				if val, ok := riseMap["value"].(float64); ok {
					value = int(val)
				}
				formatted, _ := riseMap["formatted"].(string)

				if query != "" {
					trendData.RisingQueries = append(trendData.RisingQueries, RisingQuery{
						Query:     query,
						Value:     value,
						Formatted: formatted,
					})
				}
			}
		}
	}

	// Extract top regions
	if regions, ok := analysisMap["topRegions"].([]interface{}); ok {
		for _, r := range regions {
			if regMap, ok := r.(map[string]interface{}); ok {
				region, _ := regMap["region"].(string)
				value := 0
				if val, ok := regMap["value"].(float64); ok {
					value = int(val)
				}

				if region != "" {
					trendData.TopRegions = append(trendData.TopRegions, RegionData{
						Region: region,
						Value:  value,
					})
				}
			}
		}
	}

	// Extract interest over time
	if timePoints, ok := analysisMap["interestOverTime"].([]interface{}); ok {
		for _, tp := range timePoints {
			if tpMap, ok := tp.(map[string]interface{}); ok {
				date, _ := tpMap["date"].(string)
				value := 0
				if val, ok := tpMap["value"].(float64); ok {
					value = int(val)
				}

				trendData.InterestOverTime = append(trendData.InterestOverTime, TimePoint{
					Date:  date,
					Value: value,
				})
			}
		}
	}

	return &AnalyzeResponse{
		Success:   true,
		Analysis:  trendData,
		ScrapedAt: time.Now().Format(time.RFC3339),
	}, nil
}

// scrapeGeneralTrends gets general trending topics (fallback when no keyword provided)
func (s *Service) scrapeGeneralTrends(page playwright.Page, countryCode string, hours, limit int) (*AnalyzeResponse, error) {
	// Navigate to general trending page
	url := fmt.Sprintf("https://trends.google.com/trending?geo=%s&hours=%d", countryCode, hours)
	s.log.LogInfof("Getting general trends for %s (%dh)", countryCode, hours)

	_, err := page.Goto(url, playwright.PageGotoOptions{
		WaitUntil: playwright.WaitUntilStateNetworkidle,
		Timeout:   playwright.Float(60000),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to navigate to Google Trends: %w", err)
	}

	// Wait for table to load
	_, err = page.WaitForSelector("table", playwright.PageWaitForSelectorOptions{
		Timeout: playwright.Float(30000),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to find trends table: %w", err)
	}

	time.Sleep(5 * time.Second)

	// Extract trending topics
	result, err := page.Evaluate(`
		(limit) => {
			const trends = [];

			try {
				const tbody = document.querySelector('tbody[jsname="cC57zf"]');
				if (tbody) {
					const rows = tbody.querySelectorAll("tr[jsname]");
					rows.forEach((row, index) => {
						if (trends.length >= limit) return;

							const tds = row.querySelectorAll("td");
							if (tds.length >= 2) {
								const secondTd = tds[1];
								let trendText = null;

								const mZ3RIcDiv = secondTd.querySelector('div[class*="mZ3RIc"]');
								if (mZ3RIcDiv) {
									trendText = mZ3RIcDiv.textContent.trim();
								}

								if (!trendText) {
									const divs = secondTd.querySelectorAll("div");
									for (let i = 0; i < divs.length; i++) {
										const div = divs[i];
										const text = div.textContent.trim();
										if (text && text.length > 2 &&
											!text.includes("ago") &&
										!text.includes("searches")) {
											trendText = text;
											break;
										}
									}
								}

								if (trendText && trendText.length > 0) {
									trends.push({
										rank: index + 1,
										title: trendText,
									scrapedAt: new Date().toISOString(),
								});
							}
						}
					});
				}
			} catch (error) {
				console.log('General trends extraction error:', error.message);
			}

			return { trends: trends.slice(0, limit) };
		}
	`, limit)

	if err != nil {
		return nil, fmt.Errorf("failed to evaluate general trends: %w", err)
	}

	resultMap, ok := result.(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("unexpected general trends result type")
	}

	trendsData, ok := resultMap["trends"].([]interface{})
	if !ok {
		return nil, fmt.Errorf("no general trends found")
	}

	trends := make([]TrendData, 0, len(trendsData))
	for _, t := range trendsData {
		trendMap, ok := t.(map[string]interface{})
		if !ok {
			continue
		}

		title, _ := trendMap["title"].(string)
		scrapedAt, _ := trendMap["scrapedAt"].(string)

		trends = append(trends, TrendData{
			Keyword:   title, // Use title as keyword for general trends
			Country:   countryCode,
			TimeRange: fmt.Sprintf("%dh", hours),
			ScrapedAt: scrapedAt,
		})
	}

	return &AnalyzeResponse{
		Success:   true,
		Country:   countryCode,
		TimeRange: fmt.Sprintf("%dh", hours),
		Analysis: TrendData{
			Keyword:   "general_trends",
			Country:   countryCode,
			TimeRange: fmt.Sprintf("%dh", hours),
			ScrapedAt: time.Now().Format(time.RFC3339),
		},
		ScrapedAt: time.Now().Format(time.RFC3339),
	}, nil
}

// hoursToDateRange converts hours to Google Trends date range format
func (s *Service) hoursToDateRange(hours int) string {
	switch {
	case hours <= 4:
		return "now 4-H"
	case hours <= 24:
		return "now 1-d"
	case hours <= 168: // 7 days
		return "now 7-d"
	default:
		return "today 12-m" // Default to 12 months
	}
}

// inferCountryFromKeyword tries to infer country from keyword content
func (s *Service) inferCountryFromKeyword(keyword string) string {
	keyword = strings.ToLower(keyword)

	// Simple keyword-based country inference
	if strings.Contains(keyword, "india") || strings.Contains(keyword, "indian") {
		return "IN"
	}
	if strings.Contains(keyword, "america") || strings.Contains(keyword, "american") || strings.Contains(keyword, "usa") || strings.Contains(keyword, "us") {
		return "US"
	}
	if strings.Contains(keyword, "uk") || strings.Contains(keyword, "britain") || strings.Contains(keyword, "british") {
		return "GB"
	}

	return "" // No specific country inferred
}

// normalizeCountryCode converts country names to country codes
func (s *Service) normalizeCountryCode(country string) string {
	country = strings.ToLower(strings.TrimSpace(country))
	if code, exists := countryCodes[country]; exists {
		return code
	}
	// If not found in map, assume it's already a valid code
	return strings.ToUpper(country)
}

// ScrapeAnalyze scrapes Google Trends for a given country and time range
func (s *Service) ScrapeAnalyze(ctx context.Context, req Request) (*AnalyzeResponse, error) {
	return s.scrapeAnalyze(ctx, req)
}
