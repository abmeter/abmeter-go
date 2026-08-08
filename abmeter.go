// Package abmeter is the ABMeter Go server SDK: feature flags and A/B
// experiments with client-side evaluation.
//
// Configure once, then resolve parameters and track events:
//
//	err := abmeter.Configure(abmeter.Config{APIKey: "..."})
//	value, err := abmeter.ResolveParameter(abmeter.User{UserID: "u_1"}, "button-color")
//	err = abmeter.TrackEvent("purchase", "u_1", map[string]any{"price": 4.99})
//
// Assignment is random across users and consistent per user. Exposures and
// events are submitted in the background; call Reset before process exit to
// flush them.
package abmeter

import (
	"context"
	"errors"
	"sync"
)

var (
	mu       sync.RWMutex
	instance *sdk
)

type sdk struct {
	config   Config
	client   *client
	provider *resolverProvider
	sub      *submitter
}

// Configure initializes the SDK singleton and starts the background
// submitter. With Config.StaticConfig set, the SDK runs offline: nothing is
// fetched and nothing is submitted. Calling Configure again replaces the
// previous configuration (its unflushed queue is discarded — call Reset
// first for a graceful handover).
func Configure(cfg Config) error {
	cfg = cfg.withDefaults()
	if err := cfg.validate(); err != nil {
		return err
	}

	next := &sdk{config: cfg}
	if cfg.StaticConfig == "" {
		next.client = newClient(&cfg)
		next.sub = newSubmitter(next.client, &cfg)
	}
	provider, err := newResolverProvider(&cfg, next.client, next.sub)
	if err != nil {
		return err
	}
	next.provider = provider

	mu.Lock()
	previous := instance
	instance = next
	mu.Unlock()

	if previous != nil && previous.sub != nil {
		previous.sub.stopNow()
	}
	if next.sub != nil {
		next.sub.start()
	}
	return nil
}

var errNotConfigured = errors.New("abmeter not configured: call abmeter.Configure first")

func current() (*sdk, error) {
	mu.RLock()
	defer mu.RUnlock()
	if instance == nil {
		return nil, errNotConfigured
	}
	return instance, nil
}

// ResolveParameter resolves the parameter for the user and queues the
// exposure for background submission (when the resolution came from an
// experiment or feature flag).
func ResolveParameter(user User, parameterSlug string) (any, error) {
	s, err := current()
	if err != nil {
		return nil, err
	}
	return s.provider.resolveParameter(user, parameterSlug)
}

// GetExposure evaluates the full exposure detail without submitting it.
// A debug/testing helper.
func GetExposure(user User, parameterSlug string) (Exposure, error) {
	s, err := current()
	if err != nil {
		return Exposure{}, err
	}
	return s.provider.getExposure(user, parameterSlug)
}

// TrackEvent queues an event for background submission.
func TrackEvent(eventSlug, userID string, customFields map[string]any) error {
	s, err := current()
	if err != nil {
		return err
	}
	if s.sub == nil {
		return nil // static config: offline, nothing to submit
	}
	s.sub.queueEvent(eventSlug, userID, customFields)
	return nil
}

// Reset gracefully tears down the SDK: queued exposures/events are flushed,
// bounded by ctx (a ctx without a deadline gets DefaultShutdownTimeout).
// Returns an error when the drain did not finish in time — anything still
// queued is lost. Safe to call when not configured.
func Reset(ctx context.Context) error {
	mu.Lock()
	s := instance
	instance = nil
	mu.Unlock()

	if s == nil || s.sub == nil {
		return nil
	}
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, DefaultShutdownTimeout)
		defer cancel()
	}
	return s.sub.stop(ctx)
}

// ResetNow tears down the SDK immediately: queued exposures/events are
// discarded without network I/O — data loss by design. Safe to call when
// not configured.
func ResetNow() {
	mu.Lock()
	s := instance
	instance = nil
	mu.Unlock()

	if s != nil && s.sub != nil {
		s.sub.stopNow()
	}
}
