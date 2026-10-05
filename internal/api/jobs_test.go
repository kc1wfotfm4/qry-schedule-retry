package api

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	"github.com/kc1wfotfm4/qry-schedule-retry/internal/store"
)

func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "service.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func doRequest(router http.Handler, method, path, body string) *httptest.ResponseRecorder {
	var reader io.Reader
	if body != "" {
		reader = bytes.NewBufferString(body)
	}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(method, path, reader)
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(recorder, request)
	return recorder
}

func errorBody(t *testing.T, recorder *httptest.ResponseRecorder) map[string]string {
	t.Helper()
	var payload struct {
		Error map[string]string `json:"error"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode error body %q: %v", recorder.Body.String(), err)
	}
	return payload.Error
}

func TestCreateJobSuccessMatchesSpec(t *testing.T) {
	router := NewRouter(newTestStore(t))

	body := `{"id":"nightly","schedule":"  30  2\t*  * *  ","max_retries":3,"base_time":"2026-10-05T09:30:00+08:00"}`
	recorder := doRequest(router, http.MethodPost, "/api/v1/jobs", body)

	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusCreated, recorder.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := map[string]any{
		"id":              "nightly",
		"schedule":        "30 2 * * *",
		"max_retries":     float64(3),
		"next_trigger_at": "2026-10-05T02:30:00Z",
		"paused":          false,
	}
	if len(got) != len(want) {
		t.Fatalf("response has %d keys, want exactly %d: %v", len(got), len(want), got)
	}
	for key, value := range want {
		if got[key] != value {
			t.Fatalf("response[%q] = %v, want %v (full: %v)", key, got[key], value, got)
		}
	}
}

func TestGetJobReturnsSameDataAfterRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "service.db")

	st, err := store.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	recorder := doRequest(NewRouter(st), http.MethodPost, "/api/v1/jobs",
		`{"id":"nightly","schedule":"30 2 * * *","max_retries":0,"base_time":"2026-10-05T09:30:00+08:00"}`)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("create status = %d: %s", recorder.Code, recorder.Body.String())
	}
	created := recorder.Body.String()
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	reopened, err := store.Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	recorder = doRequest(NewRouter(reopened), http.MethodGet, "/api/v1/jobs/nightly", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("get status = %d: %s", recorder.Code, recorder.Body.String())
	}
	if recorder.Body.String() != created {
		t.Fatalf("get body = %s, want %s", recorder.Body.String(), created)
	}
}

func TestCreateJobDefaultsBaseTimeAndReturnsUTC(t *testing.T) {
	router := NewRouter(newTestStore(t))
	recorder := doRequest(router, http.MethodPost, "/api/v1/jobs", `{"id":"wild","schedule":"* * * * *","max_retries":100}`)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d: %s", recorder.Code, recorder.Body.String())
	}
	var got struct {
		NextTriggerAt string `json:"next_trigger_at"`
		Paused        bool   `json:"paused"`
		MaxRetries    int    `json:"max_retries"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.MaxRetries != 100 || got.Paused {
		t.Fatalf("unexpected job: %+v", got)
	}
	parsed, err := time.Parse(time.RFC3339, got.NextTriggerAt)
	if err != nil {
		t.Fatalf("next_trigger_at %q is not RFC3339 UTC: %v", got.NextTriggerAt, err)
	}
	if parsed.Second() != 0 {
		t.Fatalf("next_trigger_at seconds = %d, want 0", parsed.Second())
	}
}

func TestDuplicateIDConflictsAndLeavesRecordUnchanged(t *testing.T) {
	router := NewRouter(newTestStore(t))
	first := doRequest(router, http.MethodPost, "/api/v1/jobs",
		`{"id":"dup","schedule":"30 2 * * *","max_retries":1,"base_time":"2026-10-05T09:30:00+08:00"}`)
	if first.Code != http.StatusCreated {
		t.Fatalf("first create: %s", first.Body.String())
	}
	second := doRequest(router, http.MethodPost, "/api/v1/jobs",
		`{"id":"dup","schedule":"0 0 * * *","max_retries":50,"base_time":"2026-10-07T00:00:00Z","extra":1}`)
	if second.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d", second.Code, http.StatusConflict)
	}
	if body := errorBody(t, second); body["code"] != "job_exists" {
		t.Fatalf("code = %q, want job_exists", body["code"])
	}
	fetched := doRequest(router, http.MethodGet, "/api/v1/jobs/dup", "")
	if fetched.Body.String() != first.Body.String() {
		t.Fatalf("record changed: %s, want %s", fetched.Body.String(), first.Body.String())
	}
}

func TestGetUnknownJob(t *testing.T) {
	router := NewRouter(newTestStore(t))
	recorder := doRequest(router, http.MethodGet, "/api/v1/jobs/ghost", "")
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusNotFound)
	}
	if body := errorBody(t, recorder); body["code"] != "job_not_found" {
		t.Fatalf("code = %q, want job_not_found", body["code"])
	}
}

