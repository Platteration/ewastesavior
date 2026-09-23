package hive

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/platteration/ewastesavior/internal/proto"
)

// Blob errors mapped to HTTP statuses.
var (
	errBlobTooLarge = errors.New("blob too large")
	errBlobMismatch = errors.New("content does not match its SHA-256")
	errDiskFull     = errors.New("not enough free disk space")
)

// minFreeDisk is the space kept free on the blob file system (DESIGN 7.1):
// max(5%, 512 MiB), but at most 10%, so a small data partition or a
// RAM-backed hive can still store outputs.
func minFreeDisk(total int64) int64 {
	return min(max(total/20, 512<<20), total/10)
}

// checkStorage updates the storage-low flag and warning from the blob file
// system's free space. While it is low, jobs with outputs are not
// dispatched: their uploads would fail.
func (s *Server) checkStorage() {
	free, total, ok := s.cfg.tune.diskFree(s.blobs.dir)
	low := ok && free < minFreeDisk(total)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.setStorageLowLocked(low, free, total)
}

func (s *Server) setStorageLowLocked(low bool, free, total int64) {
	if low == s.storageLow {
		return
	}
	s.storageLow = low
	if !low {
		delete(s.warnings, "storage")
		s.log.Info("hive storage has free space again", "free_bytes", free)
		s.notifyLocked()
		return
	}
	s.warnings["storage"] = fmt.Sprintf("the hive's storage is nearly full (%d MiB free of %d MiB): jobs with outputs wait; free space with 'savior ctl gc' or by deleting old jobs",
		free>>20, total>>20)
	s.log.Warn("hive storage nearly full; jobs with outputs wait", "free_bytes", free, "total_bytes", total)
}

// blobStore holds blob files at <dir>/<2 hex>/<sha256> (DESIGN 9).
type blobStore struct {
	dir string
	tmp string
}

func newBlobStore(dir string) (*blobStore, error) {
	b := &blobStore{dir: dir, tmp: filepath.Join(dir, ".tmp")}
	if err := os.RemoveAll(b.tmp); err != nil {
		return nil, fmt.Errorf("clean blob temp dir: %w", err)
	}
	if err := os.MkdirAll(b.tmp, 0o700); err != nil {
		return nil, fmt.Errorf("create blob dir: %w", err)
	}
	return b, nil
}

// path returns where a blob lives. Callers validate sha; anything else
// maps to a name that never exists.
func (b *blobStore) path(sha string) string {
	if !proto.ValidSHA256(sha) {
		return filepath.Join(b.dir, ".invalid")
	}
	return filepath.Join(b.dir, sha[:2], sha)
}

// receive streams r into a temp file while hashing it. The caller moves the
// file into place with commit (under the state lock) or removes it.
func (b *blobStore) receive(r io.Reader, want string, limit int64) (tmp, sum string, size int64, err error) {
	f, err := os.CreateTemp(b.tmp, "up-*")
	if err != nil {
		return "", "", 0, err
	}
	tmp = f.Name()
	defer func() {
		if err != nil {
			f.Close()
			os.Remove(tmp)
		}
	}()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(r, limit+1))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return "", "", 0, errBlobTooLarge
		}
		return "", "", 0, fmt.Errorf("receive blob: %w", err)
	}
	if n > limit {
		return "", "", 0, errBlobTooLarge
	}
	sum = hex.EncodeToString(h.Sum(nil))
	if want != "" && sum != want {
		return "", "", 0, errBlobMismatch
	}
	if err = f.Sync(); err != nil {
		return "", "", 0, err
	}
	if err = f.Close(); err != nil {
		return "", "", 0, err
	}
	return tmp, sum, n, nil
}

// commit moves a received temp file to its final place.
func (b *blobStore) commit(tmp, sha string) error {
	if err := os.MkdirAll(filepath.Dir(b.path(sha)), 0o700); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, b.path(sha)); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// reconcileBlobs aligns metadata with the files on disk at startup.
