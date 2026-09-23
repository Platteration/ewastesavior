package ctl

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/platteration/ewastesavior/internal/proto"
)

// ErrHashMismatch is returned when downloaded or uploaded bytes do not
// match their SHA-256.
var ErrHashMismatch = errors.New("content does not match its sha256")

// UploadFile stores a local file as a hive blob and returns its info. The
// file is hashed first and then streamed; the hive skips the transfer when
// it already has the blob.
func (c *Client) UploadFile(ctx context.Context, name string) (proto.BlobInfo, error) {
	f, err := os.Open(name)
	if err != nil {
		return proto.BlobInfo{}, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return proto.BlobInfo{}, err
	}
	if !st.Mode().IsRegular() {
		return proto.BlobInfo{}, fmt.Errorf("%s is not a regular file", name)
	}
	if st.Size() > proto.MaxBlobBytes {
		return proto.BlobInfo{}, fmt.Errorf("%s is larger than %d bytes", name, int64(proto.MaxBlobBytes))
	}
	info, err := c.Upload(ctx, f)
	if err != nil {
		return info, fmt.Errorf("upload %s: %w", name, err)
	}
	return info, nil
}

// Upload hashes r, rewinds it and uploads it as a blob.
func (c *Client) Upload(ctx context.Context, r io.ReadSeeker) (proto.BlobInfo, error) {
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return proto.BlobInfo{}, err
	}
	sha, size, err := hashReader(ctx, r)
	if err != nil {
		return proto.BlobInfo{}, err
	}
	return c.PutBlob(ctx, sha, size, r)
}

// hashReader returns the hex SHA-256 and length of everything r yields.
func hashReader(ctx context.Context, r io.Reader) (string, int64, error) {
	h := sha256.New()
	buf := make([]byte, 256<<10)
	var n int64
	for {
		if err := ctx.Err(); err != nil {
			return "", n, err
		}
		k, err := r.Read(buf)
		h.Write(buf[:k])
		n += int64(k)
		if n > proto.MaxBlobBytes {
			return "", n, fmt.Errorf("larger than %d bytes", int64(proto.MaxBlobBytes))
		}
		if err == io.EOF {
			return hex.EncodeToString(h.Sum(nil)), n, nil
		}
		if err != nil {
			return "", n, err
		}
	}
}

// PutBlob uploads exactly size bytes of r as blob sha. r is rewound before
// every attempt. The hive verifies the hash.
func (c *Client) PutBlob(ctx context.Context, sha string, size int64, r io.ReadSeeker) (proto.BlobInfo, error) {
	if !proto.ValidSHA256(sha) {
		return proto.BlobInfo{}, fmt.Errorf("invalid blob hash %q", truncate(sanitizeCell(sha), 80))
	}
	if err := c.ensureSession(ctx); err != nil {
		return proto.BlobInfo{}, err
	}
	ctx, kick, stop := c.stallContext(ctx)
	defer stop()
	body := func() (io.Reader, int64, error) {
		if _, err := r.Seek(0, io.SeekStart); err != nil {
			return nil, 0, err
		}
		return &kickReader{r: io.LimitReader(r, size), kick: kick}, size, nil
	}
	resp, err := c.do(ctx, apiRequest{
		method:      http.MethodPut,
		path:        escapePath("/api/v1/blobs", sha),
		body:        body,
		contentType: "application/octet-stream",
		expect100:   true,
	})
	if err != nil {
		return proto.BlobInfo{}, stallErr(ctx, err)
	}
	defer drainClose(resp.Body)
	kick()
	var info proto.BlobInfo
	if err := decodeJSONBody(resp.Body, maxHelloBody, &info); err != nil {
		return info, stallErr(ctx, err)
	}
	if info.SHA256 != sha {
		return info, fmt.Errorf("the hive stored blob %q instead of %s", truncate(sanitizeCell(info.SHA256), 80), sha)
	}
	return info, nil
}

// FetchBlob streams blob sha to w and verifies its hash. It returns the
// number of bytes written; on a mismatch the error wraps ErrHashMismatch
// and the caller must discard what was written.
func (c *Client) FetchBlob(ctx context.Context, sha string, w io.Writer) (int64, error) {
	if !proto.ValidSHA256(sha) {
		return 0, fmt.Errorf("invalid blob hash %q", truncate(sanitizeCell(sha), 80))
	}
	return c.download(ctx, escapePath("/api/v1/blobs", sha), w, proto.MaxBlobBytes, sha)
}

