package ctl

import (
	"bytes"
	"context"
	"debug/elf"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/platteration/ewastesavior/internal/proto"
)

// multiFlag collects a repeatable string flag.
type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

// optInt is an int flag that remembers whether it was set.
type optInt struct {
	v   int
	set bool
}

func (o *optInt) String() string { return strconv.Itoa(o.v) }
func (o *optInt) Set(s string) error {
	n, err := strconv.Atoi(s)
	if err != nil {
		return errors.New("not an integer")
	}
	o.v, o.set = n, true
	return nil
}

// secondsFlag accepts whole seconds ("90") or a Go duration ("1h30m").
type secondsFlag int

func (s *secondsFlag) String() string { return strconv.Itoa(int(*s)) }
func (s *secondsFlag) Set(v string) error {
	if n, err := strconv.Atoi(v); err == nil {
		*s = secondsFlag(n)
		return nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d%time.Second != 0 {
		return errors.New("want whole seconds or a duration like 90s or 2h")
	}
	*s = secondsFlag(d / time.Second)
	return nil
}

// jobFlags are the flags of run and script.
type jobFlags struct {
	name      string
	count     int
	cores     float64
	mem, disk int
	timeout   secondsFlag
	retries   optInt
	arch      string
	cpuFlags  string
	minMem    int
	labels    multiFlag
	nodes     multiFlag
	network   bool
	isolation string
	inputs    multiFlag
	inputURLs multiFlag
	outputs   multiFlag
	env       multiFlag
	priority  int
	wait      bool
	fetch     string
}

func (jf *jobFlags) register(f *flags) {
	f.StringVar(&jf.name, "name", "", "job name")
	f.IntVar(&jf.count, "count", 1, "number of tasks; each gets SAVIOR_TASK_INDEX and {{index}}/{{count}} are expanded")
	f.Float64Var(&jf.cores, "cores", 0, "cores per task (default 1)")
	f.IntVar(&jf.mem, "mem", 0, "memory per task in MB (default 128)")
	f.IntVar(&jf.disk, "disk", 0, "scratch disk per task in MB (default 64)")
	f.Var(&jf.timeout, "timeout", "task time limit: seconds or a duration like 2h (default 1h)")
	f.Var(&jf.retries, "retries", "retries after a failed attempt (default 1)")
	f.StringVar(&jf.arch, "arch", "", "allowed architectures, e.g. amd64,386 (default: those of ELF programs among the inputs; any = no limit)")
	f.StringVar(&jf.cpuFlags, "cpu-flags", "", "required CPU flags, e.g. sse2,avx")
	f.IntVar(&jf.minMem, "min-mem", 0, "only nodes with at least this much RAM (MB)")
	f.Var(&jf.labels, "label", "only nodes with label k=v (repeatable)")
	f.Var(&jf.nodes, "node", "only these nodes, by name or ID (repeatable or comma-separated)")
	f.BoolVar(&jf.network, "network", false, "give tasks network access")
	f.StringVar(&jf.isolation, "isolation", "", "full (default: only fully sandboxed nodes) or any")
	f.Var(&jf.inputs, "input", "local FILE[:NAME] to upload and place in the work directory (repeatable)")
	f.Var(&jf.inputURLs, "input-url", "URL:SHA256:SIZE:NAME downloaded by the node (repeatable)")
	f.Var(&jf.outputs, "output", "glob of result files to collect, e.g. 'out/*.txt' (repeatable)")
	f.Var(&jf.env, "env", "K=V environment variable, or K to copy it from here (repeatable)")
	f.IntVar(&jf.priority, "priority", 0, "queue priority -1000..1000 (higher first)")
	f.BoolVar(&jf.wait, "wait", false, "wait until the job finishes; exit non-zero if it fails")
	f.StringVar(&jf.fetch, "fetch", "", "after success, download outputs into DIR (implies --wait)")
}

// localInput is a file to upload before submitting.
type localInput struct {
	file string
	sha  string
	size int64
}

// buildJob turns job flags plus a command or script into a spec. Local
// inputs are hashed (not yet uploaded); the spec is validated with
// defaults applied, so mistakes surface before any upload.
func (a *app) buildJob(jf *jobFlags, command []string, script string) (proto.JobSpec, []localInput, error) {
	spec := proto.JobSpec{
		Name:      jf.name,
		Command:   command,
		Script:    script,
		Count:     jf.count,
		Resources: proto.Resources{Cores: jf.cores, MemMB: jf.mem, DiskMB: jf.disk},
		TimeoutS:  int(jf.timeout),
		Network:   jf.network,
		Priority:  jf.priority,
	}
	if len(command) > 0 {
		spec.Kind = proto.KindExec
	} else {
		spec.Kind = proto.KindScript
	}
	if jf.retries.set {
		r := jf.retries.v
		spec.Retries = &r
	}
	arch := splitList(jf.arch)
	if len(arch) == 1 && arch[0] == "any" {
		arch = nil
	}
	spec.Requirements = proto.Requirements{
		Arch:      arch,
		CPUFlags:  splitList(jf.cpuFlags),
		MinMemMB:  jf.minMem,
		Isolation: jf.isolation,
	}
	for _, n := range jf.nodes {
		for _, ref := range splitList(n) {
			if err := checkRef("node", ref); err != nil {
				return spec, nil, err
			}
			spec.Requirements.Nodes = append(spec.Requirements.Nodes, ref)
		}
	}
	for _, l := range jf.labels {
		k, v, ok := strings.Cut(l, "=")
		if !ok {
			return spec, nil, fmt.Errorf("--label %q: want k=v", sanitizeCell(l))
		}
		if err := checkLabel(k, v); err != nil {
			return spec, nil, err
		}
		if spec.Requirements.Labels == nil {
			spec.Requirements.Labels = map[string]string{}
		}
		spec.Requirements.Labels[k] = v
	}
	for _, e := range jf.env {
		k, v, ok := strings.Cut(e, "=")
		if !ok {
			if v = a.getenv(k); v == "" {
				return spec, nil, fmt.Errorf("--env %s: not set here; use --env %s=VALUE", sanitizeCell(k), sanitizeCell(k))
			}
		}
		if spec.Env == nil {
			spec.Env = map[string]string{}
		}
		spec.Env[k] = v
	}
	spec.Outputs = append(spec.Outputs, jf.outputs...)
	var locals []localInput
	names := map[string]bool{}
	var localBytes int64
	for _, in := range jf.inputs {
		file, name, err := splitInput(in)
		if err != nil {
			return spec, nil, err
		}
		sha, size, exec, err := hashLocalFile(file)
		if err != nil {
			return spec, nil, err
		}
		if names[name] {
			return spec, nil, fmt.Errorf("two inputs are named %q", name)
		}
		names[name] = true
		if len(command) > 0 && strings.TrimPrefix(command[0], "./") == name {
			exec = true
		}
		spec.Inputs = append(spec.Inputs, proto.Input{Name: name, Blob: sha, Executable: exec})
		locals = append(locals, localInput{file: file, sha: sha, size: size})
		localBytes += size
	}
	if jf.arch == "" {
		// Programs shipped as inputs only run on their own architecture
		// (SaviorOS's 64-bit kernel has no 32-bit emulation).
		for _, l := range locals {
			if ar := elfArch(l.file); ar != "" && !slices.Contains(spec.Requirements.Arch, ar) {
				spec.Requirements.Arch = append(spec.Requirements.Arch, ar)
			}
		}
		if len(spec.Requirements.Arch) > 0 {
			sort.Strings(spec.Requirements.Arch)
			fmt.Fprintf(a.stderr, "note: the inputs contain %s programs, so tasks run only on %s nodes (--arch any overrides)\n",
				strings.Join(spec.Requirements.Arch, " and "), strings.Join(spec.Requirements.Arch, " or "))
		}
	}
	for _, s := range jf.inputURLs {
		in, err := parseInputURL(s)
		if err != nil {
			return spec, nil, err
		}
		if names[in.Name] {
			return spec, nil, fmt.Errorf("two inputs are named %q", in.Name)
		}
		names[in.Name] = true
		spec.Inputs = append(spec.Inputs, in)
	}
	check := spec
	proto.ApplyJobDefaults(&check)
	if err := proto.ValidateJobSpec(&check); err != nil {
		return spec, nil, err
	}
	var urlBytes int64
	for _, in := range check.Inputs {
		urlBytes += in.Size
	}
	if total := localBytes + urlBytes; check.Resources.DiskMB > 0 && total > int64(check.Resources.DiskMB)<<20 {
		return spec, nil, fmt.Errorf("the inputs need %s but each task gets %d MB of scratch disk; raise --disk",
			fmtBytes(total), check.Resources.DiskMB)
	}
	return spec, locals, nil
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// splitInput parses FILE[:NAME]. An existing file name containing colons
// (C:\data\x) is taken whole; otherwise NAME follows the last colon.
func splitInput(s string) (file, name string, err error) {
	file = s
	if _, statErr := os.Stat(s); statErr != nil {
		if i := strings.LastIndexByte(s, ':'); i > 0 {
			file, name = s[:i], s[i+1:]
		}
	}
	if name == "" {
		name = filepath.Base(file)
	}
	if !proto.ValidRelPath(name) {
		return "", "", fmt.Errorf("--input %s: %q is not a valid input name (a relative path without .., \\ or :)", sanitizeCell(s), sanitizeCell(name))
	}
	return file, name, nil
}

// hashLocalFile returns a regular file's SHA-256, size and whether it has an
// execute bit.
func hashLocalFile(name string) (string, int64, bool, error) {
	f, err := os.Open(name)
	if err != nil {
		return "", 0, false, fmt.Errorf("--input: %w", err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", 0, false, err
	}
	if !st.Mode().IsRegular() {
		return "", 0, false, fmt.Errorf("--input %s is not a regular file", name)
	}
	sha, size, err := hashReader(context.Background(), f)
	if err != nil {
		return "", 0, false, fmt.Errorf("--input %s: %w", name, err)
	}
	return sha, size, st.Mode().Perm()&0o111 != 0, nil
}

// elfArch returns the GOARCH name of the machine an ELF program or shared
// library is built for, or "" when name is not one (or is for a machine
// without a name here).
func elfArch(name string) string {
	f, err := elf.Open(name)
	if err != nil {
		return ""
	}
	defer f.Close()
	if f.Type != elf.ET_EXEC && f.Type != elf.ET_DYN {
		return ""
	}
	switch {
	case f.Machine == elf.EM_X86_64 && f.Class == elf.ELFCLASS64:
		return "amd64"
	case f.Machine == elf.EM_386:
		return "386"
	case f.Machine == elf.EM_AARCH64:
		return "arm64"
	case f.Machine == elf.EM_ARM:
		return "arm"
	case f.Machine == elf.EM_RISCV && f.Class == elf.ELFCLASS64:
		return "riscv64"
	}
	return ""
}

// parseInputURL parses URL:SHA256:SIZE:NAME (the URL may contain colons).
func parseInputURL(s string) (proto.Input, error) {
	bad := fmt.Errorf("--input-url %q: want URL:SHA256:SIZE:NAME", truncate(sanitizeCell(s), 80))
	parts := strings.Split(s, ":")
	if len(parts) < 5 {
		return proto.Input{}, bad
	}
	n := len(parts)
	name, sizeS, sha := parts[n-1], parts[n-2], strings.ToLower(parts[n-3])
	u := strings.Join(parts[:n-3], ":")
	size, err := strconv.ParseInt(sizeS, 10, 64)
	if err != nil || size <= 0 || !proto.ValidSHA256(sha) || name == "" {
		return proto.Input{}, bad
	}
	return proto.Input{Name: name, URL: u, SHA256: sha, Size: size}, nil
}

func cmdRun(a *app, ctx context.Context, args []string) int {
	f := a.flagSet("run", "[flags] -- command args...", `Run a command on the swarm (count tasks). Flags go before the command.
Examples:
  savior ctl run --count 10 --input render.py --output 'frame-*.png' -- python3 render.py {{index}}
  savior ctl run --wait --cores 2 --mem 512 -- sh -c 'nproc; uname -a'
Here --timeout is the task time limit; give the request timeout before "run".`, "timeout")
	var jf jobFlags
	jf.register(f)
	command, code, ok := a.parseCommand(f, args)
	if !ok {
		return code
	}
	if len(command) == 0 {
		return a.usageError("no command given (put it after --)")
	}
	return a.submitBuilt(ctx, &jf, command, "")
}

func cmdScript(a *app, ctx context.Context, args []string) int {
	f := a.flagSet("script", "FILE [flags]", "Run a /bin/sh script on the swarm (FILE - reads standard input). Takes the same flags as run.", "timeout")
	var jf jobFlags
	jf.register(f)
	pos, code, ok := a.parse(f, args, 1, 1)
	if !ok {
		return code
	}
	var r io.Reader = a.stdin
	if pos[0] != "-" {
		file, err := os.Open(pos[0])
		if err != nil {
			return a.fail(err)
		}
		defer file.Close()
		r = file
	}
	b, err := io.ReadAll(io.LimitReader(r, 1<<20+1))
	if err != nil {
		return a.fail(err)
	}
	if len(b) > 1<<20 {
		return a.fail(errors.New("the script is larger than 1 MiB; ship big files as --input"))
	}
	return a.submitBuilt(ctx, &jf, nil, string(b))
}

func (a *app) submitBuilt(ctx context.Context, jf *jobFlags, command []string, script string) int {
	spec, locals, err := a.buildJob(jf, command, script)
	if err != nil {
		return a.usageError("%v", err)
	}
	c, err := a.connect(ctx)
	if err != nil {
		return a.fail(err)
	}
	done := map[string]bool{}
	for _, in := range locals {
		if done[in.sha] {
			continue
		}
		f, err := os.Open(in.file)
		if err != nil {
			return a.fail(err)
		}
		_, err = c.PutBlob(ctx, in.sha, in.size, f)
		f.Close()
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return a.fail(err)
			}
			return a.fail(fmt.Errorf("upload %s: %w (did the file change?)", in.file, err))
		}
		done[in.sha] = true
		fmt.Fprintf(a.stderr, "uploaded %s (%s)\n", in.file, fmtBytes(in.size))
	}
	jd, err := c.SubmitJob(ctx, spec)
	if err != nil {
		return a.fail(err)
	}
	return a.afterSubmit(ctx, c, jd, jf.wait || jf.fetch != "", jf.fetch)
}

// afterSubmit prints the new job and optionally waits for it.
func (a *app) afterSubmit(ctx context.Context, c *Client, jd proto.JobDetail, wait bool, fetch string) int {
	if !wait {
		if a.g.json {
			return a.printJSON(jd)
		}
		fmt.Fprintf(a.stdout, "Submitted job %s (%d tasks).\n", sanitizeCell(jd.ID), jd.Count)
		if jd.Warning != "" {
			a.warn("%s", sanitizeCell(jd.Warning))
		}
		fmt.Fprintf(a.stdout, "Follow it with: savior ctl wait %s\n", sanitizeCell(jd.ID))
		return 0
	}
	if !a.g.json {
		fmt.Fprintf(a.stderr, "Submitted job %s (%d tasks).\n", sanitizeCell(jd.ID), jd.Count)
	}
	return a.waitJob(ctx, c, jd.ID, fetch)
}

func cmdSubmit(a *app, ctx context.Context, args []string) int {
	f := a.flagSet("submit", "JOB.json [--wait] [--fetch DIR]", "Submit a JobSpec in JSON (JOB.json - reads standard input). Inputs must be blobs or URLs.")
	wait := f.Bool("wait", false, "wait until the job finishes")
	fetch := f.String("fetch", "", "after success, download outputs into DIR (implies --wait)")
	pos, code, ok := a.parse(f, args, 1, 1)
	if !ok {
		return code
	}
	var r io.Reader = a.stdin
	if pos[0] != "-" {
		file, err := os.Open(pos[0])
		if err != nil {
			return a.fail(err)
		}
		defer file.Close()
		r = file
	}
	b, err := io.ReadAll(io.LimitReader(r, 4<<20+1))
	if err != nil {
		return a.fail(err)
	}
	if len(b) > 4<<20 {
		return a.fail(errors.New("job file larger than 4 MiB"))
	}
	var spec proto.JobSpec
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&spec); err != nil {
		return a.usageError("%s: %v", pos[0], err)
	}
	check := spec
	proto.ApplyJobDefaults(&check)
	if err := proto.ValidateJobSpec(&check); err != nil {
		return a.usageError("%s: %v", pos[0], err)
	}
	c, err := a.connect(ctx)
	if err != nil {
		return a.fail(err)
	}
	jd, err := c.SubmitJob(ctx, spec)
	if err != nil {
		return a.fail(err)
	}
	return a.afterSubmit(ctx, c, jd, *wait || *fetch != "", *fetch)
}

