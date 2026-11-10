package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// workerService is the systemd unit fleetci join -service installs.
const workerService = "fleetci-worker.service"

var systemdDir = "/etc/systemd/system"

// workerUnit runs the worker. On SIGTERM it removes its VMs, which may
// take two minutes (Worker.Run): KillMode=mixed sends it to fleetci alone
// and the VMs' processes are killed only after that, or the timeout.
const workerUnit = `[Unit]
Description=fleetci worker
Documentation=https://github.com/nesty92/fleetci
Wants=network-online.target
After=network-online.target

[Service]
ExecStart=%s worker -config %s
Restart=always
RestartSec=5
KillMode=mixed
TimeoutStopSec=150

[Install]
WantedBy=multi-user.target
`

func systemctl(args ...string) error {
	out, err := exec.Command("systemctl", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// installService writes the worker's unit and (re)starts it, so a join run
// again starts the worker with what it wrote.
func installService(dir, bin, config string, ctl func(...string) error, out io.Writer) error {
	if _, err := os.Stat("/run/systemd/system"); err != nil && dir == systemdDir {
		return fmt.Errorf("no systemd here: run fleetci worker -config %s yourself", config)
	}
	for _, p := range []string{bin, config} {
		if !filepath.IsAbs(p) || strings.ContainsAny(p, " \t\n\"'\\%$;") {
			return fmt.Errorf("-service needs a plain absolute path, not %q", p)
		}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	unit := filepath.Join(dir, workerService)
	if err := os.WriteFile(unit, fmt.Appendf(nil, workerUnit, bin, config), 0o644); err != nil {
		return err
	}
	fmt.Fprintf(out, "ok    %-14s wrote %s\n", "service", unit)
	for _, args := range [][]string{{"daemon-reload"}, {"enable", workerService}, {"restart", workerService}} {
		if err := ctl(args...); err != nil {
			return errors.Join(err, fmt.Errorf("the unit is in %s: fix what systemctl says, then systemctl restart %s", unit, workerService))
		}
	}
	fmt.Fprintf(out, "ok    %-14s %s enabled and started\n", "service", workerService)
	return nil
}
