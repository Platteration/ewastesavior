package runner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/platteration/ewastesavior/internal/proto"
)

// rootLister lists a task workdir through an os.Root, so no symlink is ever
// followed out of the directory.
type rootLister struct{ root *os.Root }

func (l rootLister) list(dir string) ([]dirEntry, error) {
	name := dir
	if name == "" {
		name = "."
	}
	f, err := l.root.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	ents, err := f.ReadDir(-1)
	if err != nil {
		return nil, err
	}
	out := make([]dirEntry, 0, len(ents))
	for _, e := range ents {
		out = append(out, dirEntry{name: e.Name(), typ: classifyDirEntry(e.Type())})
	}
	return out, nil
}

func (l rootLister) lstat(p string) (entryType, bool) {
	fi, err := l.root.Lstat(p)
	if err != nil {
		return entryOther, false
	}
	return classifyMode(fi.Mode()), true
}

func classifyDirEntry(m os.FileMode) entryType {
	if m.Type() == 0 {
		return entryFile
	}
	if m.IsDir() {
		return entryDir
	}
	return entryOther
}

func classifyMode(m os.FileMode) entryType { return classifyDirEntry(m) }

// collectOutputs matches the output patterns, verifies each candidate and
// uploads it (DESIGN 8.5). It returns the uploaded outputs.
func (r *Runner) collectOutputs(ctx context.Context, tk *taskDir, ts *taskState, logs io.Writer) ([]proto.Output, error) {
	if len(tk.task.Outputs) == 0 {
		return nil, nil
	}
	files, notes, err := matchOutputs(rootLister{tk.wd}, tk.task.Outputs)
	if err != nil {
		return nil, err
	}
	for _, n := range notes {
		fmt.Fprintf(logs, "savior: %s\n", n)
	}
	if len(files) > proto.MaxOutputFiles {
		return nil, fmt.Errorf("too many output files (%d, max %d)", len(files), proto.MaxOutputFiles)
	}
	ts.setPhase(proto.PhaseUploading)
	var outs []proto.Output
	var total int64
	for _, name := range files {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		out, ok, err := r.uploadOne(ctx, tk, ts, name, logs)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		total += out.Size
		if total > proto.MaxOutputBytes {
			return nil, fmt.Errorf("outputs exceed %d bytes", int64(proto.MaxOutputBytes))
		}
		outs = append(outs, out)
	}
	sort.Slice(outs, func(i, j int) bool { return outs[i].Name < outs[j].Name })
	return outs, nil
}

// uploadOne verifies and uploads a single candidate output. ok is false
// (with a log note) when the candidate is not a safe regular file.
func (r *Runner) uploadOne(ctx context.Context, tk *taskDir, ts *taskState, name string, logs io.Writer) (proto.Output, bool, error) {
	f, err := tk.wd.OpenFile(name, os.O_RDONLY|oNoFollow|oNonblock, 0)
	if err != nil {
		fmt.Fprintf(logs, "savior: skipping output %q: %v\n", name, err)
		return proto.Output{}, false, nil
	}
	defer f.Close()
	var st syscall.Stat_t
	if err := syscall.Fstat(int(f.Fd()), &st); err != nil {
		return proto.Output{}, false, fmt.Errorf("fstat output %q: %w", name, err)
	}
	switch {
	case st.Mode&syscall.S_IFMT != syscall.S_IFREG:
		fmt.Fprintf(logs, "savior: skipping output %q: not a regular file\n", name)
		return proto.Output{}, false, nil
	case st.Nlink != 1:
		fmt.Fprintf(logs, "savior: skipping output %q: it has %d hard links\n", name, st.Nlink)
		return proto.Output{}, false, nil
	case int(st.Uid) != tk.uid:
		fmt.Fprintf(logs, "savior: skipping output %q: owned by uid %d, not the task uid %d\n", name, st.Uid, tk.uid)
		return proto.Output{}, false, nil
	}

	h := sha256.New()
	size, err := io.Copy(h, f)
	if err != nil {
		return proto.Output{}, false, fmt.Errorf("read output %q: %w", name, err)
	}
	sum := hex.EncodeToString(h.Sum(nil))
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return proto.Output{}, false, fmt.Errorf("output %q: %w", name, err)
	}
	rd := countReader{r: io.LimitReader(f, size), fn: ts.addXfer}
	if err := r.tr.UploadBlob(ctx, sum, size, rd); err != nil {
		return proto.Output{}, false, fmt.Errorf("upload output %q: %w", name, err)
	}
	return proto.Output{Name: name, Blob: sum, Size: size}, true, nil
}

// countReader reports every read to fn (upload progress).
type countReader struct {
	r  io.Reader
	fn func(int64)
}

func (c countReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if n > 0 && c.fn != nil {
		c.fn(int64(n))
	}
	return n, err
}

// removeAllIn removes name and its contents inside root, never following a
// symlink out of root.
func removeAllIn(root *os.Root, name string) error {
	fi, err := root.Lstat(name)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if fi.IsDir() {
		if sub, err := root.OpenRoot(name); err == nil {
			if f, err := sub.Open("."); err == nil {
				ents, _ := f.ReadDir(-1)
				f.Close()
				for _, e := range ents {
					removeAllIn(sub, e.Name()) // relative to sub
				}
			}
			sub.Close()
		}
	}
	return root.Remove(name)
}

// exitInfo extracts the exit code and terminating signal from a wait error.
// ok is false when the error is not a normal process exit.
func exitInfo(werr error) (code int, signal syscall.Signal, ok bool) {
	if werr == nil {
		return 0, 0, true
	}
	var ee *exec.ExitError
	if errors.As(werr, &ee) {
		if ws, isWS := ee.Sys().(syscall.WaitStatus); isWS {
			if ws.Signaled() {
				return 0, ws.Signal(), true
			}
			return ws.ExitStatus(), 0, true
		}
		return ee.ExitCode(), 0, true
	}
	return 0, 0, false
}

func signalName(s syscall.Signal) string { return s.String() }

// reapGroup waits until no process remains in the process group pgid.
func reapGroup(pgid int, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if err := unix.Kill(-pgid, 0); err == unix.ESRCH {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
}