func cmdWait(a *app, ctx context.Context, args []string) int {
	f := a.flagSet("wait", "<job> [--fetch DIR]", "Wait until a job finishes. Exits 0 if it succeeded, 1 otherwise.")
	fetch := f.String("fetch", "", "after success, download outputs into DIR")
	pos, code, ok := a.parse(f, args, 1, 1)
	if !ok {
		return code
	}
	c, err := a.connect(ctx)
	if err != nil {
		return a.fail(err)
	}
	return a.waitJob(ctx, c, pos[0], *fetch)
}

// waitJob polls until the job is terminal, shows progress on stderr,
// explains failures and optionally downloads the outputs. With --json it
// prints the final job (plus the download result when fetching) as one
// document.
func (a *app) waitJob(ctx context.Context, c *Client, id, fetch string) int {
	p := &progressLine{w: a.stderr, tty: a.stderrTTY}
	jd, err := c.WaitJob(ctx, id, a.pollInterval, p.update)
	p.done()
	if err != nil {
		if ctx.Err() != nil {
			fmt.Fprintf(a.stderr, "Stopped waiting; the job keeps running. Check it with: savior ctl job %s\n", sanitizeCell(id))
			return 130
		}
		return a.fail(err)
	}
	code := 0
	switch jd.State {
	case proto.JobSucceeded:
		if !a.g.json {
			fmt.Fprintf(a.stdout, "Job %s succeeded (%d tasks).\n", sanitizeCell(jd.ID), jd.Count)
		}
	case proto.JobFailed:
		fmt.Fprintf(a.stderr, "Job %s failed: %d of %d tasks failed.\n", sanitizeCell(jd.ID), jd.Counts.Failed, jd.Count)
		a.reportFailures(ctx, c, jd.ID)
		code = 1
	default:
		fmt.Fprintf(a.stderr, "Job %s was %s.\n", sanitizeCell(jd.ID), sanitizeCell(string(jd.State)))
		code = 1
	}
	if code != 0 || fetch == "" {
		if a.g.json {
			if rc := a.printJSON(jd); rc != 0 {
				return rc
			}
		}
		return code
	}
	res, rc := a.fetchOutputs(ctx, c, jd.ID, fetch)
	if a.g.json && res != nil {
		if jrc := a.printJSON(struct {
			Job      proto.JobDetail `json:"job"`
			Download *DownloadResult `json:"download"`
		}{jd, res}); jrc != 0 {
			return jrc
		}
	}
	return rc
}

