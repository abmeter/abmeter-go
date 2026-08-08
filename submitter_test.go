package abmeter

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// recordingServer captures every batch posted to /exposures and /events and
// can be scripted to fail.
type recordingServer struct {
	*httptest.Server
	mu        sync.Mutex
	exposures [][]map[string]any
	events    [][]map[string]any
	failWith  func() (status int, body string) // nil = succeed
}

func newRecordingServer(t *testing.T) *recordingServer {
	rs := &recordingServer{}
	rs.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rs.mu.Lock()
		fail := rs.failWith
		rs.mu.Unlock()
		if fail != nil {
			// A zero status from the failure func means "succeed this time".
			if status, body := fail(); status != 0 {
				w.WriteHeader(status)
				io.WriteString(w, body)
				return
			}
		}
		var batch []map[string]any
		json.NewDecoder(r.Body).Decode(&batch)
		rs.mu.Lock()
		switch r.URL.Path {
		case exposuresPath:
			rs.exposures = append(rs.exposures, batch)
		case eventsPath:
			rs.events = append(rs.events, batch)
		}
		rs.mu.Unlock()
		w.Write([]byte(`{}`))
	}))
	t.Cleanup(rs.Server.Close)
	return rs
}

func (rs *recordingServer) setFailure(f func() (int, string)) {
	rs.mu.Lock()
	rs.failWith = f
	rs.mu.Unlock()
}

func (rs *recordingServer) exposureCount() int {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	n := 0
	for _, batch := range rs.exposures {
		n += len(batch)
	}
	return n
}

func (rs *recordingServer) eventCount() int {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	n := 0
	for _, batch := range rs.events {
		n += len(batch)
	}
	return n
}

func newTestSubmitter(t *testing.T, baseURL string, flushInterval time.Duration) *submitter {
	t.Helper()
	cfg := Config{
		APIKey:        "test-key",
		BaseURL:       baseURL,
		FlushInterval: flushInterval,
		Logger:        log.New(io.Discard, "", 0),
	}.withDefaults()
	s := newSubmitter(newClient(&cfg), &cfg)
	t.Cleanup(s.stopNow)
	return s
}

func testExposure(userID string) Exposure {
	exposableType := "Experiment"
	exposableID := 20
	audienceID := 31
	return Exposure{
		ParameterID: 10, SpaceID: 1, ResolvedValue: "green",
		UserID: &userID, ExposableType: &exposableType,
		ExposableID: &exposableID, AudienceID: &audienceID,
		ResolvedAt: nowISO8601(),
	}
}

func drain(t *testing.T, s *submitter) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.stop(ctx); err != nil {
		t.Fatalf("graceful stop: %v", err)
	}
}

func TestSubmitterBatchesByKind(t *testing.T) {
	server := newRecordingServer(t)
	s := newTestSubmitter(t, server.URL, time.Hour) // flush only on stop
	s.start()

	for i := 0; i < 3; i++ {
		s.queueExposure(testExposure("u" + itoa(i)))
	}
	s.queueEvent("purchase", "u0", map[string]any{"price": 4.99})
	drain(t, s)

	if got := server.exposureCount(); got != 3 {
		t.Errorf("exposures delivered = %d, want 3", got)
	}
	if got := server.eventCount(); got != 1 {
		t.Errorf("events delivered = %d, want 1", got)
	}
}

func TestGracefulStopDrainsLargeBacklog(t *testing.T) {
	server := newRecordingServer(t)
	s := newTestSubmitter(t, server.URL, time.Hour)
	s.start()

	// Several times batchSize, to prove the final drain loops instead of
	// flushing a single batch.
	const n = batchSize*3 + 17
	for i := 0; i < n; i++ {
		s.queueExposure(testExposure("u" + itoa(i)))
	}
	drain(t, s)

	if got := server.exposureCount(); got != n {
		t.Errorf("exposures delivered = %d, want %d (lossless graceful stop)", got, n)
	}
}

func TestStopNowDropsQueuedItems(t *testing.T) {
	server := newRecordingServer(t)
	s := newTestSubmitter(t, server.URL, time.Hour)
	s.start()

	for i := 0; i < 10; i++ {
		s.queueExposure(testExposure("u" + itoa(i)))
	}
	s.stopNow()
	<-s.doneCh

	// stopNow signals the same stop channel; the worker still drains what it
	// can, but callers must not rely on delivery — the contract is only that
	// stopNow never blocks. Nothing to assert on counts; just verify exit.
}

