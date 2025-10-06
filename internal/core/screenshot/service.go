package screenshot

import (
	"context"
	"encoding/json"
	"fmt"
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

// GoogleTrendsRequest defines the request structure for Google Trends scraping
type Request struct {
	Country string `json:"country"`
	Hours   int    `json:"hours"`
	Limit   *int   `json:"limit,omitempty"`
}

// TrendData represents a single trending topic
type TrendData struct {
	Rank      int    `json:"rank"`
	Title     string `json:"title"`
	Country   string `json:"country"`
	TimeRange string `json:"time_range"`
	ScrapedAt string `json:"scraped_at"`
}

// TrendsResponse contains the scraped trends data
type TrendsResponse struct {
	Success          bool        `json:"success"`
	Country          string      `json:"country"`
	TimeRange        string      `json:"time_range"`
	TotalTrends      int         `json:"total_trends"`
	ScrapingDuration string      `json:"scraping_duration"`
	ScrapedAt        string      `json:"scraped_at"`
	Trends           []TrendData `json:"trends,omitempty"`
	Error            *string     `json:"error,omitempty"`
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

const TaskTypeTrends = "trends:task"

func New(cfg config.Config, jobs *job.JobService) (*Service, error) {
	s := &Service{
		log:            logger.New("TrendsService"),
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
	task := asynq.NewTask(TaskTypeTrends, payload)
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

	_, err := s.scrapeTrends(ctx, p.Request)
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

// scrapeTrends performs the actual Google Trends scraping
func (s *Service) scrapeTrends(ctx context.Context, req Request) (*TrendsResponse, error) {
	// Normalize country code
	countryCode := s.normalizeCountryCode(req.Country)
	limit := 25 // default
	if req.Limit != nil && *req.Limit > 0 {
		limit = *req.Limit
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

		result, err := s.scrapeTrendsSingleAttempt(page, countryCode, req.Hours, limit)
		page.Close()

		if err == nil {
			result.ScrapingDuration = fmt.Sprintf("%dms", time.Since(startTime).Milliseconds())
			return result, nil
		}

		lastErr = err
		s.log.LogWarnf("Scraping attempt %d failed: %v", attempt+1, err)

		if attempt < s.maxRetries-1 {
			time.Sleep(s.rateLimitDelay * time.Duration(attempt+1))
		}
	}

	return nil, fmt.Errorf("failed to scrape trends after %d attempts: %w", s.maxRetries, lastErr)
}

// scrapeTrendsSingleAttempt performs a single scraping attempt
func (s *Service) scrapeTrendsSingleAttempt(page playwright.Page, geo string, hours, limit int) (*TrendsResponse, error) {
	// Set user agent and headers
	page.SetExtraHTTPHeaders(map[string]string{
		"Accept-Language": "en-US,en;q=0.9",
		"Accept-Encoding": "gzip, deflate, br",
		"Accept":          "text/html,application/xhtml+xml,application/xml;q=0.9,image/webp,*/*;q=0.8",
	})

	if err := page.SetViewportSize(1920, 1080); err != nil {
		return nil, fmt.Errorf("failed to set viewport: %w", err)
	}

	url := fmt.Sprintf("https://trends.google.com/trending?geo=%s&hours=%d", geo, hours)
	s.log.LogInfof("Navigating to: %s", url)

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

	// Additional wait for dynamic content
	time.Sleep(5 * time.Second)

	// Extract trends data
	result, err := page.Evaluate(`
		(limit, geo, hours) => {
			const trends = [];
			const debugInfo = [];

			try {
				debugInfo.push("Starting page evaluation...");

				// Find tbody with trends data
				const tbody = document.querySelector('tbody[jsname="cC57zf"]');
				if (tbody) {
					debugInfo.push('Found tbody with jsname="cC57zf"');
					const rows = tbody.querySelectorAll("tr[jsname]");
					debugInfo.push('Found ' + rows.length + ' trend rows in tbody');

					rows.forEach((row, index) => {
						if (trends.length >= limit) return;

						try {
							const tds = row.querySelectorAll("td");
							debugInfo.push('Row ' + index + ': Found ' + tds.length + ' td elements');

							if (tds.length >= 2) {
								const secondTd = tds[1];

								// Try multiple selectors for trend text
								let trendText = null;

								// Try div with class containing "mZ3RIc"
								const mZ3RIcDiv = secondTd.querySelector('div[class*="mZ3RIc"]');
								if (mZ3RIcDiv) {
									trendText = mZ3RIcDiv.textContent.trim();
									debugInfo.push('Row ' + index + ': Found mZ3RIc div with text: "' + trendText + '"');
								}

								// Fallback: try any div that's not a metadata div
								if (!trendText) {
									const divs = secondTd.querySelectorAll("div");
									debugInfo.push('Row ' + index + ': Fallback - found ' + divs.length + ' divs in second td');

									for (let i = 0; i < divs.length; i++) {
										const div = divs[i];
										const text = div.textContent.trim();
										debugInfo.push('Row ' + index + ', Div ' + i + ': "' + text + '" (classes: "' + div.className + '")');

										// Skip divs that contain metadata like "ago" or "searches"
										if (text && text.length > 2 &&
											!text.includes("ago") &&
											!text.includes("searches") &&
											!text.includes("24h") &&
											!text.includes("48h") &&
											!text.includes("7d") &&
											!div.classList.contains("Rz403")) {
											trendText = text;
											debugInfo.push('Row ' + index + ': Selected trend text: "' + trendText + '"');
											break;
										}
									}
								}

								if (trendText && trendText.length > 0) {
									trends.push({
										rank: index + 1,
										title: trendText,
										country: geo,
										timeRange: hours + 'h',
										scrapedAt: new Date().toISOString(),
									});
									debugInfo.push('Successfully added trend ' + (index + 1) + ': ' + trendText);
								} else {
									debugInfo.push('Row ' + index + ': No valid trend text found');
								}
							}
						} catch (error) {
							debugInfo.push('Error processing row ' + index + ': ' + error.message);
						}
					});
				} else {
					debugInfo.push('tbody with jsname="cC57zf" not found');
				}

				// Fallback methods if tbody approach didn't work
				if (trends.length === 0) {
					debugInfo.push("Trying alternative selectors...");

					const alternativeSelectors = [
						'table tr[jsname] td:nth-child(2) div[class*="mZ3RIc"]',
						'table tr[jsname] td:nth-child(2) div:not([class*="Rz403"])',
						"tr[jsname] td:nth-child(2) div",
						'table tr td div[class*="mZ3RIc"]',
						"tbody tr td:nth-child(2) div",
					];

					for (const selector of alternativeSelectors) {
						const elements = document.querySelectorAll(selector);
						debugInfo.push('Selector "' + selector + '" found ' + elements.length + ' elements');

						elements.forEach((element, index) => {
							if (trends.length >= limit) return;

							const text = element.textContent.trim();
							debugInfo.push('Alternative selector element ' + index + ': "' + text + '"');

							if (text && text.length > 2 &&
								!text.includes("ago") &&
								!text.includes("searches") &&
								!text.includes("24h") &&
								!text.includes("48h") &&
								!text.includes("7d")) {
								trends.push({
									rank: trends.length + 1,
									title: text,
									country: geo,
									timeRange: hours + 'h',
									scrapedAt: new Date().toISOString(),
								});
								debugInfo.push('Alternative method found trend: ' + text);
							}
						});

						if (trends.length > 0) {
							debugInfo.push('Successfully found ' + trends.length + ' trends using selector: ' + selector);
							break;
						}
					}
				}
			} catch (error) {
				debugInfo.push('Error in page evaluation: ' + error.message);
			}

			return {
				trends: trends.slice(0, limit),
				debugInfo: debugInfo,
			};
		}
	`)

	if err != nil {
		return nil, fmt.Errorf("failed to evaluate page: %w", err)
	}

	// Parse the result
	resultMap, ok := result.(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("unexpected result type from page evaluation")
	}

	trendsData, ok := resultMap["trends"].([]interface{})
	if !ok {
		return nil, fmt.Errorf("no trends found in page evaluation result")
	}

	trends := make([]TrendData, 0, len(trendsData))
	for _, t := range trendsData {
		trendMap, ok := t.(map[string]interface{})
		if !ok {
			continue
		}

		rank, _ := trendMap["rank"].(float64)
		title, _ := trendMap["title"].(string)
		country, _ := trendMap["country"].(string)
		timeRange, _ := trendMap["time_range"].(string)
		scrapedAt, _ := trendMap["scrapedAt"].(string)

		trends = append(trends, TrendData{
			Rank:      int(rank),
			Title:     title,
			Country:   country,
			TimeRange: timeRange,
			ScrapedAt: scrapedAt,
		})
	}

	if len(trends) == 0 {
		return nil, fmt.Errorf("no trends found - page structure might have changed")
	}

	return &TrendsResponse{
		Success:     true,
		Country:     geo,
		TimeRange:   fmt.Sprintf("%dh", hours),
		TotalTrends: len(trends),
		ScrapedAt:   time.Now().Format(time.RFC3339),
		Trends:      trends,
	}, nil
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

// ScrapeTrends scrapes Google Trends for a given country and time range
func (s *Service) ScrapeTrends(ctx context.Context, req Request) (*TrendsResponse, error) {
	return s.scrapeTrends(ctx, req)
}
