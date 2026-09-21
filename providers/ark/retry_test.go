package ark

import (
	"context"
	"errors"
	"fmt"
	"testing"

	arkmodel "github.com/volcengine/volcengine-go-sdk/service/arkruntime/model"

	"github.com/loomagent/loom"
)

func TestClassifierAPIError(t *testing.T) {
	tests := []struct {
		name   string
		status int
		want   loom.ErrorClass
	}{
		{name: "bad request", status: 400, want: loom.ErrorClassPermanent},
		{name: "model not open", status: 404, want: loom.ErrorClassPermanent},
		{name: "rate limit", status: 429, want: loom.ErrorClassRateLimit},
		{name: "service unavailable", status: 503, want: loom.ErrorClassTransient},
		{name: "server error", status: 500, want: loom.ErrorClassTransient},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := fmt.Errorf("wrapped: %w", &arkmodel.APIError{HTTPStatusCode: tt.status})
			if got := (classifier{}).ClassifyError(err); got != tt.want {
				t.Fatalf("ClassifyError(status=%d) = %s, want %s", tt.status, got, tt.want)
			}
			if tt.status == 503 && !(classifier{}).IsServiceUnavailable(err) {
				t.Fatal("503 must open the shared service-unavailable circuit")
			}
		})
	}
}

func TestClassifierCoversEverySource(t *testing.T) {
	api := func(status int) error { return &arkmodel.APIError{HTTPStatusCode: status} }
	req := func(status int) error { return &arkmodel.RequestError{HTTPStatusCode: status} }
	tests := []struct {
		name               string
		err                error
		want               loom.ErrorClass
		serviceUnavailable bool
	}{
		{name: "nil", err: nil, want: loom.ErrorClassUnknown},
		{name: "cancelled", err: context.Canceled, want: loom.ErrorClassPermanent},
		{name: "deadline", err: context.DeadlineExceeded, want: loom.ErrorClassPermanent},
		{name: "unauthorized", err: api(401), want: loom.ErrorClassPermanent},
		{name: "payment required", err: api(402), want: loom.ErrorClassPermanent},
		{name: "forbidden", err: api(403), want: loom.ErrorClassPermanent},
		{name: "other client error", err: api(418), want: loom.ErrorClassPermanent},
		{name: "rate limit", err: api(429), want: loom.ErrorClassRateLimit},
		{name: "unavailable", err: api(503), want: loom.ErrorClassTransient, serviceUnavailable: true},
		{name: "request error unauthorized", err: req(401), want: loom.ErrorClassPermanent},
		{name: "request error unavailable", err: req(503), want: loom.ErrorClassTransient, serviceUnavailable: true},
		{name: "network failure", err: errors.New("connection reset"), want: loom.ErrorClassTransient},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			wrapped := tt.err
			if tt.err != nil {
				wrapped = fmt.Errorf("wrapped: %w", tt.err)
			}
			if got := (classifier{}).ClassifyError(wrapped); got != tt.want {
				t.Fatalf("ClassifyError(%v) = %s, want %s", tt.err, got, tt.want)
			}
			if got := (classifier{}).IsServiceUnavailable(wrapped); got != tt.serviceUnavailable {
				t.Fatalf("IsServiceUnavailable(%v) = %t, want %t", tt.err, got, tt.serviceUnavailable)
			}
		})
	}
}
