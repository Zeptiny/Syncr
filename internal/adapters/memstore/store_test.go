package memstore_test

import (
	"testing"

	"github.com/Zeptiny/Syncr/internal/adapters/memstore"
	"github.com/Zeptiny/Syncr/internal/adapters/storetest"
)

func TestContract(t *testing.T) {
	t.Parallel()
	storetest.Run(t, func(*testing.T) storetest.Store { return memstore.New() })
}
