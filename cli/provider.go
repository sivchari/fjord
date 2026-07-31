package cli

import (
	"fmt"

	clusterprovider "github.com/sivchari/fjord/internal/provider"
	"github.com/sivchari/fjord/internal/rask"
)

// newClusterProvider constructs the clusterprovider.Provider fjord runs
// clusters on (rask).
func newClusterProvider() (clusterprovider.Provider, error) {
	provider, err := rask.NewProvider()
	if err != nil {
		return nil, fmt.Errorf("new rask provider: %w", err)
	}

	return provider, nil
}