// progressLine shows one updating status line on a terminal, or a line per
// change otherwise.
type progressLine struct {
	w     io.Writer
	tty   bool
	last  string
	width int
}

func (p *progressLine) update(jd proto.JobDetail) {
	n := jd.Counts
	line := fmt.Sprintf("job %s %s: %d/%d succeeded, %d running, %d pending, %d failed",
		sanitizeCell(jd.ID), sanitizeCell(string(jd.State)), n.Succeeded, jd.Count, n.Running+n.Assigned, n.Pending, n.Failed)
	if line == p.last {
		return
	}
	p.last = line
	if !p.tty {
		fmt.Fprintln(p.w, line)
		return
	}
	pad := ""
	if len(line) < p.width {
		pad = strings.Repeat(" ", p.width-len(line))
	}
	fmt.Fprintf(p.w, "\r%s%s", line, pad)
	p.width = max(p.width, len(line))
}

func (p *progressLine) done() {
	if p.tty && p.width > 0 {
		fmt.Fprintf(p.w, "\r%s\r", strings.Repeat(" ", p.width))
	}
}

// reportFailures prints the errors and last log lines of a job's failed
// tasks.
func (a *app) reportFailures(ctx context.Context, c *Client, jobID string) {
	const maxShown, logLines = 5, 10
	page, err := c.Tasks(ctx, jobID, TasksQuery{State: proto.TaskFailed, Limit: maxShown})
	if err != nil {
		a.warn("could not list the failed tasks: %s", sanitize(err.Error()))
		return
	}
	for _, t := range page.Tasks {
		fmt.Fprintf(a.stderr, "\nTask %s (index %d) failed on %s after %d attempt(s): %s\n",
			sanitizeCell(t.ID), t.Index, sanitizeCell(orDash(t.Node)), t.Attempt, sanitizeCell(taskError(t)))
		data, _, err := c.Logs(ctx, t.ID, 0, 0)
		if err != nil {
			a.warn("could not read its log: %s", sanitize(err.Error()))
			continue
		}
		lines := lastLines(data, logLines)
		if len(lines) == 0 {
			fmt.Fprintln(a.stderr, "  (no output)")
			continue
		}
		fmt.Fprintf(a.stderr, "  last %d lines of output:\n", len(lines))
		for _, l := range lines {
			fmt.Fprintf(a.stderr, "  | %s\n", sanitizeCell(l))
		}
	}
	if page.NextOffset != 0 {
		fmt.Fprintf(a.stderr, "\nMore failed tasks: savior ctl tasks %s --state failed\n", sanitizeCell(jobID))
	}
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// taskError describes why a task failed.
func taskError(t proto.TaskView) string {
	var parts []string
	if t.ErrorKind != "" {
		parts = append(parts, t.ErrorKind)
	}
	if t.ExitCode != nil && (t.ErrorKind == "" || t.ErrorKind == proto.ErrExit) {
		parts = append(parts, fmt.Sprintf("exit code %d", *t.ExitCode))
	}
	if t.Error != "" {
		parts = append(parts, t.Error)
	}
	if len(parts) == 0 {
		return "unknown error"
	}
	return strings.Join(parts, ": ")
}

// lastLines returns up to n final lines of data.
func lastLines(data []byte, n int) []string {
	s := strings.TrimRight(string(data), "\n")
	if s == "" {
		return nil
	}
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, "\r")
	}
	return lines
}

