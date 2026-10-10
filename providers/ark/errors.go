package ark

import (
	"errors"
	"fmt"
	"strings"

	"github.com/loomagent/loom"
	arkmodel "github.com/volcengine/volcengine-go-sdk/service/arkruntime/model"
)

// Ark's business code is authoritative; a phrase in Message is not. A dot
// separates policy subcategories, so similarly named parameter errors cannot
// match. HTTPStatusCode is absent (zero) on the SDK's in-stream APIError.
func normalizeError(err error) error {
	if err == nil || errors.Is(err, loom.ErrSensitiveContentRisk) {
		return err
	}
	e, ok := errors.AsType[*arkmodel.APIError](err)
	if !ok || (e.HTTPStatusCode != 0 && e.HTTPStatusCode != 400) {
		return err
	}
	base, suffix, hasSuffix := strings.Cut(e.Code, ".")
	if hasSuffix && strings.TrimSpace(suffix) == "" {
		return err
	}
	switch base {
	case "SensitiveContentDetected", "InputTextSensitiveContentDetected", "OutputTextSensitiveContentDetected",
		"InputImageSensitiveContentDetected", "OutputImageSensitiveContentDetected",
		"InputVideoSensitiveContentDetected", "OutputVideoSensitiveContentDetected",
		"InputAudioSensitiveContentDetected", "OutputAudioSensitiveContentDetected",
		"InputTextRiskDetection", "OutputTextRiskDetection", "InputImageRiskDetection", "OutputImageRiskDetection":
		return fmt.Errorf("%w: %w", loom.ErrSensitiveContentRisk, err)
	default:
		return err
	}
}
