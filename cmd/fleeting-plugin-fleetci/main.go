// Command fleeting-plugin-fleetci is the fleeting plugin gitlab-runner loads
// to give each job its own VM, sized by the job's class.
package main

import (
	"gitlab.com/gitlab-org/fleeting/fleeting/plugin"

	"github.com/nesty92/fleetci/internal/fleetci"
)

func main() {
	plugin.Main(&fleetci.InstanceGroup{}, plugin.VersionInfo{Name: "fleeting-plugin-fleetci"})
}
