package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kc1wfotfm4/qry-schedule-retry/internal/store"
)

func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "service.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func doRequest(router http.Handler, method, path, body string) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	request := httptest.NewRequest(method, path, reader)
	router.ServeHTTP(recorder, request)
	return recorder
}

func errorCode(t *testing.T, recorder *httptest.ResponseRecorder) string {
	t.Helper()
	var payload struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("error body is not JSON: %v (%s)", err, recorder.Body.String())
	}
	return payload.Error.Code
}

func TestRegisterJobReturnsCreated(t *testing.T) {
	router := NewRouter(openTestStore(t))
	recorder := doRequest(router, http.MethodPost, "/api/v1/jobs",
		`{"id":"nightly-1","schedule":" 30   2 * * * ","max_retries":3,"base_time":"2026-10-05T09:30:00+08:00","ignored":true}`)

	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d (%s)", recorder.Code, http.StatusCreated, recorder.Body.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	want := map[string]any{
		"id":              "nightly-1",
		"schedule":        "30 2 * * *",
		"max_retries":     float64(3),
		"next_trigger_at": "2026-10-05T02:30:00Z",
		"paused":          false,
	}
	for key, value := range want {
		if payload[key] != value {
			t.Fatalf("body[%q] = %v, want %v (body %s)", key, payload[key], value, recorder.Body.String())
		}
	}
	if len(payload) != len(want) {
		t.Fatalf("body has unexpected fields: %s", recorder.Body.String())
	}
}

