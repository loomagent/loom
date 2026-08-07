package loom

import (
	"errors"
	"testing"
)

func TestErrUnsupportedCompatibilityAlias(t *testing.T) {
	if !errors.Is(ErrUnsupported, ErrUnsupportedCapability) {
		t.Fatal("ErrUnsupported must preserve ErrUnsupportedCapability identity")
	}
}