func cmdJobs(a *app, ctx context.Context, args []string) int {
	f := a.flagSet("jobs", "[--state S] [--limit N] [--before SEQ]", "List jobs, newest first.")
	state := f.String("state", "", "only jobs in this state: queued, running, succeeded, failed, canceled")
	limit := f.Int("limit", 50, "at most this many jobs")
	before := f.Uint64("before", 0, "only jobs submitted before this sequence number (paging)")
	if _, code, ok := a.parse(f, args, 0, 0); !ok {
		return code
	}
	switch proto.JobState(*state) {
	case "", proto.JobQueued, proto.JobRunning, proto.JobSucceeded, proto.JobFailed, proto.JobCanceled:
	default:
		return a.usageError("unknown state %q", sanitizeCell(*state))
	}
	if *limit < 1 {
		return a.usageError("--limit must be positive")
	}
	c, err := a.connect(ctx)
	if err != nil {
		return a.fail(err)
	}
	jobs, err := c.Jobs(ctx, JobsQuery{State: proto.JobState(*state), Limit: *limit, Before: *before})
	if err != nil {
		return a.fail(err)
	}
	if a.g.json {
		return a.printJSON(jobs)
	}
	now := a.now()
	t := newTable(a.stdout, "ID", "SEQ", "NAME", "STATE", "DONE", "RUNNING", "PENDING", "FAILED", "SUBMITTED", "WARNING")
	for _, j := range jobs {
		n := j.Counts
		t.row(j.ID, strconv.FormatUint(j.Seq, 10), j.Name, string(j.State), fmt.Sprintf("%d/%d", n.Succeeded, j.Count),
			strconv.Itoa(n.Running+n.Assigned), strconv.Itoa(n.Pending), strconv.Itoa(n.Failed),
			fmtAge(j.CreatedAt, now), truncate(j.Warning, 50))
	}
	t.flush()
	return 0
}

