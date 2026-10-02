package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/kc1wfotfm4/qry-schedule-retry/internal/store"
)

func newTestRouter(t *testing.T) (*gin.Engine, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "service.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return NewRouter(st), st
}

func doJSON(t *testing.T, router *gin.Engine, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body != "" {
		reader = bytes.NewReader([]byte(body))
	} else {
		reader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func decodeState(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var state map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &state); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return state
}

func errorCode(rec *httptest.ResponseRecorder) string {
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		return ""
	}
	return body.Error.Code
}

func TestCreateJobLifecycle(t *testing.T) {
	router, _ := newTestRouter(t)

	rec := doJSON(t, router, http.MethodPost, "/api/v1/jobs", `{"job":"backup","schedule":"0 0 * * *","maxRetries":3}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d body = %s", rec.Code, rec.Body.String())
	}
	state := decodeState(t, rec)
	for field, want := range map[string]any{
		"job": "backup", "schedule": "0 0 * * *", "maxRetries": float64(3),
		"failureRetries": float64(0), "paused": false,
	} {
		if state[field] != want {
			t.Errorf("state[%s] = %v, want %v", field, state[field], want)
		}
	}
	next, ok := state["nextRunAt"].(string)
	if !ok {
		t.Fatal("nextRunAt missing")
	}
	parsed, err := time.Parse(time.RFC3339, next)
	if err != nil {
		t.Fatalf("nextRunAt %q is not RFC3339: %v", next, err)
	}
	if parsed.UTC().Format("15:04") != "00:00" {
		t.Fatalf("nextRunAt = %s, want midnight UTC", next)
	}
	attempts, ok := state["attempts"].([]any)
	if !ok || len(attempts) != 0 {
		t.Fatalf("attempts = %v, want empty array", state["attempts"])
	}

	rec = doJSON(t, router, http.MethodPost, "/api/v1/jobs", `{"job":"backup","schedule":"0 0 * * *","maxRetries":3}`)
	if rec.Code != http.StatusConflict || errorCode(rec) != "job_exists" {
		t.Fatalf("duplicate = %d %s", rec.Code, errorCode(rec))
	}

	rec = doJSON(t, router, http.MethodGet, "/api/v1/jobs/backup", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("get status = %d", rec.Code)
	}
	if got := decodeState(t, rec)["job"]; got != "backup" {
		t.Fatalf("get job = %v", got)
	}

	rec = doJSON(t, router, http.MethodGet, "/api/v1/jobs/missing", "")
	if rec.Code != http.StatusNotFound || errorCode(rec) != "job_not_found" {
		t.Fatalf("missing = %d %s", rec.Code, errorCode(rec))
	}
}

func TestCreateJobValidation(t *testing.T) {
	router, _ := newTestRouter(t)
	bodies := []string{
		"", "not json", "{}",
		`{"job":"","schedule":"0 0 * * *","maxRetries":3}`,
		`{"job":"x","schedule":"60 0 * * *","maxRetries":3}`,
		`{"job":"x","schedule":"0 0 * *","maxRetries":3}`,
		`{"job":"x","schedule":"0 0 * * *","maxRetries":-1}`,
		`{"job":123,"schedule":"0 0 * * *","maxRetries":3}`,
		`{"job":"x","schedule":"0 0 * * *"}`,
		`{"job":"x","schedule":"0 0 * * *","maxRetries":3,"extra":true}`,
		`{"job":"x","schedule":"0 0 * * *","maxRetries":true}`,
		`null`,
	}
	for i, body := range bodies {
		rec := doJSON(t, router, http.MethodPost, "/api/v1/jobs", body)
		if rec.Code != http.StatusBadRequest || errorCode(rec) != "invalid_request" {
			t.Errorf("case %d body=%q: %d %s", i, body, rec.Code, errorCode(rec))
		}
	}
}

func TestAttemptsRetryAndPauseFlow(t *testing.T) {
	router, _ := newTestRouter(t)

	rec := doJSON(t, router, http.MethodPost, "/api/v1/jobs", `{"job":"j","schedule":"0 * * * *","maxRetries":1}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %s", rec.Body.String())
	}

	post := func(body string) *httptest.ResponseRecorder {
		return doJSON(t, router, http.MethodPost, "/api/v1/jobs/j/attempts", body)
	}

	rec = post(`{"sequence":1,"outcome":"failure","attemptedAt":"2026-10-03T10:00:00Z"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("first failure = %d %s", rec.Code, rec.Body.String())
	}
	state := decodeState(t, rec)
	if state["failureRetries"] != float64(1) || state["paused"] != false {
		t.Fatalf("state after first failure: %+v", state)
	}
	if state["nextRunAt"] != "2026-10-03T10:02:00Z" {
		t.Fatalf("nextRunAt = %v, want 2 minutes later", state["nextRunAt"])
	}

	rec = post(`{"sequence":1,"outcome":"failure","attemptedAt":"2026-10-03T10:00:00Z"}`)
	if rec.Code != http.StatusConflict || errorCode(rec) != "sequence_conflict" {
		t.Fatalf("dup sequence = %d %s", rec.Code, errorCode(rec))
	}
	state = decodeState(t, doJSON(t, router, http.MethodGet, "/api/v1/jobs/j", ""))
	if state["failureRetries"] != float64(1) {
		t.Fatalf("retries changed by conflicted attempt: %v", state["failureRetries"])
	}

	rec = post(`{"sequence":2,"outcome":"failure","attemptedAt":"2026-10-03T10:05:00Z"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("second failure = %d %s", rec.Code, rec.Body.String())
	}
	state = decodeState(t, rec)
	if state["paused"] != true || state["failureRetries"] != float64(1) {
		t.Fatalf("state after exhaustion: %+v", state)
	}
	if state["nextRunAt"] != "2026-10-03T10:02:00Z" {
		t.Fatalf("schedule must not advance on exhaustion, got %v", state["nextRunAt"])
	}
	attempts := state["attempts"].([]any)
	if len(attempts) != 2 {
		t.Fatalf("attempts = %v", attempts)
	}

	rec = post(`{"sequence":3,"outcome":"success","attemptedAt":"2026-10-03T10:06:00Z"}`)
	if rec.Code != http.StatusConflict || errorCode(rec) != "job_paused" {
		t.Fatalf("attempt while paused = %d %s", rec.Code, errorCode(rec))
	}

	rec = doJSON(t, router, http.MethodPost, "/api/v1/jobs/j/pause", `{"paused":false}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("resume = %d", rec.Code)
	}
	state = decodeState(t, rec)
	if state["paused"] != false || state["failureRetries"] != float64(1) {
		t.Fatalf("resume altered counters: %+v", state)
	}

	rec = post(`{"sequence":3,"outcome":"success","attemptedAt":"2026-10-03T10:06:00Z"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("success after resume = %d %s", rec.Code, rec.Body.String())
	}
	state = decodeState(t, rec)
	if state["failureRetries"] != float64(0) || state["paused"] != false {
		t.Fatalf("state after success: %+v", state)
	}
	if state["nextRunAt"] != "2026-10-03T11:00:00Z" {
		t.Fatalf("nextRunAt after success = %v, want next cron tick", state["nextRunAt"])
	}
	attempts = state["attempts"].([]any)
	if len(attempts) != 3 {
		t.Fatalf("attempts = %v", attempts)
	}
}

func TestAttemptValidationAndMissingJob(t *testing.T) {
	router, _ := newTestRouter(t)

	rec := doJSON(t, router, http.MethodPost, "/api/v1/jobs/nope/attempts",
		`{"sequence":1,"outcome":"success","attemptedAt":"2026-10-03T10:00:00Z"}`)
	if rec.Code != http.StatusNotFound || errorCode(rec) != "job_not_found" {
		t.Fatalf("missing job = %d %s", rec.Code, errorCode(rec))
	}

	if _, err := store.Open(filepath.Join(t.TempDir(), "x.db")); err != nil {
		t.Fatal(err)
	}
	doJSON(t, router, http.MethodPost, "/api/v1/jobs", `{"job":"j","schedule":"0 * * * *","maxRetries":3}`)

	bodies := []string{
		`{"sequence":0,"outcome":"success","attemptedAt":"2026-10-03T10:00:00Z"}`,
		`{"sequence":1,"outcome":"ok","attemptedAt":"2026-10-03T10:00:00Z"}`,
		`{"sequence":1,"outcome":"success","attemptedAt":"2026-10-03T10:00:00+08:00"}`,
		`{"sequence":1,"outcome":"success","attemptedAt":"not-a-time"}`,
		`{"outcome":"success","attemptedAt":"2026-10-03T10:00:00Z"}`,
		`{"sequence":"1","outcome":"success","attemptedAt":"2026-10-03T10:00:00Z"}`,
	}
	for i, body := range bodies {
		rec := doJSON(t, router, http.MethodPost, "/api/v1/jobs/j/attempts", body)
		if rec.Code != http.StatusBadRequest || errorCode(rec) != "invalid_request" {
			t.Errorf("case %d: %d %s", i, rec.Code, errorCode(rec))
		}
	}
}

func TestPauseValidation(t *testing.T) {
	router, _ := newTestRouter(t)
	doJSON(t, router, http.MethodPost, "/api/v1/jobs", `{"job":"j","schedule":"0 * * * *","maxRetries":3}`)

	rec := doJSON(t, router, http.MethodPost, "/api/v1/jobs/missing/pause", `{"paused":true}`)
	if rec.Code != http.StatusNotFound || errorCode(rec) != "job_not_found" {
		t.Fatalf("pause missing = %d %s", rec.Code, errorCode(rec))
	}
	for _, body := range []string{`{}`, `{"paused":"yes"}`, `null`} {
		rec = doJSON(t, router, http.MethodPost, "/api/v1/jobs/j/pause", body)
		if rec.Code != http.StatusBadRequest || errorCode(rec) != "invalid_request" {
			t.Fatalf("body %q: %d %s", body, rec.Code, errorCode(rec))
		}
	}
}

func TestBackoffPowersOfTwoAcrossMultipleFailures(t *testing.T) {
	router, _ := newTestRouter(t)
	doJSON(t, router, http.MethodPost, "/api/v1/jobs", `{"job":"j","schedule":"0 * * * *","maxRetries":5}`)

	want := map[int]string{1: "10:02", 2: "10:04", 3: "10:08", 4: "10:16", 5: "10:32"}
	for seq := 1; seq <= 5; seq++ {
		body := `{"sequence":` + itoa(seq) + `,"outcome":"failure","attemptedAt":"2026-10-03T10:00:00Z"}`
		rec := doJSON(t, router, http.MethodPost, "/api/v1/jobs/j/attempts", body)
		if rec.Code != http.StatusOK {
			t.Fatalf("failure %d = %d %s", seq, rec.Code, rec.Body.String())
		}
		state := decodeState(t, rec)
		next := state["nextRunAt"].(string)
		parsed, _ := time.Parse(time.RFC3339, next)
		if got := parsed.UTC().Format("15:04"); got != want[seq] {
			t.Errorf("seq %d next = %s, want %s", seq, got, want[seq])
		}
	}
	rec := doJSON(t, router, http.MethodPost, "/api/v1/jobs/j/attempts",
		`{"sequence":6,"outcome":"failure","attemptedAt":"2026-10-03T10:00:00Z"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("exhausting failure = %d %s", rec.Code, rec.Body.String())
	}
	state := decodeState(t, rec)
	if state["paused"] != true {
		t.Fatal("job should pause exactly when retries are exhausted")
	}
	if state["failureRetries"] != float64(5) {
		t.Fatalf("retries = %v, want 5", state["failureRetries"])
	}
	if state["nextRunAt"] != "2026-10-03T10:32:00Z" {
		t.Fatalf("nextRunAt = %v, must stay at last backoff on exhaustion", state["nextRunAt"])
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf []byte
	for n > 0 {
		buf = append([]byte{byte('0' + n%10)}, buf...)
		n /= 10
	}
	return string(buf)
}
