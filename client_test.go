package abmeter

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func newTestClient(baseURL string) *client {
	cfg := Config{APIKey: "test-key", BaseURL: baseURL}.withDefaults()
	return newClient(&cfg)
}

func TestGetAssignmentConfigSendsBearerAndParsesConfig(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("Authorization = %q", r.Header.Get("Authorization"))
		}
		if r.URL.Path != assignmentConfigPath {
			t.Errorf("path = %q", r.URL.Path)
		}
		w.Header().Set("ETag", `W/"abc"`)
		payload, _ := json.Marshal(map[string]string{"version": "v1", "config": testConfigJSON})
		w.Write(payload)
	}))
	defer server.Close()

	response, err := newTestClient(server.URL).getAssignmentConfig("")
	if err != nil {
		t.Fatal(err)
	}
	if response.notModified {
		t.Error("unexpected notModified")
	}
	if response.etag != `W/"abc"` {
		t.Errorf("etag = %q", response.etag)
	}
	if _, err := newResolver(response.configJSON); err != nil {
		t.Errorf("returned config not parseable: %v", err)
	}
}

func TestGetAssignmentConfigETagRoundTrip(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("If-None-Match") == `W/"abc"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `W/"abc"`)
		payload, _ := json.Marshal(map[string]string{"config": testConfigJSON})
		w.Write(payload)
	}))
	defer server.Close()

	c := newTestClient(server.URL)
	first, err := c.getAssignmentConfig("")
	if err != nil {
		t.Fatal(err)
	}
	second, err := c.getAssignmentConfig(first.etag)
	if err != nil {
		t.Fatal(err)
	}
	if !second.notModified {
		t.Error("expected 304 notModified on matching ETag")
	}
	if second.etag != `W/"abc"` {
		t.Errorf("304 must keep the etag, got %q", second.etag)
	}
	if calls.Load() != 2 {
		t.Errorf("calls = %d", calls.Load())
	}
}

func TestAPIErrorMapping(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]any{"error": "invalid key", "code": "unauthorized"})
	}))
	defer server.Close()

	_, err := newTestClient(server.URL).getAssignmentConfig("")
	apiErr, ok := err.(*APIError)
	if !ok {
		t.Fatalf("expected *APIError, got %T: %v", err, err)
	}
	if apiErr.Status != 401 || apiErr.Message != "invalid key" || apiErr.Code != "unauthorized" {
		t.Errorf("unexpected APIError: %+v", apiErr)
	}
	if apiErr.Retryable() {
		t.Error("401 must not be retryable")
	}
}

func TestAPIErrorClassification(t *testing.T) {
	cases := []struct {
		status    int
		retryable bool
	}{
		{500, true}, {503, true}, {408, true}, {429, true},
		{400, false}, {401, false}, {404, false}, {422, false},
	}
	for _, c := range cases {
		e := &APIError{Status: c.status}
		if e.Retryable() != c.retryable {
			t.Errorf("status %d retryable = %v, want %v", c.status, e.Retryable(), c.retryable)
		}
	}

	partial := newAPIError(400, []byte(`{"error":"validation","details":{"failures":[{"index":0}],"invalid_count":1}}`))
	if !partial.PartialFailure() {
		t.Error("expected partial failure")
	}
	if partial.FailureCount() != 1 {
		t.Errorf("failure count = %d", partial.FailureCount())
	}
	if newAPIError(400, []byte(`{"error":"bad"}`)).PartialFailure() {
		t.Error("400 without failures must not be a partial failure")
	}
	if plain := newAPIError(502, []byte("Bad Gateway")); plain.Message != "Bad Gateway" {
		t.Errorf("non-JSON body message = %q", plain.Message)
	}
}

func TestSubmitExposuresPostsExactWireFormat(t *testing.T) {
	var received []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != exposuresPath {
			t.Errorf("path = %q", r.URL.Path)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("content type = %q", ct)
		}
		json.NewDecoder(r.Body).Decode(&received)
		w.Write([]byte(`{}`))
	}))
	defer server.Close()

	userID := "u1"
	exposableType := "Experiment"
	exposableID := 20
	audienceID := 31
	err := newTestClient(server.URL).submitExposures([]Exposure{{
		ParameterID: 10, SpaceID: 1, ResolvedValue: "green",
		UserID: &userID, ExposableType: &exposableType,
		ExposableID: &exposableID, AudienceID: &audienceID,
		ResolvedAt: "2026-01-01T00:00:00Z",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(received) != 1 {
		t.Fatalf("received %d exposures", len(received))
	}
	for _, key := range []string{
		"parameter_id", "space_id", "resolved_value", "user_id",
		"exposable_type", "exposable_id", "audience_id", "resolved_at",
	} {
		if _, ok := received[0][key]; !ok {
			t.Errorf("wire exposure missing key %q (got %v)", key, received[0])
		}
	}
}

func TestTrackEventsPostsExactWireFormat(t *testing.T) {
	var received []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != eventsPath {
			t.Errorf("path = %q", r.URL.Path)
		}
		json.NewDecoder(r.Body).Decode(&received)
		w.Write([]byte(`{}`))
	}))
	defer server.Close()

	err := newTestClient(server.URL).trackEvents([]event{{
		EventSlug: "purchase", UserID: "u1",
		OccurredAt: "2026-01-01T00:00:00Z", CustomFields: map[string]any{"price": 4.99},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(received) != 1 {
		t.Fatalf("received %d events", len(received))
	}
	for _, key := range []string{"event_slug", "user_id", "occurred_at", "custom_fields"} {
		if _, ok := received[0][key]; !ok {
			t.Errorf("wire event missing key %q (got %v)", key, received[0])
		}
	}
}

func TestEmptyBatchesAreNotSent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("no request expected for empty batches")
	}))
	defer server.Close()

	c := newTestClient(server.URL)
	if err := c.submitExposures(nil); err != nil {
		t.Error(err)
	}
	if err := c.trackEvents(nil); err != nil {
		t.Error(err)
	}
}
