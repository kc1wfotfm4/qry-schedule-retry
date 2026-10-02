package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/kc1wfotfm4/qry-schedule-retry/internal/schedule"
	"github.com/kc1wfotfm4/qry-schedule-retry/internal/store"
)

// NewRouter wires the public HTTP surface described in README.md.
func NewRouter(st *store.Store) *gin.Engine {
	gin.SetMode(gin.ReleaseMode)
	router := gin.New()
	router.Use(gin.Recovery())

	h := &handler{store: st}

	router.GET("/healthz", h.healthz)

	router.POST("/api/v1/jobs", h.createJob)
	router.GET("/api/v1/jobs/:job", h.getJob)
	router.POST("/api/v1/jobs/:job/attempts", h.recordAttempt)
	router.POST("/api/v1/jobs/:job/pause", h.setPaused)

	router.NoRoute(func(c *gin.Context) {
		writeError(c, http.StatusNotFound, "route_not_found", "no route matches this path")
	})
	return router
}

type handler struct {
	store *store.Store
}

type createJobRequest struct {
	Job        *string `json:"job"`
	Schedule   *string `json:"schedule"`
	MaxRetries *int    `json:"maxRetries"`
}

type attemptRequest struct {
	Sequence    *int    `json:"sequence"`
	Outcome     *string `json:"outcome"`
	AttemptedAt *string `json:"attemptedAt"`
}

type pauseRequest struct {
	Paused *bool `json:"paused"`
}

type jobState struct {
	Job            string         `json:"job"`
	Schedule       string         `json:"schedule"`
	MaxRetries     int            `json:"maxRetries"`
	FailureRetries int            `json:"failureRetries"`
	Paused         bool           `json:"paused"`
	NextRunAt      string         `json:"nextRunAt"`
	Attempts       []attemptState `json:"attempts"`
}

type attemptState struct {
	Sequence    int    `json:"sequence"`
	Outcome     string `json:"outcome"`
	AttemptedAt string `json:"attemptedAt"`
}