func TestInvalidSchedule(t *testing.T) {
	router := NewRouter(newTestStore(t))
	cases := []string{
		`{"id":"a","schedule":"60 * * * *","max_retries":0}`,
		`{"id":"a","schedule":"* 24 * * *","max_retries":0}`,
		`{"id":"a","schedule":"0 0 1 * *","max_retries":0}`,
		`{"id":"a","schedule":"*/5 * * * *","max_retries":0}`,
		`{"id":"a","schedule":"0 0 * *","max_retries":0}`,
	}
	for _, body := range cases {
		recorder := doRequest(router, http.MethodPost, "/api/v1/jobs", body)
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("body %s: status = %d, want 400", body, recorder.Code)
		}
		if got := errorBody(t, recorder)["code"]; got != "invalid_schedule" {
			t.Fatalf("body %s: code = %q, want invalid_schedule", body, got)
		}
	}
}

func TestInvalidRequests(t *testing.T) {
	router := NewRouter(newTestStore(t))
	cases := []struct {
		name string
		body string
	}{
		{"malformed json", `{"id":`},
		{"non object array", `[1,2,3]`},
		{"non object number", `123`},
		{"null", `null`},
		{"missing id", `{"schedule":"* * * * *","max_retries":0}`},
		{"missing schedule", `{"id":"a","max_retries":0}`},
		{"missing max retries", `{"id":"a","schedule":"* * * * *"}`},
		{"wrong id type", `{"id":5,"schedule":"* * * * *","max_retries":0}`},
		{"wrong schedule type", `{"id":"a","schedule":5,"max_retries":0}`},
		{"float retries", `{"id":"a","schedule":"* * * * *","max_retries":1.5}`},
		{"string retries", `{"id":"a","schedule":"* * * * *","max_retries":"1"}`},
		{"negative retries", `{"id":"a","schedule":"* * * * *","max_retries":-1}`},
		{"retries too large", `{"id":"a","schedule":"* * * * *","max_retries":101}`},
		{"id empty", `{"id":"","schedule":"* * * * *","max_retries":0}`},
		{"id bad character", `{"id":"a b","schedule":"* * * * *","max_retries":0}`},
		{"id too long", `{"id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","schedule":"* * * * *","max_retries":0}`},
		{"base time naive", `{"id":"a","schedule":"* * * * *","max_retries":0,"base_time":"2026-10-05T09:30:00"}`},
		{"base time not a time", `{"id":"a","schedule":"* * * * *","max_retries":0,"base_time":"soon"}`},
		{"trailing value", `{"id":"a","schedule":"* * * * *","max_retries":0} null`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := doRequest(router, http.MethodPost, "/api/v1/jobs", tc.body)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", recorder.Code, recorder.Body.String())
			}
			if got := errorBody(t, recorder)["code"]; got != "invalid_request" {
				t.Fatalf("code = %q, want invalid_request", got)
			}
		})
	}
}

func TestInvalidPathID(t *testing.T) {
	router := NewRouter(newTestStore(t))
	for _, id := range []string{"a b", "a.b", "!", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"} {
		path := "/api/v1/jobs/" + url.PathEscape(id)
		recorder := doRequest(router, http.MethodGet, path, "")
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("id %q: status = %d, want 400", id, recorder.Code)
		}
		if got := errorBody(t, recorder)["code"]; got != "invalid_request" {
			t.Fatalf("id %q: code = %q, want invalid_request", id, got)
		}
	}
}

func TestExtraFieldsIgnoredAndPausedFalse(t *testing.T) {
	router := NewRouter(newTestStore(t))
	recorder := doRequest(router, http.MethodPost, "/api/v1/jobs",
		`{"id":"x","schedule":"* * * * *","max_retries":0,"paused":true,"unknown":"value"}`)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d: %s", recorder.Code, recorder.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["paused"] != false {
		t.Fatalf("paused = %v, want false", got["paused"])
	}
	if _, ok := got["unknown"]; ok {
		t.Fatalf("unknown field echoed: %v", got)
	}
}

func TestIDsAreCaseSensitive(t *testing.T) {
	router := NewRouter(newTestStore(t))
	for _, id := range []string{"Job", "job"} {
		body := `{"id":"` + id + `","schedule":"* * * * *","max_retries":0}`
		if recorder := doRequest(router, http.MethodPost, "/api/v1/jobs", body); recorder.Code != http.StatusCreated {
			t.Fatalf("create %q status = %d: %s", id, recorder.Code, recorder.Body.String())
		}
	}
}

func TestStorageUnavailable(t *testing.T) {
	st := newTestStore(t)
	router := NewRouter(st)
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	post := doRequest(router, http.MethodPost, "/api/v1/jobs",
		`{"id":"a","schedule":"* * * * *","max_retries":0}`)
	if post.Code != http.StatusServiceUnavailable {
		t.Fatalf("post status = %d, want 503", post.Code)
	}
	if got := errorBody(t, post)["code"]; got != "storage_unavailable" {
		t.Fatalf("post code = %q, want storage_unavailable", got)
	}

	get := doRequest(router, http.MethodGet, "/api/v1/jobs/a", "")
	if get.Code != http.StatusServiceUnavailable {
		t.Fatalf("get status = %d, want 503", get.Code)
	}
	if got := errorBody(t, get)["code"]; got != "storage_unavailable" {
		t.Fatalf("get code = %q, want storage_unavailable", got)
	}
}
