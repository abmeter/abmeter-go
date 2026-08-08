package abmeter

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func resetSDK(t *testing.T) {
	t.Helper()
	t.Cleanup(ResetNow)
}

func quietLogger() *log.Logger {
	return log.New(io.Discard, "", 0)
}

func TestConfigureRequiresKeyOrStaticConfig(t *testing.T) {
	resetSDK(t)
	if err := Configure(Config{}); err == nil {
		t.Error("expected validation error without APIKey or StaticConfig")
	}
}

func TestUnconfiguredCallsReturnError(t *testing.T) {
	resetSDK(t)
	ResetNow()
	if _, err := ResolveParameter(User{UserID: "u"}, "p"); err == nil {
		t.Error("ResolveParameter must error when unconfigured")
	}
	if _, err := GetExposure(User{UserID: "u"}, "p"); err == nil {
		t.Error("GetExposure must error when unconfigured")
	}
	if err := TrackEvent("e", "u", nil); err == nil {
		t.Error("TrackEvent must error when unconfigured")
	}
	if err := Reset(context.Background()); err != nil {
		t.Errorf("Reset when unconfigured must be a no-op, got %v", err)
	}
}

func TestStaticConfigMode(t *testing.T) {
	resetSDK(t)
	err := Configure(Config{StaticConfig: testConfigJSON, Logger: quietLogger()})
	if err != nil {
		t.Fatal(err)
	}

	value, err := ResolveParameter(User{UserID: "u1"}, "discount")
	if err != nil {
		t.Fatal(err)
	}
	if value != 0.1 {
		t.Errorf("value = %v, want 0.1", value)
	}

	// Offline: tracking is accepted and discarded.
	if err := TrackEvent("purchase", "u1", map[string]any{"price": 1}); err != nil {
		t.Errorf("TrackEvent in static mode: %v", err)
	}
	if err := Reset(context.Background()); err != nil {
		t.Errorf("Reset in static mode: %v", err)
	}
}

func TestStaticConfigParseErrorSurfacesAtConfigure(t *testing.T) {
	resetSDK(t)
	if err := Configure(Config{StaticConfig: "{not json", Logger: quietLogger()}); err == nil {
		t.Error("expected parse error at Configure time")
	}
}

