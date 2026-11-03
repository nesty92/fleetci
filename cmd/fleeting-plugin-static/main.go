// Command fleeting-plugin-static is a fleeting plugin that serves one
// existing host at a fixed address.
package main

import (
	"gitlab.com/gitlab-org/fleeting/fleeting/plugin"

	"github.com/nesty92/fleetci/internal/static"
)

func main() {
	plugin.Main(&static.InstanceGroup{}, plugin.VersionInfo{Name: "fleeting-plugin-static"})
}
