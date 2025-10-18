package keywords

import (
	"github.com/gofiber/fiber/v2"
)

type Handler struct {
	service *Service
}

func NewHandler(service *Service) *Handler {
	return &Handler{
		service: service,
	}
}

// HandleGoogleAdsKeywords handles POST /keywords/googleAds
func (h *Handler) HandleGoogleAdsKeywords(c *fiber.Ctx) error {
	var req KeywordsRequest
	if err := c.BodyParser(&req); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error":   "Invalid request body",
			"details": err.Error(),
		})
	}

	// Validate required fields
	if req.Target == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "target is required",
		})
	}

	// Set defaults if not provided
	if req.TargetType == "" {
		req.TargetType = "page"
	}
	if req.Limit == 0 {
		req.Limit = 10
	}
	if req.SortBy == "" {
		req.SortBy = "relevance"
	}

	// Make request to DataForSEO
	resp, err := h.service.GetKeywordsForSite(req)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error":   "Failed to fetch keywords from DataForSEO",
			"details": err.Error(),
		})
	}

	// Return the response
	return c.JSON(resp)
}
