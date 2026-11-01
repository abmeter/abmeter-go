package abmeter

import (
	"context"
	"fmt"
	"log"
	"time"
)

// event is the exact wire format of POST /api/v1/events items.
type event struct {
	EventSlug    string         `json:"event_slug"`
	UserID       string         `json:"user_id"`
	OccurredAt   string         `json:"occurred_at"`
	CustomFields map[string]any `json:"custom_fields"`
}

type itemKind string

const (
	kindExposure itemKind = "exposure"
	kindEvent    itemKind = "event"
)

type queueItem struct {
	kind     itemKind
	exposure Exposure
	event    event
}

type retryItem struct {
	queueItem
	attempts int
}

// submitter drains queued exposures and events to the platform from a single
// worker goroutine, flushing every flushInterval.
//
// The retry contract is asymmetric by event kind and feeds the recovery
// harness's completeness gate — preserve it exactly:
//   - Rate limited (429): the submitter pauses for the response's
//     RetryAfter and submits nothing until it passes. The rate-limited batch,
//     and any batch already taken from the main queue in the same flush, go
//     to the bounded retry queue with attempts unchanged: the wait never
//     counts, so these items drop only when the retry queue is full.
//   - Other retryable APIError (5xx/408): both kinds go to the bounded retry
//     queue (max 1000 items, max 3 attempts each).
//   - Partial validation failure / permanent APIError: dropped — validation
//     won't change.
//   - Unknown transport error on FIRST submit: dropped (unknown cause, both
//     kinds). On RETRY: exposures are re-queued, events are dropped.
//
// The main queue is deliberately unbounded (mutex-guarded slice, matching
// Ruby's Queue): a bounded channel would silently drop under burst load,
// which the completeness gate would surface as data loss.
//
// The final drain waits out pauses up to maxDrainPause in total, then drops
// what is left: stop(ctx) abandons the worker, so the drain must end alone.
type submitter struct {
	queue         chan queueItem // enqueue side; effectively unbounded via pending
	flushInterval time.Duration
	client        *client
	logger        *log.Logger

	// Owned by the worker goroutine between start and doneCh close.
	pending     []queueItem
	retryQueue  []retryItem
	pausedUntil time.Time

	// Injectable for tests.
	now   func() time.Time
	sleep func(time.Duration)

	stopCh chan struct{}
	doneCh chan struct{}
}

// queueBuffer only smooths bursts between worker wakeups; the worker moves
// items into the unbounded pending slice on every loop turn, so the channel
// never fills in practice (a full channel briefly blocks the producer, it
// never drops).
const queueBuffer = 4096

func newSubmitter(apiClient *client, cfg *Config) *submitter {
	return &submitter{
		queue:         make(chan queueItem, queueBuffer),
		flushInterval: cfg.FlushInterval,
		client:        apiClient,
		logger:        cfg.Logger,
		now:           time.Now,
		sleep:         time.Sleep,
		stopCh:        make(chan struct{}),
		doneCh:        make(chan struct{}),
	}
}

func (s *submitter) start() {
	go s.run()
}

func (s *submitter) queueExposure(exposure Exposure) {
	s.enqueue(queueItem{kind: kindExposure, exposure: exposure})
}

func (s *submitter) queueEvent(eventSlug, userID string, customFields map[string]any) {
	s.enqueue(queueItem{kind: kindEvent, event: event{
		EventSlug:    eventSlug,
		UserID:       userID,
		OccurredAt:   nowISO8601(),
		CustomFields: customFields,
	}})
}

func (s *submitter) enqueue(item queueItem) {
	select {
	case <-s.doneCh:
		// Submitter already stopped; drop silently (matches Ruby post-reset).
	default:
		select {
		case s.queue <- item:
		case <-s.doneCh:
		}
	}
}

// stop signals a graceful shutdown and waits for the worker's final drain,
// bounded by ctx. Returns an error when ctx expired before the drain
// finished (anything left in the queues is abandoned with the worker).
func (s *submitter) stop(ctx context.Context) error {
	select {
	case <-s.stopCh:
	default:
		close(s.stopCh)
	}
	select {
	case <-s.doneCh:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("graceful shutdown timed out: %w", ctx.Err())
	}
}

