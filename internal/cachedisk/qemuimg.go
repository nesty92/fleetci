package cachedisk

import (
	"context"
	"fmt"
	"io"
	"os"
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

// Flatten copies the chain into dst. With base, dst is a copy of src
// rebased onto it, safely: what the layers between them hold is copied
// in (convert -B would copy src's own clusters only). Without, dst has no
// backing file. The chain's layers are never written once kept, so it
// reads them while VMs have them open (-U): without it, qemu-img fails on
// their locks.
func (q QemuImg) Flatten(src, base, dst string) error {
	if base == "" {
		return q.run(time.Hour, "convert", "-q", "-U", "-O", "qcow2", src, dst)
	}
	if err := copyFile(src, dst); err != nil {
		return err
	}
	return q.run(time.Hour, "rebase", "-q", "-U", "-f", "qcow2", "-F", "qcow2", "-b", base, dst)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// Rebase points the layer at path to parent without touching its data
// (-u): the parent is the same layer, at this node's path.
func (q QemuImg) Rebase(path, parent string) error {
	return q.run(time.Minute, "rebase", "-q", "-u", "-f", "qcow2", "-F", "qcow2", "-b", parent, path)
}
