// Package proto defines the wire types exchanged between the hive, node
// agents and the operator CLI. See docs/DESIGN.md section 7.
//
// Everything here is plain data: no behavior beyond small helpers, so every
// other package can depend on it without pulling anything else in.
package proto

import (
	"fmt"
	"strings"
	"time"
)

// APIVersion is bumped on incompatible protocol changes.
const APIVersion = 1

// Well-known ports.
const (
	DefaultPort   = 7700 // hive HTTPS
	DiscoveryPort = 7701 // UDP beacons and probes
)

// Discovery service identifiers.
const (
	BeaconService = "savior-hive"
	ProbeService  = "savior-probe"
)

// Role is a function a node performs in the swarm.
type Role string

const (
	RoleCompute Role = "compute"
	RoleDisplay Role = "display"
	RoleHive    Role = "hive"
)

// HasRole reports whether r is in roles.
func HasRole(roles []Role, r Role) bool {
	for _, x := range roles {
		if x == r {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Hardware inventory and live metrics

// Inventory is the static hardware description of a node, collected once at
// agent start (see internal/hwinfo).
type Inventory struct {
	Hostname      string    `json:"hostname"`
	Arch          string    `json:"arch"`       // GOARCH style: amd64, 386, arm64, arm
	MachineArch   string    `json:"machine"`    // uname -m: x86_64, i686, ...
	Kernel        string    `json:"kernel"`     // kernel release
	OSVersion     string    `json:"os_version"` // SaviorOS version (from /etc/savior-release) or ""
	CPUModel      string    `json:"cpu_model"`  // "Intel(R) Pentium(R) M processor 1.60GHz"
	CPUVendor     string    `json:"cpu_vendor"` // GenuineIntel, AuthenticAMD, ...
	CPUFlags      []string  `json:"cpu_flags"`  // subset of interest: lm pae nx sse sse2 sse3 ssse3 sse4_1 sse4_2 avx avx2 aes vmx svm hypervisor
	Cores         int       `json:"cores"`      // online logical CPUs
	PhysicalCores int       `json:"physical_cores"`
	CPUMHz        int       `json:"cpu_mhz"` // max frequency if known, else current
	MemTotalMB    int       `json:"mem_total_mb"`
	SwapTotalMB   int       `json:"swap_total_mb"`
	Disks         []Disk    `json:"disks,omitempty"`
	NICs          []NIC     `json:"nics,omitempty"`
	Displays      []Display `json:"displays,omitempty"`
	GPUs          []GPU     `json:"gpus,omitempty"`
	HasBattery    bool      `json:"has_battery"`
	IsLaptop      bool      `json:"is_laptop"` // DMI chassis type or battery present
	Vendor        string    `json:"vendor"`    // DMI sys_vendor
	Product       string    `json:"product"`   // DMI product_name (+ product_version for Lenovo)
	BIOSDate      string    `json:"bios_date"`
	Virtualized   bool      `json:"virtualized"`
	BenchScore    int       `json:"bench_score"` // single-core SHA-256 throughput in MB/s (rough speed indicator)
}

// Disk is a block device (not partitions).
type Disk struct {
	Name       string `json:"name"` // sda, nvme0n1, mmcblk0
	SizeMB     int64  `json:"size_mb"`
	Model      string `json:"model,omitempty"`
	Rotational bool   `json:"rotational"`
	Removable  bool   `json:"removable"`
	Transport  string `json:"transport,omitempty"` // usb, sata, nvme, ... when known
}

// NIC is a network interface.
type NIC struct {
	Name     string   `json:"name"`
	MAC      string   `json:"mac"`
	Wireless bool     `json:"wireless"`
	Bus      string   `json:"bus,omitempty"`        // pci, usb, virtual
	SpeedMb  int      `json:"speed_mbps,omitempty"` // link speed when up and known
	Up       bool     `json:"up"`
	Addrs    []string `json:"addrs,omitempty"` // CIDR strings
}

// Display is a framebuffer device.
type Display struct {
	Name   string `json:"name"`   // fb0
	Driver string `json:"driver"` // /sys/class/graphics/fb0/name, e.g. "i915drmfb", "VESA VGA"
	Width  int    `json:"width"`
	Height int    `json:"height"`
	BPP    int    `json:"bpp"`
}

// GPU is a DRM device.
type GPU struct {
	Card   string `json:"card"`   // card0
	Driver string `json:"driver"` // i915, radeon, nouveau, amdgpu, bochs-drm
	Vendor string `json:"vendor"` // PCI vendor ID, e.g. 0x8086
	Device string `json:"device"` // PCI device ID
}

// HasCPUFlag reports whether the inventory lists flag.
func (inv *Inventory) HasCPUFlag(flag string) bool {
	for _, f := range inv.CPUFlags {
		if f == flag {
			return true
		}
	}
	return false
}

// Metrics are live measurements sampled at each heartbeat.
type Metrics struct {
	Time           time.Time `json:"time"`
	UptimeS        int64     `json:"uptime_s"`
	Load1          float64   `json:"load1"`
	CPUPercent     float64   `json:"cpu_percent"` // whole machine, 0-100, since previous sample
	MemAvailableMB int       `json:"mem_available_mb"`
	TempC          float64   `json:"temp_c"`          // hottest thermal zone / hwmon sensor; 0 = unknown
	OnBattery      bool      `json:"on_battery"`      // true only when positively known
	BatteryPercent int       `json:"battery_percent"` // -1 = no battery
	BatteryStatus  string    `json:"battery_status,omitempty"`
	LidClosed      bool      `json:"lid_closed"`
	NetRxBytes     uint64    `json:"net_rx_bytes"`
	NetTxBytes     uint64    `json:"net_tx_bytes"`
}

// ---------------------------------------------------------------------------
// Resources

// Resources describes CPU/memory/disk amounts.
type Resources struct {
	Cores  float64 `json:"cores"`
	MemMB  int     `json:"mem_mb"`
	DiskMB int     `json:"disk_mb"`
}

// Fits reports whether need fits into r (disk is advisory and not checked
// when either side is zero).
func (r Resources) Fits(need Resources) bool {
	const eps = 1e-9
	if need.Cores > r.Cores+eps || need.MemMB > r.MemMB {
		return false
	}
	if need.DiskMB > 0 && r.DiskMB > 0 && need.DiskMB > r.DiskMB {
		return false
	}
	return true
}

// Sub returns r - o.
func (r Resources) Sub(o Resources) Resources {
	return Resources{Cores: r.Cores - o.Cores, MemMB: r.MemMB - o.MemMB, DiskMB: r.DiskMB - o.DiskMB}
}

// Add returns r + o.
func (r Resources) Add(o Resources) Resources {
	return Resources{Cores: r.Cores + o.Cores, MemMB: r.MemMB + o.MemMB, DiskMB: r.DiskMB + o.DiskMB}
}

// ---------------------------------------------------------------------------
// Discovery

// Beacon is broadcast by the hive on UDP DiscoveryPort.
type Beacon struct {
	Svc         string `json:"svc"` // BeaconService
	V           int    `json:"v"`   // APIVersion
	HiveID      string `json:"hive_id"`
	Port        int    `json:"port"`
	Fingerprint string `json:"fp"`
	SwarmHint   string `json:"swarm_hint"`
	Version     string `json:"version"`
}

// Probe is broadcast by nodes looking for a hive.
type Probe struct {
	Svc string `json:"svc"` // ProbeService
	V   int    `json:"v"`
}

// ---------------------------------------------------------------------------
// Join handshake

// Hello is returned by GET /api/v1/hello.
type Hello struct {
	HiveID     string    `json:"hive_id"`
	APIVersion int       `json:"api_version"`
	Version    string    `json:"version"`
	Nonce      string    `json:"nonce"`
	Time       time.Time `json:"time"`
	SwarmHint  string    `json:"swarm_hint"`
}

// RegisterRequest is POSTed to /api/v1/register.
type RegisterRequest struct {
	NodeID    string            `json:"node_id"`
	Name      string            `json:"name"` // configured/default name; hive keeps an admin-set name if any
	Roles     []Role            `json:"roles"`
	Labels    map[string]string `json:"labels,omitempty"`
	Version   string            `json:"version"`
	Inventory Inventory         `json:"inventory"`
	Total     Resources         `json:"total"` // allocatable resources
	HiveNonce string            `json:"hive_nonce"`
	NodeNonce string            `json:"node_nonce"`
	Proof     string            `json:"proof"`
}

// RegisterResponse is the reply to a successful registration.
type RegisterResponse struct {
	NodeID             string     `json:"node_id"`
	Name               string     `json:"name"` // effective name
	Token              string     `json:"token"`
	HiveProof          string     `json:"hive_proof"`
	HeartbeatIntervalS int        `json:"heartbeat_interval_s"`
	Time               time.Time  `json:"time"`
	Directives         Directives `json:"directives"`
}

// ---------------------------------------------------------------------------
// Heartbeat

// NodeState is the node's self-reported state.
type NodeState string

const (
	NodeIdle     NodeState = "idle"
	NodeBusy     NodeState = "busy"
	NodePaused   NodeState = "paused"   // power/thermal policy
	NodeDraining NodeState = "draining" // admin drain; finishing current tasks
)

// NodeStatus is sent in every heartbeat.
type NodeStatus struct {
	State        NodeState    `json:"state"`
	Reason       string       `json:"reason,omitempty"` // why paused / not accepting
	Metrics      Metrics      `json:"metrics"`
	Total        Resources    `json:"total"`
	Free         Resources    `json:"free"`
	RunningTasks []string     `json:"running_tasks"`
	Display      DisplayState `json:"display"`
	Addrs        []string     `json:"addrs,omitempty"`
}

// HeartbeatRequest is POSTed to /api/v1/heartbeat.
type HeartbeatRequest struct {
	Status NodeStatus `json:"status"`
}

// HeartbeatResponse carries the hive's instructions.
type HeartbeatResponse struct {
	Time       time.Time  `json:"time"`
	Directives Directives `json:"directives"`
}

// Directives tell a node what the hive wants. Display is the complete
// desired display spec (nodes compare Rev to detect changes).
type Directives struct {
	Name        string       `json:"name,omitempty"` // effective name (after admin rename)
	Drain       bool         `json:"drain"`
	CancelTasks []string     `json:"cancel_tasks,omitempty"`
	Display     *DisplaySpec `json:"display,omitempty"` // nil = keep local default
	Action      string       `json:"action,omitempty"`  // one-shot: "identify", "reboot", "poweroff"
	ActionID    string       `json:"action_id,omitempty"`
}

// Node actions.
const (
	ActionIdentify = "identify"
	ActionReboot   = "reboot"
	ActionPoweroff = "poweroff"
)

// ---------------------------------------------------------------------------
// Display

// Display modes.
const (
	DisplayStatus    = "status"
	DisplayOff       = "off"
	DisplayColor     = "color"
	DisplayText      = "text"
	DisplayClock     = "clock"
	DisplayImage     = "image"
	DisplaySlideshow = "slideshow"
	DisplayDashboard = "dashboard"
	DisplayWall      = "wall"
	DisplayTest      = "test"
)

// DisplayModes lists all valid DisplaySpec modes.
var DisplayModes = []string{DisplayStatus, DisplayOff, DisplayColor, DisplayText, DisplayClock,
	DisplayImage, DisplaySlideshow, DisplayDashboard, DisplayWall, DisplayTest}

// Media references an image: a hive blob (preferred) or a URL.
type Media struct {
	Blob string `json:"blob,omitempty"` // sha256 hex
	URL  string `json:"url,omitempty"`
}

// Key returns a cache key for m.
func (m Media) Key() string {
	if m.Blob != "" {
		return "blob:" + m.Blob
	}
	return "url:" + m.URL
}

// DisplaySpec is the desired content of a node's screen.
type DisplaySpec struct {
	Rev         int64     `json:"rev"` // bumped by the hive on every change
	Mode        string    `json:"mode"`
	Title       string    `json:"title,omitempty"`
	Text        string    `json:"text,omitempty"`
	FG          string    `json:"fg,omitempty"` // "#rrggbb"; default white
	BG          string    `json:"bg,omitempty"` // "#rrggbb"; default black
	Image       *Media    `json:"image,omitempty"`
	Images      []Media   `json:"images,omitempty"`
	IntervalS   int       `json:"interval_s,omitempty"` // slideshow; default 10
	Fit         string    `json:"fit,omitempty"`        // contain (default), cover, stretch
	Rotate      int       `json:"rotate,omitempty"`     // 0, 90, 180, 270
	ClockFormat string    `json:"clock_format,omitempty"`
	Wall        *WallTile `json:"wall,omitempty"`
}

// WallTile is one screen's position in a video wall.
type WallTile struct {
	WallID  string       `json:"wall_id"`
	Rows    int          `json:"rows"`
	Cols    int          `json:"cols"`
	Row     int          `json:"row"`
	Col     int          `json:"col"`
	BezelPx int          `json:"bezel_px,omitempty"` // pixels hidden behind each screen edge
	Content *DisplaySpec `json:"content"`            // image, slideshow, text or color spec spread over the wall
}

// DisplayState is what the node reports about its screen.
type DisplayState struct {
	Active bool   `json:"active"` // a framebuffer is being driven
	Device string `json:"device,omitempty"`
	Width  int    `json:"width,omitempty"`
	Height int    `json:"height,omitempty"`
	Mode   string `json:"mode,omitempty"`
	Rev    int64  `json:"rev,omitempty"`
	Error  string `json:"error,omitempty"`
}

// WallSpec defines a video wall made of several display nodes.
type WallSpec struct {
	ID      string      `json:"id"`
	Name    string      `json:"name"`
	Rows    int         `json:"rows"`
	Cols    int         `json:"cols"`
	BezelPx int         `json:"bezel_px,omitempty"`
	Cells   []WallCell  `json:"cells"`
	Content DisplaySpec `json:"content"` // mode must be image, slideshow, text or color
}

// WallCell places a node in the wall grid.
type WallCell struct {
	Node string `json:"node"` // node ID (names are resolved to IDs on save)
	Row  int    `json:"row"`
	Col  int    `json:"col"`
}

// ---------------------------------------------------------------------------
// Jobs and tasks

// Job kinds.
const (
	KindExec   = "exec"
	KindScript = "script"
)

// Input is a file placed in the task working directory before it starts.
type Input struct {
	Name       string `json:"name"` // relative path inside the workdir
	Blob       string `json:"blob,omitempty"`
	URL        string `json:"url,omitempty"`
	SHA256     string `json:"sha256,omitempty"` // required with URL
	Executable bool   `json:"executable,omitempty"`
}

// Requirements restrict which nodes may run a task.
type Requirements struct {
	Arch     []string          `json:"arch,omitempty"`       // any of (GOARCH names)
	MinMemMB int               `json:"min_mem_mb,omitempty"` // node total memory
	CPUFlags []string          `json:"cpu_flags,omitempty"`  // all of
	Labels   map[string]string `json:"labels,omitempty"`     // all equal
	Nodes    []string          `json:"nodes,omitempty"`      // node IDs or names, any of
}

// JobSpec is what an operator submits.
type JobSpec struct {
	Name         string            `json:"name,omitempty"`
	Kind         string            `json:"kind,omitempty"`    // exec | script (inferred when empty)
	Command      []string          `json:"command,omitempty"` // exec: argv; supports {{index}} {{count}}
	Script       string            `json:"script,omitempty"`  // script: /bin/sh script body
	Env          map[string]string `json:"env,omitempty"`
	Inputs       []Input           `json:"inputs,omitempty"`
	Outputs      []string          `json:"outputs,omitempty"` // glob patterns relative to workdir
	Resources    Resources         `json:"resources"`
	Requirements Requirements      `json:"requirements"`
	TimeoutS     int               `json:"timeout_s,omitempty"`
	Retries      *int              `json:"retries,omitempty"` // nil = default (1)
	Network      bool              `json:"network,omitempty"`
	Count        int               `json:"count,omitempty"`
	Priority     int               `json:"priority,omitempty"`
}

// Job states.
type JobState string

const (
	JobQueued    JobState = "queued"
	JobRunning   JobState = "running"
	JobSucceeded JobState = "succeeded"
	JobFailed    JobState = "failed"
	JobCanceled  JobState = "canceled"
)

// Terminal reports whether s is final.
func (s JobState) Terminal() bool {
	return s == JobSucceeded || s == JobFailed || s == JobCanceled
}

// Task states.
type TaskState string

const (
	TaskPending   TaskState = "pending"
	TaskAssigned  TaskState = "assigned"
	TaskRunning   TaskState = "running"
	TaskSucceeded TaskState = "succeeded"
	TaskFailed    TaskState = "failed"
	TaskCanceled  TaskState = "canceled"
	// Report-only states: the hive turns these back into pending.
	TaskPreempted TaskState = "preempted"
	TaskLost      TaskState = "lost"
)

// Terminal reports whether s is final.
func (s TaskState) Terminal() bool {
	return s == TaskSucceeded || s == TaskFailed || s == TaskCanceled
}

// Task is one unit of work as delivered to a node. Command/Script already
// have {{index}}/{{count}} substituted and Env includes the SAVIOR_* vars.
type Task struct {
	ID        string            `json:"id"`
	JobID     string            `json:"job_id"`
	JobName   string            `json:"job_name,omitempty"`
	Index     int               `json:"index"`
	Count     int               `json:"count"`
	Attempt   int               `json:"attempt"` // 1-based
	Kind      string            `json:"kind"`
	Command   []string          `json:"command,omitempty"`
	Script    string            `json:"script,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
	Inputs    []Input           `json:"inputs,omitempty"`
	Outputs   []string          `json:"outputs,omitempty"`
	Resources Resources         `json:"resources"`
	TimeoutS  int               `json:"timeout_s"`
	Network   bool              `json:"network"`
}

// Output is a file produced by a task and stored as a hive blob.
type Output struct {
	Name string `json:"name"` // path relative to workdir
	Blob string `json:"blob"`
	Size int64  `json:"size"`
}

// TaskReport is POSTed by the node to /api/v1/tasks/{id}/report.
// State is running (start notification), succeeded, failed, canceled or preempted.
type TaskReport struct {
	State      TaskState `json:"state"`
	ExitCode   int       `json:"exit_code"`
	Error      string    `json:"error,omitempty"`
	Outputs    []Output  `json:"outputs,omitempty"`
	StartedAt  time.Time `json:"started_at,omitempty"`
	FinishedAt time.Time `json:"finished_at,omitempty"`
	CPUSeconds float64   `json:"cpu_seconds,omitempty"`
	MaxMemMB   int       `json:"max_mem_mb,omitempty"`
}

// ClaimRequest is POSTed to /api/v1/claim.
type ClaimRequest struct {
	Free  Resources `json:"free"`
	Max   int       `json:"max"`    // max tasks to return (default 1)
	WaitS int       `json:"wait_s"` // long-poll seconds (0-30)
}

// ClaimResponse carries the tasks assigned to the node (possibly none).
type ClaimResponse struct {
	Tasks []Task `json:"tasks"`
}

// ---------------------------------------------------------------------------
// Admin views

// NodeLiveness is the hive's view of reachability.
type NodeLiveness string

const (
	NodeOnline  NodeLiveness = "online"
	NodeOffline NodeLiveness = "offline"
)

// NodeView is the admin view of a node.
type NodeView struct {
	ID           string            `json:"id"`
	Name         string            `json:"name"`
	Roles        []Role            `json:"roles"`
	Labels       map[string]string `json:"labels,omitempty"`
	Liveness     NodeLiveness      `json:"liveness"`
	Addr         string            `json:"addr"` // remote IP as seen by the hive
	Version      string            `json:"version"`
	FirstSeen    time.Time         `json:"first_seen"`
	LastSeen     time.Time         `json:"last_seen"`
	Drain        bool              `json:"drain"`
	Inventory    Inventory         `json:"inventory"`
	Status       NodeStatus        `json:"status"`
	Display      *DisplaySpec      `json:"display,omitempty"` // desired
	Allocated    Resources         `json:"allocated"`         // hive-side accounting of assigned tasks
	RunningTasks []string          `json:"running_tasks"`     // hive-side assignment
}

// NodePatch updates a node. Nil fields are unchanged.
type NodePatch struct {
	Name    *string            `json:"name,omitempty"`
	Labels  *map[string]string `json:"labels,omitempty"` // replaces all labels
	Drain   *bool              `json:"drain,omitempty"`
	Display *DisplaySpec       `json:"display,omitempty"` // rev is assigned by the hive
}

// NodeAction requests a one-shot action on a node.
type NodeAction struct {
	Action  string `json:"action"`            // identify | reboot | poweroff
	Seconds int    `json:"seconds,omitempty"` // identify duration (default 30)
}

// TaskCounts summarizes task states within a job.
type TaskCounts struct {
	Pending   int `json:"pending"`
	Assigned  int `json:"assigned"`
	Running   int `json:"running"`
	Succeeded int `json:"succeeded"`
	Failed    int `json:"failed"`
	Canceled  int `json:"canceled"`
}

// JobView is the admin summary of a job.
type JobView struct {
	ID         string     `json:"id"`
	Spec       JobSpec    `json:"spec"`
	State      JobState   `json:"state"`
	Counts     TaskCounts `json:"counts"`
	CreatedAt  time.Time  `json:"created_at"`
	StartedAt  *time.Time `json:"started_at,omitempty"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	Warning    string     `json:"warning,omitempty"`
}

// TaskView is the admin view of a task.
type TaskView struct {
	ID         string     `json:"id"`
	JobID      string     `json:"job_id"`
	Index      int        `json:"index"`
	State      TaskState  `json:"state"`
	Node       string     `json:"node,omitempty"` // node ID currently/last assigned
	Attempts   int        `json:"attempts"`
	ExitCode   int        `json:"exit_code"`
	Error      string     `json:"error,omitempty"`
	Outputs    []Output   `json:"outputs,omitempty"`
	AssignedAt *time.Time `json:"assigned_at,omitempty"`
	StartedAt  *time.Time `json:"started_at,omitempty"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	CPUSeconds float64    `json:"cpu_seconds,omitempty"`
	MaxMemMB   int        `json:"max_mem_mb,omitempty"`
}

// JobDetail is a job with all its tasks.
type JobDetail struct {
	JobView
	Tasks []TaskView `json:"tasks"`
}

// BlobInfo describes a stored blob.
type BlobInfo struct {
	SHA256    string    `json:"sha256"`
	Size      int64     `json:"size"`
	CreatedAt time.Time `json:"created_at"`
}

// SwarmStats are aggregate numbers for dashboards.
type SwarmStats struct {
	Time             time.Time      `json:"time"`
	NodesOnline      int            `json:"nodes_online"`
	NodesOffline     int            `json:"nodes_offline"`
	NodesByState     map[string]int `json:"nodes_by_state"` // idle/busy/paused/draining (online only)
	Cores            int            `json:"cores"`          // online logical CPUs
	CoresAllocatable float64        `json:"cores_allocatable"`
	CoresInUse       float64        `json:"cores_in_use"`
	MemMB            int            `json:"mem_mb"`
	BenchTotal       int            `json:"bench_total"` // sum of bench scores (online)
	Displays         int            `json:"displays"`
	JobsByState      map[string]int `json:"jobs_by_state"`
	TasksByState     map[string]int `json:"tasks_by_state"`
	TasksCompleted   int64          `json:"tasks_completed"` // since hive start
	CPUSecondsTotal  float64        `json:"cpu_seconds_total"`
}

// HiveInfo describes the hive (admin).
type HiveInfo struct {
	HiveID      string    `json:"hive_id"`
	Version     string    `json:"version"`
	APIVersion  int       `json:"api_version"`
	Fingerprint string    `json:"fingerprint"`
	SwarmHint   string    `json:"swarm_hint"`
	StartedAt   time.Time `json:"started_at"`
	Listen      string    `json:"listen"`
	DataDir     string    `json:"data_dir"`
}

// ErrorResponse is the body of every non-2xx JSON response.
type ErrorResponse struct {
	Error string `json:"error"`
}

// ---------------------------------------------------------------------------
// Helpers

// ValidSHA256 reports whether s is 64 lowercase hex characters.
func ValidSHA256(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// ValidRelPath reports whether p is a safe relative path for use inside a
// task working directory: non-empty, not absolute, no "..", no NUL, no
// backslashes, no leading "./" segments resolving outside.
func ValidRelPath(p string) bool {
	if p == "" || len(p) > 255 || strings.ContainsAny(p, "\x00\\") || strings.HasPrefix(p, "/") {
		return false
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." || seg == "" {
			return false
		}
	}
	return true
}

// ParseColor parses "#rrggbb" or "#rgb". It returns ok=false for anything else.
func ParseColor(s string) (r, g, b uint8, ok bool) {
	if len(s) == 0 || s[0] != '#' {
		return 0, 0, 0, false
	}
	h := s[1:]
	hexv := func(c byte) (uint8, bool) {
		switch {
		case c >= '0' && c <= '9':
			return c - '0', true
		case c >= 'a' && c <= 'f':
			return c - 'a' + 10, true
		case c >= 'A' && c <= 'F':
			return c - 'A' + 10, true
		}
		return 0, false
	}
	switch len(h) {
	case 3:
		var v [3]uint8
		for i := 0; i < 3; i++ {
			x, ok := hexv(h[i])
			if !ok {
				return 0, 0, 0, false
			}
			v[i] = x*16 + x
		}
		return v[0], v[1], v[2], true
	case 6:
		var v [3]uint8
		for i := 0; i < 3; i++ {
			a, ok1 := hexv(h[2*i])
			b, ok2 := hexv(h[2*i+1])
			if !ok1 || !ok2 {
				return 0, 0, 0, false
			}
			v[i] = a*16 + b
		}
		return v[0], v[1], v[2], true
	}
	return 0, 0, 0, false
}

// ValidateDisplaySpec checks a display spec for structural errors.
func ValidateDisplaySpec(s *DisplaySpec) error {
	ok := false
	for _, m := range DisplayModes {
		if s.Mode == m {
			ok = true
		}
	}
	if !ok {
		return fmt.Errorf("unknown display mode %q", s.Mode)
	}
	for _, c := range []string{s.FG, s.BG} {
		if c != "" {
			if _, _, _, ok := ParseColor(c); !ok {
				return fmt.Errorf("invalid color %q (want #rrggbb)", c)
			}
		}
	}
	switch s.Rotate {
	case 0, 90, 180, 270:
	default:
		return fmt.Errorf("rotate must be 0, 90, 180 or 270")
	}
	switch s.Fit {
	case "", "contain", "cover", "stretch":
	default:
		return fmt.Errorf("fit must be contain, cover or stretch")
	}
	checkMedia := func(m Media) error {
		if (m.Blob == "") == (m.URL == "") {
			return fmt.Errorf("media needs exactly one of blob or url")
		}
		if m.Blob != "" && !ValidSHA256(m.Blob) {
			return fmt.Errorf("invalid blob hash %q", m.Blob)
		}
		if m.URL != "" && !strings.HasPrefix(m.URL, "http://") && !strings.HasPrefix(m.URL, "https://") {
			return fmt.Errorf("media url must be http(s)")
		}
		return nil
	}
	switch s.Mode {
	case DisplayImage:
		if s.Image == nil {
			return fmt.Errorf("image mode needs image")
		}
		if err := checkMedia(*s.Image); err != nil {
			return err
		}
	case DisplaySlideshow:
		if len(s.Images) == 0 {
			return fmt.Errorf("slideshow mode needs images")
		}
		for _, m := range s.Images {
			if err := checkMedia(m); err != nil {
				return err
			}
		}
		if s.IntervalS < 0 {
			return fmt.Errorf("interval_s must be >= 0")
		}
	case DisplayWall:
		w := s.Wall
		if w == nil || w.Content == nil {
			return fmt.Errorf("wall mode needs wall.content")
		}
		if w.Rows < 1 || w.Cols < 1 || w.Rows > 16 || w.Cols > 16 || w.Row < 0 || w.Col < 0 || w.Row >= w.Rows || w.Col >= w.Cols {
			return fmt.Errorf("invalid wall geometry")
		}
		switch w.Content.Mode {
		case DisplayImage, DisplaySlideshow, DisplayText, DisplayColor:
		default:
			return fmt.Errorf("wall content mode must be image, slideshow, text or color")
		}
		if err := ValidateDisplaySpec(w.Content); err != nil {
			return fmt.Errorf("wall content: %w", err)
		}
	}
	return nil
}
