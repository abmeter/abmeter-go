# Changelog

## 0.2.0 (2026-11-01)

- A failed config refresh logs one line through the configured logger and keeps serving the last known config.
- On a 429 the submitter pauses for the response's `Retry-After` (30 seconds when the header is missing or unreadable, at least 1 second, at most 600) and keeps the batch queued instead of dropping it. `APIError.RetryAfter` carries the pause.
