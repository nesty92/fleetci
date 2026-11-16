package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/nesty92/fleetci/internal/shell"
)

// sshCmd connects to a job's VM at the tailcat address its log gave: the
// system's ssh, through fleetci as its ProxyCommand. The address
// identifies the VM's fleetci, so ssh checks no host key.
func sshCmd(args []string) error {
	fs := flag.NewFlagSet("ssh", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "Usage: fleetci ssh [flags] [user@]<tailcat address> [ssh options] [command [args...]]")
		fs.PrintDefaults()
	}
	derp := fs.String("derp-map-url", os.Getenv("TAILCAT_DERPMAP_URL"), "where tailcat finds its relays; empty, tailcat.dev's")
	proxy := fs.Bool("proxy", false, "be ssh's ProxyCommand: join stdin and stdout to the address's SSH port")
	verbose := fs.Bool("v", false, "log tailcat's progress to stderr")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		fs.Usage()
		return errors.New("no address")
	}
	user, addr, ok := strings.Cut(fs.Arg(0), "@")
	if !ok {
		addr, user = user, ""
	}
	logf := func(string, ...any) {}
	if *verbose {
		logf = func(f string, a ...any) { fmt.Fprintf(os.Stderr, f+"\n", a...) }
	}
	if *proxy {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		dctx, cancel := context.WithTimeout(ctx, time.Minute)
		defer cancel()
		conn, err := shell.Dial(dctx, addr, *derp, logf)
		if err != nil {
			return err
		}
		defer conn.Close()
		go func() {
			_, _ = io.Copy(conn, os.Stdin)
			if cw, ok := conn.(interface{ CloseWrite() error }); ok {
				_ = cw.CloseWrite()
			}
		}()
		_, err = io.Copy(os.Stdout, conn)
		return err
	}
	sshExe, err := exec.LookPath("ssh")
	if err != nil {
		return fmt.Errorf("no ssh client on the PATH: %w", err)
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	pc := []string{shQuote(self), "ssh", "-proxy"}
	if *derp != "" {
		pc = append(pc, "-derp-map-url", shQuote(*derp))
	}
	if *verbose {
		pc = append(pc, "-v")
	}
	pc = append(pc, shQuote(addr))
	dest := "fleetci-vm"
	if user != "" {
		dest = user + "@" + dest
	}
	argv := []string{"ssh",
		"-o", "StrictHostKeyChecking no",
		"-o", "UserKnownHostsFile " + os.DevNull,
		"-o", "UpdateHostKeys no",
		"-o", "LogLevel ERROR",
		"-o", "ProxyCommand=" + strings.Join(pc, " "),
		// ssh's own options may follow the destination: -L, -t, -i.
		dest}
	argv = append(argv, fs.Args()[1:]...)
	return syscall.Exec(sshExe, argv, os.Environ())
}

// shQuote quotes s for the shell ssh runs its ProxyCommand with.
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
