package hive

import (
	"sort"
	"time"

	"github.com/platteration/ewastesavior/internal/proto"
)

// Persisted records follow one rule: maps, slices and pointers inside them
// are never modified in place, only replaced. That lets the snapshot copy
// records shallowly under the lock and serialize them outside it. The few
// slices that are appended to (FailedNodes, History) are cloned by the
// snapshot.

// nodeRecord is the persisted part of a node (DESIGN 9 "Node records").
type nodeRecord struct {
	ID            string             `json:"id"`
	Name          string             `json:"name"`
	AdminName     bool               `json:"admin_name,omitempty"`
	RequestedName string             `json:"requested_name,omitempty"`
	ConfigLabels  map[string]string  `json:"config_labels,omitempty"`
	AdminLabels   map[string]string  `json:"admin_labels,omitempty"`
	Roles         []proto.Role       `json:"roles"`
	Display       *proto.DisplaySpec `json:"display,omitempty"`
	NodeRotate    int                `json:"node_rotate"`
	AdminRotate   *int               `json:"admin_rotate,omitempty"`
	Drain         bool               `json:"drain,omitempty"`
	Approved      bool               `json:"approved"`
	Denied        bool               `json:"denied,omitempty"` // an admin revoked approval; open joins don't re-approve
	Keyless       bool               `json:"keyless,omitempty"`
	Quarantine    string             `json:"quarantine,omitempty"`
	HWIDs         []string           `json:"hw_ids,omitempty"`
	FirstSeen     time.Time          `json:"first_seen"`
	LastSeen      time.Time          `json:"last_seen"`
	Inventory     proto.Inventory    `json:"inventory"`
	Metrics       proto.Metrics      `json:"metrics"`
	BootID        string             `json:"boot_id,omitempty"`
	WallID        string             `json:"wall_id,omitempty"`
	Version       string             `json:"version,omitempty"`
	Addr          string             `json:"addr,omitempty"`
	ScratchInRAM  bool               `json:"scratch_in_ram,omitempty"`
	Sandbox       string             `json:"sandbox,omitempty"`
	SandboxCaps   []string           `json:"sandbox_caps,omitempty"`
	Total         proto.Resources    `json:"total"`
	Warnings      []string           `json:"warnings,omitempty"`
}

// node is a node record plus in-process runtime state.
type node struct {
	nodeRecord
	tokenHash   string
	lastHB      time.Time // monotonic; zero = not seen since the hive started
	online      bool      // liveness as of the last loop pass
	status      proto.NodeStatus
	held        map[string]heldTask // task ID -> charged assignment
	claimID     string
	claimTasks  []proto.TaskRef
	claims      int
	actions     []*action
	nodeErrs    []time.Time          // node errors (monotonic) on tasks that then succeeded elsewhere, within the quarantine window
	fastFails   map[string]time.Time // task ID -> failure time, for tasks that ran < 10 s
	reservedFor string
	shortCode   string // proto.ShortCode(ID), for node references
}

// heldTask is an assignment whose resources are charged to a node. It
// outlives the task's live state for canceled and hive-failed tasks until
// the node stops listing them (DESIGN 8.4).
type heldTask struct {
	lease  string
	charge proto.Resources
}

type action struct {
	proto.ActionDirective
	created time.Time // monotonic
}

func newNode(rec nodeRecord) *node {
	return &node{nodeRecord: rec, held: map[string]heldTask{}, fastFails: map[string]time.Time{},
		shortCode: proto.ShortCode(rec.ID)}
}

func (n *node) isOnline(now time.Time, offlineAfter time.Duration) bool {
	return !n.lastHB.IsZero() && now.Sub(n.lastHB) < offlineAfter
}

func (n *node) hasRole(r proto.Role) bool { return proto.HasRole(n.Roles, r) }

// effectiveLabels are config labels overridden key by key by admin labels.
func (n *node) effectiveLabels() map[string]string {
	if len(n.ConfigLabels) == 0 && len(n.AdminLabels) == 0 {
		return nil
	}
	out := make(map[string]string, len(n.ConfigLabels)+len(n.AdminLabels))
	for k, v := range n.ConfigLabels {
		out[k] = v
	}
	for k, v := range n.AdminLabels {
		out[k] = v
	}
	return out
}

func (n *node) effectiveRotate() int {
	if n.AdminRotate != nil {
		return *n.AdminRotate
	}
	return n.NodeRotate
}

var fullIsolationCaps = []string{"mountns", "pidns", "netns", "ipcns", "utsns", "cgroup2", "seccomp", "nnp", "privdrop"}

// fullIsolation reports whether the node's sandbox has every capability
// DESIGN 6.5 requires for isolation=full.
func (n *node) fullIsolation() bool {
	if n.Sandbox == "none" {
		return false
	}
	for _, c := range fullIsolationCaps {
		if !containsStr(n.SandboxCaps, c) {
			return false
		}
	}
	return true
}

