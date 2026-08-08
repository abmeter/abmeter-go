package abmeter

import (
	"encoding/json"
	"net/http"
)

// APIError is returned for non-success responses from the ABMeter API.
type APIError struct {
	Status  int
	Message string
	Code    string
	Details map[string]any
}

func newAPIError(status int, body []byte) *APIError {
	apiErr := &APIError{Status: status, Details: map[string]any{}}

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
