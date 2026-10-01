package hive

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/platteration/ewastesavior/internal/proto"
)

// On-disk state (DESIGN 9):
//
//   - state.json: nodes, walls, the blob index and every job that may still
//     change, rewritten as a whole on structural changes;
//   - jobs/<job id>.json: a job that has finished for good (every task
//     terminal), written once when it settles and removed when the job is
//     deleted. Its records never change again, so they are not rewritten
//     with state.json (at the retention limits they are most of the state);
//   - live.json: the nodes' liveness fields, so heartbeats don't rewrite
//     state.json either.
const (
	stateFile     = "state.json"
	liveFile      = "live.json"
	jobsDir       = "jobs"
	minPersistGap = 2 * time.Second
	slowFSPersist = 30 * time.Second // hive_data in RAM or on vfat
)

// readSnapshot reads and decodes one state file.
func readSnapshot(path string) (*snapshot, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var snap snapshot
	dec := json.NewDecoder(bufio.NewReaderSize(f, 256<<10))
	if err := dec.Decode(&snap); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if snap.Format != snapshotFormat {
		return nil, fmt.Errorf("%s: unsupported format %d", path, snap.Format)
	}
	return &snap, nil
}

// loadState loads state.json, falling back to state.json.prev when the
// main file is missing or unreadable (DESIGN 9), then the settled jobs and
// the nodes' liveness.
func (s *Server) loadState() error {
	cur := filepath.Join(s.data.path, stateFile)
	prev := cur + ".prev"
	snap, err := readSnapshot(cur)
	switch {
	case err == nil:
	case errors.Is(err, os.ErrNotExist):
		if p, perr := readSnapshot(prev); perr == nil {
			snap = p
			s.log.Warn("state.json is missing; loaded state.json.prev")
		} else if !errors.Is(perr, os.ErrNotExist) {
			s.quarantineFile(prev)
			s.warnings["state"] = fmt.Sprintf("saved state could not be loaded (%v); started empty", perr)
			s.log.Error("saved state could not be loaded; starting empty", "err", perr)
		}
	default:
		s.log.Error("state.json is unreadable; trying state.json.prev", "err", err)
		if p, perr := readSnapshot(prev); perr == nil {
			snap = p
			s.quarantineFile(cur)
			s.warnings["state"] = "state.json was damaged; loaded the previous copy (state.json.prev)"
		} else {
			s.quarantineFile(cur)
			s.warnings["state"] = fmt.Sprintf("saved state could not be loaded (%v); started empty", err)
		}
	}
	if snap == nil {
		snap = &snapshot{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.applySnapshotLocked(snap)
	s.applyLiveLocked()
	s.recovering = true // the window itself starts when New returns
	return nil
}

// quarantineFile moves an unreadable state file aside so it is never
// overwritten silently.
func (s *Server) quarantineFile(path string) {
	dst := fmt.Sprintf("%s.corrupt-%d", path, time.Now().Unix())
	if err := os.Rename(path, dst); err == nil {
		s.log.Warn("kept unreadable state file", "path", dst)
	}
}

// applySnapshotLocked rebuilds in-memory state from a snapshot and the
// settled jobs in jobs/. Counters are recomputed rather than trusted;
// assigned tasks become unconfirmed until their nodes re-register (DESIGN
// 8.6).
func (s *Server) applySnapshotLocked(snap *snapshot) {
	now := s.startMono
	for _, rec := range snap.Nodes {
		if !proto.ValidNodeID(rec.ID) || s.nodes[rec.ID] != nil {
			continue
		}
		if !proto.ValidNodeName(rec.Name) || s.nameTakenLocked(rec.Name, rec.ID) {
			rec.Name = s.defaultNameLocked(rec.ID)
		}
		s.nodes[rec.ID] = newNode(rec)
	}
	for i := range snap.Walls {
		w := snap.Walls[i]
		if w.ID == "" || s.walls[w.ID] != nil || proto.ValidateWallSpec(&w) != nil {
			s.log.Warn("dropping an invalid wall from the saved state", "wall", w.ID)
			continue
		}
		s.walls[w.ID] = &w
	}
	for _, br := range snap.Blobs {
		if proto.ValidSHA256(br.SHA256) {
			s.blobMeta[br.SHA256] = &blob{blobRecord: br, touchedMono: now, uploadedMono: now}
		}
	}
	// A settled job's own file is newer than any copy of it in state.json
	// (state.json drops a job only after its file is on disk), so those
	// load first and win.
	var orphans []*task
	s.loadSettledJobsLocked(&orphans)
	for i := range snap.Jobs {
		s.applyJobLocked(&snap.Jobs[i], snap.SavedAt, &orphans)
	}
	maxSeq, maxDoneSeq := uint64(0), uint64(0)
	for _, j := range s.jobs {
		maxSeq = max(maxSeq, j.Seq)
		maxDoneSeq = max(maxDoneSeq, j.DoneSeq)
	}
	sort.Slice(s.queue, func(a, b int) bool { return queueLess(s.queue[a], s.queue[b]) })
	s.nextSeq = snap.NextSeq
	if s.nextSeq <= maxSeq {
		s.nextSeq = maxSeq + 1
	}
	// Jobs saved before DoneSeq existed have 0 and are retained by Seq,
	// before every job that finished since.
	s.nextDoneSeq = max(snap.NextDoneSeq, maxDoneSeq+1)
	for _, t := range orphans {
		s.requeueLocked(t, requeueOpts{outcome: "lost", err: "node record no longer exists"})
	}
	// Wall membership must agree in both directions.
	for _, n := range s.nodes {
		if w := s.walls[n.WallID]; n.WallID != "" && (w == nil || !wallHasNode(w, n.ID)) {
			n.WallID = ""
		}
	}
	for id, w := range s.walls {
		for _, c := range w.Cells {
			if n := s.nodes[c.Node]; n != nil && n.WallID == "" {
				n.WallID = id
			}
		}
	}
	s.dirty = false
}

// applyJobLocked restores one saved job and its task records. It returns
// nil for an invalid job or one already loaded. Active tasks whose node
// record is gone are added to orphans.
func (s *Server) applyJobLocked(js *jobSnapshot, savedAt time.Time, orphans *[]*task) *job {
	if js.ID == "" || s.jobs[js.ID] != nil || js.Spec.Count < 1 {
		return nil
	}
	now := s.startMono
	j := &job{jobRecord: js.jobRecord}
	if j.NextIndex > j.Spec.Count {
		j.NextIndex = j.Spec.Count
	}
	sort.Slice(js.Tasks, func(a, b int) bool { return js.Tasks[a].Index < js.Tasks[b].Index })
	for _, tr := range js.Tasks {
		if tr.ID == "" || s.tasks[tr.ID] != nil || tr.Index < 0 || tr.Index >= j.Spec.Count {
			continue
		}
		tr.JobID = j.ID
		t := &task{taskRecord: tr, job: j}
		switch {
		case t.active() && t.CancelRequested:
			t.State, t.CancelRequested = proto.TaskCanceled, false
			if t.FinishedAt == nil {
				t.FinishedAt = timePtr(savedAt)
			}
		case t.active():
			t.unconfirmed = true
			t.assignedMono = now
			t.xferMono = now
			if n := s.nodes[t.Node]; n != nil {
				n.held[t.ID] = heldTask{lease: t.Lease, charge: charge(j.Spec.Resources, n.ScratchInRAM)}
			} else {
				*orphans = append(*orphans, t)
			}
		case t.State == proto.TaskPending:
			j.requeued = append(j.requeued, t)
		case t.State.Terminal():
		default:
			t.State = proto.TaskPending
			j.requeued = append(j.requeued, t)
		}
		j.tasks = append(j.tasks, t)
		s.tasks[t.ID] = t
		j.adjust(t.bucket(), 1)
	}
	if j.Canceled {
		j.counts.Canceled += j.undispatched()
	} else {
		j.counts.Pending += j.undispatched()
	}
	s.taskRecords += len(j.tasks)
	s.jobs[j.ID] = j
	s.addJobRefsLocked(j, 1)
	if !j.finished() {
		s.queue = append(s.queue, j)
	} else if j.FinishedAt == nil {
		j.FinishedAt = timePtr(savedAt)
	}
	return j
}

// settled reports whether a job has finished for good: it is finished and
// no task is assigned or running any more (a canceled job's tasks may
// still be stopping). From then on nothing writes its jobRecord, task
// records or task list (only deleting the job remains), which is what lets
// persistHeld encode a settled job outside the state mutex without copying
// it.
func settled(j *job) bool {
	if j.FinishedAt == nil || !j.finished() {
		return false
	}
	for _, t := range j.tasks {
		if !t.State.Terminal() {
			return false
		}
	}
	return true
}

// validJobFileID reports whether a job ID can name its file in jobs/.
// Hive job IDs always can; others stay in state.json.
func validJobFileID(id string) bool {
	if id == "" || len(id) > 64 {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

// jobFile is the content of jobs/<job id>.json.
type jobFile struct {
	Format  int          `json:"format"`
	SavedAt time.Time    `json:"saved_at"`
	Job     *jobSnapshot `json:"job,omitempty"`
}

// loadSettledJobsLocked loads the settled jobs from jobs/, one file at a
// time, so a restart never holds all of them in decoded form at once.
func (s *Server) loadSettledJobsLocked(orphans *[]*task) {
	dir := filepath.Join(s.data.path, jobsDir)
	ents, err := os.ReadDir(dir)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			s.warnings["jobs"] = "finished jobs could not be loaded: " + err.Error()
			s.log.Error("reading the saved finished jobs failed", "err", err)
		}
		return
	}
	var bad []string
	for _, e := range ents {
		name := e.Name()
		path := filepath.Join(dir, name)
		if strings.HasPrefix(name, ".") {
			os.Remove(path) // a temp file of an interrupted save
			continue
		}
		id, ok := strings.CutSuffix(name, ".json")
		if !ok || !validJobFileID(id) || !e.Type().IsRegular() {
			continue
		}
		jf, err := readJobFile(path)
		if err == nil && jf.Job.ID != id {
			err = fmt.Errorf("%s holds job %q", path, jf.Job.ID)
		}
		if err != nil {
			s.log.Error("a saved finished job could not be loaded", "err", err)
			s.quarantineFile(path)
			bad = append(bad, id)
			continue
		}
		switch j := s.applyJobLocked(jf.Job, jf.SavedAt, orphans); {
		case j == nil:
		case settled(j):
			j.archived = true
		default:
			// Never written that way; state.json keeps it from the next
			// save on and the file goes.
			s.archiveDel = append(s.archiveDel, id)
		}
	}
	if len(bad) > 0 {
		s.warnings["jobs"] = fmt.Sprintf("%d saved finished job(s) could not be loaded and were kept aside as jobs/<id>.json.corrupt-* (%s)",
			len(bad), strings.Join(bad, ", "))
	}
}

func readJobFile(path string) (*jobFile, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var jf jobFile
	if err := json.NewDecoder(bufio.NewReaderSize(f, 64<<10)).Decode(&jf); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if jf.Format != snapshotFormat || jf.Job == nil {
		return nil, fmt.Errorf("%s: unsupported format %d", path, jf.Format)
	}
	return &jf, nil
}

// persist writes the state when it's dirty (or always with force).
func (s *Server) persist(force bool) error {
	s.persistMu.Lock()
	defer s.persistMu.Unlock()
	return s.persistHeld(force)
}

// persistHeld is persist with persistMu held. Jobs that have settled since
// the last save are written to their own files first; state.json then
// leaves them out.
func (s *Server) persistHeld(force bool) error {
	s.mu.Lock()
	if !force && !s.dirty {
		s.mu.Unlock()
		return nil
	}
	var settledNow []*job
	for _, j := range s.jobs {
		if !j.archived && validJobFileID(j.ID) && settled(j) {
			settledNow = append(settledNow, j)
		}
	}
	s.mu.Unlock()

	written := s.writeJobFiles(settledNow)

	s.mu.Lock()
	for _, j := range written {
		if s.jobs[j.ID] == j {
			j.archived = true
		} else {
			s.archiveDel = append(s.archiveDel, j.ID) // deleted meanwhile
		}
	}
	snap := s.snapshotLocked()
	gone := s.archiveDel
	s.archiveDel = nil
	s.dirty, s.liveDirty = false, false
	s.mu.Unlock()

	sortSnapshot(snap)
	start := time.Now()
	err := writeSnapshot(s.data.path, snap)
	s.lastWrite = time.Now()
	s.lastWriteDur = s.lastWrite.Sub(start)
	if err == nil {
		s.lastLiveWrite = s.lastWrite
		// Only now: until this state.json is on disk, the one before may
		// still hold an older copy of a deleted job, which its file
		// overrides.
		s.removeJobFiles(gone)
	}

	s.mu.Lock()
	if err != nil {
		s.dirty = true
		s.archiveDel = append(gone, s.archiveDel...)
		s.warnings["persist"] = "the hive state could not be saved: " + err.Error()
	} else {
		delete(s.warnings, "persist")
	}
	s.mu.Unlock()
	if err != nil && (s.persistErr == nil || s.persistErr.Error() != err.Error()) {
		s.log.Error("saving hive state failed", "err", err)
	}
	s.persistErr = err
	return err
}

// writeJobFiles writes settled jobs to jobs/<id>.json and returns those
// that are on disk. A job whose file can't be written stays in state.json.
// It runs without the state mutex: settled jobs don't change.
func (s *Server) writeJobFiles(jobs []*job) []*job {
	if len(jobs) == 0 {
		return nil
	}
	dir := filepath.Join(s.data.path, jobsDir)
	saved := s.now()
	var out []*job
	var firstErr error
	w := newJSONWriter(nil) // one buffer for all files
	for _, j := range jobs {
		if err := writeJobFile(dir, j, saved, w); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		out = append(out, j)
	}
	if len(out) > 0 {
		syncDir(dir)
	}
	if firstErr != nil {
		s.log.Warn("could not save finished jobs to their own files; state.json keeps them", "failed", len(jobs)-len(out), "err", firstErr)
	}
	return out
}

// removeJobFiles removes the files of deleted jobs.
func (s *Server) removeJobFiles(ids []string) {
	if len(ids) == 0 {
		return
	}
	dir := filepath.Join(s.data.path, jobsDir)
	for _, id := range ids {
		if err := os.Remove(filepath.Join(dir, id+".json")); err != nil && !errors.Is(err, os.ErrNotExist) {
			s.log.Warn("could not remove the file of a deleted job", "job", id, "err", err)
		}
	}
	syncDir(dir)
}

// persistSync saves now, before an admin request is answered (DESIGN 9).
// A failure is logged and shown in HiveInfo.Warnings; the change stays in
// memory and is retried by the background loop.
func (s *Server) persistSync() {
	s.mu.Lock()
	s.dirty = true
	s.mu.Unlock()
	_ = s.persist(false)
}

// maybePersist writes dirty state respecting the minimum write interval:
// max(2 s, 10x the last write), 30 s in RAM or on vfat. Liveness-only
// changes go to live.json, at most every 60 s.
func (s *Server) maybePersist() {
	if !s.persistMu.TryLock() {
		return
	}
	defer s.persistMu.Unlock()
	gap := minPersistGap
	if s.cfg.tune.minPersist > 0 {
		gap = s.cfg.tune.minPersist
	}
	if g := 10 * s.lastWriteDur; g > gap {
		gap = g
	}
	if (s.data.ram || s.data.vfat) && s.cfg.tune.minPersist == 0 && gap < slowFSPersist {
		gap = slowFSPersist
	}
	s.mu.Lock()
	dirty, live := s.dirty, s.liveDirty
	s.mu.Unlock()
	if dirty && time.Since(s.lastWrite) >= gap {
		_ = s.persistHeld(false)
		return
	}
	if live && time.Since(s.lastLiveWrite) >= s.cfg.tune.livenessPersist {
		s.persistLive()
	}
}

// liveRecord is the liveness of one node, saved in live.json.
type liveRecord struct {
	ID       string        `json:"id"`
	LastSeen time.Time     `json:"last_seen"`
	Metrics  proto.Metrics `json:"metrics"`
	Addr     string        `json:"addr,omitempty"`
}

type liveSnapshot struct {
	Format  int          `json:"format"`
	SavedAt time.Time    `json:"saved_at"`
	Nodes   []liveRecord `json:"nodes"`
}

// persistLive saves the nodes' liveness fields (last_seen, metrics,
// address) to live.json, a file of O(nodes) bytes: heartbeats must not
// rewrite state.json, which holds every unsettled job and the blob index.
// Best effort: a failure is retried at the next interval.
func (s *Server) persistLive() {
	s.mu.Lock()
	if !s.liveDirty {
		s.mu.Unlock()
		return
	}
	ls := liveSnapshot{Format: snapshotFormat, SavedAt: s.now(), Nodes: make([]liveRecord, 0, len(s.nodes))}
	for _, n := range s.nodes {
		ls.Nodes = append(ls.Nodes, liveRecord{ID: n.ID, LastSeen: n.LastSeen, Metrics: n.Metrics, Addr: n.Addr})
	}
	s.liveDirty = false
	s.mu.Unlock()
	sort.Slice(ls.Nodes, func(i, k int) bool { return ls.Nodes[i].ID < ls.Nodes[k].ID })
	b, err := json.Marshal(ls)
	if err == nil {
		err = writeFileAtomic(filepath.Join(s.data.path, liveFile), append(b, '\n'), 0o600)
	}
	s.lastLiveWrite = time.Now()
	if err != nil {
		s.mu.Lock()
		s.liveDirty = true
		s.mu.Unlock()
		s.log.Warn("saving node liveness failed", "err", err)
	}
}

// applyLiveLocked restores liveness saved in live.json after state.json,
// where it is newer. A missing or damaged file only loses liveness.
func (s *Server) applyLiveLocked() {
	b, err := os.ReadFile(filepath.Join(s.data.path, liveFile))
	if err != nil {
		return
	}
	var ls liveSnapshot
	if err := json.Unmarshal(b, &ls); err != nil || ls.Format != snapshotFormat {
		s.log.Warn("ignoring unreadable live.json", "err", err)
		return
	}
	for _, r := range ls.Nodes {
		if n := s.nodes[r.ID]; n != nil && r.LastSeen.After(n.LastSeen) {
			n.LastSeen, n.Metrics, n.Addr = r.LastSeen, r.Metrics, r.Addr
		}
	}
}

// jsonWriter streams a JSON document one record at a time. Encoding the
// whole state with one Encode call builds it in a single buffer first
// (several times the state's size at the retention limits) and leaves that
// buffer in encoding/json's pool afterwards.
type jsonWriter struct {
	bw  *bufio.Writer
	enc *json.Encoder
	err error
}

func newJSONWriter(w io.Writer) *jsonWriter {
	bw := bufio.NewWriterSize(w, 256<<10)
	return &jsonWriter{bw: bw, enc: json.NewEncoder(bw)}
}

// reset starts a new document on dst.
func (w *jsonWriter) reset(dst io.Writer) {
	w.bw.Reset(dst)
	w.err = nil
}

func (w *jsonWriter) raw(s string) {
	if w.err == nil {
		_, w.err = w.bw.WriteString(s)
	}
}

func (w *jsonWriter) value(v any) {
	if w.err == nil {
		w.err = w.enc.Encode(v)
	}
}

// open writes v, a struct that encodes as a non-empty JSON object, without
// its closing brace, so fields can follow.
func (w *jsonWriter) open(v any) {
	if w.err != nil {
		return
	}
	b, err := json.Marshal(v)
	if err != nil || len(b) < 3 || b[len(b)-1] != '}' {
		w.err = fmt.Errorf("encode %T: %v", v, err)
		return
	}
	_, w.err = w.bw.Write(b[:len(b)-1])
}

// writeArray writes ,"name":[...] with each element encoded on its own.
func writeArray[T any](w *jsonWriter, name string, items []T, each func(*T)) {
	w.raw(`,"` + name + `":[`)
	for i := range items {
		if i > 0 {
			w.raw(",")
		}
		each(&items[i])
	}
	w.raw("]")
}

// job writes a jobSnapshot, one task record at a time.
func (w *jsonWriter) job(js *jobSnapshot) {
	w.open(&js.jobRecord)
	writeArray(w, "tasks", js.Tasks, func(t *taskRecord) { w.value(t) })
	w.raw("}")
}

// settledJob writes a settled job straight from its records, in the
// jobSnapshot format.
func (w *jsonWriter) settledJob(j *job) {
	w.open(&j.jobRecord)
	writeArray(w, "tasks", j.tasks, func(t **task) { w.value(&(*t).taskRecord) })
	w.raw("}")
}

func (w *jsonWriter) flush() error {
	if w.err == nil {
		w.err = w.bw.Flush()
	}
	return w.err
}

// writeSnapshot writes state.json.tmp, fsyncs it, keeps the current file
// as state.json.prev and renames the new one into place.
func writeSnapshot(dir string, snap *snapshot) error {
	cur := filepath.Join(dir, stateFile)
	tmp := cur + ".tmp"
	prev := cur + ".prev"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	w := newJSONWriter(f)
	w.open(&snap.snapshotHeader)
	writeArray(w, "nodes", snap.Nodes, func(n *nodeRecord) { w.value(n) })
	writeArray(w, "jobs", snap.Jobs, w.job)
	writeArray(w, "walls", snap.Walls, func(x *proto.WallSpec) { w.value(x) })
	writeArray(w, "blobs", snap.Blobs, func(b *blobRecord) { w.value(b) })
	w.raw("}\n")
	if err := w.flush(); err != nil {
		f.Close()
		return fmt.Errorf("encode state: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if _, err := os.Stat(cur); err == nil {
		os.Remove(prev)
		// A hard link keeps state.json in place at every instant; file
		// systems without links (vfat) get a rename instead.
		if err := os.Link(cur, prev); err != nil {
			if err := os.Rename(cur, prev); err != nil {
				return fmt.Errorf("keep previous state: %w", err)
			}
		}
	}
	if err := os.Rename(tmp, cur); err != nil {
		return err
	}
	syncDir(dir)
	return nil
}

// writeJobFile writes the settled job j to jobs/<id>.json through a synced
// temp file, encoding with w.
func writeJobFile(dir string, j *job, saved time.Time, w *jsonWriter) error {
	f, err := os.CreateTemp(dir, "."+j.ID+".json.tmp*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	ok := false
	defer func() {
		if !ok {
			f.Close()
			os.Remove(tmp)
		}
	}()
	if err := f.Chmod(0o600); err != nil {
		return err
	}
	w.reset(f)
	w.open(&jobFile{Format: snapshotFormat, SavedAt: saved})
	w.raw(`,"job":`)
	w.settledJob(j)
	w.raw("}\n")
	if err := w.flush(); err != nil {
		return fmt.Errorf("encode job %s: %w", j.ID, err)
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, filepath.Join(dir, j.ID+".json")); err != nil {
		return err
	}
	ok = true
	return nil
}

func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync() // not supported everywhere (Windows); best effort
		d.Close()
	}
}