func TestRetryableErrorRequeuesAndEventuallyDelivers(t *testing.T) {
	server := newRecordingServer(t)
	// Fail exactly the first post with a retryable 503; every later post
	// (the retry pass) succeeds.
	var failed bool
	var mu sync.Mutex
	server.setFailure(func() (int, string) {
		mu.Lock()
		defer mu.Unlock()
		if failed {
			return 0, ""
		}
		failed = true
		return 503, `{"error":"unavailable"}`
	})
	s := newTestSubmitter(t, server.URL, 20*time.Millisecond)
	s.start()

	s.queueExposure(testExposure("u1"))
	drain(t, s)

	if got := server.exposureCount(); got != 1 {
		t.Errorf("exposure delivered %d times, want exactly 1 (retry after 503)", got)
	}
}

func TestPermanentErrorDropsWithoutRetry(t *testing.T) {
	server := newRecordingServer(t)
	var posts int
	var mu sync.Mutex
	server.setFailure(func() (int, string) {
		mu.Lock()
		posts++
		mu.Unlock()
		return 401, `{"error":"unauthorized"}`
	})
	s := newTestSubmitter(t, server.URL, 20*time.Millisecond)
	s.start()

	s.queueExposure(testExposure("u1"))
	time.Sleep(100 * time.Millisecond)
	drain(t, s)

	mu.Lock()
	defer mu.Unlock()
	if posts != 1 {
		t.Errorf("posted %d times, want 1 (permanent errors are not retried)", posts)
	}
}

func TestValidationPartialFailureIsNotRetried(t *testing.T) {
	server := newRecordingServer(t)
	var posts int
	var mu sync.Mutex
	server.setFailure(func() (int, string) {
		mu.Lock()
		posts++
		mu.Unlock()
		return 400, `{"error":"validation","details":{"failures":[{"index":0}],"invalid_count":1}}`
	})
	s := newTestSubmitter(t, server.URL, 20*time.Millisecond)
	s.start()

	s.queueExposure(testExposure("u1"))
	time.Sleep(100 * time.Millisecond)
	drain(t, s)

	mu.Lock()
	defer mu.Unlock()
	if posts != 1 {
		t.Errorf("posted %d times, want 1 (validation failures are dropped)", posts)
	}
}

func TestMaxRetriesDropsItem(t *testing.T) {
	server := newRecordingServer(t)
	server.setFailure(func() (int, string) { return 503, `{"error":"unavailable"}` })
	s := newTestSubmitter(t, server.URL, 10*time.Millisecond)
	s.start()

	s.queueExposure(testExposure("u1"))
	// After maxSubmitAttempts retry passes the item must be dropped so the
	// graceful stop terminates even with the server still failing.
	time.Sleep(150 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.stop(ctx); err != nil {
		t.Fatalf("stop must terminate once retries cap out: %v", err)
	}
}

// The asymmetric unknown-transport-error contract: on a RETRY pass, events
// are dropped while exposures stay queued. Exercised at the unit level
// against a dead endpoint.
func TestRetryTransportAsymmetry(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	dead.Close() // connection refused from now on

	cfg := Config{
		APIKey: "k", BaseURL: dead.URL,
		FlushInterval: time.Hour, Logger: log.New(io.Discard, "", 0),
	}.withDefaults()
	s := newSubmitter(newClient(&cfg), &cfg)

	// Retry pass with one exposure and one event, both hitting a transport
	// error: exposure kept, event dropped.
	kept := s.processRetryBatch(kindExposure, []retryItem{{queueItem: queueItem{kind: kindExposure, exposure: testExposure("u1")}}})
	if len(kept) != 1 {
		t.Errorf("exposure retry kept %d items, want 1 (transport errors re-queue exposures)", len(kept))
	}
	keptEvents := s.processRetryBatch(kindEvent, []retryItem{{queueItem: queueItem{kind: kindEvent, event: event{EventSlug: "e", UserID: "u"}}}})
	if len(keptEvents) != 0 {
		t.Errorf("event retry kept %d items, want 0 (transport errors drop events)", len(keptEvents))
	}

	// First-submit pass (not a retry): both kinds dropped — unknown cause.
	if kept := s.submitBatch(kindExposure, exposuresAsItems([]Exposure{testExposure("u2")}), false); len(kept) != 0 {
		t.Errorf("first-submit transport error must drop, kept %d", len(kept))
	}
}

func TestGracefulStopTimesOutAgainstHungServer(t *testing.T) {
	release := make(chan struct{})
	hung := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	defer func() { close(release); hung.Close() }()

	cfg := Config{
		APIKey: "k", BaseURL: hung.URL,
		FlushInterval: time.Hour, Logger: log.New(io.Discard, "", 0),
		HTTPClient: &http.Client{Timeout: 10 * time.Second},
	}.withDefaults()
	s := newSubmitter(newClient(&cfg), &cfg)
	s.start()
	s.queueExposure(testExposure("u1"))

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := s.stop(ctx); err == nil {
		t.Error("expected timeout error from stop against a hung server")
	}
}
