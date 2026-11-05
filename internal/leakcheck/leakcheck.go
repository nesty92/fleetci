// Package leakcheck fails a package's tests when they leave goroutines
// running. Each tested package calls it from its TestMain:
//
//	func TestMain(m *testing.M) { leakcheck.Main(m) }
package leakcheck

import (
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/onsi/gomega"
	"github.com/onsi/gomega/gleak"
)

// Timeout is how long the goroutines the tests started get to end.
const Timeout = time.Minute

// Main runs the tests, then waits for every goroutine they started to end. One
// still running after Timeout fails the run, with its stack.
func Main(m *testing.M) {
	before := gleak.Goroutines()
	code := m.Run()
	var leak string
	g := gomega.NewGomega(func(message string, _ ...int) { leak = message })
	g.Eventually(gleak.Goroutines).WithTimeout(Timeout).ShouldNot(gleak.HaveLeaked(before))
	if leak != "" {
		fmt.Fprintln(os.Stderr, "leakcheck: the tests left goroutines running\n"+leak)
		code = max(code, 1)
	}
	os.Exit(code)
}