// End-to-end against a fake API: configure -> resolve (config fetched with
// ETag) -> exposure queued -> event tracked -> Reset drains both.
func TestEndToEndResolveTrackReset(t *testing.T) {
	resetSDK(t)

	var mu sync.Mutex
	var configFetches, notModified int
	var exposures, events []map[string]any

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case assignmentConfigPath:
			mu.Lock()
			configFetches++
			mu.Unlock()
			if r.Header.Get("If-None-Match") == `W/"v1"` {
				mu.Lock()
				notModified++
				mu.Unlock()
				w.WriteHeader(http.StatusNotModified)
				return
			}
			w.Header().Set("ETag", `W/"v1"`)
			json.NewEncoder(w).Encode(map[string]string{"config": testConfigJSON})
		case exposuresPath:
			var batch []map[string]any
			json.NewDecoder(r.Body).Decode(&batch)
			mu.Lock()
			exposures = append(exposures, batch...)
			mu.Unlock()
			w.Write([]byte(`{}`))
		case eventsPath:
			var batch []map[string]any
			json.NewDecoder(r.Body).Decode(&batch)
			mu.Lock()
			events = append(events, batch...)
			mu.Unlock()
			w.Write([]byte(`{}`))
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
		}
	}))
	defer server.Close()

	err := Configure(Config{
		APIKey:        "test-key",
		BaseURL:       server.URL,
		FlushInterval: time.Hour, // drain only via Reset
		FetchInterval: time.Nanosecond,
		Logger:        quietLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}

	value, err := ResolveParameter(User{UserID: "e2e-user"}, "button-color")
	if err != nil {
		t.Fatal(err)
	}
	if value != "blue" && value != "green" {
		t.Errorf("value = %v", value)
	}

	// FetchInterval elapsed: the next resolve re-checks with If-None-Match
	// and gets a 304 — same resolver kept.
	again, err := ResolveParameter(User{UserID: "e2e-user"}, "button-color")
	if err != nil {
		t.Fatal(err)
	}
	if again != value {
		t.Errorf("resolution changed across 304 refresh: %v then %v", value, again)
	}

	if err := TrackEvent("purchase", "e2e-user", map[string]any{"price": 4.99}); err != nil {
		t.Fatal(err)
	}

	if err := Reset(context.Background()); err != nil {
		t.Fatalf("Reset: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if configFetches < 2 || notModified < 1 {
		t.Errorf("config fetches = %d (304s: %d), want a full fetch then a 304", configFetches, notModified)
	}
	// 100% allocation: both resolves expose, second is a new record too.
	if len(exposures) != 2 {
		t.Errorf("exposures delivered = %d, want 2", len(exposures))
	}
	if len(events) != 1 {
		t.Errorf("events delivered = %d, want 1", len(events))
	}
	if len(events) == 1 {
		if events[0]["event_slug"] != "purchase" || events[0]["user_id"] != "e2e-user" {
			t.Errorf("event wire = %v", events[0])
		}
		occurredAt, _ := events[0]["occurred_at"].(string)
		if !strings.HasSuffix(occurredAt, "Z") {
			t.Errorf("occurred_at %q not ISO8601 UTC", occurredAt)
		}
	}
	if len(exposures) > 0 {
		if exposures[0]["exposable_type"] != "Experiment" {
			t.Errorf("exposure wire = %v", exposures[0])
		}
	}
}

func TestDefaultResolutionIsNotSubmitted(t *testing.T) {
	resetSDK(t)

	var mu sync.Mutex
	exposurePosts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case assignmentConfigPath:
			json.NewEncoder(w).Encode(map[string]string{"config": testConfigJSON})
		case exposuresPath:
			mu.Lock()
			exposurePosts++
			mu.Unlock()
			w.Write([]byte(`{}`))
		default:
			w.Write([]byte(`{}`))
		}
	}))
	defer server.Close()

	err := Configure(Config{
		APIKey: "k", BaseURL: server.URL,
		FlushInterval: time.Hour, Logger: quietLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	// `discount` is controlled by nothing: default path, no exposure queued.
	if _, err := ResolveParameter(User{UserID: "u"}, "discount"); err != nil {
		t.Fatal(err)
	}
	if err := Reset(context.Background()); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	if exposurePosts != 0 {
		t.Errorf("default-path resolution must not submit exposures, got %d posts", exposurePosts)
	}
}

func TestStaleConfigServedWhenRefreshFails(t *testing.T) {
	resetSDK(t)

	var mu sync.Mutex
	failing := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != assignmentConfigPath {
			w.Write([]byte(`{}`))
			return
		}
		mu.Lock()
		fail := failing
		mu.Unlock()
		if fail {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(`{"error":"boom"}`))
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"config": testConfigJSON})
	}))
	defer server.Close()

	err := Configure(Config{
		APIKey: "k", BaseURL: server.URL,
		FetchInterval: time.Nanosecond, FlushInterval: time.Hour,
		Logger: quietLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveParameter(User{UserID: "u"}, "discount"); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	failing = true
	mu.Unlock()

	// Refresh now fails, but the previously-fetched config keeps serving.
	if _, err := ResolveParameter(User{UserID: "u"}, "discount"); err != nil {
		t.Errorf("resolve must serve stale config across a refresh failure, got %v", err)
	}
	Reset(context.Background())
}

func TestGetExposureDoesNotSubmit(t *testing.T) {
	resetSDK(t)
	if err := Configure(Config{StaticConfig: testConfigJSON, Logger: quietLogger()}); err != nil {
		t.Fatal(err)
	}
	exposure, err := GetExposure(User{UserID: "u1"}, "button-color")
	if err != nil {
		t.Fatal(err)
	}
	if exposure.ExposableType == nil || *exposure.ExposableType != "Experiment" {
		t.Errorf("exposure = %+v", exposure)
	}
}
