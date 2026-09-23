package runner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/platteration/ewastesavior/internal/proto"
)

// inputError marks a failure that is reported as ErrorKind "input".
type inputError struct{ err error }

func (e inputError) Error() string { return e.err.Error() }
func (e inputError) Unwrap() error { return e.err }

func inputErrorf(format string, args ...any) error {
	return inputError{fmt.Errorf(format, args...)}
}

// owner is the uid/gid files in the workdir are given (-1 = unchanged).
type owner struct{ uid, gid int }

// writeInputs fetches every input and writes it into the workdir wd. Files
// are created with O_CREATE|O_EXCL|O_NOFOLLOW, then chowned and chmodded
// through their descriptor.
func (r *Runner) writeInputs(ctx context.Context, wd *os.Root, t proto.Task, own owner, ts *taskState) error {
	for _, in := range t.Inputs {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := mkdirParents(wd, in.Name, own); err != nil {
			return inputErrorf("input %q: %v", in.Name, err)
		}
		limit := inputLimit(in, t)
		f, err := wd.OpenFile(in.Name, os.O_WRONLY|os.O_CREATE|os.O_EXCL|oNoFollow, 0o600)
		if err != nil {
			return inputErrorf("input %q: %v", in.Name, err)
		}
		err = r.fillInput(ctx, f, in, limit, ts)
		if err == nil {
			err = finishFile(f, own, inputMode(in))
		}
		if cerr := f.Close(); err == nil && cerr != nil {
			err = cerr
		}
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			var ie inputError
			if errors.As(err, &ie) {
				return err
			}
			return inputErrorf("input %q: %v", in.Name, err)
		}
	}
	return nil
}

// fillInput writes one input's content into f and verifies its hash.
func (r *Runner) fillInput(ctx context.Context, f *os.File, in proto.Input, limit int64, ts *taskState) error {
	sha := in.Blob
	if sha == "" {
		sha = in.SHA256
	}
	fetch := func(ctx context.Context, w io.Writer) (int64, error) {
		cw := countWriter{w: w, fn: ts.addXfer}
		if in.Blob != "" {
			return r.tr.FetchBlob(ctx, in.Blob, cw)
		}
		return r.tr.FetchURL(ctx, in.URL, in.Size, cw)
	}
	if !r.cache.cacheable(limit) {
		// Too big to keep a second copy in RAM: stream into the workdir.
		h := sha256.New()
		if _, err := fetch(ctx, &limitWriter{w: io.MultiWriter(f, h), n: limit}); err != nil {
			return fetchErr(in, err)
		}
		if got := hex.EncodeToString(h.Sum(nil)); got != sha {
			return inputErrorf("input %q: content hash mismatch (got %s)", in.Name, got)
		}
		return nil
	}
	for attempt := 0; ; attempt++ {
		if err := r.cache.ensure(ctx, sha, limit, fetch); err != nil {
			return fetchErr(in, err)
		}
		src, err := r.cache.open(sha)
		if err != nil {
			return err
		}
		h := sha256.New()
		_, err = io.Copy(io.MultiWriter(f, h), io.LimitReader(src, limit+1))
		src.Close()
		if err != nil {
			return err
		}
		if hex.EncodeToString(h.Sum(nil)) == sha {
			return nil
		}
		// The cached copy went bad (bad RAM or disk): refetch once.
		r.cache.drop(sha)
		r.log.Warn("cached blob failed verification; refetching", "sha256", sha)
		if attempt > 0 {
			return inputErrorf("input %q: cached content does not verify", in.Name)
		}
		if err := truncate(f); err != nil {
			return err
		}
	}
}

func fetchErr(in proto.Input, err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	src := "blob " + in.Blob
	if in.URL != "" {
		src = "url"
	}
	return inputErrorf("input %q (%s): %v", in.Name, src, err)
}

// inputLimit is the most bytes an input may have: its declared size, else
// the task's disk allowance, never more than a hive blob can be.
func inputLimit(in proto.Input, t proto.Task) int64 {
	limit := int64(proto.MaxBlobBytes)
	if d := int64(effectiveDiskMB(t)) << 20; d < limit {
		limit = d
	}
	if in.Size > 0 && in.Size < limit {
		limit = in.Size
	}
	return limit
}

func inputMode(in proto.Input) os.FileMode {
	if in.Executable {
		return 0o755
	}
	return 0o644
}

// finishFile sets owner and mode through the open descriptor.
func finishFile(f *os.File, own owner, mode os.FileMode) error {
	if own.uid >= 0 {
		if err := f.Chown(own.uid, own.gid); err != nil {
			return err
		}
	}
	return f.Chmod(mode)
}

func truncate(f *os.File) error {
	if err := f.Truncate(0); err != nil {
		return err
	}
	_, err := f.Seek(0, io.SeekStart)
	return err
}

// mkdirParents creates the parent directories of name inside wd, owned by
// own. Existing components must be real directories.
func mkdirParents(wd *os.Root, name string, own owner) error {
	segs := strings.Split(name, "/")
	for i := 1; i < len(segs); i++ {
		dir := strings.Join(segs[:i], "/")
		err := wd.Mkdir(dir, 0o755)
		if err != nil {
			if !errors.Is(err, os.ErrExist) {
				return err
			}
			fi, lerr := wd.Lstat(dir)
			if lerr != nil {
				return lerr
			}
			if !fi.IsDir() {
				return fmt.Errorf("%s exists and is not a directory", dir)
			}
			continue
		}
		d, err := wd.OpenFile(dir, os.O_RDONLY|oNoFollow, 0)
		if err != nil {
			return err
		}
		err = finishFile(d, own, 0o755)
		d.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

// writeFileInRoot creates name in wd exclusively with the given content.
func writeFileInRoot(wd *os.Root, name string, data []byte, own owner, mode os.FileMode) error {
	f, err := wd.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL|oNoFollow, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = finishFile(f, own, mode)
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}