func (h *handler) healthz(c *gin.Context) {
	if err := h.store.Ping(); err != nil {
		writeError(c, http.StatusServiceUnavailable, "storage_unavailable", "database is not available")
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok", "database": "ok"})
}

func (h *handler) createJob(c *gin.Context) {
	var req createJobRequest
	if !decodeBody(c, &req) {
		return
	}
	if req.Job == nil || req.Schedule == nil || req.MaxRetries == nil ||
		*req.Job == "" || *req.MaxRetries < 0 {
		writeInvalidRequest(c)
		return
	}
	parsed, err := schedule.Parse(*req.Schedule)
	if err != nil {
		writeInvalidRequest(c)
		return
	}

	created, err := h.store.CreateJob(*req.Job, *req.Schedule, *req.MaxRetries, parsed.NextAfter)
	if err != nil {
		h.writeStoreError(c, err, "create job failed")
		return
	}
	h.respondJob(c, http.StatusCreated, created, nil)
}

func (h *handler) getJob(c *gin.Context) {
	job, attempts, err := h.store.GetJob(c.Param("job"))
	if err != nil {
		h.writeStoreError(c, err, "load job failed")
		return
	}
	h.respondJob(c, http.StatusOK, job, attempts)
}

func (h *handler) recordAttempt(c *gin.Context) {
	var req attemptRequest
	if !decodeBody(c, &req) {
		return
	}
	if req.Sequence == nil || req.Outcome == nil || req.AttemptedAt == nil ||
		*req.Sequence < 1 || (*req.Outcome != "success" && *req.Outcome != "failure") {
		writeInvalidRequest(c)
		return
	}
	attemptedAt, ok := parseUTCTime(c, *req.AttemptedAt)
	if !ok {
		return
	}

	updated, err := h.store.RecordAttempt(c.Param("job"), store.Attempt{
		Sequence:    *req.Sequence,
		Outcome:     *req.Outcome,
		AttemptedAt: attemptedAt,
	}, prepareNext)
	if err != nil {
		h.writeStoreError(c, err, "record attempt failed")
		return
	}

	// A failure that exhausts retries pauses the job inside the transaction.
	_, attempts, err := h.store.GetJob(c.Param("job"))
	if err != nil {
		h.writeStoreError(c, err, "load job failed")
		return
	}
	h.respondJob(c, http.StatusOK, updated, attempts)
}

func (h *handler) setPaused(c *gin.Context) {
	var req pauseRequest
	if !decodeBody(c, &req) {
		return
	}
	if req.Paused == nil {
		writeInvalidRequest(c)
		return
	}
	job, err := h.store.SetPaused(c.Param("job"), *req.Paused)
	if err != nil {
		h.writeStoreError(c, err, "update pause failed")
		return
	}
	_, attempts, err := h.store.GetJob(c.Param("job"))
	if err != nil {
		h.writeStoreError(c, err, "load job failed")
		return
	}
	h.respondJob(c, http.StatusOK, job, attempts)
}

func (h *handler) respondJob(c *gin.Context, status int, job *store.Job, attempts []store.Attempt) {
	state := jobState{
		Job:            job.Job,
		Schedule:       job.Schedule,
		MaxRetries:     job.MaxRetries,
		FailureRetries: job.FailureRetries,
		Paused:         job.Paused,
		NextRunAt:      formatTime(job.NextRunAt),
		Attempts:       make([]attemptState, 0, len(attempts)),
	}
	for _, attempt := range attempts {
		state.Attempts = append(state.Attempts, attemptState{
			Sequence:    attempt.Sequence,
			Outcome:     attempt.Outcome,
			AttemptedAt: formatTime(attempt.AttemptedAt),
		})
	}
	c.JSON(status, state)
}

// prepareNext re-parses the stored cron expression inside the attempt
// transaction's caller, turning it into the next-trigger function.
func prepareNext(expr string) (store.NextFunc, error) {
	parsed, err := schedule.Parse(expr)
	if err != nil {
		return nil, err
	}
	return parsed.NextAfter, nil
}

// writeStoreError maps the store's sentinel conflicts to the published error
// contract while hiding every implementation detail behind a generic message.
func (h *handler) writeStoreError(c *gin.Context, err error, fallbackMessage string) {
	switch {
	case errors.Is(err, store.ErrJobNotFound):
		writeError(c, http.StatusNotFound, "job_not_found", "the requested job does not exist")
	case errors.Is(err, store.ErrJobExists):
		writeError(c, http.StatusConflict, "job_exists", "a job with this name already exists")
	case errors.Is(err, store.ErrSequenceConflict):
		writeError(c, http.StatusConflict, "sequence_conflict", "an attempt with this sequence already exists")
	case errors.Is(err, store.ErrJobPaused):
		writeError(c, http.StatusConflict, "job_paused", "job is paused and does not accept attempts")
	default:
		writeError(c, http.StatusInternalServerError, "internal_error", fallbackMessage)
	}
}

// decodeBody parses exactly one JSON object. Malformed bodies, unknown fields,
// multiple values and trailing tokens are all invalid requests.
func decodeBody(c *gin.Context, target any) bool {
	body, err := io.ReadAll(c.Request.Body)
	if err != nil || len(bytes.TrimSpace(body)) == 0 {
		writeInvalidRequest(c)
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		writeInvalidRequest(c)
		return false
	}
	if decoder.More() {
		writeInvalidRequest(c)
		return false
	}
	return true
}

// parseUTCTime accepts only RFC3339 timestamps stamped at the UTC offset (Z or
// +00:00), as required by the contract.
func parseUTCTime(c *gin.Context, value string) (time.Time, bool) {
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		writeInvalidRequest(c)
		return time.Time{}, false
	}
	_, offset := parsed.Zone()
	if offset != 0 {
		writeInvalidRequest(c)
		return time.Time{}, false
	}
	return parsed.In(time.UTC), true
}

func formatTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}

func writeInvalidRequest(c *gin.Context) {
	writeError(c, http.StatusBadRequest, "invalid_request", "the request body or one of its fields is invalid")
}

func writeError(c *gin.Context, status int, code, message string) {
	c.JSON(status, gin.H{"error": gin.H{"code": code, "message": message}})
}
