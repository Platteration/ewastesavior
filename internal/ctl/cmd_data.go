package ctl

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strconv"
	"time"

	"github.com/platteration/ewastesavior/internal/proto"
)

func cmdLogs(a *app, ctx context.Context, args []string) int {
	f := a.flagSet("logs", "[-f] <task>", "Print a task's combined stdout and stderr. With -f, keep following until the task finishes.")
	var follow bool
	f.BoolVar(&follow, "f", false, "follow the log")
	f.BoolVar(&follow, "follow", false, "follow the log")
	offset := f.Int64("offset", 0, "start at this byte offset")
	pos, code, ok := a.parse(f, args, 1, 1)
	if !ok {
		return code
	}
	c, err := a.connect(ctx)
	if err != nil {
		return a.fail(err)
	}
	var out io.Writer = a.stdout
	var sw *sanitizeWriter
	if a.stdoutTTY {
		sw = &sanitizeWriter{w: a.stdout}
		out = sw
	}
	err = a.followLog(ctx, c, pos[0], *offset, follow, out)
	if sw != nil {
		_ = sw.Flush()
	}
	if err != nil {
		if follow && errors.Is(err, context.Canceled) {
			return 0 // Ctrl-C is how -f ends early
		}
		return a.fail(err)
	}
	return 0
}

// followLog copies a task log to out from offset. Without follow it stops
// when the hive has no more data; with follow it long-polls until the task
// is terminal and drained.
func (a *app) followLog(ctx context.Context, c *Client, taskID string, off int64, follow bool, out io.Writer) error {
	const wait = 25 * time.Second
	for {
		w := time.Duration(0)
		if follow {
			w = wait
		}
		start := time.Now()
		data, next, err := c.Logs(ctx, taskID, off, w)
		if err != nil {
			return err
		}
		switch {
		case next < off:
			// A new attempt started a new log stream.
			fmt.Fprintf(a.stderr, "--- log restarted (new attempt) ---\n")
			off = 0
			continue
		case next-int64(len(data)) > off:
			fmt.Fprintf(a.stderr, "--- %d bytes no longer available ---\n", next-int64(len(data))-off)
		}
		if len(data) > 0 {
			if _, err := out.Write(data); err != nil {
				return err
			}
		}
		off = next
		if len(data) > 0 {
			continue
		}
		if !follow {
			return nil
		}
		t, err := c.Task(ctx, taskID)
		if err != nil {
			return err
		}
		if t.State.Terminal() {
			// One last read picks up anything written just before the end.
			data, next, err := c.Logs(ctx, taskID, off, 0)
			if err != nil {
				return err
			}
			if next >= off && len(data) > 0 {
				if _, err := out.Write(data); err != nil {
					return err
				}
			}
			return nil
		}
		if time.Since(start) < time.Second {
			// The hive answered at once without data; don't spin.
			timer := time.NewTimer(time.Second)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
		}
	}
}

func cmdOutputs(a *app, ctx context.Context, args []string) int {
	f := a.flagSet("outputs", "<job> [-o DIR] [--zip]", `List a job's output files, or download them. With -o DIR each file is saved
as DIR/task-<index>/<name>; existing files are never overwritten and unsafe
names are refused. With --zip, one zip file is saved instead.`)
	out := f.String("o", "", "download into this directory (with --zip: the zip file, - for stdout)")
	zip := f.Bool("zip", false, "download a single zip file")
	pos, code, ok := a.parse(f, args, 1, 1)
	if !ok {
		return code
	}
	jobID := pos[0]
	if err := checkRef("job", jobID); err != nil {
		return a.usageError("%v", err)
	}
	c, err := a.connect(ctx)
	if err != nil {
		return a.fail(err)
	}
	if *zip {
		name := *out
		if name == "" {
			name = jobID + "-outputs.zip"
		}
		return a.saveStream(name, func(w io.Writer) (int64, error) { return c.OutputsZip(ctx, jobID, w) })
	}
	if *out != "" {
		return a.downloadOutputs(ctx, c, jobID, *out)
	}
	entries, err := c.Outputs(ctx, jobID)
	if err != nil {
		return a.fail(err)
	}
	if a.g.json {
		return a.printJSON(entries)
	}
	t := newTable(a.stdout, "TASK", "INDEX", "NAME", "SIZE", "SHA256")
	var total int64
	for _, e := range entries {
		t.row(e.TaskID, strconv.Itoa(e.Index), e.Name, fmtBytes(e.Size), shortHash(e.Blob))
		total += e.Size
	}
	t.flush()
	fmt.Fprintf(a.stderr, "%d file(s), %s. Download them with: savior ctl outputs %s -o DIR\n", len(entries), fmtBytes(total), sanitizeCell(jobID))
	return 0
}

