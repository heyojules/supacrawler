package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/hibiken/asynq"

	"scraper/internal/config"
	"scraper/internal/core/analyzer"
	"scraper/internal/core/crawler"
	"scraper/internal/core/job"
	"scraper/internal/core/keywords"
	"scraper/internal/core/mapper"
	"scraper/internal/core/scraper"
	"scraper/internal/logger"
	rds "scraper/internal/platform/redis"
	tasks "scraper/internal/platform/tasks"
	"scraper/internal/server"
	"scraper/internal/worker"
)

func main() {
	cfg := config.Load()
	log.Printf("[scraper] starting at %s (env=%s)\n", cfg.HTTPAddr, cfg.AppEnv)

	// Initialize logger
	logr := logger.New("main")

	// Redis client
	redisSvc, err := rds.New(rds.Options{
		Addr:     cfg.RedisAddr,
		Password: cfg.RedisPassword,
	})
	if err != nil {
		log.Fatal(err)
	}
	defer redisSvc.Close()

	// Asynq client and server
	taskClient := tasks.New(redisSvc)
	asynqServer := asynq.NewServer(redisSvc.AsynqRedisOpt(), asynq.Config{
		Concurrency: 10,
		Queues:      map[string]int{"default": 1},
	})

	// Core services
	jobSvc := job.NewJobService(redisSvc)

	mapSvc := mapper.NewMapService()
	scraperSvc := scraper.NewScraperService(redisSvc)
	crawlerSvc := crawler.NewCrawlerService(jobSvc, taskClient, mapSvc, scraperSvc, cfg)
	analyzerSvc, err := analyzer.New(cfg, jobSvc)
	if err != nil {
		log.Fatal(err)
	}

	// Keywords service - using the API key from environment
	log.Printf("[main] SEO API Key length: %d", len(cfg.SEOApiKey))
	if cfg.SEOApiKey == "" {
		log.Printf("[main] WARNING: SEO_API_KEY is empty!")
	}
	keywordsSvc := keywords.NewService(cfg.SEOApiKey)

	// Worker mux
	mux := worker.NewMux()
	mux.HandleFunc(tasks.TaskTypeCrawl, crawlerSvc.HandleCrawlTask)
	mux.HandleFunc(analyzer.TaskTypeAnalyze, analyzerSvc.HandleTask)

	// Start worker
	_, workerCancel := context.WithCancel(context.Background())
	go func() {
		if err := asynqServer.Start(mux.Mux()); err != nil {
			log.Printf("[worker] stopped: %v\n", err)
		}
	}()

	// HTTP server
	app := fiber.New(fiber.Config{
		AppName: "Supacrawler Engine",
		JSONEncoder: func(v interface{}) ([]byte, error) {
			var buf bytes.Buffer
			encoder := json.NewEncoder(&buf)
			encoder.SetEscapeHTML(false)
			if err := encoder.Encode(v); err != nil {
				return nil, err
			}
			return buf.Bytes(), nil
		},
	})
	// Serve saved artifacts (e.g., screenshots) from DATA_DIR under /files
	app.Static("/files", cfg.DataDir)

	// Register routes with health handler
	deps := server.Dependencies{
		Job:      jobSvc,
		Crawler:  crawlerSvc,
		Scraper:  scraperSvc,
		Map:      mapSvc,
		Analyzer: analyzerSvc,
		Keywords: keywordsSvc,
		Tasks:    taskClient,
		Redis:    redisSvc,
	}
	healthHandler := server.RegisterRoutes(app, deps)

	// Mark application as ready after all services are initialized
	go func() {
		time.Sleep(5 * time.Second) // Allow services to fully initialize
		healthHandler.SetReady()
	}()

	// Graceful shutdown
	shutdown := make(chan os.Signal, 1)
	signal.Notify(shutdown, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-shutdown
		logr.LogInfo("Shutting down...")
		workerCancel()
		asynqServer.Shutdown()

		// Close analyzer service browser
		if err := analyzerSvc.Close(); err != nil {
			logr.LogWarnf("Error closing analyzer service: %v", err)
		}

		_ = app.ShutdownWithTimeout(5 * time.Second)
	}()

	if err := app.Listen(cfg.HTTPAddr); err != nil {
		log.Fatalf("server listen: %v", err)
	}
}