// allocated sums the resources charged to the node.
func (n *node) allocated() proto.Resources {
	var r proto.Resources
	for _, h := range n.held {
		r = r.Add(h.charge)
	}
	return r
}

// charge is what a task's resources cost a node: with scratch in RAM the
// disk quota is memory.
func charge(need proto.Resources, scratchInRAM bool) proto.Resources {
	if scratchInRAM {
		return proto.Resources{Cores: need.Cores, MemMB: need.MemMB + need.DiskMB}
	}
	return need
}

// jobRecord is the persisted part of a job.
type jobRecord struct {
	ID         string        `json:"id"`
	Seq        uint64        `json:"seq"`
	Spec       proto.JobSpec `json:"spec"`
	CreatedAt  time.Time     `json:"created_at"`
	StartedAt  *time.Time    `json:"started_at,omitempty"`
	FinishedAt *time.Time    `json:"finished_at,omitempty"`
	NextIndex  int           `json:"next_index"`
	Canceled   bool          `json:"canceled,omitempty"`
	// DoneSeq is assigned from a persisted counter when the job finishes or
	// is canceled; retention deletes the jobs that finished first.
	DoneSeq uint64 `json:"done_seq,omitempty"`
}

// job is a job with its lazily created task records.
type job struct {
	jobRecord
	counts   proto.TaskCounts
	tasks    []*task // records, in index order
	requeued []*task // pending records, FIFO
}

func (j *job) adjust(st proto.TaskState, d int) {
	c := &j.counts
	switch st {
	case proto.TaskPending:
		c.Pending += d
	case proto.TaskAssigned:
		c.Assigned += d
	case proto.TaskRunning:
		c.Running += d
	case proto.TaskSucceeded:
		c.Succeeded += d
	case proto.TaskFailed:
		c.Failed += d
	case proto.TaskCanceled:
		c.Canceled += d
	}
}

func (j *job) finished() bool {
	c := j.counts
	return c.Succeeded+c.Failed+c.Canceled >= j.Spec.Count
}

func (j *job) state() proto.JobState {
	if j.Canceled {
		return proto.JobCanceled
	}
	if j.finished() {
		switch {
		case j.counts.Failed > 0:
			return proto.JobFailed
		case j.counts.Canceled > 0:
			return proto.JobCanceled
		}
		return proto.JobSucceeded
	}
	if j.StartedAt == nil {
		return proto.JobQueued
	}
	return proto.JobRunning
}

func (j *job) undispatched() int { return j.Spec.Count - j.NextIndex }

func (j *job) retries() int {
	if j.Spec.Retries == nil {
		return proto.DefaultRetries
	}
	return *j.Spec.Retries
}

// queueLess orders jobs by priority (desc), then Seq (asc).
func queueLess(a, b *job) bool {
	if a.Spec.Priority != b.Spec.Priority {
		return a.Spec.Priority > b.Spec.Priority
	}
	return a.Seq < b.Seq
}

// taskRecord is the persisted part of a task.
type taskRecord struct {
	ID              string              `json:"id"`
	JobID           string              `json:"job_id"`
	Index           int                 `json:"index"`
	State           proto.TaskState     `json:"state"`
	Node            string              `json:"node,omitempty"`
	Lease           string              `json:"lease,omitempty"`
	Attempt         int                 `json:"attempt"`
	Failures        int                 `json:"failures,omitempty"`
	NodeErrors      int                 `json:"node_errors,omitempty"`
	Interruptions   int                 `json:"interruptions,omitempty"`
	ExitCode        *int                `json:"exit_code,omitempty"`
	ErrorKind       string              `json:"error_kind,omitempty"`
	Error           string              `json:"error,omitempty"`
	Outputs         []proto.Output      `json:"outputs,omitempty"`
	AssignedAt      *time.Time          `json:"assigned_at,omitempty"`
	StartedAt       *time.Time          `json:"started_at,omitempty"`
	FinishedAt      *time.Time          `json:"finished_at,omitempty"`
	RunS            float64             `json:"run_s,omitempty"`
	CPUSeconds      float64             `json:"cpu_seconds,omitempty"`
	MaxMemMB        int                 `json:"max_mem_mb,omitempty"`
	FailedNodes     []string            `json:"failed_nodes,omitempty"`
	History         []proto.AttemptView `json:"history,omitempty"`
	CancelRequested bool                `json:"cancel_requested,omitempty"`
	// DoneLease/DoneState remember the last terminal report processed, so
	// a replay of it is answered 200 (DESIGN 8.4).
	DoneLease string          `json:"done_lease,omitempty"`
	DoneState proto.TaskState `json:"done_state,omitempty"`
	// The log tail file holds stream bytes [LogBase, LogEnd) of the last
	// attempt that produced output.
	LogBase int64 `json:"log_base,omitempty"`
	LogEnd  int64 `json:"log_end,omitempty"`
}

