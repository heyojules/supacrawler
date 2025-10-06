package analyzer

import (
	"fmt"
	"scraper/internal/core/job"
	tasks "scraper/internal/platform/tasks"

	"github.com/gofiber/fiber/v2"
)

type Handler struct {
	service *Service
	tasks   *tasks.Client
	jobs    *job.JobService
}

func NewHandler(service *Service, tasks *tasks.Client, jobs *job.JobService) *Handler {
	return &Handler{service: service, tasks: tasks, jobs: jobs}
}

// HandleCreateAnalyze handles Google Trends analysis requests
func (h *Handler) HandleCreateAnalyze(c *fiber.Ctx) error {
	var req Request
	if err := c.BodyParser(&req); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(map[string]interface{}{
			"success": false,
			"error":   "invalid request body",
		})
	}

	// Keyword is now the primary input, country is optional
	if req.Keyword == "" {
		return c.Status(fiber.StatusBadRequest).JSON(map[string]interface{}{
			"success": false,
			"error":   "keyword is required",
		})
	}

	// Set defaults
	if req.Hours <= 0 {
		req.Hours = 24 // Default to 24 hours
	}

	// For synchronous requests (when limit is specified), analyze directly
	if req.Limit != nil && *req.Limit > 0 {
		result, err := h.service.ScrapeAnalyze(c.Context(), req)
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(map[string]interface{}{
				"success": false,
				"error":   err.Error(),
			})
		}
		return c.JSON(result)
	}

	// For async requests, enqueue the job
	id, err := h.service.Enqueue(c.Context(), h.tasks, req)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(map[string]interface{}{
			"success": false,
			"error":   err.Error(),
		})
	}

	return c.JSON(map[string]interface{}{
		"success":    true,
		"job_id":     id,
		"keyword":    req.Keyword,
		"country":    req.Country,
		"time_range": fmt.Sprintf("%dh", req.Hours),
		"message":    "Google Trends analysis job queued",
	})
}

// HandleGetAnalyze handles getting Google Trends job status/results
func (h *Handler) HandleGetAnalyze(c *fiber.Ctx) error {
	jobID := c.Query("job_id")
	if jobID == "" {
		return c.Status(fiber.StatusBadRequest).JSON(map[string]interface{}{
			"success": false,
			"error":   "job_id is required",
		})
	}

	j, err := h.jobs.GetJobStatus(c.Context(), jobID)
	if err != nil {
		return c.Status(fiber.StatusNotFound).JSON(map[string]interface{}{
			"success": false,
			"error":   "job not found",
		})
	}

	// If job is not completed, return status
	if j.Status != job.StatusCompleted {
		status := "processing"
		if j.Status == job.StatusFailed {
			status = "failed"
		}
		return c.Status(fiber.StatusAccepted).JSON(map[string]interface{}{
			"success": true,
			"job_id":  j.JobID,
			"status":  status,
		})
	}

	// For completed jobs, we would need to store and retrieve the trends data
	// For now, return a placeholder response
	return c.JSON(map[string]interface{}{
		"success": true,
		"job_id":  j.JobID,
		"status":  "completed",
		"message": "Trends data would be available here",
	})
}

// HandleCreate keeps the old screenshot endpoint for backward compatibility
func (h *Handler) HandleCreate(c *fiber.Ctx) error {
	return h.HandleCreateAnalyze(c)
}

// HandleGet keeps the old screenshot endpoint for backward compatibility
func (h *Handler) HandleGet(c *fiber.Ctx) error {
	return h.HandleGetAnalyze(c)
}
