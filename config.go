package abmeter

import (
	"errors"
	"log"
	"net/http"
	"os"
	"time"
)

// Defaults shared across the SDK. Mirrors the Ruby/Python constants.
const (
	DefaultBaseURL         = "https://abmeter.ai"
	DefaultFlushInterval   = 60 * time.Second
	DefaultFetchInterval   = 60 * time.Second
	DefaultShutdownTimeout = 5 * time.Second

	batchSize         = 100
	maxSubmitAttempts = 3
	maxRetryQueueSize = 1000

	// A 429 pauses the submitter for the response's Retry-After, read as
	// delta-seconds. A missing or invalid value falls back to
	// defaultRetryAfter; the value is clamped to [minRetryAfter,
	// maxRetryAfter] (maxRetryAfter is the API's rate-limit window).
	defaultRetryAfter = 30 * time.Second
	minRetryAfter     = time.Second
	maxRetryAfter     = 600 * time.Second
	// maxDrainPause bounds the total time the final drain waits out pauses.
	maxDrainPause = 5 * time.Second
)

// Config configures the SDK. Zero values take the documented defaults.
type Config struct {
	// APIKey authenticates against the ABMeter API. Required unless
	// StaticConfig is set.
	APIKey string
	// BaseURL is the API origin. Default: DefaultBaseURL.
	BaseURL string
	// StaticConfig is a JSON assignment config used instead of the API —
	// offline mode: nothing is fetched and nothing is submitted.
	StaticConfig string
	// FlushInterval is the pause between background submit flushes.
	// Default: DefaultFlushInterval.
	FlushInterval time.Duration
	// FetchInterval is how long a fetched assignment config is trusted
	// before the next resolve re-checks it (with If-None-Match, so an
	// unchanged config costs a 304). Default: DefaultFetchInterval.
	FetchInterval time.Duration
	// Logger receives submitter errors. Default: a stderr logger. Pass
	// log.New(io.Discard, "", 0) to silence.
	Logger *log.Logger
	// HTTPClient overrides the HTTP client (e.g. for tests or custom
	// transports). Default: a client with a 30s timeout.
	HTTPClient *http.Client
}

// withDefaults returns a copy with zero values replaced by defaults.
func (c Config) withDefaults() Config {
	if c.BaseURL == "" {
		c.BaseURL = DefaultBaseURL
	}
	if c.FlushInterval == 0 {
		c.FlushInterval = DefaultFlushInterval
	}
	if c.FetchInterval == 0 {
		c.FetchInterval = DefaultFetchInterval
	}
	if c.Logger == nil {
		c.Logger = log.New(os.Stderr, "abmeter: ", log.LstdFlags)
	}
	return c
}

func (c Config) validate() error {
	if c.StaticConfig != "" {
		return nil
	}
	if c.APIKey == "" {
		return errors.New("no API key provided")
	}
	return nil
}
