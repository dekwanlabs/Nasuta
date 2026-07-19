package contract_test

import (
	"testing"

	"github.com/dekwanlabs/nasuta/semantic/contract"
)

// TestContractAgainstMemory drives the shared contract suite against the
// in-memory reference store. It runs in normal CI (no live backend required)
// and guards the contract logic itself, independent of Qdrant or Milvus.
func TestContractAgainstMemory(t *testing.T) {
	contract.Run(t, contract.NewMemory())
}
