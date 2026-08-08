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
//   - Retryable APIError (5xx/408/429): both kinds go to the bounded retry
//     queue (max 1000 items, max 3 attempts each).
//   - Partial validation failure / permanent APIError: dropped — validation
//     won't change.
//   - Unknown transport error on FIRST submit: dropped (unknown cause, both
//     kinds). On RETRY: exposures are re-queued, events are dropped.
//
// The main queue is deliberately unbounded (mutex-guarded slice, matching
// Ruby's Queue): a bounded channel would silently drop under burst load,
// which the completeness gate would surface as data loss.
type submitter struct {
	queue         chan queueItem // enqueue side; effectively unbounded via pending
	flushInterval time.Duration
	client        *client
	logger        *log.Logger

	// Owned by the worker goroutine between start and doneCh close.
	pending    []queueItem
	retryQueue []retryItem

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
	s.processRetryQueue()

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
		s.submitBatch(kindEvent, eventsAsItems(events), false)
	}
}

// drainAll flushes until both queues are empty — the flush barrier used by
// graceful shutdown and flushSync. Items that keep failing retryably are
// dropped by the attempts cap, so this terminates.
func (s *submitter) drainAll() {
	for {
		s.collect()
		if len(s.pending) == 0 && len(s.retryQueue) == 0 {
			return
		}
		s.flushOnce()
	}
}

func (s *submitter) submitBatch(kind itemKind, items []queueItem, isRetry bool) []queueItem {
	if len(items) == 0 {
		return nil
	}
	err := s.send(kind, items)
	if err == nil {
		return nil
	}

	if apiErr, ok := err.(*APIError); ok {
		switch {
		case apiErr.Retryable():
			s.log(fmt.Sprintf("retryable error submitting %d %ss: %s", len(items), kind, apiErr.Message))
			if isRetry {
				return items
			}
			s.addToRetryQueue(items)
			return nil
		case apiErr.PartialFailure():
			s.log(fmt.Sprintf("%d out of %d %ss failed validation", apiErr.FailureCount(), len(items), kind))
			return nil
		default:
			s.log(fmt.Sprintf("permanent error submitting %ss: %s", kind, apiErr.Message))
			return nil
		}
	}

	// Unknown transport error: drop on first submit (unknown cause). On
	// retry the contract is asymmetric — exposures re-queue, events drop.
	if isRetry && kind == kindExposure {
		s.log(fmt.Sprintf("failed to submit %d %ss: %v", len(items), kind, err))
		return items
	}
	s.log(fmt.Sprintf("failed to submit %d %ss: %v", len(items), kind, err))
	return nil
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
	for _, item := range items {
		if len(s.retryQueue) < maxRetryQueueSize {
			s.retryQueue = append(s.retryQueue, retryItem{queueItem: item})
		} else {
			s.log(fmt.Sprintf("retry queue full, dropping %s", item.kind))
		}
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
		s.retryQueue = append(s.retryQueue, s.processRetryBatch(kind, batch)...)
	}
}

// processRetryBatch bumps attempts (dropping items at the cap), resubmits,
// and returns the items to keep for a future retry.
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
	if kept := s.submitBatch(kind, data, true); len(kept) > 0 {
		return active
	}
	return nil
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
