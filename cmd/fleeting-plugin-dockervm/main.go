// Command fleeting-plugin-dockervm is a fleeting plugin whose instances are
// local Docker-in-Docker containers. It stands in for a cloud plugin when
// trying fleetci on one machine.
package main

import (
	"gitlab.com/gitlab-org/fleeting/fleeting/plugin"

	"github.com/nesty92/fleetci/internal/dockervm"
)

func main() {
	plugin.Main(&dockervm.InstanceGroup{}, plugin.VersionInfo{Name: "fleeting-plugin-dockervm"})
}