// downloadOutputs implements `outputs -o DIR`.
func (a *app) downloadOutputs(ctx context.Context, c *Client, jobID, dir string) int {
	res, rc := a.fetchOutputs(ctx, c, jobID, dir)
	if a.g.json && res != nil {
		if jrc := a.printJSON(res); jrc != 0 {
			return jrc
		}
	}
	return rc
}

// fetchOutputs saves a job's outputs into dir, lists the written files
// (unless --json) and reports refusals. res is nil when nothing could be
// attempted.
func (a *app) fetchOutputs(ctx context.Context, c *Client, jobID, dir string) (*DownloadResult, int) {
	entries, err := c.Outputs(ctx, jobID)
	if err != nil {
		return nil, a.fail(err)
	}
	res, err := c.DownloadOutputs(ctx, entries, dir)
	if err != nil {
		return nil, a.fail(err)
	}
	if !a.g.json {
		for _, w := range res.Written {
			fmt.Fprintln(a.stdout, sanitizeCell(w.Path))
		}
	}
	for _, r := range res.Refused {
		fmt.Fprintf(a.stderr, "savior ctl: skipped output of task %d: %s\n", r.Entry.Index, sanitizeCell(r.Reason))
	}
	if !a.g.json {
		fmt.Fprintf(a.stderr, "Saved %d of %d output file(s) in %s.\n", len(res.Written), len(entries), dir)
	}
	if len(res.Refused) > 0 {
		return &res, 1
	}
	return &res, 0
}

// saveStream writes a download to name ("-" = stdout) with O_EXCL, and
// removes a partial file on failure.
func (a *app) saveStream(name string, get func(io.Writer) (int64, error)) int {
	if name == "-" {
		if a.stdoutTTY {
			return a.usageError("refusing to write binary data to a terminal; use -o FILE")
		}
		if _, err := get(a.stdout); err != nil {
			return a.fail(err)
		}
		return 0
	}
	f, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return a.fail(fmt.Errorf("%s already exists; not overwriting it", name))
		}
		return a.fail(err)
	}
	n, err := get(f)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(name)
		return a.fail(err)
	}
	if !a.g.json {
		fmt.Fprintf(a.stderr, "Saved %s (%s).\n", name, fmtBytes(n))
	}
	return 0
}

func cmdFetch(a *app, ctx context.Context, args []string) int {
	f := a.flagSet("fetch", "<sha256> -o FILE", "Download a blob and verify its hash (-o - writes to standard output).")
	out := f.String("o", "", "output file (required; never overwritten)")
	pos, code, ok := a.parse(f, args, 1, 1)
	if !ok {
		return code
	}
	if !proto.ValidSHA256(pos[0]) {
		return a.usageError("%q is not a sha256 (64 lowercase hex digits)", sanitizeCell(pos[0]))
	}
	if *out == "" {
		return a.usageError("-o FILE is required")
	}
	c, err := a.connect(ctx)
	if err != nil {
		return a.fail(err)
	}
	return a.saveStream(*out, func(w io.Writer) (int64, error) { return c.FetchBlob(ctx, pos[0], w) })
}

func cmdUpload(a *app, ctx context.Context, args []string) int {
	f := a.flagSet("upload", "FILE", "Upload a file as a blob and print its sha256 (for job specs and display media).")
	pos, code, ok := a.parse(f, args, 1, 1)
	if !ok {
		return code
	}
	c, err := a.connect(ctx)
	if err != nil {
		return a.fail(err)
	}
	info, err := c.UploadFile(ctx, pos[0])
	if err != nil {
		return a.fail(err)
	}
	if a.g.json {
		return a.printJSON(info)
	}
	fmt.Fprintf(a.stdout, "%s  %s  %s\n", info.SHA256, fmtBytes(info.Size), pos[0])
	return 0
}

