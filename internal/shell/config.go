package shell

import (
	"time"

	"github.com/nesty92/fleetci/internal/fleet"
)

// Config is a forge's [<forge>.shell] table: set, a failed job's VM can be
// kept for a shell, as the forge's job asks.
type Config struct {
	// Hold is how long a failed job's VM is kept. Default 10m. A held VM
	// counts against its class's max_instances.
	Hold fleet.Duration `json:"hold" toml:"hold"`
	// DERPMapURL is where tailcat finds the relays it connects through
	// when no direct path works. Default tailcat.dev's.
	DERPMapURL string `json:"derp_map_url" toml:"derp_map_url"`
}

// DefaultHold is how long a failed job's VM is kept by default.
const DefaultHold = 10 * time.Minute

// HoldFor is how long a failed job's VM is kept.
func (c *Config) HoldFor() time.Duration {
	if c.Hold > 0 {
		return time.Duration(c.Hold)
	}
	return DefaultHold
}
