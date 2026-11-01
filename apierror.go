package abmeter

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// APIError is returned for non-success responses from the ABMeter API.
type APIError struct {
	Status  int
	Message string
	Code    string
	Details map[string]any
	// RetryAfter is how long to wait before the next submit. Set on a 429
	// only, from the Retry-After header; zero otherwise.
	RetryAfter time.Duration
}

func newAPIError(status int, header http.Header, body []byte) *APIError {
	apiErr := &APIError{Status: status, Details: map[string]any{}}
	if status == http.StatusTooManyRequests {
		apiErr.RetryAfter = parseRetryAfter(header.Get("Retry-After"))
	}

	var parsed struct {
		Error   string         `json:"error"`
		Code    string         `json:"code"`
		Details map[string]any `json:"details"`
	}
	if err := json.Unmarshal(body, &parsed); err == nil && parsed.Error != "" {
		apiErr.Message = parsed.Error
		apiErr.Code = parsed.Code
		if parsed.Details != nil {
			apiErr.Details = parsed.Details
		}
	} else {
		apiErr.Message = string(body)
	}
	if apiErr.Message == "" {
		apiErr.Message = "Unknown error"
	}
	return apiErr
}

// parseRetryAfter reads a delta-seconds Retry-After value. HTTP dates,
// negative and non-numeric values fall back to defaultRetryAfter.
func parseRetryAfter(value string) time.Duration {
	seconds, err := strconv.ParseUint(strings.TrimSpace(value), 10, 64)
	switch {
	case errors.Is(err, strconv.ErrRange):
		return maxRetryAfter
	case err != nil:
		return defaultRetryAfter
	case seconds > uint64(maxRetryAfter/time.Second):
		return maxRetryAfter
	}
	return max(time.Duration(seconds)*time.Second, minRetryAfter)
}

func (e *APIError) Error() string {
	return e.Message
}

// Retryable reports whether the failure is transient: server errors,
// request timeout, or rate limiting.
func (e *APIError) Retryable() bool {
	return e.Status >= 500 ||
		e.Status == http.StatusRequestTimeout ||
		e.Status == http.StatusTooManyRequests
}

// RateLimited reports a 429: the submitter pauses for RetryAfter instead of
// counting an attempt.
func (e *APIError) RateLimited() bool {
	return e.Status == http.StatusTooManyRequests
}

// PartialFailure reports a 400 where some items in the batch failed
// validation (the response details carry the failures). Never retried:
// validation won't change.
func (e *APIError) PartialFailure() bool {
	if e.Status != http.StatusBadRequest {
		return false
	}
	failures, ok := e.Details["failures"].([]any)
	return ok && len(failures) > 0
}

// FailureCount returns the number of items the platform rejected.
func (e *APIError) FailureCount() int {
	if count, ok := e.Details["invalid_count"].(float64); ok {
		return int(count)
	}
	return 0
}
