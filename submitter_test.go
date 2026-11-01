package abmeter

import (
	"bytes"
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

// recordingServer captures every batch posted to /exposures and /events and
// can be scripted to fail.
type recordingServer struct {
	*httptest.Server
	mu        sync.Mutex
	exposures [][]map[string]any
	events    [][]map[string]any
	failWith  func() (status int, body string) // nil = succeed
	posts     int
	// retryAfter is sent as the Retry-After header on every 429.
	retryAfter string
}

func newRecordingServer(t *testing.T) *recordingServer {
	rs := &recordingServer{}
	rs.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rs.mu.Lock()
		fail := rs.failWith
		rs.posts++
		retryAfter := rs.retryAfter
		rs.mu.Unlock()
		if fail != nil {
			// A zero status from the failure func means "succeed this time".
			if status, body := fail(); status != 0 {
				if status == http.StatusTooManyRequests && retryAfter != "" {
					w.Header().Set("Retry-After", retryAfter)
				}
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

func (rs *recordingServer) postCount() int {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return rs.posts
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
	if kept, _ := s.submitBatch(kindExposure, exposuresAsItems([]Exposure{testExposure("u2")}), false); len(kept) != 0 {
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

// fakeClock drives the submitter's pause without real waiting. Tests that use
// it call flushOnce/drainAll directly from the test goroutine.
type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time          { return c.now }
func (c *fakeClock) Advance(d time.Duration) { c.now = c.now.Add(d) }

func newPausableSubmitter(t *testing.T, baseURL string) (*submitter, *fakeClock, *bytes.Buffer) {
	t.Helper()
	logs := &bytes.Buffer{}
	cfg := Config{
		APIKey: "k", BaseURL: baseURL,
		FlushInterval: time.Hour, Logger: log.New(logs, "", 0),
	}.withDefaults()
	s := newSubmitter(newClient(&cfg), &cfg)
	clock := &fakeClock{now: time.Unix(1_700_000_000, 0)}
	s.now = clock.Now
	s.sleep = func(d time.Duration) { t.Fatalf("unexpected sleep %v", d) }
	return s, clock, logs
}

// failFirst returns a failure func that answers status for the first n posts
// and succeeds afterwards.
func failFirst(n int, status int) func() (int, string) {
	var mu sync.Mutex
	var calls int
	return func() (int, string) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		if calls <= n {
			return status, `{"error":"failing"}`
		}
		return 0, ""
	}
}

func TestRateLimitPausesThenDeliversEverything(t *testing.T) {
	server := newRecordingServer(t)
	server.retryAfter = "7"
	// More consecutive 429s than maxSubmitAttempts: the wait never counts.
	server.setFailure(failFirst(maxSubmitAttempts+3, http.StatusTooManyRequests))
	s, clock, logs := newPausableSubmitter(t, server.URL)

	s.pending = append(s.pending,
		queueItem{kind: kindExposure, exposure: testExposure("u1")},
		queueItem{kind: kindExposure, exposure: testExposure("u2")},
		queueItem{kind: kindEvent, event: event{EventSlug: "purchase", UserID: "u1"}},
	)
	s.flushOnce()

	// The exposure batch got the 429; the event batch from the same flush
	// went to the retry queue without being sent.
	if got := server.postCount(); got != 1 {
		t.Fatalf("posts after first flush = %d, want 1", got)
	}
	if len(s.retryQueue) != 3 || len(s.pending) != 0 {
		t.Fatalf("retry=%d pending=%d, want 3 and 0", len(s.retryQueue), len(s.pending))
	}
	if !strings.Contains(logs.String(), "warning: rate limited, pausing submits for 7s (2 exposures to retry)") {
		t.Errorf("missing pause warning, logs:\n%s", logs.String())
	}

	// While paused nothing is sent, including newly queued items.
	s.pending = append(s.pending, queueItem{kind: kindExposure, exposure: testExposure("u3")})
	s.flushOnce()
	if got := server.postCount(); got != 1 {
		t.Fatalf("posts while paused = %d, want 1", got)
	}
	if len(s.pending) != 1 {
		t.Fatalf("pending while paused = %d, want 1", len(s.pending))
	}

	for i := 0; i < 20 && (len(s.pending) > 0 || len(s.retryQueue) > 0); i++ {
		clock.Advance(7 * time.Second)
		s.flushOnce()
	}

	if got := server.exposureCount(); got != 3 {
		t.Errorf("exposures delivered = %d, want 3 (no loss under 429)", got)
	}
	if got := server.eventCount(); got != 1 {
		t.Errorf("events delivered = %d, want 1 (no loss under 429)", got)
	}
	if strings.Contains(logs.String(), "max retries exceeded") {
		t.Errorf("429 counted as an attempt, logs:\n%s", logs.String())
	}
}

func TestRateLimitOnRetryKeepsAttempts(t *testing.T) {
	server := newRecordingServer(t)
	server.retryAfter = "1"
	server.setFailure(func() (int, string) { return http.StatusTooManyRequests, "" })
	s, _, _ := newPausableSubmitter(t, server.URL)

	s.retryQueue = []retryItem{{queueItem: queueItem{kind: kindExposure, exposure: testExposure("u1")}, attempts: 1}}
	s.processRetryQueue()

	if len(s.retryQueue) != 1 || s.retryQueue[0].attempts != 1 {
		t.Fatalf("retry queue = %+v, want one item with attempts 1", s.retryQueue)
	}
}

func TestRateLimitFullRetryQueueDropsAndLogsCountOnce(t *testing.T) {
	server := newRecordingServer(t)
	server.setFailure(func() (int, string) { return http.StatusTooManyRequests, "" })
	s, _, logs := newPausableSubmitter(t, server.URL)

	for len(s.retryQueue) < maxRetryQueueSize-2 {
		s.retryQueue = append(s.retryQueue, retryItem{queueItem: queueItem{kind: kindEvent}})
	}
	items := make([]queueItem, 5)
	for i := range items {
		items[i] = queueItem{kind: kindExposure, exposure: testExposure("u" + itoa(i))}
	}
	s.submitBatch(kindExposure, items, false)

	if len(s.retryQueue) != maxRetryQueueSize {
		t.Errorf("retry queue = %d, want %d", len(s.retryQueue), maxRetryQueueSize)
	}
	if got := strings.Count(logs.String(), "retry queue full"); got != 1 {
		t.Errorf("retry-queue-full lines = %d, want 1; logs:\n%s", got, logs.String())
	}
	if !strings.Contains(logs.String(), "retry queue full, dropping 3 exposures") {
		t.Errorf("missing drop count, logs:\n%s", logs.String())
	}
}

func TestDrainAllWaitsOutShortPause(t *testing.T) {
	server := newRecordingServer(t)
	server.retryAfter = "2"
	server.setFailure(failFirst(1, http.StatusTooManyRequests))
	s, clock, _ := newPausableSubmitter(t, server.URL)
	var slept time.Duration
	s.sleep = func(d time.Duration) { slept += d; clock.Advance(d) }

	s.pending = append(s.pending, queueItem{kind: kindExposure, exposure: testExposure("u1")})
	s.drainAll()

	if got := server.exposureCount(); got != 1 {
		t.Errorf("exposures delivered = %d, want 1", got)
	}
	if slept != 2*time.Second {
		t.Errorf("slept %v, want 2s", slept)
	}
}

func TestDrainAllStopsWithinBudgetUnderLongPause(t *testing.T) {
	server := newRecordingServer(t)
	server.retryAfter = "2"
	server.setFailure(func() (int, string) { return http.StatusTooManyRequests, "" })
	s, clock, logs := newPausableSubmitter(t, server.URL)
	var slept time.Duration
	s.sleep = func(d time.Duration) { slept += d; clock.Advance(d) }

	s.pending = append(s.pending,
		queueItem{kind: kindExposure, exposure: testExposure("u1")},
		queueItem{kind: kindEvent, event: event{EventSlug: "e", UserID: "u1"}},
	)
	s.drainAll()

	if slept > maxDrainPause {
		t.Errorf("drain slept %v, budget is %v", slept, maxDrainPause)
	}
	if len(s.pending) != 0 || len(s.retryQueue) != 0 {
		t.Errorf("drain left pending=%d retry=%d", len(s.pending), len(s.retryQueue))
	}
	if !strings.Contains(logs.String(), "dropping 2 items") {
		t.Errorf("missing drop count, logs:\n%s", logs.String())
	}
}

func TestDrainAllDropsAtOnceWhenPauseExceedsBudget(t *testing.T) {
	server := newRecordingServer(t)
	s, clock, logs := newPausableSubmitter(t, server.URL)
	s.pausedUntil = clock.Now().Add(maxRetryAfter)

	s.pending = append(s.pending, queueItem{kind: kindExposure, exposure: testExposure("u1")})
	s.drainAll() // sleep is wired to fail the test

	if server.postCount() != 0 || len(s.pending) != 0 {
		t.Errorf("posts=%d pending=%d, want 0 and 0", server.postCount(), len(s.pending))
	}
	if !strings.Contains(logs.String(), "dropping 1 items") {
		t.Errorf("missing drop count, logs:\n%s", logs.String())
	}
}
