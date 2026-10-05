package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/kc1wfotfm4/qry-schedule-retry/internal/schedule"
	"github.com/kc1wfotfm4/qry-schedule-retry/internal/store"
)

// jobIDPattern is the published id shape: 1-64 ASCII letters, digits,
// underscores or hyphens, case-sensitive.
var jobIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// jsonIntegerPattern matches a JSON integer literal and nothing else.
var jsonIntegerPattern = regexp.MustCompile(`^-?[0-9]+$`)

// registerJobRequest keeps every field a pointer so a missing field is
// distinguishable from a present one. Unknown fields are ignored.
type registerJobRequest struct {
	ID         *string          `json:"id"`
	Schedule   *string          `json:"schedule"`
	MaxRetries *json.RawMessage `json:"max_retries"`
	BaseTime   *string          `json:"base_time"`
}

type jobResponse struct {
	ID            string `json:"id"`
	Schedule      string `json:"schedule"`
	MaxRetries    int    `json:"max_retries"`
	NextTriggerAt string `json:"next_trigger_at"`
	Paused        bool   `json:"paused"`
}

func writeError(c *gin.Context, status int, code, message string) {
	c.JSON(status, gin.H{"error": gin.H{"code": code, "message": message}})
}

func invalidRequest(c *gin.Context, message string) {
	writeError(c, http.StatusBadRequest, "invalid_request", message)
}

func storageUnavailable(c *gin.Context) {
	writeError(c, http.StatusServiceUnavailable, "storage_unavailable", "database is not available")
}

func toResponse(job store.Job) jobResponse {
	return jobResponse{
		ID:            job.ID,
		Schedule:      job.Schedule,
		MaxRetries:    job.MaxRetries,
		NextTriggerAt: job.NextTriggerAt.UTC().Format(time.RFC3339),
		Paused:        job.Paused,
	}
}

func registerJob(st *store.Store) gin.HandlerFunc {
	return func(c *gin.Context) {
		var request registerJobRequest
		decoder := json.NewDecoder(c.Request.Body)
		if err := decoder.Decode(&request); err != nil {
			invalidRequest(c, "request body must be a single JSON object")
			return
		}
		if err := decoder.Decode(&struct{}{}); err != io.EOF {
			invalidRequest(c, "request body must be a single JSON object")
			return
		}
		if request.ID == nil || request.Schedule == nil || request.MaxRetries == nil {
			invalidRequest(c, "id, schedule and max_retries are required")
			return
		}
		if !jobIDPattern.MatchString(*request.ID) {
			invalidRequest(c, "id must be 1 to 64 ASCII letters, digits, underscores or hyphens")
			return
		}
		raw := strings.TrimSpace(string(*request.MaxRetries))
		maxRetries, err := strconv.ParseInt(raw, 10, 64)
		if !jsonIntegerPattern.MatchString(raw) || err != nil || maxRetries < 0 || maxRetries > 100 {
			invalidRequest(c, "max_retries must be an integer between 0 and 100")
			return
		}
		var baseTime *time.Time
		if request.BaseTime != nil {
			parsed, err := time.Parse(time.RFC3339, *request.BaseTime)
			if err != nil {
				invalidRequest(c, "base_time must be an RFC3339 timestamp with a time zone offset")
				return
			}
			baseTime = &parsed
		}
		parsed, err := schedule.Parse(*request.Schedule)
		if err != nil {
			writeError(c, http.StatusBadRequest, "invalid_schedule", "schedule must be five fields: minute 0-59 and hour 0-23 as * or integers, the remaining fields *")
			return
		}
		job, err := st.RegisterJob(*request.ID, parsed.String(), int(maxRetries), baseTime, parsed.Next)
		if errors.Is(err, store.ErrJobExists) {
			writeError(c, http.StatusConflict, "job_exists", "a job with this id already exists")
			return
		}
		if err != nil {
			storageUnavailable(c)
			return
		}
		c.JSON(http.StatusCreated, toResponse(job))
	}
}

func getJob(st *store.Store) gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.Param("id")
		if !jobIDPattern.MatchString(id) {
			invalidRequest(c, "id must be 1 to 64 ASCII letters, digits, underscores or hyphens")
			return
		}
		job, err := st.GetJob(id)
		if errors.Is(err, store.ErrJobNotFound) {
			writeError(c, http.StatusNotFound, "job_not_found", "no job exists with this id")
			return
		}
		if err != nil {
			storageUnavailable(c)
			return
		}
		c.JSON(http.StatusOK, toResponse(job))
	}
}