func cmdJob(a *app, ctx context.Context, args []string) int {
	f := a.flagSet("job", "<id>", "Show a job and its normalized spec.")
	pos, code, ok := a.parse(f, args, 1, 1)
	if !ok {
		return code
	}
	c, err := a.connect(ctx)
	if err != nil {
		return a.fail(err)
	}
	jd, err := c.Job(ctx, pos[0])
	if err != nil {
		return a.fail(err)
	}
	if a.g.json {
		return a.printJSON(jd)
	}
	s := jd.Spec
	n := jd.Counts
	k := newKV(a.stdout)
	k.add("Job", strings.TrimSpace(fmt.Sprintf("%s (seq %d) %s", jd.ID, jd.Seq, jd.Name)))
	k.add("State", fmt.Sprintf("%s: %d/%d succeeded, %d running, %d pending, %d failed, %d canceled",
		jd.State, n.Succeeded, jd.Count, n.Running+n.Assigned, n.Pending, n.Failed, n.Canceled))
	k.add("Submitted", fmtTime(jd.CreatedAt))
	if jd.StartedAt != nil {
		k.add("Started", fmtTime(*jd.StartedAt))
	}
	if jd.FinishedAt != nil {
		k.add("Finished", fmtTime(*jd.FinishedAt))
	}
	if s.Kind == proto.KindScript {
		first, _, _ := strings.Cut(strings.TrimSpace(s.Script), "\n")
		k.add("Script", truncate(first, 80)+fmt.Sprintf(" (%d bytes)", len(s.Script)))
	} else {
		k.add("Command", truncate(strings.Join(s.Command, " "), 200))
	}
	retries := proto.DefaultRetries
	if s.Retries != nil {
		retries = *s.Retries
	}
	k.add("Resources", fmt.Sprintf("%s core(s), %d MB memory, %d MB disk, timeout %s, %d retries, priority %d",
		fmtCores(s.Resources.Cores), s.Resources.MemMB, s.Resources.DiskMB,
		fmtDuration(time.Duration(s.TimeoutS)*time.Second), retries, s.Priority))
	q := s.Requirements
	var req []string
	if len(q.Arch) > 0 {
		req = append(req, "arch "+strings.Join(q.Arch, ","))
	}
	if q.MinMemMB > 0 {
		req = append(req, fmt.Sprintf("min %d MB RAM", q.MinMemMB))
	}
	if len(q.CPUFlags) > 0 {
		req = append(req, "cpu "+strings.Join(q.CPUFlags, ","))
	}
	if len(q.Labels) > 0 {
		req = append(req, "labels "+sortedKV(q.Labels))
	}
	if len(q.Nodes) > 0 {
		req = append(req, "nodes "+strings.Join(q.Nodes, ","))
	}
	if q.Isolation != "" {
		req = append(req, "isolation "+q.Isolation)
	}
	if s.Network {
		req = append(req, "network")
	}
	k.add("Requirements", strings.Join(req, "; "))
	if len(s.Inputs) > 0 {
		names := make([]string, len(s.Inputs))
		for i, in := range s.Inputs {
			names[i] = in.Name
		}
		k.add("Inputs", truncate(strings.Join(names, " "), 200))
	}
	if len(s.Outputs) > 0 {
		k.add("Outputs", strings.Join(s.Outputs, " "))
	}
	if len(s.Env) > 0 {
		keys := make([]string, 0, len(s.Env))
		for key := range s.Env {
			keys = append(keys, key)
		}
		slices.Sort(keys)
		k.add("Env", truncate(strings.Join(keys, " "), 200))
	}
	k.add("Warning", jd.Warning)
	k.flush()
	return 0
}

