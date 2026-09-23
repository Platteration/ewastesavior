package runner

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/platteration/ewastesavior/internal/proto"
)

// Fixed parts of the task environment (DESIGN 12 step 5).
const (
	taskPath   = "/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"
	scriptName = ".savior-script"
	tmpDirName = ".savior-tmp" // TMPDIR when the task has no private /tmp
)

var (
	taskIDRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)
	envKeyRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)
)

// taskDirName is the workdir (and cgroup suffix) name "<task_id>.<attempt>".
func taskDirName(t proto.Task) string { return t.ID + "." + strconv.Itoa(t.Attempt) }

// validateTask checks the fields the runner relies on. It returns the
// ErrorKind to report and a reason.
func validateTask(t proto.Task) (kind, reason string) {
	switch {
	case !taskIDRE.MatchString(t.ID):
		return proto.ErrInternal, fmt.Sprintf("invalid task id %q", t.ID)
	case t.Lease == "":
		return proto.ErrInternal, "task has no lease"
	case t.Attempt < 0 || t.Attempt > 1<<20:
		return proto.ErrInternal, fmt.Sprintf("invalid attempt %d", t.Attempt)
	case t.TimeoutS < 1 || t.TimeoutS > proto.MaxTimeoutS:
		return proto.ErrInternal, fmt.Sprintf("invalid timeout_s %d", t.TimeoutS)
	case t.Resources.Cores <= 0 || t.Resources.MemMB < 1 || t.Resources.DiskMB < 0:
		return proto.ErrInternal, "invalid resources"
	}
	switch t.Kind {
	case proto.KindExec:
		if len(t.Command) == 0 || t.Command[0] == "" {
			return proto.ErrInternal, "exec task without a command"
		}
		for _, a := range t.Command {
			if strings.IndexByte(a, 0) >= 0 {
				return proto.ErrInternal, "command argument contains NUL"
			}
		}
	case proto.KindScript:
		if strings.TrimSpace(t.Script) == "" {
			return proto.ErrInternal, "script task without a script"
		}
	default:
		return proto.ErrInternal, fmt.Sprintf("unknown task kind %q", t.Kind)
	}
	if len(t.Inputs) > proto.MaxInputs {
		return proto.ErrInput, "too many inputs"
	}
	seen := map[string]bool{}
	for _, in := range t.Inputs {
		if !proto.ValidRelPath(in.Name) || seen[in.Name] {
			return proto.ErrInput, fmt.Sprintf("invalid or duplicate input name %q", in.Name)
		}
		seen[in.Name] = true
		switch {
		case in.Blob != "" && in.URL == "":
			if !proto.ValidSHA256(in.Blob) {
				return proto.ErrInput, fmt.Sprintf("input %q: invalid blob hash", in.Name)
			}
		case in.URL != "" && in.Blob == "":
			if !proto.ValidSHA256(in.SHA256) || in.Size <= 0 {
				return proto.ErrInput, fmt.Sprintf("input %q: url inputs need sha256 and size", in.Name)
			}
		default:
			return proto.ErrInput, fmt.Sprintf("input %q needs exactly one of blob or url", in.Name)
		}
	}
	for _, in := range t.Inputs {
		// An input nested under another input's name can't be created.
		for p := in.Name; strings.Contains(p, "/"); {
			p = p[:strings.LastIndexByte(p, '/')]
			if seen[p] {
				return proto.ErrInput, fmt.Sprintf("input %q is inside input file %q", in.Name, p)
			}
		}
	}
	if len(t.Outputs) > proto.MaxOutputs {
		return proto.ErrOutput, "too many output patterns"
	}
	for _, o := range t.Outputs {
		if !proto.ValidOutputPattern(o) {
			return proto.ErrOutput, fmt.Sprintf("invalid output pattern %q", o)
		}
	}
	return "", ""
}

// buildEnv returns the task environment, sorted: the fixed variables, then
// the spec's env (which may override them), then the SAVIOR_* variables
// derived from the task, which always win. Invalid keys are dropped.
func buildEnv(t proto.Task, home, tmp string) []string {
	env := map[string]string{
		"PATH":   taskPath,
		"HOME":   home,
		"TMPDIR": tmp,
		"LANG":   "C.UTF-8",
	}
	for k, v := range t.Env {
		if envKeyRE.MatchString(k) && strings.IndexByte(v, 0) < 0 {
			env[k] = v
		}
	}
	env["SAVIOR_TASK_ID"] = t.ID
	env["SAVIOR_JOB_ID"] = t.JobID
	env["SAVIOR_TASK_INDEX"] = strconv.Itoa(t.Index)
	env["SAVIOR_TASK_COUNT"] = strconv.Itoa(t.Count)
	env["SAVIOR_ATTEMPT"] = strconv.Itoa(t.Attempt)
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}

// taskArgv is the command line: the exec argv, or /bin/sh <script path>.
func taskArgv(t proto.Task, scriptPath string) []string {
	if t.Kind == proto.KindScript {
		return []string{"/bin/sh", scriptPath}
	}
	return append([]string(nil), t.Command...)
}

// effectiveDiskMB never returns 0: a tmpfs with size=0 has no limit.
func effectiveDiskMB(t proto.Task) int {
	if t.Resources.DiskMB < 1 {
		return 1
	}
	return t.Resources.DiskMB
}
