package cachedisk

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// QemuImg makes layers with qemu-img.
type QemuImg struct {
	// Path is the qemu-img binary; "" finds it on PATH.
	Path string
}

func (q QemuImg) run(timeout time.Duration, args ...string) error {
	bin := q.Path
	if bin == "" {
		bin = "qemu-img"
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("qemu-img %s: %w: %s", args[0], err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Create makes an empty layer, on parent when it isn't "".
func (q QemuImg) Create(path, parent string, size int64) error {
	args := []string{"create", "-q", "-f", "qcow2"}
	if parent != "" {
		args = append(args, "-F", "qcow2", "-b", parent)
	}
	return q.run(time.Minute, append(args, path, strconv.FormatInt(size, 10))...)
}

// Flatten copies the whole chain into dst, which has no backing file. The
// chain's layers are never written once kept, so it reads them while VMs
// have them open (-U): without it, qemu-img fails on their locks.
func (q QemuImg) Flatten(src, dst string) error {
	return q.run(time.Hour, "convert", "-q", "-U", "-O", "qcow2", src, dst)
}