func cmdTasks(a *app, ctx context.Context, args []string) int {
	f := a.flagSet("tasks", "<job> [--state S] [--limit N] [--offset N]", "List a job's tasks. Tasks not dispatched yet have no record.")
	state := f.String("state", "", "only tasks in this state: pending, assigned, running, succeeded, failed, canceled")
	limit := f.Int("limit", 100, "at most this many tasks (max 1000)")
	offset := f.Int("offset", 0, "skip this many tasks (paging)")
	pos, code, ok := a.parse(f, args, 1, 1)
	if !ok {
		return code
	}
	switch proto.TaskState(*state) {
	case "", proto.TaskPending, proto.TaskAssigned, proto.TaskRunning, proto.TaskSucceeded, proto.TaskFailed, proto.TaskCanceled:
	default:
		return a.usageError("unknown state %q", sanitizeCell(*state))
	}
	if *limit < 1 || *limit > 1000 || *offset < 0 {
		return a.usageError("--limit must be 1..1000 and --offset non-negative")
	}
	c, err := a.connect(ctx)
	if err != nil {
		return a.fail(err)
	}
	page, err := c.Tasks(ctx, pos[0], TasksQuery{State: proto.TaskState(*state), Limit: *limit, Offset: *offset})
	if err != nil {
		return a.fail(err)
	}
	if a.g.json {
		return a.printJSON(page)
	}
	t := newTable(a.stdout, "TASK", "INDEX", "STATE", "NODE", "ATTEMPT", "EXIT", "RUN", "INFO")
	for _, tv := range page.Tasks {
		exit := ""
		if tv.ExitCode != nil {
			exit = strconv.Itoa(*tv.ExitCode)
		}
		run := ""
		if tv.RunS > 0 {
			run = fmtDuration(time.Duration(tv.RunS * float64(time.Second)))
		}
		info := tv.WaitReason
		if tv.State == proto.TaskFailed {
			info = taskError(tv)
		}
		t.row(tv.ID, strconv.Itoa(tv.Index), string(tv.State), tv.Node, strconv.Itoa(tv.Attempt), exit, run, truncate(info, 60))
	}
	t.flush()
	if page.Undispatched > 0 {
		fmt.Fprintf(a.stderr, "%d task(s) not dispatched yet.\n", page.Undispatched)
	}
	if page.NextOffset > 0 {
		fmt.Fprintf(a.stderr, "More: savior ctl tasks %s --offset %d\n", sanitizeCell(pos[0]), page.NextOffset)
	}
	return 0
}

