// Package api wires the public HTTP surface and validates every request body.
package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/kc1wfotfm4/qry-schedule-retry/internal/schedule"
	"github.com/kc1wfotfm4/qry-schedule-retry/internal/store"
)

type handlers struct {
	store *store.Store
}

// NewRouter wires the public HTTP surface. The service contract in README.md
// describes the error shape every entry must keep.
func NewRouter(st *store.Store) *gin.Engine {
	gin.SetMode(gin.ReleaseMode)
	router := gin.New()
	router.Use(gin.Recovery())

	h := &handlers{store: st}

	router.GET("/healthz", h.getHealthz)
	router.POST("/api/v1/jobs", h.createJob)
	router.GET("/api/v1/jobs/:job", h.getJob)
	router.POST("/api/v1/jobs/:job/attempts", h.recordAttempt)
	router.POST("/api/v1/jobs/:job/pause", h.setPaused)

	router.NoRoute(func(c *gin.Context) {
		writeError(c, http.StatusNotFound, "route_not_found", "no route matches this path")
	})
	return router
}

func (h *handlers) getHealthz(c *gin.Context) {
	if err := h.store.Ping(); err != nil {
		writeError(c, http.StatusServiceUnavailable, "storage_unavailable", "database is not available")
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok", "database": "ok"})
}

type createJobRequest struct {
	Job        *string `json:"job"`
	Schedule   *string `json:"schedule"`
	MaxRetries *int    `json:"maxRetries"`
}

func (h *handlers) createJob(c *gin.Context) {
	var req createJobRequest
	if !decodeBody(c, &req) {
		return
	}
	if req.Job == nil || strings.TrimSpace(*req.Job) == "" {
		invalidRequest(c, "job must be a non-empty string")
		return
	}
	if req.Schedule == nil || !validCron(*req.Schedule) {
		invalidRequest(c, "schedule must be a valid five-field UTC cron expression")
		return
	}
	if req.MaxRetries == nil || *req.MaxRetries < 0 {
		invalidRequest(c, "maxRetries must be an integer greater than or equal to 0")
		return
	}

	job, err := h.store.CreateJob(c.Request.Context(), *req.Job, *req.Schedule, *req.MaxRetries)
	if err != nil {
		writeStoreError(c, err)
		return
	}
	c.JSON(http.StatusCreated, job)
}

func (h *handlers) getJob(c *gin.Context) {
	job, err := h.store.GetJob(c.Request.Context(), c.Param("job"))
	if err != nil {
		writeStoreError(c, err)
		return
	}
	c.JSON(http.StatusOK, job)
}

type recordAttemptRequest struct {
	Sequence    *int    `json:"sequence"`
	Outcome     *string `json:"outcome"`
	AttemptedAt *string `json:"attemptedAt"`
}

func (h *handlers) recordAttempt(c *gin.Context) {
	jobName := c.Param("job")
	if _, err := h.store.GetJob(c.Request.Context(), jobName); err != nil {
		writeStoreError(c, err)
		return
	}

	var req recordAttemptRequest
	if !decodeBody(c, &req) {
		return
	}
	if req.Sequence == nil || *req.Sequence < 1 {
		invalidRequest(c, "sequence must be an integer starting at 1")
		return
	}
	if req.Outcome == nil || (*req.Outcome != "success" && *req.Outcome != "failure") {
		invalidRequest(c, "outcome must be success or failure")
		return
	}
	attemptedAt, ok := parseAttemptedAt(c, req.AttemptedAt)
	if !ok {
		return
	}

	job, err := h.store.RecordAttempt(c.Request.Context(), jobName, *req.Sequence, *req.Outcome, attemptedAt)
	if err != nil {
		writeStoreError(c, err)
		return
	}
	c.JSON(http.StatusOK, job)
}

type setPausedRequest struct {
	Paused *bool `json:"paused"`
}

func (h *handlers) setPaused(c *gin.Context) {
	var req setPausedRequest
	if !decodeBody(c, &req) {
		return
	}
	if req.Paused == nil {
		invalidRequest(c, "paused must be a boolean")
		return
	}
	job, err := h.store.SetPaused(c.Request.Context(), c.Param("job"), *req.Paused)
	if err != nil {
		writeStoreError(c, err)
		return
	}
	c.JSON(http.StatusOK, job)
}

// decodeBody parses a JSON object without unknown fields or trailing data and
// reports validation errors using the published error shape.
func decodeBody(c *gin.Context, dst any) bool {
	raw, err := io.ReadAll(c.Request.Body)
	if err != nil {
		invalidRequest(c, "request body could not be read")
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		invalidRequest(c, "request body must be a valid JSON object")
		return false
	}
	var extra json.RawMessage
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		invalidRequest(c, "request body must contain a single JSON object")
		return false
	}
	return true
}

func parseAttemptedAt(c *gin.Context, value *string) (time.Time, bool) {
	if value == nil {
		invalidRequest(c, "attemptedAt must be an RFC3339 UTC timestamp")
		return time.Time{}, false
	}
	parsed, err := time.Parse(time.RFC3339, *value)
	if err != nil {
		invalidRequest(c, "attemptedAt must be an RFC3339 UTC timestamp")
		return time.Time{}, false
	}
	_, offset := parsed.Zone()
	if offset != 0 {
		invalidRequest(c, "attemptedAt must be an RFC3339 UTC timestamp")
		return time.Time{}, false
	}
	return parsed.UTC(), true
}

func validCron(expr string) bool {
	cron, err := schedule.Parse(expr)
	if err != nil {
		return false
	}
	_, err = cron.NextAfter(time.Now().UTC())
	return err == nil
}

func invalidRequest(c *gin.Context, message string) {
	writeError(c, http.StatusBadRequest, "invalid_request", message)
}

func writeStoreError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, store.ErrJobExists):
		writeError(c, http.StatusConflict, "job_exists", "a job with this name already exists")
	case errors.Is(err, store.ErrJobNotFound):
		writeError(c, http.StatusNotFound, "job_not_found", "the requested job does not exist")
	case errors.Is(err, store.ErrJobPaused):
		writeError(c, http.StatusConflict, "job_paused", "the job is paused and cannot accept attempts")
	case errors.Is(err, store.ErrSequenceConflict):
		writeError(c, http.StatusConflict, "sequence_conflict", "an attempt with this sequence already exists")
	default:
		writeError(c, http.StatusInternalServerError, "internal_error", "the request could not be completed")
	}
}

func writeError(c *gin.Context, status int, code, message string) {
	c.JSON(status, gin.H{"error": gin.H{"code": code, "message": message}})
}
