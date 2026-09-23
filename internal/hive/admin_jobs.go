package hive

import (
	"archive/zip"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/platteration/ewastesavior/internal/auth"
	"github.com/platteration/ewastesavior/internal/proto"
)

const (
	defaultListLimit = 100
	maxListLimit     = 1000
)

func (s *Server) jobViewLocked(j *job) proto.JobView {
	return proto.JobView{
		ID:         j.ID,
		Seq:        j.Seq,
		Name:       j.Spec.Name,
		Count:      j.Spec.Count,
		Priority:   j.Spec.Priority,
		State:      j.state(),
		Counts:     j.counts,
		CreatedAt:  j.CreatedAt,
		StartedAt:  j.StartedAt,
		FinishedAt: j.FinishedAt,
		Warning:    s.jobWarningLocked(j),
	}
}

func (s *Server) taskViewLocked(t *task, now time.Time) proto.TaskView {
	v := proto.TaskView{
		ID:            t.ID,
		JobID:         t.JobID,
		Index:         t.Index,
		State:         t.bucket(),
		Node:          t.Node,
		Attempt:       t.Attempt,
		Failures:      t.Failures,
		NodeErrors:    t.NodeErrors,
		Interruptions: t.Interruptions,
		ExitCode:      t.ExitCode,
		ErrorKind:     t.ErrorKind,
		Error:         t.Error,
		WaitReason:    s.waitReasonLocked(t, now),
		Outputs:       t.Outputs,
		AssignedAt:    t.AssignedAt,
		StartedAt:     t.StartedAt,
		FinishedAt:    t.FinishedAt,
		RunS:          t.RunS,
		CPUSeconds:    t.CPUSeconds,
		MaxMemMB:      t.MaxMemMB,
		FailedNodes:   append([]string(nil), t.FailedNodes...),
		History:       append([]proto.AttemptView(nil), t.History...),
	}
	if t.active() && t.runS > v.RunS {
		v.RunS = t.runS
	}
	return v
}

func queryInt(r *http.Request, key string, def, min, max int) (int, error) {
	v := r.URL.Query().Get(key)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < min {
		return 0, fmt.Errorf("invalid %s", key)
	}
	if n > max {
		n = max
	}
	return n, nil
}

func (s *Server) handleJobs(w http.ResponseWriter, r *http.Request, _ adminCtx) {
	limit, err := queryInt(r, "limit", defaultListLimit, 1, maxListLimit)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "%s", err.Error())
		return
	}
	var before uint64
	if v := r.URL.Query().Get("before"); v != "" {
		if before, err = strconv.ParseUint(v, 10, 64); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid before")
			return
		}
	}
	state := proto.JobState(r.URL.Query().Get("state"))
	s.mu.Lock()
	jobs := make([]*job, 0, len(s.jobs))
	for _, j := range s.jobs {
		if (before == 0 || j.Seq < before) && (state == "" || j.state() == state) {
			jobs = append(jobs, j)
		}
	}
	sort.Slice(jobs, func(a, b int) bool { return jobs[a].Seq > jobs[b].Seq })
	if len(jobs) > limit {
		jobs = jobs[:limit]
	}
	out := make([]proto.JobView, len(jobs))
	for i, j := range jobs {
		out[i] = s.jobViewLocked(j)
	}
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, out)
}

// handleJobSubmit normalizes, validates and stores a job, persisting it
// before answering (DESIGN 7.2, 9).
func (s *Server) handleJobSubmit(w http.ResponseWriter, r *http.Request, _ adminCtx) {
	var spec proto.JobSpec
	if !decodeJSON(w, r, &spec, true) {
		return
	}
	proto.ApplyJobDefaults(&spec)
	if err := proto.ValidateJobSpec(&spec); err != nil {
		writeErr(w, http.StatusBadRequest, "%s", err.Error())
		return
	}
	s.mu.Lock()
	if len(s.queue) >= maxActiveJobs {
		s.mu.Unlock()
		writeErr(w, http.StatusServiceUnavailable, "too many unfinished jobs")
		return
	}
	for _, in := range spec.Inputs {
		if in.Blob != "" && s.blobMeta[in.Blob] == nil {
			s.mu.Unlock()
			writeErr(w, http.StatusBadRequest, "input %s: blob %s not found (upload it first)", in.Name, in.Blob)
			return
		}
	}
	if len(spec.Requirements.Nodes) > 0 {
		ids, err := s.resolveRequirementNodesLocked(spec.Requirements.Nodes)
		if err != nil {
			s.mu.Unlock()
			writeErr(w, http.StatusBadRequest, "%s", err.Error())
			return
		}
		spec.Requirements.Nodes = ids
	}
	id := "j" + auth.NewID(8)
	for s.jobs[id] != nil {
		id = "j" + auth.NewID(8)
	}
	j := &job{jobRecord: jobRecord{ID: id, Seq: s.nextSeq, Spec: spec, CreatedAt: s.now()}}
	s.nextSeq++
	j.counts.Pending = spec.Count
	s.jobs[id] = j
	i := sort.Search(len(s.queue), func(k int) bool { return queueLess(j, s.queue[k]) })
	s.queue = append(s.queue, nil)
	copy(s.queue[i+1:], s.queue[i:])
	s.queue[i] = j
	s.dirty = true
	s.notifyLocked()
	detail := proto.JobDetail{JobView: s.jobViewLocked(j), Spec: j.Spec}
	s.mu.Unlock()
	s.persistSync()
	s.log.Info("job submitted", "job", id, "name", spec.Name, "count", spec.Count)
	writeJSON(w, http.StatusOK, detail)
}