// download streams a GET response body to w. max < 0 means no limit
// beyond the blob maximum; wantSHA, if set, is verified.
func (c *Client) download(ctx context.Context, p string, w io.Writer, max int64, wantSHA string) (int64, error) {
	if max < 0 {
		max = proto.MaxBlobBytes
	}
	if err := c.ensureSession(ctx); err != nil {
		return 0, err
	}
	ctx, kick, stop := c.stallContext(ctx)
	defer stop()
	resp, err := c.do(ctx, apiRequest{method: http.MethodGet, path: p})
	if err != nil {
		return 0, stallErr(ctx, err)
	}
	defer drainClose(resp.Body)
	h := sha256.New()
	dst := w
	if wantSHA != "" {
		dst = io.MultiWriter(w, h)
	}
	n, err := io.Copy(dst, &capReader{r: &kickReader{r: resp.Body, kick: kick}, left: max})
	if err != nil {
		return n, stallErr(ctx, err)
	}
	if resp.ContentLength >= 0 && n != resp.ContentLength {
		return n, fmt.Errorf("short download: %d of %d bytes", n, resp.ContentLength)
	}
	if wantSHA != "" {
		if got := hex.EncodeToString(h.Sum(nil)); got != wantSHA {
			return n, fmt.Errorf("%w: got %s, want %s", ErrHashMismatch, got, wantSHA)
		}
	}
	return n, nil
}

// OutputFile is one downloaded output.
type OutputFile struct {
	Entry proto.OutputEntry `json:"entry"`
	Path  string            `json:"path"` // local path of the written file
}

// RefusedOutput is an output that was not written, and why.
type RefusedOutput struct {
	Entry  proto.OutputEntry `json:"entry"`
	Reason string            `json:"reason"`
}

// DownloadResult reports what DownloadOutputs did.
type DownloadResult struct {
	Written []OutputFile    `json:"written"`
	Refused []RefusedOutput `json:"refused"`
}

// OutputPath returns the relative slash path under which an output is
// saved (task-<index>/<name>, like the hive's zip), or an error if its name
// is not a safe relative path on this OS.
func OutputPath(e proto.OutputEntry) (string, error) {
	if e.Index < 0 || e.Index > proto.MaxTaskCount {
		return "", fmt.Errorf("invalid task index %d", e.Index)
	}
	if !proto.ValidRelPath(e.Name) || !filepath.IsLocal(filepath.FromSlash(e.Name)) {
		return "", fmt.Errorf("unsafe output name %q", truncate(sanitizeCell(e.Name), 120))
	}
	return path.Join("task-"+strconv.Itoa(e.Index), e.Name), nil
}

// DownloadOutputs saves job outputs below dir as task-<index>/<name>. Names
// are checked with proto.ValidRelPath and filepath.IsLocal, every file is
// created through an os.Root on dir (so symlinks cannot lead outside it)
// with O_EXCL (existing files are never overwritten), and contents are
// verified against their hash. Refused or failed entries are reported in
// the result; the error is only set when dir itself is unusable or ctx
// ends.
func (c *Client) DownloadOutputs(ctx context.Context, entries []proto.OutputEntry, dir string) (DownloadResult, error) {
	var res DownloadResult
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return res, err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return res, err
	}
	defer root.Close()
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		rel, err := OutputPath(e)
		if err == nil && !proto.ValidSHA256(e.Blob) {
			err = fmt.Errorf("invalid blob hash for %q", truncate(sanitizeCell(e.Name), 120))
		}
		if err == nil && (e.Size < 0 || e.Size > proto.MaxBlobBytes) {
			err = fmt.Errorf("invalid size %d", e.Size)
		}
		if err == nil {
			err = c.saveOutput(ctx, root, rel, e)
		}
		if err != nil {
			if ctx.Err() != nil {
				return res, ctx.Err()
			}
			res.Refused = append(res.Refused, RefusedOutput{Entry: e, Reason: err.Error()})
			continue
		}
		res.Written = append(res.Written, OutputFile{Entry: e, Path: filepath.Join(dir, filepath.FromSlash(rel))})
	}
	return res, nil
}

func (c *Client) saveOutput(ctx context.Context, root *os.Root, rel string, e proto.OutputEntry) error {
	if err := mkdirAllRoot(root, path.Dir(rel)); err != nil {
		return err
	}
	name := filepath.FromSlash(rel)
	f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("%s already exists; not overwriting it", rel)
		}
		return err
	}
	n, err := c.download(ctx, escapePath("/api/v1/blobs", e.Blob), f, e.Size, e.Blob)
	if err == nil && n != e.Size {
		err = fmt.Errorf("size is %d bytes, the hive listed %d", n, e.Size)
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = root.Remove(name)
		return err
	}
	return nil
}

// mkdirAllRoot creates the slash-separated directory dir and its parents
// inside root.
func mkdirAllRoot(root *os.Root, dir string) error {
	if dir == "." || dir == "" {
		return nil
	}
	cur := ""
	for _, seg := range strings.Split(dir, "/") {
		cur = path.Join(cur, seg)
		err := root.Mkdir(filepath.FromSlash(cur), 0o755)
		if err != nil && !errors.Is(err, fs.ErrExist) {
			return err
		}
	}
	return nil
}
