package node

import (
	"os"
	"testing"

	"github.com/platteration/ewastesavior/internal/runner"
)

// TestMain lets the test binary act as the sandbox shim: the runner
// re-executes its own binary as "<exe> sandbox-exec ...".
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "sandbox-exec" {
		os.Exit(runner.SandboxExecMain(os.Args[2:]))
	}
	os.Exit(m.Run())
}
