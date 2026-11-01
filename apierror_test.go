package abmeter

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

// errorEnvelopeSample mirrors one entry of testdata/error_envelope.json, a
// copy of the API's golden fixture (api/spec/fixtures/error_envelope.json).
type errorEnvelopeSample struct {
	Name     string `json:"name"`
	Status   int    `json:"status"`
	Body     string `json:"body"`
	Expected struct {
		Message        *string        `json:"message"`
		Code           *string        `json:"code"`
		Details        map[string]any `json:"details"`
		Retryable      bool           `json:"retryable"`
		PartialFailure bool           `json:"partial_failure"`
		FailureCount   int            `json:"failure_count"`
	} `json:"expected"`
}

func TestAPIErrorParsesErrorEnvelopeFixture(t *testing.T) {
	raw, err := os.ReadFile("testdata/error_envelope.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Samples []errorEnvelopeSample `json:"samples"`
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}

	for _, sample := range fixture.Samples {
		t.Run(sample.Name, func(t *testing.T) {
			apiErr := newAPIError(sample.Status, nil, []byte(sample.Body))
			want := sample.Expected

			if want.Message != nil && apiErr.Message != *want.Message {
				t.Errorf("Message = %q, want %q", apiErr.Message, *want.Message)
			}
			wantCode := ""
			if want.Code != nil {
				wantCode = *want.Code
			}
			if apiErr.Code != wantCode {
				t.Errorf("Code = %q, want %q", apiErr.Code, wantCode)
			}
			if !reflect.DeepEqual(apiErr.Details, want.Details) {
				t.Errorf("Details = %v, want %v", apiErr.Details, want.Details)
			}
			if apiErr.Retryable() != want.Retryable {
				t.Errorf("Retryable() = %v, want %v", apiErr.Retryable(), want.Retryable)
			}
			if apiErr.PartialFailure() != want.PartialFailure {
				t.Errorf("PartialFailure() = %v, want %v", apiErr.PartialFailure(), want.PartialFailure)
			}
			if apiErr.FailureCount() != want.FailureCount {
				t.Errorf("FailureCount() = %d, want %d", apiErr.FailureCount(), want.FailureCount)
			}
		})
	}
}
