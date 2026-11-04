// Command fleeting-plugin-fleetci is the fleeting plugin gitlab-runner loads
// to give each job its own VM, sized by the job's class.
package main

import (
	"gitlab.com/gitlab-org/fleeting/fleeting/plugin"

	"github.com/nesty92/fleetci/internal/fleetci"
)

// Set at release build time.
var (
	version  = "dev"
	revision = ""
	builtAt  = ""
)

func main() {
	plugin.Main(&fleetci.InstanceGroup{}, plugin.VersionInfo{
		Name:      "fleeting-plugin-fleetci",
		Version:   version,
		Revision:  revision,
		Reference: "https://github.com/nesty92/fleetci",
		BuiltAt:   builtAt,
	})
}
