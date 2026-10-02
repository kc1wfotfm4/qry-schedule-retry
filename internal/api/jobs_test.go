package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/kc1wfotfm4/qry-schedule-retry/internal/store"
)

type testServer struct {
	router http.Handler
	store  *store.Store
}

func newTestServer(t *testing.T) *testServer {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "service.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return &testServer{router: NewRouter(st), store: st}
}

func (ts *testServer) do(t *testing.T, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		reader = bytes.NewReader(raw)
	} else {
		reader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	ts.router.ServeHTTP(rec, req)
	return rec
}

func (ts *testServer) doRaw(t *testing.T, method, path, raw string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewBufferString(raw))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	ts.router.ServeHTTP(rec, req)
	return rec
}

func decodeJob(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var payload struct {
		Job        string           `json:"job"`
		Schedule   string           `json:"schedule"`
		MaxRetries int              `json:"maxRetries"`
		Retries    int              `json:"retries"`
		Paused     bool             `json:"paused"`
		NextRunAt  string           `json:"nextRunAt"`
		Attempts   []map[string]any `json:"attempts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode job: %v\nbody: %s", err, rec.Body.String())
	}
	out := map[string]any{
		"job": payload.Job, "schedule": payload.Schedule,
		"maxRetries": payload.MaxRetries, "retries": payload.Retries,
		"paused": payload.Paused, "nextRunAt": payload.NextRunAt,
		"attempts": payload.Attempts,
	}
	return out
}

func errorCode(rec *httptest.ResponseRecorder) string {
	var payload struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		return ""
	}
	return payload.Error.Code
}

func TestCreateJobSuccess(t *testing.T) {
	ts := newTestServer(t)
	before := time.Now().UTC()
	rec := ts.do(t, http.MethodPost, "/api/v1/jobs", map[string]any{
		"job": "nightly", "schedule": "5 0 * * *", "maxRetries": 3,
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	job := decodeJob(t, rec)
	if job["job"] != "nightly" || job["schedule"] != "5 0 * * *" || job["maxRetries"] != 3 {
		t.Fatalf("unexpected job fields: %v", job)
	}
	if job["retries"] != 0 || job["paused"] != false {
		t.Fatalf("new job must start active with zero retries: %v", job)
	}
	nextRun, err := time.Parse(time.RFC3339, job["nextRunAt"].(string))
	if err != nil {
		t.Fatalf("nextRunAt not RFC3339: %v", err)
	}
	want := time.Date(before.Year(), before.Month(), before.Day()+1, 0, 5, 0, 0, time.UTC)
	if !nextRun.Equal(want) {
		t.Fatalf("nextRunAt = %s, want %s", nextRun, want)
	}
	attempts, ok := job["attempts"].([]map[string]any)
	if !ok || len(attempts) != 0 {
		t.Fatalf("attempts = %v, want empty array", job["attempts"])
	}
}

func TestCreateJobDuplicate(t *testing.T) {
	ts := newTestServer(t)
	body := map[string]any{"job": "dup", "schedule": "0 * * * *", "maxRetries": 1}
	if rec := ts.do(t, http.MethodPost, "/api/v1/jobs", body); rec.Code != http.StatusCreated {
		t.Fatalf("first create status = %d", rec.Code)
	}
	rec := ts.do(t, http.MethodPost, "/api/v1/jobs", body)
	if rec.Code != http.StatusConflict || errorCode(rec) != "job_exists" {
		t.Fatalf("dup: status = %d code = %s", rec.Code, errorCode(rec))
	}
}

func TestCreateJobInvalidRequests(t *testing.T) {
	ts := newTestServer(t)
	cases := []map[string]any{
		{"schedule": "0 * * * *", "maxRetries": 1},                            // missing job
		{"job": "", "schedule": "0 * * * *", "maxRetries": 1},                 // empty job
		{"job": 42, "schedule": "0 * * * *", "maxRetries": 1},                 // wrong type
		{"job": "x", "maxRetries": 1},                                         // missing schedule
		{"job": "x", "schedule": "61 * * * *", "maxRetries": 1},               // bad cron
		{"job": "x", "schedule": "0 * * *", "maxRetries": 1},                  // four fields
		{"job": "x", "schedule": "0 * * * *", "maxRetries": -1},               // negative
		{"job": "x", "schedule": "0 * * * *", "maxRetries": 1.5},              // float
		{"job": "x", "schedule": "0 0 31 2 *", "maxRetries": 1},               // impossible cron
		{"job": "x", "schedule": "0 * * * *", "maxRetries": 1, "bogus": true}, // unknown field
	}
	paths := []string{
		"/api/v1/jobs", "/api/v1/jobs", "/api/v1/jobs", "/api/v1/jobs",
		"/api/v1/jobs", "/api/v1/jobs", "/api/v1/jobs", "/api/v1/jobs",
		"/api/v1/jobs", "/api/v1/jobs",
	}
	for i, payload := range cases {
		rec := ts.do(t, http.MethodPost, paths[i], payload)
		if rec.Code != http.StatusBadRequest || errorCode(rec) != "invalid_request" {
			t.Fatalf("case %d: status = %d code = %s body = %s", i, rec.Code, errorCode(rec), rec.Body.String())
		}
	}
	for _, raw := range []string{"not json", "", `{"job":"x","schedule":"0 * * * *","maxRetries":1}{}`} {
		rec := ts.doRaw(t, http.MethodPost, "/api/v1/jobs", raw)
		if rec.Code != http.StatusBadRequest || errorCode(rec) != "invalid_request" {
			t.Fatalf("raw %q: status = %d code = %s", raw, rec.Code, errorCode(rec))
		}
	}
}

func TestGetJob(t *testing.T) {
	ts := newTestServer(t)
	ts.do(t, http.MethodPost, "/api/v1/jobs", map[string]any{
		"job": "report", "schedule": "0 12 * * MON", "maxRetries": 2,
	})
	rec := ts.do(t, http.MethodGet, "/api/v1/jobs/report", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if job := decodeJob(t, rec); job["job"] != "report" {
		t.Fatalf("unexpected job: %v", job)
	}

	missing := ts.do(t, http.MethodGet, "/api/v1/jobs/nope", nil)
	if missing.Code != http.StatusNotFound || errorCode(missing) != "job_not_found" {
		t.Fatalf("missing: status = %d code = %s", missing.Code, errorCode(missing))
	}
}

func TestRecordAttemptSuccessResetsAndAdvances(t *testing.T) {
	ts := newTestServer(t)
	ts.do(t, http.MethodPost, "/api/v1/jobs", map[string]any{
		"job": "a", "schedule": "0 * * * *", "maxRetries": 2,
	})
	ts.do(t, http.MethodPost, "/api/v1/jobs/a/attempts", map[string]any{
		"sequence": 1, "outcome": "failure", "attemptedAt": "2026-03-01T10:30:00Z",
	})
	rec := ts.do(t, http.MethodPost, "/api/v1/jobs/a/attempts", map[string]any{
		"sequence": 2, "outcome": "success", "attemptedAt": "2026-03-01T10:45:30Z",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	job := decodeJob(t, rec)
	if job["retries"] != 0 {
		t.Fatalf("retries = %v, want 0", job["retries"])
	}
	if job["paused"] != false {
		t.Fatalf("paused = %v, want false", job["paused"])
	}
	if job["nextRunAt"] != "2026-03-01T11:00:00Z" {
		t.Fatalf("nextRunAt = %v, want next cron after attempt", job["nextRunAt"])
	}
	attempts := job["attempts"].([]map[string]any)
	if len(attempts) != 2 || attempts[1]["outcome"] != "success" {
		t.Fatalf("attempts = %v", job["attempts"])
	}
}

func TestRecordAttemptFailureBackoff(t *testing.T) {
	ts := newTestServer(t)
	ts.do(t, http.MethodPost, "/api/v1/jobs", map[string]any{
		"job": "b", "schedule": "0 0 * * *", "maxRetries": 3,
	})
	base := "2026-03-01T10:00:00Z"
	cases := []struct {
		seq     int
		next    string
		retries int
	}{
		{1, "2026-03-01T10:02:00Z", 1},
		{2, "2026-03-01T10:04:00Z", 2},
		{3, "2026-03-01T10:08:00Z", 3},
	}
	var last map[string]any
	for i, tc := range cases {
		rec := ts.do(t, http.MethodPost, "/api/v1/jobs/b/attempts", map[string]any{
			"sequence": tc.seq, "outcome": "failure", "attemptedAt": base,
		})
		if rec.Code != http.StatusOK {
			t.Fatalf("attempt %d status = %d body = %s", tc.seq, rec.Code, rec.Body.String())
		}
		last = decodeJob(t, rec)
		if last["nextRunAt"] != tc.next || last["retries"] != tc.retries {
			t.Fatalf("case %d: %v", i, last)
		}
		if last["paused"] != false {
			t.Fatalf("case %d paused early", i)
		}
	}
	// Next failure exhausts retries: pause and keep the previous next run.
	rec := ts.do(t, http.MethodPost, "/api/v1/jobs/b/attempts", map[string]any{
		"sequence": 4, "outcome": "failure", "attemptedAt": base,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("exhaust status = %d body = %s", rec.Code, rec.Body.String())
	}
	exhausted := decodeJob(t, rec)
	if exhausted["paused"] != true || exhausted["retries"] != 3 {
		t.Fatalf("exhausted state = %v", exhausted)
	}
	if exhausted["nextRunAt"] != last["nextRunAt"] {
		t.Fatalf("nextRunAt advanced on exhaustion: %v vs %v", exhausted["nextRunAt"], last["nextRunAt"])
	}
	if got := len(exhausted["attempts"].([]map[string]any)); got != 4 {
		t.Fatalf("attempts len = %d, want 4", got)
	}
}

func TestRecordAttemptMaxRetriesZeroPausesImmediately(t *testing.T) {
	ts := newTestServer(t)
	create := ts.do(t, http.MethodPost, "/api/v1/jobs", map[string]any{
		"job": "c", "schedule": "0 * * * *", "maxRetries": 0,
	})
	originalNext := decodeJob(t, create)["nextRunAt"]
	rec := ts.do(t, http.MethodPost, "/api/v1/jobs/c/attempts", map[string]any{
		"sequence": 1, "outcome": "failure", "attemptedAt": "2026-03-01T10:30:00Z",
	})
	job := decodeJob(t, rec)
	if job["paused"] != true || job["retries"] != 0 || job["nextRunAt"] != originalNext {
		t.Fatalf("state = %v", job)
	}
}

func TestRecordAttemptErrors(t *testing.T) {
	ts := newTestServer(t)
	ts.do(t, http.MethodPost, "/api/v1/jobs", map[string]any{
		"job": "d", "schedule": "0 * * * *", "maxRetries": 2,
	})
	missing := ts.do(t, http.MethodPost, "/api/v1/jobs/nope/attempts", map[string]any{
		"sequence": 1, "outcome": "success", "attemptedAt": "2026-03-01T10:30:00Z",
	})
	if missing.Code != http.StatusNotFound || errorCode(missing) != "job_not_found" {
		t.Fatalf("missing job: %d %s", missing.Code, errorCode(missing))
	}

	bad := []map[string]any{
		{"outcome": "success", "attemptedAt": "2026-03-01T10:30:00Z"}, // no sequence
		{"sequence": 0, "outcome": "success", "attemptedAt": "2026-03-01T10:30:00Z"},
		{"sequence": 1, "outcome": "ok", "attemptedAt": "2026-03-01T10:30:00Z"},
		{"sequence": 1, "outcome": "success", "attemptedAt": "2026-03-01 10:30:00"},
		{"sequence": 1, "outcome": "success", "attemptedAt": "2026-03-01T10:30:00+08:00"},
		{"sequence": 1, "outcome": "success"},
	}
	for i, payload := range bad {
		rec := ts.do(t, http.MethodPost, "/api/v1/jobs/d/attempts", payload)
		if rec.Code != http.StatusBadRequest || errorCode(rec) != "invalid_request" {
			t.Fatalf("bad case %d: %d %s", i, rec.Code, errorCode(rec))
		}
	}

	first := ts.do(t, http.MethodPost, "/api/v1/jobs/d/attempts", map[string]any{
		"sequence": 7, "outcome": "success", "attemptedAt": "2026-03-01T10:30:00Z",
	})
	if first.Code != http.StatusOK {
		t.Fatalf("first: %d %s", first.Code, first.Body.String())
	}
	dup := ts.do(t, http.MethodPost, "/api/v1/jobs/d/attempts", map[string]any{
		"sequence": 7, "outcome": "success", "attemptedAt": "2026-03-01T10:31:00Z",
	})
	if dup.Code != http.StatusConflict || errorCode(dup) != "sequence_conflict" {
		t.Fatalf("dup: %d %s", dup.Code, errorCode(dup))
	}

	ts.do(t, http.MethodPost, "/api/v1/jobs/d/pause", map[string]any{"paused": true})
	paused := ts.do(t, http.MethodPost, "/api/v1/jobs/d/attempts", map[string]any{
		"sequence": 8, "outcome": "success", "attemptedAt": "2026-03-01T10:32:00Z",
	})
	if paused.Code != http.StatusConflict || errorCode(paused) != "job_paused" {
		t.Fatalf("paused: %d %s", paused.Code, errorCode(paused))
	}
}

func TestPauseEndpoint(t *testing.T) {
	ts := newTestServer(t)
	ts.do(t, http.MethodPost, "/api/v1/jobs", map[string]any{
		"job": "p", "schedule": "0 * * * *", "maxRetries": 2,
	})
	rec := ts.do(t, http.MethodPost, "/api/v1/jobs/p/pause", map[string]any{"paused": true})
	if rec.Code != http.StatusOK {
		t.Fatalf("pause status = %d", rec.Code)
	}
	if paused := decodeJob(t, rec)["paused"]; paused != true {
		t.Fatalf("paused = %v", paused)
	}
	rec = ts.do(t, http.MethodPost, "/api/v1/jobs/p/pause", map[string]any{"paused": false})
	if paused := decodeJob(t, rec)["paused"]; paused != false {
		t.Fatalf("paused = %v, want false", paused)
	}

	for _, raw := range []string{`{}`, `{"paused":"yes"}`, `{"paused":true,"x":1}`} {
		bad := ts.doRaw(t, http.MethodPost, "/api/v1/jobs/p/pause", raw)
		if bad.Code != http.StatusBadRequest || errorCode(bad) != "invalid_request" {
			t.Fatalf("raw %q: %d %s", raw, bad.Code, errorCode(bad))
		}
	}
	missing := ts.do(t, http.MethodPost, "/api/v1/jobs/nope/pause", map[string]any{"paused": true})
	if missing.Code != http.StatusNotFound || errorCode(missing) != "job_not_found" {
		t.Fatalf("missing: %d %s", missing.Code, errorCode(missing))
	}
}

func TestConcurrentDuplicateSequence(t *testing.T) {
	ts := newTestServer(t)
	ts.do(t, http.MethodPost, "/api/v1/jobs", map[string]any{
		"job": "race", "schedule": "0 * * * *", "maxRetries": 5,
	})
	var wg sync.WaitGroup
	statuses := make(chan int, 10)
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := ts.do(t, http.MethodPost, "/api/v1/jobs/race/attempts", map[string]any{
				"sequence": 1, "outcome": "failure", "attemptedAt": "2026-03-01T10:00:00Z",
			})
			statuses <- rec.Code
		}()
	}
	wg.Wait()
	close(statuses)
	ok, conflict := 0, 0
	for status := range statuses {
		switch status {
		case http.StatusOK:
			ok++
		case http.StatusConflict:
			conflict++
		default:
			t.Fatalf("unexpected status %d", status)
		}
	}
	if ok != 1 || conflict != 9 {
		t.Fatalf("ok = %d, conflict = %d, want 1 and 9", ok, conflict)
	}
	state := decodeJob(t, ts.do(t, http.MethodGet, "/api/v1/jobs/race", nil))
	if got := len(state["attempts"].([]map[string]any)); got != 1 {
		t.Fatalf("stored attempts = %d, want 1", got)
	}
	if state["retries"] != 1 {
		t.Fatalf("retries = %v, want 1", state["retries"])
	}
}

func TestTimeOutputIsRFC3339(t *testing.T) {
	ts := newTestServer(t)
	create := ts.do(t, http.MethodPost, "/api/v1/jobs", map[string]any{
		"job": "z", "schedule": "0 * * * *", "maxRetries": 1,
	})
	var raw map[string]any
	if err := json.Unmarshal(create.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	if _, err := time.Parse(time.RFC3339, fmt.Sprint(raw["nextRunAt"])); err != nil {
		t.Fatalf("nextRunAt not RFC3339: %v", err)
	}
}
