package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/kc1wfotfm4/qry-schedule-retry/internal/schedule"
	"github.com/kc1wfotfm4/qry-schedule-retry/internal/store"
)

type jobRequest struct {
	ID         *string `json:"id"`
	Schedule   *string `json:"schedule"`
	MaxRetries *int    `json:"max_retries"`
	BaseTime   *string `json:"base_time"`
}

func createJob(st *store.Store) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req jobRequest
		decoder := json.NewDecoder(c.Request.Body)
		if err := decoder.Decode(&req); err != nil {
			respondError(c, http.StatusBadRequest, "invalid_request", "request body is not a valid job object")
			return
		}
		// Reject trailing content such as a second JSON value after the object.
		if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
			respondError(c, http.StatusBadRequest, "invalid_request", "request body is not a valid job object")
			return
		}

		if req.ID == nil || req.Schedule == nil || req.MaxRetries == nil {
			respondError(c, http.StatusBadRequest, "invalid_request", "id, schedule and max_retries are required")
			return
		}
		if !validID(*req.ID) {
			respondError(c, http.StatusBadRequest, "invalid_request", "id must be 1 to 64 ASCII letters, digits, underscores or hyphens")
			return
		}
		if *req.MaxRetries < 0 || *req.MaxRetries > 100 {
			respondError(c, http.StatusBadRequest, "invalid_request", "max_retries must be an integer between 0 and 100")
			return
		}
		var baseTime *time.Time
		if req.BaseTime != nil {
			parsed, err := time.Parse(time.RFC3339, *req.BaseTime)
			if err != nil {
				respondError(c, http.StatusBadRequest, "invalid_request", "base_time must be an RFC3339 timestamp with a timezone offset")
				return
			}
			baseTime = &parsed
		}

		parsedSchedule, err := schedule.Parse(*req.Schedule)
		if err != nil {
			respondError(c, http.StatusBadRequest, "invalid_schedule", "schedule must be five fields: minute and hour as * or integers in range, day fields as *")
			return
		}

		job, err := st.RegisterJob(store.RegisterJobInput{
			ID:         *req.ID,
			Schedule:   parsedSchedule,
			MaxRetries: *req.MaxRetries,
			BaseTime:   baseTime,
		})
		if err != nil {
			respondStoreError(c, err)
			return
		}
		respondJob(c, http.StatusCreated, job)
	}
}

func getJob(st *store.Store) gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.Param("id")
		if !validID(id) {
			respondError(c, http.StatusBadRequest, "invalid_request", "id must be 1 to 64 ASCII letters, digits, underscores or hyphens")
			return
		}
		job, err := st.GetJob(id)
		if err != nil {
			respondStoreError(c, err)
			return
		}
		respondJob(c, http.StatusOK, job)
	}
}

func respondJob(c *gin.Context, status int, job *store.Job) {
	c.JSON(status, gin.H{
		"id":              job.ID,
		"schedule":        job.Schedule,
		"max_retries":     job.MaxRetries,
		"next_trigger_at": job.NextTriggerAt.UTC().Format(time.RFC3339),
		"paused":          job.Paused,
	})
}

func respondStoreError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, store.ErrJobExists):
		respondError(c, http.StatusConflict, "job_exists", "a job with this id already exists")
	case errors.Is(err, store.ErrJobNotFound):
		respondError(c, http.StatusNotFound, "job_not_found", "no job with this id exists")
	default:
		respondError(c, http.StatusServiceUnavailable, "storage_unavailable", "database is not available")
	}
}

func respondError(c *gin.Context, status int, code, message string) {
	c.JSON(status, gin.H{"error": gin.H{"code": code, "message": message}})
}

// validID reports whether id is 1 to 64 ASCII letters, digits, underscores or
// hyphens. Matching is case-sensitive at the storage layer.
func validID(id string) bool {
	if len(id) < 1 || len(id) > 64 {
		return false
	}
	for i := 0; i < len(id); i++ {
		switch ch := id[i]; {
		case ch >= 'a' && ch <= 'z':
		case ch >= 'A' && ch <= 'Z':
		case ch >= '0' && ch <= '9':
		case ch == '_' || ch == '-':
		default:
			return false
		}
	}
	return true
}
