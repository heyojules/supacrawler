package server

import (
	"scraper/internal/core/analyzer"
	"scraper/internal/core/crawler"
	"scraper/internal/core/job"
	"scraper/internal/core/mapper"
	"scraper/internal/core/scraper"
	"scraper/internal/health"
	"scraper/internal/platform/redis"
	tasks "scraper/internal/platform/tasks"

	"github.com/gofiber/fiber/v2"
)

type Dependencies struct {
	Job      *job.JobService
	Crawler  *crawler.CrawlerService
	Scraper  *scraper.ScraperService
	Map      *mapper.Service
	Analyzer *analyzer.Service
	Tasks    *tasks.Client
	Redis    *redis.Service
}

func RegisterRoutes(app *fiber.App, d Dependencies) *health.HealthHandler {
	// Health endpoints
	healthHandler := health.NewHealthHandler(d.Redis)
	app.Get("/v1/health", health.HealthLimiter(), healthHandler.HandleHealth)

	api := app.Group("/v1")

	scraperHandler := scraper.NewScraperHandler(d.Scraper, d.Map)
	api.Get("/scrape", scraperHandler.HandleGetScrape)

	crawlerHandler := crawler.NewCrawlerHandler(d.Job, d.Crawler)
	api.Post("/crawl", crawlerHandler.HandleCreateCrawl)
	api.Get("/crawl/:jobId", crawlerHandler.HandleGetCrawl)

	analyzerHandler := analyzer.NewHandler(d.Analyzer, d.Tasks, d.Job)
	api.Post("/analyze", analyzerHandler.HandleCreateAnalyze)
	api.Get("/analyze", analyzerHandler.HandleGetAnalyze)

	// Keep old screenshot endpoints for backward compatibility
	api.Post("/screenshots", analyzerHandler.HandleCreate)
	api.Get("/screenshots", analyzerHandler.HandleGet)

	return healthHandler
}
