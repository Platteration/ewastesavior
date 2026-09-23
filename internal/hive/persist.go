package hive

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/platteration/ewastesavior/internal/proto"
)

const (
	stateFile     = "state.json"
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
// main file is missing or unreadable (DESIGN 9).
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
	s.mu.Lock()
	defer s.mu.Unlock()
	if snap != nil {
		s.applySnapshotLocked(snap)
	}
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

// applySnapshotLocked rebuilds in-memory state from a snapshot. Counters
// are recomputed rather than trusted; assigned tasks become unconfirmed
// until their nodes re-register (DESIGN 8.6).
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
	var orphans []*task
	maxSeq, maxDoneSeq := uint64(0), uint64(0)
	for _, js := range snap.Jobs {
		if js.ID == "" || s.jobs[js.ID] != nil || js.Spec.Count < 1 {
			continue
		}
		j := &job{jobRecord: js.jobRecord}
		if j.NextIndex > j.Spec.Count {
			j.NextIndex = j.Spec.Count
		}
		if j.Seq > maxSeq {
			maxSeq = j.Seq
		}
		if j.DoneSeq > maxDoneSeq {
			maxDoneSeq = j.DoneSeq
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
					t.FinishedAt = timePtr(snap.SavedAt)
				}
			case t.active():
				t.unconfirmed = true
				t.assignedMono = now
				t.xferMono = now
				if n := s.nodes[t.Node]; n != nil {
					n.held[t.ID] = heldTask{lease: t.Lease, charge: charge(j.Spec.Resources, n.ScratchInRAM)}
				} else {
					orphans = append(orphans, t)
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
		if !j.finished() {
			s.queue = append(s.queue, j)
		} else if j.FinishedAt == nil {
			j.FinishedAt = timePtr(snap.SavedAt)
		}
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

// persist writes the state when it's dirty (or always with force).
func (s *Server) persist(force bool) error {
	s.persistMu.Lock()
	defer s.persistMu.Unlock()
	return s.persistHeld(force)
}

// persistHeld is persist with persistMu held.
func (s *Server) persistHeld(force bool) error {
	s.mu.Lock()
	if !force && !s.dirty && !s.liveDirty {
		s.mu.Unlock()
		return nil
	}
	snap := s.snapshotLocked()
	s.dirty, s.liveDirty = false, false
	s.mu.Unlock()

	start := time.Now()
	err := writeSnapshot(s.data.path, snap)
	s.lastWrite = time.Now()
	s.lastWriteDur = s.lastWrite.Sub(start)

	s.mu.Lock()
	if err != nil {
		s.dirty = true
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
// max(2 s, 10x the last write), 30 s in RAM or on vfat; liveness-only
// changes at most every 60 s.
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
	since := time.Since(s.lastWrite)
	liveGap := s.cfg.tune.livenessPersist
	if liveGap < gap {
		liveGap = gap
	}
	if (dirty && since >= gap) || (live && since >= liveGap) {
		_ = s.persistHeld(false)
	}
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
	bw := bufio.NewWriterSize(f, 256<<10)
	if err := json.NewEncoder(bw).Encode(snap); err != nil {
		f.Close()
		return fmt.Errorf("encode state: %w", err)
	}
	if err := bw.Flush(); err != nil {
		f.Close()
		return err
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

func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync() // not supported everywhere (Windows); best effort
		d.Close()
	}
}