func cmdBlobs(a *app, ctx context.Context, args []string) int {
	f := a.flagSet("blobs", "[rm <sha256>]", "List the hive's blobs, or delete one that nothing references.")
	pos, code, ok := a.parse(f, args, 0, 2)
	if !ok {
		return code
	}
	if len(pos) > 0 && (pos[0] != "rm" || len(pos) != 2) {
		return a.usageError("usage: savior ctl blobs [rm <sha256>]")
	}
	c, err := a.connect(ctx)
	if err != nil {
		return a.fail(err)
	}
	if len(pos) == 2 {
		if err := c.DeleteBlob(ctx, pos[1]); err != nil {
			return a.fail(err)
		}
		if !a.g.json {
			fmt.Fprintf(a.stdout, "Deleted blob %s.\n", pos[1])
		}
		return 0
	}
	blobs, err := c.Blobs(ctx)
	if err != nil {
		return a.fail(err)
	}
	if a.g.json {
		return a.printJSON(blobs)
	}
	now := a.now()
	t := newTable(a.stdout, "SHA256", "SIZE", "CREATED", "TOUCHED", "REFERENCED")
	var total int64
	for _, b := range blobs {
		ref := "no"
		if b.Referenced {
			ref = "yes"
		}
		t.row(b.SHA256, fmtBytes(b.Size), fmtTime(b.CreatedAt), fmtAge(b.LastTouched, now), ref)
		total += b.Size
	}
	t.flush()
	fmt.Fprintf(a.stderr, "%d blob(s), %s.\n", len(blobs), fmtBytes(total))
	return 0
}

func cmdGC(a *app, ctx context.Context, args []string) int {
	f := a.flagSet("gc", "", "Delete blobs that nothing references and that were not used in the last hour.")
	if _, code, ok := a.parse(f, args, 0, 0); !ok {
		return code
	}
	c, err := a.connect(ctx)
	if err != nil {
		return a.fail(err)
	}
	r, err := c.GC(ctx)
	if err != nil {
		return a.fail(err)
	}
	if a.g.json {
		return a.printJSON(r)
	}
	fmt.Fprintf(a.stdout, "Deleted %d blob(s), freed %s.\n", r.Deleted, fmtBytes(r.FreedBytes))
	return 0
}

func cmdStats(a *app, ctx context.Context, args []string) int {
	f := a.flagSet("stats", "", "Show swarm totals: nodes, cores, memory, jobs and tasks.")
	if _, code, ok := a.parse(f, args, 0, 0); !ok {
		return code
	}
	c, err := a.connect(ctx)
	if err != nil {
		return a.fail(err)
	}
	st, err := c.Stats(ctx)
	if err != nil {
		return a.fail(err)
	}
	if a.g.json {
		return a.printJSON(st)
	}
	k := newKV(a.stdout)
	k.add("Nodes", fmt.Sprintf("%d online, %d offline; %s", st.NodesOnline, st.NodesOffline, sortedCounts(st.NodesByState)))
	k.add("CPU", fmt.Sprintf("%d threads, %s allocatable, %s in use", st.Cores, fmtCores(st.CoresAllocatable), fmtCores(st.CoresInUse)))
	k.add("Memory", fmtMB(st.MemMB))
	k.add("Speed", fmt.Sprintf("%d MB/s SHA-256 total", st.BenchTotal))
	k.add("Displays", strconv.Itoa(st.Displays))
	k.add("Jobs", sortedCounts(st.JobsByState))
	k.add("Tasks", sortedCounts(st.TasksByState))
	k.add("Completed", fmt.Sprintf("%d tasks, %.0f cpu-seconds since the hive started", st.TasksCompleted, st.CPUSecondsTotal))
	k.flush()
	return 0
}

func sortedCounts(m map[string]int) string {
	if len(m) == 0 {
		return "none"
	}
	s := make(map[string]string, len(m))
	for k, v := range m {
		s[k] = strconv.Itoa(v)
	}
	return sortedKV(s)
}
