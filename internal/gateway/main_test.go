package gateway_test

import (
	"testing"

	"github.com/nesty92/fleetci/internal/leakcheck"
)

func TestMain(m *testing.M) { leakcheck.Main(m) }