// stopNow signals shutdown without waiting: queued items are discarded
// (no network I/O beyond what the worker already has in flight).
func (s *submitter) stopNow() {
	select {
	case <-s.stopCh:
	default:
		close(s.stopCh)
	}
}

func (s *submitter) run() {
	defer close(s.doneCh)
	ticker := time.NewTicker(s.flushInterval)
	defer ticker.Stop()

	for {
		s.collect()
		select {
		case item := <-s.queue:
			s.pending = append(s.pending, item)
		case <-ticker.C:
			s.flushOnce()
		case <-s.stopCh:
			s.collect()
			s.drainAll()
			dropped := len(s.pending) + len(s.retryQueue)
			if dropped > 0 {
				s.log(fmt.Sprintf("shutdown with %d items undelivered", dropped))
			}
			return
		}
	}
}

// collect moves everything currently buffered in the channel into pending
// without blocking.
func (s *submitter) collect() {
	for {
		select {
		case item := <-s.queue:
			s.pending = append(s.pending, item)
		default:
			return
		}
	}
}

// flushOnce processes the retry queue, then submits up to batchSize pending
// items grouped by kind — one flush-interval turn.
func (s *submitter) flushOnce() {
	s.collect()
	if s.paused() {
		return
	}
	s.processRetryQueue()
	if s.paused() {
		return
	}

	n := len(s.pending)
	if n > batchSize {
		n = batchSize
	}
	if n == 0 {
		return
	}
	batch := s.pending[:n]
	s.pending = append([]queueItem(nil), s.pending[n:]...)

	var exposures []Exposure
	var events []event
	for _, item := range batch {
		switch item.kind {
		case kindExposure:
			exposures = append(exposures, item.exposure)
		case kindEvent:
			events = append(events, item.event)
		}
	}
	if len(exposures) > 0 {
		s.submitBatch(kindExposure, exposuresAsItems(exposures), false)
	}
	if len(events) > 0 {
		if s.paused() {
			s.addToRetryQueue(eventsAsItems(events))
		} else {
			s.submitBatch(kindEvent, eventsAsItems(events), false)
		}
	}
}

// drainAll flushes until both queues are empty — the flush barrier used by
// graceful shutdown. Items that keep failing with 5xx/408 are dropped by the
// attempts cap; rate-limit pauses are waited out up to maxDrainPause in
// total, then everything left is dropped, so this terminates.
func (s *submitter) drainAll() {
	var waited time.Duration
	for {
		s.collect()
		if len(s.pending) == 0 && len(s.retryQueue) == 0 {
			return
		}
		if wait := s.pausedUntil.Sub(s.now()); wait > 0 {
			if waited+wait > maxDrainPause {
				dropped := len(s.pending) + len(s.retryQueue)
				s.pending = nil
				s.retryQueue = nil
				s.log(fmt.Sprintf("rate limited for %ds more, past the drain budget; dropping %d items", ceilSeconds(wait), dropped))
				return
			}
			s.sleep(wait)
			waited += wait
			continue
		}
		s.flushOnce()
	}
}

func (s *submitter) paused() bool {
	return s.now().Before(s.pausedUntil)
}

func (s *submitter) pause(retryAfter time.Duration, count int, kind itemKind) {
	if until := s.now().Add(retryAfter); until.After(s.pausedUntil) {
		s.pausedUntil = until
	}
	s.log(fmt.Sprintf("warning: rate limited, pausing submits for %ds (%d %ss to retry)", ceilSeconds(retryAfter), count, kind))
}