// resolveRequirementNodesLocked maps node IDs or names to IDs (DESIGN 6.4).
func (s *Server) resolveRequirementNodesLocked(refs []string) ([]string, error) {
	var ids []string
	for _, ref := range refs {
		byID := s.nodes[ref]
		var byName *node
		lower := strings.ToLower(ref)
		for _, n := range s.nodes {
			if n.Name == lower {
				byName = n
			}
		}
		switch {
		case byID != nil && byName != nil && byID != byName:
			return nil, fmt.Errorf("node %q is ambiguous (an ID of one node and the name of another)", proto.Sanitize(ref, 64, false))
		case byID != nil:
			ids = append(ids, byID.ID)
		case byName != nil:
			ids = append(ids, byName.ID)
		default:
			return nil, fmt.Errorf("unknown node %q in requirements", proto.Sanitize(ref, 64, false))
		}
	}
	return ids, nil
}

func (s *Server) handleJobGet(w http.ResponseWriter, r *http.Request, _ adminCtx) {
	s.mu.Lock()
	j := s.jobs[r.PathValue("id")]
	if j == nil {
		s.mu.Unlock()
		writeErr(w, http.StatusNotFound, "no such job")
		return
	}
	d := proto.JobDetail{JobView: s.jobViewLocked(j), Spec: j.Spec}
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, d)
}

func (s *Server) handleJobTasks(w http.ResponseWriter, r *http.Request, _ adminCtx) {
	offset, err := queryInt(r, "offset", 0, 0, int(^uint(0)>>1))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "%s", err.Error())
		return
	}
	limit, err := queryInt(r, "limit", defaultListLimit, 1, maxListLimit)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "%s", err.Error())
		return
	}
	state := proto.TaskState(r.URL.Query().Get("state"))
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	j := s.jobs[r.PathValue("id")]
	if j == nil {
		writeErr(w, http.StatusNotFound, "no such job")
		return
	}
	var match []*task
	for _, t := range j.tasks {
		if state == "" || t.bucket() == state {
			match = append(match, t)
		}
	}
	page := proto.TaskPage{Tasks: []proto.TaskView{}, Total: len(match), Undispatched: j.undispatched()}
	if offset < len(match) {
		end := offset + limit
		if end > len(match) {
			end = len(match)
		}
		for _, t := range match[offset:end] {
			page.Tasks = append(page.Tasks, s.taskViewLocked(t, now))
		}
		if end < len(match) {
			page.NextOffset = end
		}
	}
	writeJSON(w, http.StatusOK, page)
}

func (s *Server) jobOutputsLocked(j *job) []proto.OutputEntry {
	out := []proto.OutputEntry{}
	for _, t := range j.tasks {
		for _, o := range t.Outputs {
			out = append(out, proto.OutputEntry{TaskID: t.ID, Index: t.Index, Name: o.Name, Blob: o.Blob, Size: o.Size})
		}
	}
	return out
}

func (s *Server) handleJobOutputs(w http.ResponseWriter, r *http.Request, _ adminCtx) {
	s.mu.Lock()
	j := s.jobs[r.PathValue("id")]
	if j == nil {
		s.mu.Unlock()
		writeErr(w, http.StatusNotFound, "no such job")
		return
	}
	out := s.jobOutputsLocked(j)
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, out)
}