func cmdTask(a *app, ctx context.Context, args []string) int {
	f := a.flagSet("task", "<task>", "Show a task, including its attempt history.")
	pos, code, ok := a.parse(f, args, 1, 1)
	if !ok {
		return code
	}
	c, err := a.connect(ctx)
	if err != nil {
		return a.fail(err)
	}
	tv, err := c.Task(ctx, pos[0])
	if err != nil {
		return a.fail(err)
	}
	if a.g.json {
		return a.printJSON(tv)
	}
	k := newKV(a.stdout)
	k.add("Task", fmt.Sprintf("%s (job %s, index %d)", tv.ID, tv.JobID, tv.Index))
	k.add("State", string(tv.State))
	k.add("Node", tv.Node)
	k.add("Attempts", fmt.Sprintf("%d dispatches, %d failures, %d node errors, %d interruptions",
		tv.Attempt, tv.Failures, tv.NodeErrors, tv.Interruptions))
	if tv.State == proto.TaskFailed || tv.Error != "" {
		k.add("Error", taskError(tv))
	}
	k.add("Waiting", tv.WaitReason)
	if tv.RunS > 0 || tv.CPUSeconds > 0 {
		k.add("Usage", fmt.Sprintf("ran %.1fs, %.1f cpu-s, peak %d MB", tv.RunS, tv.CPUSeconds, tv.MaxMemMB))
	}
	if len(tv.FailedNodes) > 0 {
		k.add("Failed on", strings.Join(tv.FailedNodes, " "))
	}
	for _, o := range tv.Outputs {
		k.add("Output", fmt.Sprintf("%s (%s)", o.Name, fmtBytes(o.Size)))
	}
	k.flush()
	if len(tv.History) > 0 {
		fmt.Fprintln(a.stdout, "\nHistory:")
		t := newTable(a.stdout, "ATTEMPT", "NODE", "ASSIGNED", "OUTCOME", "EXIT", "ERROR")
		for _, h := range tv.History {
			t.row(strconv.Itoa(h.Attempt), h.Node, fmtTime(h.AssignedAt), h.Outcome, strconv.Itoa(h.ExitCode), truncate(h.Error, 60))
		}
		t.flush()
	}
	return 0
}

