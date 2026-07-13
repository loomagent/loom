package sourceregistrytest

import (
	"testing"

	"github.com/loomagent/loom/sourceregistry"
)

func TestMemoryStoreContract(t *testing.T) {
	TestStore(t, func(*testing.T) sourceregistry.Store {
		return sourceregistry.NewMemoryStore()
	})
}
