package abmeter

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

const (
	assignmentConfigPath = "/api/v1/assignment-config"
	exposuresPath        = "/api/v1/exposures"
	eventsPath           = "/api/v1/events"

	httpTimeout = 30 * time.Second
)

// client is the HTTP client for the ABMeter API.
type client struct {
	baseURL string
	apiKey  string
	http    *http.Client
}

func newClient(cfg *Config) *client {
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: httpTimeout}
	}
	return &client{baseURL: cfg.BaseURL, apiKey: cfg.APIKey, http: httpClient}
}

// configResponse is the result of an assignment-config fetch. When the server
// answers 304 Not Modified (the sent ETag still matches), notModified is true
// and configJSON is empty.
type configResponse struct {
	configJSON  []byte
	etag        string
	notModified bool
}

// getAssignmentConfig fetches the assignment config, sending If-None-Match
// when a previous fetch produced an ETag so an unchanged config costs a 304
// instead of a full body.
func (c *client) getAssignmentConfig(etag string) (configResponse, error) {
	req, err := http.NewRequest(http.MethodGet, c.baseURL+assignmentConfigPath, nil)
	if err != nil {
		return configResponse{}, err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return configResponse{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotModified {
		return configResponse{etag: etag, notModified: true}, nil
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return configResponse{}, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return configResponse{}, newAPIError(resp.StatusCode, resp.Header, body)
	}

	var parsed struct {
		Config string `json:"config"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return configResponse{}, fmt.Errorf("invalid assignment-config response: %w", err)
	}
	return configResponse{configJSON: []byte(parsed.Config), etag: resp.Header.Get("ETag")}, nil
}

func (c *client) submitExposures(exposures []Exposure) error {
	if len(exposures) == 0 {
		return nil
	}
	return c.post(exposuresPath, exposures)
}

func (c *client) trackEvents(events []event) error {
	if len(events) == 0 {
		return nil
	}
	return c.post(eventsPath, events)
}

func (c *client) post(path string, payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(resp.Body)
		return newAPIError(resp.StatusCode, resp.Header, respBody)
	}
	return nil
}
