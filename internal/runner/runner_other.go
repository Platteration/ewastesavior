//go:build !linux

package runner

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/platteration/ewastesavior/internal/proto"
)

// sysState is empty outside Linux.
type sysState struct{}

func (r *Runner) init() error { return ErrUnsupported }

// SetCPULimit is not supported outside Linux.
func (r *Runner) SetCPULimit(cores float64) error { return ErrUnsupported }

// Run is not supported outside Linux; New never returns a Runner there.
func (r *Runner) Run(ctx context.Context, t proto.Task, logs io.Writer, progress func(proto.RunningTask)) proto.TaskReport {
	return proto.TaskReport{Lease: t.Lease, State: proto.TaskFailed, ErrorKind: proto.ErrInternal, Error: ErrUnsupported.Error()}
}

// SandboxExecMain is the `savior sandbox-exec` shim; it only works on Linux.
func SandboxExecMain(args []string) int {
	fmt.Fprintln(os.Stderr, "savior sandbox-exec: only supported on Linux")
	return shimFailCode
}
