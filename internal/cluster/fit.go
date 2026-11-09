package cluster

import (
	"fmt"
	"maps"
	"slices"

	"github.com/nesty92/fleetci/internal/fleet"
)

// imageDiskMB is what the image and its snapshots take on a worker's disk,
// besides the VMs' own disks, which are thin clones of it.
const imageDiskMB = 8 * 1024

// FitClasses works out which classes a worker runs: those whose VM fits its
// budget at least once, on its architecture, with KVM, the disk for the
// image, and the labels the class asks for. How many run at once is never
// configured: the worker's budget is shared by its classes.
func FitClasses(classes map[string]*ClassSpec, f Facts) Fit {
	fit := Fit{Refused: map[string]string{}}
	for _, name := range slices.Sorted(maps.Keys(classes)) {
		c := classes[name]
		if why := refuse(name, c, f); why != "" {
			fit.Refused[name] = why
			continue
		}
		fit.Classes = append(fit.Classes, Class{Name: name, ClassSpec: *c})
	}
	return fit
}

func refuse(name string, c *ClassSpec, f Facts) string {
	switch {
	case len(f.Only) > 0 && !slices.Contains(f.Only, name):
		return "not in the worker's classes"
	case f.KVM != "":
		return "no KVM: " + f.KVM
	case f.Arch != c.Arch:
		return fmt.Sprintf("the class is %s, the worker %s", c.Arch, f.Arch)
	case c.CPUs > f.CPUs:
		return fmt.Sprintf("needs %d CPUs, the worker's budget has %d", c.CPUs, f.CPUs)
	case int64(c.Memory) > f.MemoryMB:
		return fmt.Sprintf("needs %s memory, the worker's budget has %s", c.Memory, fleet.Size(f.MemoryMB))
	case f.DiskMB < imageDiskMB:
		return fmt.Sprintf("needs %s free disk for the image, the worker has %s", fleet.Size(imageDiskMB), fleet.Size(f.DiskMB))
	}
	for k, v := range c.On {
		if f.Labels[k] != v {
			return fmt.Sprintf("needs the label %s=%s", k, v)
		}
	}
	return ""
}