func TestRegisterJobDefaultsBaseTime(t *testing.T) {
	router := NewRouter(openTestStore(t))
	recorder := doRequest(router, http.MethodPost, "/api/v1/jobs",
		`{"id":"job_a","schedule":"* * * * *","max_retries":0}`)

	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d (%s)", recorder.Code, http.StatusCreated, recorder.Body.String())
	}
	var payload struct {
		NextTriggerAt string `json:"next_trigger_at"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	if !strings.HasSuffix(payload.NextTriggerAt, ":00Z") {
		t.Fatalf("next_trigger_at = %q, want UTC with zero seconds", payload.NextTriggerAt)
	}
}

func TestRegisterDuplicateKeepsOriginal(t *testing.T) {
	router := NewRouter(openTestStore(t))
	body := `{"id":"job_a","schedule":"30 2 * * *","max_retries":1,"base_time":"2026-10-05T09:30:00+08:00"}`
	if recorder := doRequest(router, http.MethodPost, "/api/v1/jobs", body); recorder.Code != http.StatusCreated {
		t.Fatalf("first register status = %d", recorder.Code)
	}

	recorder := doRequest(router, http.MethodPost, "/api/v1/jobs",
		`{"id":"job_a","schedule":"0 0 * * *","max_retries":9}`)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusConflict)
	}
	if code := errorCode(t, recorder); code != "job_exists" {
		t.Fatalf("code = %q, want job_exists", code)
	}

	recorder = doRequest(router, http.MethodGet, "/api/v1/jobs/job_a", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("get status = %d, want %d", recorder.Code, http.StatusOK)
	}
	var payload map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	if payload["schedule"] != "30 2 * * *" || payload["max_retries"] != float64(1) {
		t.Fatalf("original record changed: %s", recorder.Body.String())
	}
}

func TestGetJobReturnsStoredValues(t *testing.T) {
	st := openTestStore(t)
	router := NewRouter(st)
	created := doRequest(router, http.MethodPost, "/api/v1/jobs",
		`{"id":"Job-A_1","schedule":"15 4 * * *","max_retries":7,"base_time":"2026-10-05T09:30:00+08:00"}`)
	if created.Code != http.StatusCreated {
		t.Fatalf("register status = %d", created.Code)
	}

	recorder := doRequest(router, http.MethodGet, "/api/v1/jobs/Job-A_1", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
	}
	if recorder.Body.String() != created.Body.String() {
		t.Fatalf("get body = %s, want stored %s", recorder.Body.String(), created.Body.String())
	}
}

func TestGetJobSurvivesReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "service.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	router := NewRouter(st)
	created := doRequest(router, http.MethodPost, "/api/v1/jobs",
		`{"id":"persisted","schedule":"45 6 * * *","max_retries":2,"base_time":"2026-10-05T09:30:00+08:00"}`)
	if created.Code != http.StatusCreated {
		t.Fatalf("register status = %d", created.Code)
	}
	st.Close()

	st, err = store.Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer st.Close()
	recorder := doRequest(NewRouter(st), http.MethodGet, "/api/v1/jobs/persisted", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
	}
	if recorder.Body.String() != created.Body.String() {
		t.Fatalf("body after reopen = %s, want %s", recorder.Body.String(), created.Body.String())
	}
}

func TestGetMissingJobReturnsNotFound(t *testing.T) {
	router := NewRouter(openTestStore(t))
	recorder := doRequest(router, http.MethodGet, "/api/v1/jobs/nope", "")
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusNotFound)
	}
	if code := errorCode(t, recorder); code != "job_not_found" {
		t.Fatalf("code = %q, want job_not_found", code)
	}
}

func TestRegisterRejectsBadRequests(t *testing.T) {
	router := NewRouter(openTestStore(t))
	cases := []struct {
		name string
		body string
		code string
	}{
		{"broken json", `{"id":`, "invalid_request"},
		{"non object", `[1,2,3]`, "invalid_request"},
		{"missing id", `{"schedule":"* * * * *","max_retries":1}`, "invalid_request"},
		{"missing schedule", `{"id":"a","max_retries":1}`, "invalid_request"},
		{"missing max_retries", `{"id":"a","schedule":"* * * * *"}`, "invalid_request"},
		{"id wrong type", `{"id":5,"schedule":"* * * * *","max_retries":1}`, "invalid_request"},
		{"id too long", `{"id":"` + strings.Repeat("a", 65) + `","schedule":"* * * * *","max_retries":1}`, "invalid_request"},
		{"id bad chars", `{"id":"a b","schedule":"* * * * *","max_retries":1}`, "invalid_request"},
		{"max_retries float", `{"id":"a","schedule":"* * * * *","max_retries":1.5}`, "invalid_request"},
		{"max_retries string", `{"id":"a","schedule":"* * * * *","max_retries":"1"}`, "invalid_request"},
		{"max_retries negative", `{"id":"a","schedule":"* * * * *","max_retries":-1}`, "invalid_request"},
		{"max_retries too big", `{"id":"a","schedule":"* * * * *","max_retries":101}`, "invalid_request"},
		{"base_time no zone", `{"id":"a","schedule":"* * * * *","max_retries":1,"base_time":"2026-10-05T09:30:00"}`, "invalid_request"},
		{"base_time garbage", `{"id":"a","schedule":"* * * * *","max_retries":1,"base_time":"tomorrow"}`, "invalid_request"},
		{"bad schedule", `{"id":"a","schedule":"*/5 * * * *","max_retries":1}`, "invalid_schedule"},
		{"schedule field count", `{"id":"a","schedule":"* * * *","max_retries":1}`, "invalid_schedule"},
		{"schedule dom value", `{"id":"a","schedule":"* * 1 * *","max_retries":1}`, "invalid_schedule"},
	}
	for _, tc := range cases {
		recorder := doRequest(router, http.MethodPost, "/api/v1/jobs", tc.body)
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("%s: status = %d, want %d (%s)", tc.name, recorder.Code, http.StatusBadRequest, recorder.Body.String())
		}
		if code := errorCode(t, recorder); code != tc.code {
			t.Fatalf("%s: code = %q, want %q", tc.name, code, tc.code)
		}
	}
}

func TestFailedRegisterLeavesNoRecord(t *testing.T) {
	router := NewRouter(openTestStore(t))
	recorder := doRequest(router, http.MethodPost, "/api/v1/jobs",
		`{"id":"ghost","schedule":"*/5 * * * *","max_retries":1}`)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusBadRequest)
	}
	recorder = doRequest(router, http.MethodGet, "/api/v1/jobs/ghost", "")
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusNotFound)
	}
}

func TestGetRejectsBadPathID(t *testing.T) {
	router := NewRouter(openTestStore(t))
	for _, id := range []string{"a%20b", "a.b", strings.Repeat("a", 65)} {
		recorder := doRequest(router, http.MethodGet, "/api/v1/jobs/"+id, "")
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("id %q: status = %d, want %d", id, recorder.Code, http.StatusBadRequest)
		}
		if code := errorCode(t, recorder); code != "invalid_request" {
			t.Fatalf("id %q: code = %q, want invalid_request", id, code)
		}
	}
}
