package ark

import (
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