func (s *Server) reconcileBlobs() error {
	onDisk := map[string]os.FileInfo{}
	subs, err := os.ReadDir(s.blobs.dir)
	if err != nil {
		return fmt.Errorf("read blob dir: %w", err)
	}
	for _, d := range subs {
		if !d.IsDir() || len(d.Name()) != 2 {
			continue
		}
		files, err := os.ReadDir(filepath.Join(s.blobs.dir, d.Name()))
		if err != nil {
			continue
		}
		for _, f := range files {
			name := f.Name()
			if !proto.ValidSHA256(name) || name[:2] != d.Name() {
				continue
			}
			if fi, err := f.Info(); err == nil && fi.Mode().IsRegular() {
				onDisk[name] = fi
			}
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for sha := range s.blobMeta {
		if _, ok := onDisk[sha]; !ok {
			delete(s.blobMeta, sha)
			s.dirty = true
		}
	}
	for sha, fi := range onDisk {
		b := s.blobMeta[sha]
		if b == nil {
			b = &blob{blobRecord: blobRecord{SHA256: sha, CreatedAt: fi.ModTime(), LastTouched: fi.ModTime()},
				touchedMono: s.startMono, uploadedMono: s.startMono}
			s.blobMeta[sha] = b
			s.dirty = true
		}
		b.Size = fi.Size()
	}
	return nil
}

func (s *Server) blobInfoLocked(b *blob, refs map[string]string) proto.BlobInfo {
	_, referenced := refs[b.SHA256]
	return proto.BlobInfo{SHA256: b.SHA256, Size: b.Size, CreatedAt: b.CreatedAt, LastTouched: b.LastTouched, Referenced: referenced}
}

func (s *Server) touchLocked(b *blob) {
	b.LastTouched = s.now()
	b.touchedMono = time.Now()
	s.liveDirty = true
}

// nodeMayReadLocked implements the node token scope for blob reads
// (DESIGN 6.4): inputs of the node's current tasks and its display media.
func (s *Server) nodeMayReadLocked(n *node, sha string) bool {
	if !n.Approved {
		return false
	}
	for id, h := range n.held {
		t := s.tasks[id]
		if t == nil || !t.liveFor(n.ID, h.lease) {
			continue
		}
		for _, in := range t.job.Spec.Inputs {
			if in.Blob == sha {
				return true
			}
		}
	}
	found := false
	forEachMedia(n.Display, func(m proto.Media) {
		found = found || m.Blob == sha
	})
	return found
}

// forEachMedia visits every media item of a display spec, wall content
// included.
func forEachMedia(d *proto.DisplaySpec, fn func(proto.Media)) {
	if d == nil {
		return
	}
	if d.Image != nil {
		fn(*d.Image)
	}
	for _, m := range d.Images {
		fn(m)
	}
	if d.Wall != nil {
		forEachMedia(d.Wall.Content, fn)
	}
}

// blobRefsLocked maps every referenced blob to a description of its first
// reference. These are the GC roots of DESIGN 8.5 (besides recent touches).
func (s *Server) blobRefsLocked() map[string]string {
	refs := map[string]string{}
	add := func(sha, what string) {
		if _, ok := refs[sha]; !ok && sha != "" {
			refs[sha] = what
		}
	}
	for _, j := range s.jobs {
		for _, in := range j.Spec.Inputs {
			add(in.Blob, fmt.Sprintf("input %s of job %s", in.Name, j.ID))
		}
		for _, t := range j.tasks {
			for _, o := range t.Outputs {
				add(o.Blob, fmt.Sprintf("output %s of task %s", o.Name, t.ID))
			}
		}
	}
	for _, n := range s.nodes {
		forEachMedia(n.Display, func(m proto.Media) { add(m.Blob, "display of node "+n.Name) })
	}
	for _, w := range s.walls {
		c := w.Content
		forEachMedia(&c, func(m proto.Media) { add(m.Blob, "wall "+w.ID) })
	}
	return refs
}

// gcLocked deletes unreferenced blobs not touched within the touch window.
// With nodeUploadsOnly it only collects blobs uploaded by nodes (outputs
// of deleted jobs, abandoned uploads), which is what runs automatically.
func (s *Server) gcLocked(nodeUploadsOnly bool, now time.Time) (int, int64) {
	refs := s.blobRefsLocked()
	var deleted int
	var freed int64
	for sha, b := range s.blobMeta {
		if _, ok := refs[sha]; ok {
			continue
		}
		if now.Sub(b.touchedMono) < s.cfg.tune.blobTouch {
			continue
		}
		if nodeUploadsOnly && (!b.NodeUpload || now.Sub(b.uploadedMono) < s.cfg.tune.uploadGC) {
			continue
		}
		if err := os.Remove(s.blobs.path(sha)); err != nil && !errors.Is(err, os.ErrNotExist) {
			s.log.Warn("blob gc: remove failed", "blob", sha, "err", err)
			continue
		}
		delete(s.blobMeta, sha)
		deleted++
		freed += b.Size
		s.dirty = true
	}
	return deleted, freed
}

func (s *Server) serveBlobFile(w http.ResponseWriter, r *http.Request, sha, filename string, size int64) {
	f, err := os.Open(s.blobs.path(sha))
	if err != nil {
		writeErr(w, http.StatusNotFound, "blob not found")
		return
	}
	defer f.Close()
	setDeadlines(w, blobDeadline(size))
	h := w.Header()
	h.Set("Content-Type", "application/octet-stream")
	cd := mime.FormatMediaType("attachment", map[string]string{"filename": filename})
	if cd == "" {
		cd = "attachment"
	}
	h.Set("Content-Disposition", cd)
	h.Set("Content-Security-Policy", blobCSP)
	h.Set("ETag", `"`+sha+`"`)
	http.ServeContent(w, r, "", time.Time{}, f)
}

func (s *Server) handleBlobGet(w http.ResponseWriter, r *http.Request, who requester) {
	sha := r.PathValue("sha")
	if !proto.ValidSHA256(sha) {
		writeErr(w, http.StatusBadRequest, "invalid blob hash")
		return
	}
	s.mu.Lock()
	if status, msg := s.blobReadAllowedLocked(who, sha); status != 0 {
		s.mu.Unlock()
		writeErr(w, status, "%s", msg)
		return
	}
	b := s.blobMeta[sha]
	if b == nil {
		s.mu.Unlock()
		writeErr(w, http.StatusNotFound, "blob not found")
		return
	}
	s.touchLocked(b)
	size := b.Size
	s.mu.Unlock()
	s.serveBlobFile(w, r, sha, sha, size)
}

// blobReadAllowedLocked returns a non-zero status when who may not read sha.
func (s *Server) blobReadAllowedLocked(who requester, sha string) (int, string) {
	if who.isAdmin() {
		return 0, ""
	}
	n := s.nodeByTokenLocked(who.nodeToken)
	if n == nil {
		return http.StatusUnauthorized, "unknown node token; register again"
	}
	if !s.nodeMayReadLocked(n, sha) {
		return http.StatusForbidden, "this node has no task or display that uses this blob"
	}
	return 0, ""
}

// uploadCharge reserves part of a lease's 1 GiB upload budget.
type uploadCharge struct {
	task     *task
	lease    string
	reserved int64
}

func (s *Server) handleBlobPut(w http.ResponseWriter, r *http.Request, who requester) {
	sha := r.PathValue("sha")
	if !proto.ValidSHA256(sha) {
		writeErr(w, http.StatusBadRequest, "invalid blob hash")
		return
	}
	if r.ContentLength > s.maxBlob {
		writeErr(w, http.StatusRequestEntityTooLarge, "blob larger than %d bytes", s.maxBlob)
		return
	}
	limit := s.maxBlob
	var ch *uploadCharge
	s.mu.Lock()
	if !who.isAdmin() {
		var status int
		var msg string
		ch, status, msg = s.chargeUploadLocked(who.nodeToken, r.URL.Query().Get("lease"), r.ContentLength)
		if status != 0 {
			s.mu.Unlock()
			writeErr(w, status, "%s", msg)
			return
		}
		if ch.reserved < limit {
			limit = ch.reserved
		}
	}
	if b := s.blobMeta[sha]; b != nil && fileExists(s.blobs.path(sha)) {
		// Content-addressed: already stored, nothing to transfer.
		s.touchLocked(b)
		s.settleChargeLocked(ch, 0)
		info := s.blobInfoLocked(b, s.blobRefsLocked())
		s.mu.Unlock()
		writeJSON(w, http.StatusOK, info)
		return
	}
	s.mu.Unlock()
	info, status, err := s.receiveBlob(w, r, sha, limit, !who.isAdmin())
	s.mu.Lock()
	var used int64
	if err == nil {
		used = info.Size
	}
	s.settleChargeLocked(ch, used)
	s.mu.Unlock()
	if err != nil {
		writeErr(w, status, "%s", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, info)
}

// chargeUploadLocked checks that a node may upload (it has a running task)
// and reserves budget from the lease with the most budget left, or the
// lease named by ?lease= (DESIGN 6.4: 1 GiB per lease).
func (s *Server) chargeUploadLocked(tok, lease string, contentLength int64) (*uploadCharge, int, string) {
	n := s.nodeByTokenLocked(tok)
	if n == nil {
		return nil, http.StatusUnauthorized, "unknown node token; register again"
	}
	if !n.Approved {
		return nil, http.StatusForbidden, "node is not approved"
	}
	var best *task
	for id, h := range n.held {
		t := s.tasks[id]
		if t == nil || !t.liveFor(n.ID, h.lease) || (lease != "" && t.Lease != lease) {
			continue
		}
		if best == nil || t.uploaded < best.uploaded {
			best = t
		}
	}
	if best == nil {
		return nil, http.StatusForbidden, "blob uploads need a running task"
	}
	left := leaseUploadCap - best.uploaded
	want := contentLength
	if want < 0 {
		want = left
	}
	if want > left || left <= 0 {
		return nil, http.StatusRequestEntityTooLarge, "upload budget of this task (1 GiB) exhausted"
	}
	best.uploaded += want
	return &uploadCharge{task: best, lease: best.Lease, reserved: want}, 0, ""
}

// settleChargeLocked returns the unused part of a reservation.
func (s *Server) settleChargeLocked(ch *uploadCharge, used int64) {
	if ch == nil || ch.task.Lease != ch.lease {
		return
	}
	ch.task.uploaded -= ch.reserved - used
}

// receiveBlob stores a request body as a blob. want == "" hashes on the fly.
func (s *Server) receiveBlob(w http.ResponseWriter, r *http.Request, want string, limit int64, byNode bool) (proto.BlobInfo, int, error) {
	need := r.ContentLength
	if need < 0 {
		need = 0
	}
	if free, total, ok := s.cfg.tune.diskFree(s.blobs.dir); ok && free-need < minFreeDisk(total) {
		if free < minFreeDisk(total) {
			s.mu.Lock()
			s.setStorageLowLocked(true, free, total)
			s.mu.Unlock()
		}
		return proto.BlobInfo{}, http.StatusInsufficientStorage, errDiskFull
	}
	d := blobDeadline(r.ContentLength)
	if r.ContentLength < 0 {
		d = blobDeadline(limit)
	}
	setDeadlines(w, d)
	body := http.MaxBytesReader(w, r.Body, limit)
	tmp, sum, size, err := s.blobs.receive(body, want, limit)
	if err != nil {
		switch {
		case errors.Is(err, errBlobTooLarge):
			return proto.BlobInfo{}, http.StatusRequestEntityTooLarge, fmt.Errorf("blob larger than %d bytes", limit)
		case errors.Is(err, errBlobMismatch):
			return proto.BlobInfo{}, http.StatusBadRequest, err
		}
		if free, total, ok := s.cfg.tune.diskFree(s.blobs.dir); ok && free < minFreeDisk(total) {
			return proto.BlobInfo{}, http.StatusInsufficientStorage, errDiskFull
		}
		return proto.BlobInfo{}, http.StatusInternalServerError, err
	}
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.blobs.commit(tmp, sum); err != nil {
		return proto.BlobInfo{}, http.StatusInternalServerError, fmt.Errorf("store blob: %w", err)
	}
	b := s.blobMeta[sum]
	if b == nil {
		wall := s.now()
		b = &blob{blobRecord: blobRecord{SHA256: sum, Size: size, CreatedAt: wall, NodeUpload: byNode}, uploadedMono: now}
		s.blobMeta[sum] = b
	}
	b.Size = size
	s.touchLocked(b)
	s.dirty = true
	return s.blobInfoLocked(b, s.blobRefsLocked()), http.StatusOK, nil
}

func (s *Server) handleBlobPost(w http.ResponseWriter, r *http.Request, _ adminCtx) {
	if r.ContentLength > s.maxBlob {
		writeErr(w, http.StatusRequestEntityTooLarge, "blob larger than %d bytes", s.maxBlob)
		return
	}
	info, status, err := s.receiveBlob(w, r, "", s.maxBlob, false)
	if err != nil {
		writeErr(w, status, "%s", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, info)
}

func (s *Server) handleBlobList(w http.ResponseWriter, r *http.Request, _ adminCtx) {
	s.mu.Lock()
	refs := s.blobRefsLocked()
	out := make([]proto.BlobInfo, 0, len(s.blobMeta))
	for _, b := range s.blobMeta {
		out = append(out, s.blobInfoLocked(b, refs))
	}
	s.mu.Unlock()
	sort.Slice(out, func(i, k int) bool {
		if !out[i].CreatedAt.Equal(out[k].CreatedAt) {
			return out[i].CreatedAt.After(out[k].CreatedAt)
		}
		return out[i].SHA256 < out[k].SHA256
	})
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleBlobDelete(w http.ResponseWriter, r *http.Request, _ adminCtx) {
	sha := r.PathValue("sha")
	if !proto.ValidSHA256(sha) {
		writeErr(w, http.StatusBadRequest, "invalid blob hash")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.blobMeta[sha] == nil {
		writeErr(w, http.StatusNotFound, "blob not found")
		return
	}
	if ref, ok := s.blobRefsLocked()[sha]; ok {
		writeErr(w, http.StatusConflict, "blob is referenced by %s", ref)
		return
	}
	if err := os.Remove(s.blobs.path(sha)); err != nil && !errors.Is(err, os.ErrNotExist) {
		writeErr(w, http.StatusInternalServerError, "delete blob: %v", err)
		return
	}
	delete(s.blobMeta, sha)
	s.dirty = true
	writeOK(w)
}

func (s *Server) handleBlobGC(w http.ResponseWriter, r *http.Request, _ adminCtx) {
	s.mu.Lock()
	n, freed := s.gcLocked(false, time.Now())
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]int64{"deleted": int64(n), "freed_bytes": freed})
}

// fillMediaDims validates that a spec's blob media exist and records their
// pixel size in Media.Width/Height (informational, DESIGN 7.2).
func (s *Server) fillMediaDims(d *proto.DisplaySpec) error {
	if d == nil {
		return nil
	}
	fix := func(m *proto.Media) error {
		if m.Blob == "" {
			return nil
		}
		w, h, err := s.blobDims(m.Blob)
		if err != nil {
			return err
		}
		m.Width, m.Height = w, h
		return nil
	}
	if d.Image != nil {
		img := *d.Image
		if err := fix(&img); err != nil {
			return err
		}
		d.Image = &img
	}
	if len(d.Images) > 0 {
		imgs := append([]proto.Media(nil), d.Images...)
		for i := range imgs {
			if err := fix(&imgs[i]); err != nil {
				return err
			}
		}
		d.Images = imgs
	}
	return nil
}

// blobDims returns an image blob's pixel size, cached in its metadata.
func (s *Server) blobDims(sha string) (int, int, error) {
	s.mu.Lock()
	b := s.blobMeta[sha]
	if b == nil {
		s.mu.Unlock()
		return 0, 0, fmt.Errorf("blob %s not found (upload it first)", sha)
	}
	if b.Width > 0 && b.Height > 0 {
		w, h := b.Width, b.Height
		s.mu.Unlock()
		return w, h, nil
	}
	s.mu.Unlock()
	f, err := os.Open(s.blobs.path(sha))
	if err != nil {
		return 0, 0, fmt.Errorf("blob %s not found (upload it first)", sha)
	}
	defer f.Close()
	cfg, _, err := image.DecodeConfig(f)
	if err != nil {
		return 0, 0, fmt.Errorf("blob %s is not a supported image", sha)
	}
	s.mu.Lock()
	if b := s.blobMeta[sha]; b != nil {
		b.Width, b.Height = cfg.Width, cfg.Height
		s.dirty = true
	}
	s.mu.Unlock()
	return cfg.Width, cfg.Height, nil
}