// handleJobOutputsZip streams every output as task-<index>/<name>, stored
// uncompressed (DESIGN 7.2). A missing blob is a 500 before anything is
// sent; a failure after the 200 aborts the connection, so a client never
// takes an incomplete archive for a complete one.
func (s *Server) handleJobOutputsZip(w http.ResponseWriter, r *http.Request, _ adminCtx) {
	s.mu.Lock()
	j := s.jobs[r.PathValue("id")]
	if j == nil {
		s.mu.Unlock()
		writeErr(w, http.StatusNotFound, "no such job")
		return
	}
	entries := s.jobOutputsLocked(j)
	jobID := j.ID
	now := time.Now()
	for _, e := range entries {
		if b := s.blobMeta[e.Blob]; b != nil {
			s.touchLocked(b)
		}
	}
	s.mu.Unlock()
	var total int64
	for _, e := range entries {
		if _, err := os.Stat(s.blobs.path(e.Blob)); err != nil {
			s.log.Warn("outputs.zip: blob missing", "job", jobID, "blob", e.Blob, "err", err)
			writeErr(w, http.StatusInternalServerError, "output blob missing: task %d %s (%s)", e.Index, e.Name, e.Blob)
			return
		}
		total += e.Size
	}
	setDeadlines(w, blobDeadline(total))
	h := w.Header()
	h.Set("Content-Type", "application/zip")
	h.Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s-outputs.zip"`, jobID))
	h.Set("Content-Security-Policy", blobCSP)
	w.WriteHeader(http.StatusOK)
	zw := zip.NewWriter(w)
	for _, e := range entries {
		f, err := os.Open(s.blobs.path(e.Blob))
		if err != nil {
			s.log.Warn("outputs.zip: blob vanished while streaming", "job", jobID, "blob", e.Blob, "err", err)
			// The 200 is already sent and closing the zip would make a
			// valid archive without this entry: abort the connection.
			panic(http.ErrAbortHandler)
		}
		fw, err := zw.CreateHeader(&zip.FileHeader{
			Name:     path.Join(fmt.Sprintf("task-%d", e.Index), e.Name),
			Method:   zip.Store,
			Modified: now,
		})
		if err == nil {
			_, err = io.CopyN(fw, f, e.Size) // a short blob is an error too
		}
		f.Close()
		if err != nil {
			panic(http.ErrAbortHandler)
		}
	}
	if err := zw.Close(); err != nil {
		panic(http.ErrAbortHandler)
	}
}

func (s *Server) handleJobCancel(w http.ResponseWriter, r *http.Request, _ adminCtx) {
	s.mu.Lock()
	j := s.jobs[r.PathValue("id")]
	if j == nil {
		s.mu.Unlock()
		writeErr(w, http.StatusNotFound, "no such job")
		return
	}
	s.cancelJobLocked(j)
	v := s.jobViewLocked(j)
	s.mu.Unlock()
	s.log.Info("job canceled", "job", v.ID)
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) handleJobDelete(w http.ResponseWriter, r *http.Request, _ adminCtx) {
	s.mu.Lock()
	j := s.jobs[r.PathValue("id")]
	if j == nil {
		s.mu.Unlock()
		writeErr(w, http.StatusNotFound, "no such job")
		return
	}
	if !j.state().Terminal() {
		s.mu.Unlock()
		writeErr(w, http.StatusConflict, "job %s is %s; cancel it first", j.ID, j.state())
		return
	}
	s.deleteJobLocked(j)
	s.mu.Unlock()
	writeOK(w)
}

func (s *Server) handleTaskGet(w http.ResponseWriter, r *http.Request, _ adminCtx) {
	s.mu.Lock()
	t := s.tasks[r.PathValue("id")]
	if t == nil {
		s.mu.Unlock()
		writeErr(w, http.StatusNotFound, "no such task")
		return
	}
	v := s.taskViewLocked(t, time.Now())
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) handleTaskOutput(w http.ResponseWriter, r *http.Request, _ adminCtx) {
	name := r.PathValue("name")
	s.mu.Lock()
	t := s.tasks[r.PathValue("id")]
	if t == nil {
		s.mu.Unlock()
		writeErr(w, http.StatusNotFound, "no such task")
		return
	}
	var out *proto.Output
	for i := range t.Outputs {
		if t.Outputs[i].Name == name {
			o := t.Outputs[i]
			out = &o
		}
	}
	if out == nil {
		s.mu.Unlock()
		writeErr(w, http.StatusNotFound, "no such output")
		return
	}
	if b := s.blobMeta[out.Blob]; b != nil {
		s.touchLocked(b)
	}
	s.mu.Unlock()
	s.serveBlobFile(w, r, out.Blob, path.Base(out.Name), out.Size)
}