// task is a task record plus runtime state of its current assignment.
type task struct {
	taskRecord
	job          *job
	assignedMono time.Time
	seen         bool // listed in the node's running_tasks this assignment
	unconfirmed  bool // restored after a hive restart, not yet re-adopted
	phase        string
	runS         float64
	xfer         int64
	xferMono     time.Time
	uploaded     int64                // bytes charged against leaseUploadCap this lease
	fastFailedOn map[string]time.Time // node ID -> when this task failed there within 10 s
	nodeErrOn    map[string]nodeErr   // node ID -> the node error this task had there
	log          *logRing
	tail         []byte // last log tail, until written to disk
}

// nodeErr is a node error a task had on some node, kept as possible
// evidence against that node until the task succeeds elsewhere.
type nodeErr struct {
	at        time.Time // monotonic
	kind, msg string
}

func (t *task) active() bool { return t.State == proto.TaskAssigned || t.State == proto.TaskRunning }

// liveFor reports whether (nodeID, lease) is the task's current, wanted
// assignment.
func (t *task) liveFor(nodeID, lease string) bool {
	return t.active() && !t.CancelRequested && t.Node == nodeID && t.Lease == lease && lease != ""
}

// bucket is the state the task is counted and shown as: cancel-requested
// assignments are shown as canceled.
func (t *task) bucket() proto.TaskState {
	if t.CancelRequested && t.active() {
		return proto.TaskCanceled
	}
	return t.State
}

// blobRecord is the persisted metadata of a stored blob.
type blobRecord struct {
	SHA256      string    `json:"sha256"`
	Size        int64     `json:"size"`
	CreatedAt   time.Time `json:"created_at"`
	LastTouched time.Time `json:"last_touched"`
	Width       int       `json:"width,omitempty"`
	Height      int       `json:"height,omitempty"`
	NodeUpload  bool      `json:"node_upload,omitempty"`
}

type blob struct {
	blobRecord
	touchedMono  time.Time
	uploadedMono time.Time
}

type session struct {
	expires     time.Time // monotonic
	expiresWall time.Time
}

type pairCode struct {
	hash        string
	code        string // kept only for the screen code
	expires     time.Time
	expiresWall time.Time
}

type reservation struct {
	taskID string
	nodeID string
	since  time.Time // monotonic
}

type otherHive struct {
	url  string
	seen time.Time // monotonic
}

// snapshot is the persisted state (state.json).
type snapshot struct {
	Format  int       `json:"format"`
	HiveID  string    `json:"hive_id"`
	SavedAt time.Time `json:"saved_at"`
	NextSeq uint64    `json:"next_seq"`
	// NextDoneSeq is the next jobRecord.DoneSeq.
	NextDoneSeq uint64           `json:"next_done_seq,omitempty"`
	Nodes       []nodeRecord     `json:"nodes"`
	Jobs        []jobSnapshot    `json:"jobs"`
	Walls       []proto.WallSpec `json:"walls"`
	Blobs       []blobRecord     `json:"blobs"`
}

type jobSnapshot struct {
	jobRecord
	Tasks []taskRecord `json:"tasks,omitempty"`
}

const snapshotFormat = 1

// snapshotLocked copies the state for serialization outside the lock.
func (s *Server) snapshotLocked() *snapshot {
	snap := &snapshot{Format: snapshotFormat, HiveID: s.hiveID, SavedAt: s.now(), NextSeq: s.nextSeq, NextDoneSeq: s.nextDoneSeq}
	snap.Nodes = make([]nodeRecord, 0, len(s.nodes))
	for _, n := range s.nodes {
		snap.Nodes = append(snap.Nodes, n.nodeRecord)
	}
	sort.Slice(snap.Nodes, func(i, k int) bool { return snap.Nodes[i].ID < snap.Nodes[k].ID })
	snap.Jobs = make([]jobSnapshot, 0, len(s.jobs))
	for _, j := range s.jobs {
		js := jobSnapshot{jobRecord: j.jobRecord, Tasks: make([]taskRecord, len(j.tasks))}
		for i, t := range j.tasks {
			r := t.taskRecord
			r.FailedNodes = append([]string(nil), r.FailedNodes...)
			r.History = append([]proto.AttemptView(nil), r.History...)
			js.Tasks[i] = r
		}
		snap.Jobs = append(snap.Jobs, js)
	}
	sort.Slice(snap.Jobs, func(i, k int) bool { return snap.Jobs[i].Seq < snap.Jobs[k].Seq })
	snap.Walls = make([]proto.WallSpec, 0, len(s.walls))
	for _, w := range s.walls {
		snap.Walls = append(snap.Walls, *w)
	}
	sort.Slice(snap.Walls, func(i, k int) bool { return snap.Walls[i].ID < snap.Walls[k].ID })
	snap.Blobs = make([]blobRecord, 0, len(s.blobMeta))
	for _, b := range s.blobMeta {
		snap.Blobs = append(snap.Blobs, b.blobRecord)
	}
	sort.Slice(snap.Blobs, func(i, k int) bool { return snap.Blobs[i].SHA256 < snap.Blobs[k].SHA256 })
	return snap
}

func containsStr(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func timePtr(t time.Time) *time.Time { return &t }