// submitBatch sends one batch and returns the items a retry pass should keep;
// rateLimited reports a 429 (the submitter is now paused).
func (s *submitter) submitBatch(kind itemKind, items []queueItem, isRetry bool) (kept []queueItem, rateLimited bool) {
	if len(items) == 0 {
		return nil, false
	}
	err := s.send(kind, items)
	if err == nil {
		return nil, false
	}

	if apiErr, ok := err.(*APIError); ok {
		switch {
		case apiErr.RateLimited():
			s.pause(apiErr.RetryAfter, len(items), kind)
			if isRetry {
				return items, true
			}
			s.addToRetryQueue(items)
			return nil, true
		case apiErr.Retryable():
			s.log(fmt.Sprintf("retryable error submitting %d %ss: %s", len(items), kind, apiErr.Message))
			if isRetry {
				return items, false
			}
			s.addToRetryQueue(items)
			return nil, false
		case apiErr.PartialFailure():
			s.log(fmt.Sprintf("%d out of %d %ss failed validation", apiErr.FailureCount(), len(items), kind))
			return nil, false
		default:
			s.log(fmt.Sprintf("permanent error submitting %ss: %s", kind, apiErr.Message))
			return nil, false
		}
	}

	// Unknown transport error: drop on first submit (unknown cause). On
	// retry the contract is asymmetric — exposures re-queue, events drop.
	s.log(fmt.Sprintf("failed to submit %d %ss: %v", len(items), kind, err))
	if isRetry && kind == kindExposure {
		return items, false
	}
	return nil, false
}

func (s *submitter) send(kind itemKind, items []queueItem) error {
	if kind == kindExposure {
		exposures := make([]Exposure, len(items))
		for i, item := range items {
			exposures[i] = item.exposure
		}
		return s.client.submitExposures(exposures)
	}
	events := make([]event, len(items))
	for i, item := range items {
		events[i] = item.event
	}
	return s.client.trackEvents(events)
}

func (s *submitter) addToRetryQueue(items []queueItem) {
	room := min(max(maxRetryQueueSize-len(s.retryQueue), 0), len(items))
	for _, item := range items[:room] {
		s.retryQueue = append(s.retryQueue, retryItem{queueItem: item})
	}
	if dropped := len(items) - room; dropped > 0 {
		s.log(fmt.Sprintf("retry queue full, dropping %d %ss", dropped, items[0].kind))
	}
}

func (s *submitter) processRetryQueue() {
	if len(s.retryQueue) == 0 {
		return
	}
	pending := s.retryQueue
	s.retryQueue = nil
	for _, kind := range []itemKind{kindExposure, kindEvent} {
		var batch []retryItem
		for _, item := range pending {
			if item.kind == kind {
				batch = append(batch, item)
			}
		}
		if s.paused() {
			s.retryQueue = append(s.retryQueue, batch...)
			continue
		}
		s.retryQueue = append(s.retryQueue, s.processRetryBatch(kind, batch)...)
	}
}

// processRetryBatch bumps attempts (dropping items at the cap), resubmits,
// and returns the items to keep for a future retry. A 429 undoes the bump:
// the wait never counts as an attempt.
func (s *submitter) processRetryBatch(kind itemKind, items []retryItem) []retryItem {
	if len(items) == 0 {
		return nil
	}
	var active []retryItem
	for _, item := range items {
		item.attempts++
		if item.attempts >= maxSubmitAttempts {
			s.log(fmt.Sprintf("max retries exceeded for %s, dropping item", kind))
		} else {
			active = append(active, item)
		}
	}
	if len(active) == 0 {
		return nil
	}

	data := make([]queueItem, len(active))
	for i, item := range active {
		data[i] = item.queueItem
	}
	kept, rateLimited := s.submitBatch(kind, data, true)
	if rateLimited {
		for i := range active {
			active[i].attempts--
		}
		return active
	}
	if len(kept) > 0 {
		return active
	}
	return nil
}

func ceilSeconds(d time.Duration) int64 {
	return int64((d + time.Second - 1) / time.Second)
}

func exposuresAsItems(exposures []Exposure) []queueItem {
	items := make([]queueItem, len(exposures))
	for i, exposure := range exposures {
		items[i] = queueItem{kind: kindExposure, exposure: exposure}
	}
	return items
}

func eventsAsItems(events []event) []queueItem {
	items := make([]queueItem, len(events))
	for i, e := range events {
		items[i] = queueItem{kind: kindEvent, event: e}
	}
	return items
}

func (s *submitter) log(message string) {
	if s.logger != nil {
		s.logger.Printf("AsyncSubmitter: %s", message)
	}
}