func cmdCancel(a *app, ctx context.Context, args []string) int {
	f := a.flagSet("cancel", "<job>", "Cancel a job: pending tasks are dropped and running ones stopped.")
	pos, code, ok := a.parse(f, args, 1, 1)
	if !ok {
		return code
	}
	c, err := a.connect(ctx)
	if err != nil {
		return a.fail(err)
	}
	jv, err := c.Cancel(ctx, pos[0])
	if err != nil {
		return a.fail(err)
	}
	if a.g.json {
		return a.printJSON(jv)
	}
	fmt.Fprintf(a.stdout, "Job %s is %s.\n", sanitizeCell(jv.ID), sanitizeCell(string(jv.State)))
	return 0
}

func cmdRm(a *app, ctx context.Context, args []string) int {
	f := a.flagSet("rm", "<job>", "Delete a finished job, its task records and logs.")
	pos, code, ok := a.parse(f, args, 1, 1)
	if !ok {
		return code
	}
	c, err := a.connect(ctx)
	if err != nil {
		return a.fail(err)
	}
	if err := c.DeleteJob(ctx, pos[0]); err != nil {
		return a.fail(err)
	}
	if !a.g.json {
		fmt.Fprintf(a.stdout, "Deleted job %s.\n", sanitizeCell(pos[0]))
	}
	return 0
}
