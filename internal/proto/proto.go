// Package proto defines the wire types exchanged between the hive, node
// agents and the operator CLI, plus the validation rules both sides share.
// See docs/DESIGN.md section 7.
//
// Everything here is plain data plus pure helpers, so every other package
// can depend on it without pulling anything else in.
package proto

import (
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
	// MinProbeSize is the minimum probe datagram size; probes are padded so
	// a beacon reply is never larger than the request (no amplification).
	MinProbeSize = 512
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

// Inventory is the static hardware description of a node, collected at
// agent start (see internal/hwinfo). All strings are sanitized by the hive
// (control characters stripped, length-capped) before storage.
type Inventory struct {
	Hostname      string      `json:"hostname"`
	Arch          string      `json:"arch"`       // GOARCH style: amd64, 386
	MachineArch   string      `json:"machine"`    // uname -m: x86_64, i686
	Kernel        string      `json:"kernel"`     // kernel release
	OSVersion     string      `json:"os_version"` // SaviorOS version (/etc/savior-release) or ""
	CPUModel      string      `json:"cpu_model"`
	CPUVendor     string      `json:"cpu_vendor"`     // GenuineIntel, AuthenticAMD, ...
	CPUFlags      []string    `json:"cpu_flags"`      // subset: lm pae nx sse sse2 sse3 ssse3 sse4_1 sse4_2 avx avx2 aes vmx svm hypervisor
	Cores         int         `json:"cores"`          // online logical CPUs
	PhysicalCores int         `json:"physical_cores"` // 0 = unknown
	CPUMHz        int         `json:"cpu_mhz"`        // max frequency if known, else current
	MemTotalMB    int         `json:"mem_total_mb"`
	SwapTotalMB   int         `json:"swap_total_mb"`
	Disks         []Disk      `json:"disks,omitempty"`
	NICs          []NIC       `json:"nics,omitempty"`
	Framebuffers  []FB        `json:"framebuffers,omitempty"`
	GPUs          []GPU       `json:"gpus,omitempty"`
	Connectors    []Connector `json:"connectors,omitempty"`
	HasBattery    bool        `json:"has_battery"`
	IsLaptop      bool        `json:"is_laptop"` // DMI chassis type, or battery present
	Vendor        string      `json:"vendor"`    // DMI sys_vendor
	Product       string      `json:"product"`   // DMI product_name (+ product_version for Lenovo)
	BIOSDate      string      `json:"bios_date"`
	Virtualized   bool        `json:"virtualized"`
	BenchScore    int         `json:"bench_score"`           // single-core SHA-256 MB/s (rough speed indicator)
	TempSensor    string      `json:"temp_sensor,omitempty"` // e.g. "coretemp", "k10temp", "acpitz"; "" = none
}

// Disk is a whole block device (not a partition).
type Disk struct {
	Name       string `json:"name"` // sda, nvme0n1, mmcblk0
	SizeMB     int64  `json:"size_mb"`
	Model      string `json:"model,omitempty"`
	Rotational bool   `json:"rotational"`
	Removable  bool   `json:"removable"`
	Transport  string `json:"transport,omitempty"` // usb, ata, nvme, mmc, virtio when known
}

// NIC is a network interface.
type NIC struct {
	Name     string   `json:"name"`
	MAC      string   `json:"mac"`
	Wireless bool     `json:"wireless"`
	Bus      string   `json:"bus,omitempty"`        // pci, usb, virtual
	Driver   string   `json:"driver,omitempty"`     // e1000e, r8169, iwlwifi
	SpeedMb  int      `json:"speed_mbps,omitempty"` // link speed when up and known
	Up       bool     `json:"up"`
	Carrier  bool     `json:"carrier"`
	Addrs    []string `json:"addrs,omitempty"` // CIDR strings
	Error    string   `json:"error,omitempty"` // e.g. "firmware missing: b43/ucode15.fw"
}

// FB is a framebuffer device.
type FB struct {
	Name   string `json:"name"`   // fb0
	Driver string `json:"driver"` // /sys/class/graphics/fb0/name, e.g. "i915drmfb", "VESA VGA", "simpledrmdrmfb"
	Width  int    `json:"width"`
	Height int    `json:"height"`
	BPP    int    `json:"bpp"`
}

// GPU is a DRM device.
type GPU struct {
	Card   string `json:"card"`   // card0
	Driver string `json:"driver"` // i915, radeon, nouveau, amdgpu, bochs-drm, simpledrm
	Vendor string `json:"vendor"` // PCI vendor ID, e.g. 0x8086
	Device string `json:"device"` // PCI device ID
}

// Connector is a DRM display output (/sys/class/drm/cardN-NAME).
type Connector struct {
	Name      string `json:"name"`                // "LVDS-1", "HDMI-A-1", "VGA-1"
	Card      string `json:"card"`                // "card0"
	Status    string `json:"status"`              // connected, disconnected, unknown
	Enabled   bool   `json:"enabled"`             // "enabled" in sysfs
	Preferred string `json:"preferred"`           // first line of "modes", e.g. "1920x1080"
	WidthMM   int    `json:"width_mm,omitempty"`  // physical size from EDID
	HeightMM  int    `json:"height_mm,omitempty"` // physical size from EDID
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
	SwapUsedMB     int       `json:"swap_used_mb"`
	MemPressure    float64   `json:"mem_pressure"` // PSI memory "some" avg10 (percent); 0 when unavailable
	// CPUTempC is the hottest CPU sensor (coretemp/k8temp/k10temp/via_cputemp,
	// else ACPI thermal zones). Implausible and stuck readings are ignored.
	// 0 = unknown.
	CPUTempC       float64 `json:"cpu_temp_c"`
	CPUTempLimitC  float64 `json:"cpu_temp_limit_c"` // effective pause threshold (config vs sensor max/crit); 0 = unknown
	ThrottleEvents uint64  `json:"throttle_events"`  // cumulative core+package thermal throttle count
	// OnBattery is true only when it's positively known: no Mains supply is
	// online, or (with no Mains supply present) a battery is Discharging.
	OnBattery            bool   `json:"on_battery"`
	BatteryPercent       int    `json:"battery_percent"` // -1 = no battery
	BatteryHealthPercent int    `json:"battery_health_percent,omitempty"`
	BatteryStatus        string `json:"battery_status,omitempty"`
	LidClosed            bool   `json:"lid_closed"`
	NetRxBytes           uint64 `json:"net_rx_bytes"`
	NetTxBytes           uint64 `json:"net_tx_bytes"`
}

// ---------------------------------------------------------------------------
// Resources

// Resources describes CPU/memory/disk amounts.
type Resources struct {
	Cores  float64 `json:"cores"`
	MemMB  int     `json:"mem_mb"`
	DiskMB int     `json:"disk_mb"`
}

// Fits reports whether need fits into r. Negative needs never fit. When
// scratchInRAM is true the node's scratch space is RAM (tmpfs), so disk is
// charged against memory: need.MemMB+need.DiskMB must fit into r.MemMB.
// Otherwise disk is checked only when r.DiskMB > 0.
func (r Resources) Fits(need Resources, scratchInRAM bool) bool {
	const eps = 1e-9
	if need.Cores < 0 || need.MemMB < 0 || need.DiskMB < 0 {
		return false
	}
	if need.Cores > r.Cores+eps {
		return false
	}
	if scratchInRAM {
		return need.MemMB+need.DiskMB <= r.MemMB
	}
	if need.MemMB > r.MemMB {
		return false
	}
	if r.DiskMB > 0 && need.DiskMB > r.DiskMB {
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

// Min returns the element-wise minimum of r and o.
func (r Resources) Min(o Resources) Resources {
	out := r
	if o.Cores < out.Cores {
		out.Cores = o.Cores
	}
	if o.MemMB < out.MemMB {
		out.MemMB = o.MemMB
	}
	if o.DiskMB < out.DiskMB {
		out.DiskMB = o.DiskMB
	}
	return out
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
	SwarmHint   string `json:"swarm_hint"` // 8 hex chars, see auth.SwarmHint
	Version     string `json:"version"`
}

// Probe is broadcast by nodes looking for a hive. Pad is filled so the
// datagram is at least MinProbeSize bytes.
type Probe struct {
	Svc string `json:"svc"` // ProbeService
	V   int    `json:"v"`
	Pad string `json:"pad"`
}

// HiveLink is the node's view of its connection to the hive. It's shown on
// the node's screen and console with a one-line fix for each state.
type HiveLink string

const (
	LinkNoNetwork           HiveLink = "no_network"           // no interface has an address
	LinkNoSwarmKey          HiveLink = "no_swarm_key"         // swarm_key not configured
	LinkSearching           HiveLink = "searching"            // waiting for a beacon
	LinkKeyMismatch         HiveLink = "key_mismatch"         // beacons seen, none with our swarm hint
	LinkUnreachable         HiveLink = "unreachable"          // hive found but HTTPS connection fails
	LinkFingerprintMismatch HiveLink = "fingerprint_mismatch" // hive_fingerprint pin doesn't match
	LinkVersionMismatch     HiveLink = "version_mismatch"     // API version differs
	LinkRateLimited         HiveLink = "rate_limited"         // hive answered 429
	LinkRejected            HiveLink = "rejected"             // proof rejected (wrong key) or join refused
	LinkPending             HiveLink = "pending"              // join_policy=approve and not yet approved
	LinkDuplicate           HiveLink = "duplicate"            // another online node has our node ID
	LinkConnected           HiveLink = "connected"
)

// ---------------------------------------------------------------------------
// Join handshake

// TimeSource says where the hive's clock came from.
const (
	TimeNTP        = "ntp"
	TimeAdmin      = "admin"       // set from an authenticated admin client's clock
	TimeRTC        = "rtc"         // hardware clock, never verified
	TimeBuildFloor = "build-floor" // clock was before the build date and was raised to it
)

// Hello is returned by GET /api/v1/hello (unauthenticated). Its Time is for
// display only; nodes must not set their clock from it.
type Hello struct {
	HiveID     string    `json:"hive_id"`
	APIVersion int       `json:"api_version"`
	Version    string    `json:"version"`
	Nonce      string    `json:"nonce"` // stateless, MAC'd, 60 s TTL, single use
	Time       time.Time `json:"time"`
}

// TaskRef identifies one assignment of a task.
type TaskRef struct {
	ID    string `json:"id"`
	Lease string `json:"lease"`
}

// Task phases on the node (RunningTask.Phase).
const (
	PhaseFetching  = "fetching"  // downloading inputs
	PhaseRunning   = "running"   // process executing
	PhaseFrozen    = "frozen"    // paused by thermal policy (timeout clock stopped)
	PhaseUploading = "uploading" // uploading outputs
	PhaseReporting = "reporting" // final report not yet acknowledged
)

// RunningTask is the node's view of one task it holds. A task stays listed
// from the moment the claim response is decoded until its final report is
// acknowledged (2xx or 409).
type RunningTask struct {
	ID        string  `json:"id"`
	Lease     string  `json:"lease"`
	Phase     string  `json:"phase"`
	RunS      float64 `json:"run_s"`      // unfrozen process seconds so far
	XferBytes int64   `json:"xfer_bytes"` // bytes fetched + uploaded so far (progress watchdog)
}

// RegisterRequest is POSTed to /api/v1/register over a connection pinned to
// the fingerprint the node saw on /hello.
type RegisterRequest struct {
	NodeID        string            `json:"node_id"`
	HWIDs         []string          `json:"hw_ids"`  // every identity candidate: "mac:001122334455", "uuid:...", "serial:..."
	BootID        string            `json:"boot_id"` // /proc/sys/kernel/random/boot_id (+ agent start nonce)
	Name          string            `json:"name"`    // configured/default name; an admin-set name wins
	Roles         []Role            `json:"roles"`
	Labels        map[string]string `json:"labels,omitempty"` // advisory; admin-set labels win
	Version       string            `json:"version"`
	Inventory     Inventory         `json:"inventory"`
	Total         Resources         `json:"total"` // allocatable resources
	ScratchInRAM  bool              `json:"scratch_in_ram"`
	Sandbox       string            `json:"sandbox"`      // effective mode: strict, auto, none
	SandboxCaps   []string          `json:"sandbox_caps"` // available isolation: mountns pidns netns ipcns utsns cgroup2 seccomp nnp privdrop
	DisplayRotate int               `json:"display_rotate"`
	// RunningTasks lets a restarted hive re-adopt work that survived it.
	RunningTasks []RunningTask `json:"running_tasks,omitempty"`
	HiveNonce    string        `json:"hive_nonce"`
	NodeNonce    string        `json:"node_nonce"`
	Proof        string        `json:"proof"`
}

// RegisterResponse is the reply to a successful registration.
type RegisterResponse struct {
	NodeID             string     `json:"node_id"`
	Name               string     `json:"name"`       // effective name
	ShortCode          string     `json:"short_code"` // 3-char code shown by identify
	Token              string     `json:"token"`
	HiveProof          string     `json:"hive_proof"`
	HeartbeatIntervalS int        `json:"heartbeat_interval_s"`
	Time               time.Time  `json:"time"`
	TimeSynced         bool       `json:"time_synced"` // hive clock is trustworthy (NTP or admin-set)
	Pending            bool       `json:"pending"`     // join_policy=approve and not yet approved
	Directives         Directives `json:"directives"`
	// AdoptedTasks lists which of RegisterRequest.RunningTasks the hive still
	// wants; the node must kill any other running task.
	AdoptedTasks []string `json:"adopted_tasks,omitempty"`
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
	State        NodeState     `json:"state"`
	Reason       string        `json:"reason,omitempty"` // why paused / not accepting
	Metrics      Metrics       `json:"metrics"`
	Total        Resources     `json:"total"`
	Free         Resources     `json:"free"`
	RunningTasks []RunningTask `json:"running_tasks"`
	Display      DisplayState  `json:"display"`
	Addrs        []string      `json:"addrs,omitempty"`
	AckedActions []string      `json:"acked_actions,omitempty"` // action IDs started or about to execute
}

// HeartbeatRequest is POSTed to /api/v1/heartbeat.
type HeartbeatRequest struct {
	Status NodeStatus `json:"status"`
}

// HeartbeatResponse carries the hive's instructions.
type HeartbeatResponse struct {
	Time       time.Time  `json:"time"`
	TimeSynced bool       `json:"time_synced"`
	Directives Directives `json:"directives"`
}

// Directives tell a node what the hive wants. They are complete desired
// state (not deltas), except Actions, which repeat until acknowledged.
type Directives struct {
	Name          string            `json:"name,omitempty"`   // effective name
	Labels        map[string]string `json:"labels,omitempty"` // effective labels
	Drain         bool              `json:"drain"`
	Pending       bool              `json:"pending"`                // not approved: no tasks, status screen only
	CancelTasks   []TaskRef         `json:"cancel_tasks,omitempty"` // repeated until the node stops listing them
	Display       *DisplaySpec      `json:"display,omitempty"`      // nil = keep local default
	DisplayRotate int               `json:"display_rotate"`         // effective rotation (admin override or node's own)
	Actions       []ActionDirective `json:"actions,omitempty"`
}

// ActionDirective is a one-shot action. The hive repeats it in every
// heartbeat response until the node lists its ID in AckedActions (at most
// 5 minutes). The node acks identify when it starts and reboot/poweroff in
// the heartbeat right before executing them; after a reboot the hive has
// already dropped the action, so it never loops.
type ActionDirective struct {
	ID      string `json:"id"`
	Action  string `json:"action"`            // identify | reboot | poweroff
	Seconds int    `json:"seconds,omitempty"` // identify duration (default 30, max 600)
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

// Media references an image: a hive blob (preferred) or a URL. For URLs an
// optional SHA256 pins the content.
type Media struct {
	Blob   string `json:"blob,omitempty"` // sha256 hex of a hive blob
	URL    string `json:"url,omitempty"`
	SHA256 string `json:"sha256,omitempty"`
	Width  int    `json:"width,omitempty"`  // filled in by the hive for blobs (informational)
	Height int    `json:"height,omitempty"` // filled in by the hive for blobs (informational)
}

// Key returns a cache key for m.
func (m Media) Key() string {
	if m.Blob != "" {
		return "blob:" + m.Blob
	}
	return "url:" + m.URL + "#" + m.SHA256
}

// DisplaySpec is the desired content of a node's screen. Rotation is not
// part of it: that's a property of how the monitor is mounted (see
// Directives.DisplayRotate).
type DisplaySpec struct {
	Rev         int64     `json:"rev"` // bumped by the hive on every change
	Mode        string    `json:"mode"`
	Title       string    `json:"title,omitempty"`
	Text        string    `json:"text,omitempty"`
	FG          string    `json:"fg,omitempty"` // "#rrggbb"; default white
	BG          string    `json:"bg,omitempty"` // "#rrggbb"; default black
	Image       *Media    `json:"image,omitempty"`
	Images      []Media   `json:"images,omitempty"`
	IntervalS   int       `json:"interval_s,omitempty"` // slideshow; 0 = default 10, else >= 3
	Fit         string    `json:"fit,omitempty"`        // contain (default), cover, stretch
	ClockFormat string    `json:"clock_format,omitempty"`
	Timezone    string    `json:"timezone,omitempty"` // IANA name; empty = node config
	Wall        *WallTile `json:"wall,omitempty"`
}

// WallTile tells one node which rectangle of a video wall it shows. The hive
// computes all geometry; the node maps the canvas rectangle (X, Y, W, H, in
// canvas units = millimeters) onto its whole (rotated) screen.
type WallTile struct {
	WallID  string       `json:"wall_id"`
	CanvasW int          `json:"canvas_w"`
	CanvasH int          `json:"canvas_h"`
	X       int          `json:"x"`
	Y       int          `json:"y"`
	W       int          `json:"w"`
	H       int          `json:"h"`
	Row     int          `json:"row"`
	Col     int          `json:"col"`
	Label   string       `json:"label,omitempty"` // "R1C2 lobby-3", drawn by the test pattern
	Content *DisplaySpec `json:"content"`         // image, slideshow, text, color or test, spread over the canvas
}

// DisplayState is what the node reports about its screen.
type DisplayState struct {
	Active      bool     `json:"active"` // a framebuffer is being driven
	Device      string   `json:"device,omitempty"`
	Driver      string   `json:"driver,omitempty"`
	Format      string   `json:"format,omitempty"` // XRGB8888, RGB565, XRGB1555, RGB888, BGR888, ...
	FBWidth     int      `json:"fb_width,omitempty"`
	FBHeight    int      `json:"fb_height,omitempty"`
	Width       int      `json:"width,omitempty"`  // logical size after rotation
	Height      int      `json:"height,omitempty"` // logical size after rotation
	Rotate      int      `json:"rotate"`
	Mode        string   `json:"mode,omitempty"`
	Rev         int64    `json:"rev,omitempty"`
	Ready       bool     `json:"ready"` // all media of the current spec loaded
	Loaded      int      `json:"loaded,omitempty"`
	Total       int      `json:"total,omitempty"`
	MediaErrors []string `json:"media_errors,omitempty"`
	Blanked     bool     `json:"blanked"`
	BlankReason string   `json:"blank_reason,omitempty"` // off, lid, idle, vt_away, error
	BlankMethod string   `json:"blank_method,omitempty"` // fbioblank, backlight, black
	Foreground  bool     `json:"foreground"`             // our VT is the active one
	RenderMS    int      `json:"render_ms,omitempty"`    // last full frame render time
	Warning     string   `json:"warning,omitempty"`
	Error       string   `json:"error,omitempty"`
}

// Rect is a rectangle in wall canvas units (millimeters).
type Rect struct {
	X int `json:"x"`
	Y int `json:"y"`
	W int `json:"w"`
	H int `json:"h"`
}

// WallSpec defines a video wall made of several display nodes.
//
// Layout: each cell's visible size comes from WidthMM/HeightMM, else the
// node's connected connector EDID size, else 400x300 mm. Column widths and
// row heights are the maximum of their cells; GapXMM/GapYMM are the
// distances between the visible areas of neighbors (bezels). A cell with
// Rect set is placed exactly there instead. The canvas is the bounding box.
type WallSpec struct {
	ID      string      `json:"id"`
	Name    string      `json:"name"`
	Rows    int         `json:"rows"`
	Cols    int         `json:"cols"`
	GapXMM  int         `json:"gap_x_mm,omitempty"`
	GapYMM  int         `json:"gap_y_mm,omitempty"`
	Cells   []WallCell  `json:"cells"`
	Content DisplaySpec `json:"content"` // mode image, slideshow, text, color or test
}

// WallCell places a node in the wall grid.
type WallCell struct {
	Node     string `json:"node"` // node ID (names are resolved to IDs on save)
	Row      int    `json:"row"`
	Col      int    `json:"col"`
	WidthMM  int    `json:"width_mm,omitempty"`
	HeightMM int    `json:"height_mm,omitempty"`
	Rect     *Rect  `json:"rect,omitempty"`
}

// ---------------------------------------------------------------------------
// Jobs and tasks

// Job kinds.
const (
	KindExec   = "exec"
	KindScript = "script"
)

// Isolation requirement values (Requirements.Isolation).
const (
	IsolationFull = "full" // default: node must run tasks with full sandbox (namespaces, cgroups, seccomp, priv drop)
	IsolationAny  = "any"  // run anywhere, including sandbox=none nodes
)

// Input is a file placed in the task working directory before it starts.
type Input struct {
	Name       string `json:"name"` // relative path inside the workdir
	Blob       string `json:"blob,omitempty"`
	URL        string `json:"url,omitempty"`
	SHA256     string `json:"sha256,omitempty"` // required with URL
	Size       int64  `json:"size,omitempty"`   // required with URL; the download aborts beyond it
	Executable bool   `json:"executable,omitempty"`
}

// Requirements restrict which nodes may run a task.
type Requirements struct {
	Arch      []string          `json:"arch,omitempty"`       // any of (GOARCH names)
	MinMemMB  int               `json:"min_mem_mb,omitempty"` // node total memory
	CPUFlags  []string          `json:"cpu_flags,omitempty"`  // all of
	Labels    map[string]string `json:"labels,omitempty"`     // all equal (effective labels)
	Nodes     []string          `json:"nodes,omitempty"`      // node IDs (names are resolved to IDs at submit)
	Isolation string            `json:"isolation,omitempty"`  // "" = full
}

// JobSpec is what an operator submits.
type JobSpec struct {
	Name         string            `json:"name,omitempty"`
	Kind         string            `json:"kind,omitempty"`    // exec | script (inferred when empty)
	Command      []string          `json:"command,omitempty"` // exec: argv; supports {{index}} {{count}}
	Script       string            `json:"script,omitempty"`  // script: /bin/sh script body
	Env          map[string]string `json:"env,omitempty"`
	Inputs       []Input           `json:"inputs,omitempty"`
	Outputs      []string          `json:"outputs,omitempty"` // glob patterns (path.Match) relative to workdir
	Resources    Resources         `json:"resources"`
	Requirements Requirements      `json:"requirements"`
	TimeoutS     int               `json:"timeout_s,omitempty"`
	Retries      *int              `json:"retries,omitempty"` // nil = default (1)
	Network      bool              `json:"network,omitempty"`
	Count        int               `json:"count,omitempty"`
	Priority     int               `json:"priority,omitempty"` // -1000..1000
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
	// TaskPreempted may be reported by nodes (battery, admin); the hive
	// requeues the task without consuming an attempt. TaskLost is internal
	// to the hive (node vanished); nodes can't report it.
	TaskPreempted TaskState = "preempted"
	TaskLost      TaskState = "lost"
)

// Terminal reports whether s is final.
func (s TaskState) Terminal() bool {
	return s == TaskSucceeded || s == TaskFailed || s == TaskCanceled
}

// Task is one assignment of a unit of work, as delivered to a node.
// Command/Script already have {{index}}/{{count}} substituted and Env
// includes the SAVIOR_* vars. Lease identifies this assignment: every
// report and log upload carries it, and the hive rejects stale leases.
type Task struct {
	ID        string            `json:"id"`
	Lease     string            `json:"lease"`
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
	Name string `json:"name"` // path relative to workdir (ValidRelPath)
	Blob string `json:"blob"`
	Size int64  `json:"size"`
}

// TaskReport is POSTed by the node to /api/v1/tasks/{id}/report. State is
// running (start notification), succeeded, failed, canceled or preempted.
// Reports are idempotent: a repeated final report for the same lease is
// acknowledged without changing anything.
type TaskReport struct {
	Lease      string    `json:"lease"`
	State      TaskState `json:"state"`
	ExitCode   int       `json:"exit_code"`
	ErrorKind  string    `json:"error_kind,omitempty"` // for failed: exit, timeout, input, sandbox, output, internal
	Error      string    `json:"error,omitempty"`
	Outputs    []Output  `json:"outputs,omitempty"`
	RunS       float64   `json:"run_s,omitempty"` // unfrozen process seconds
	CPUSeconds float64   `json:"cpu_seconds,omitempty"`
	MaxMemMB   int       `json:"max_mem_mb,omitempty"`
}

// Error kinds. Only exit and timeout consume an attempt; the others are node
// errors that requeue the task elsewhere (see DESIGN.md section 8).
const (
	ErrExit     = "exit"
	ErrTimeout  = "timeout"
	ErrInput    = "input"
	ErrSandbox  = "sandbox"
	ErrOutput   = "output"
	ErrInternal = "internal"
)

// CountsAsAttempt reports whether a failure of this kind consumes an attempt.
func CountsAsAttempt(kind string) bool { return kind == ErrExit || kind == ErrTimeout }

// ClaimRequest is POSTed to /api/v1/claim. ClaimID is random per attempt
// and reused on retries: if the previous response was lost in transit, the
// hive returns the same tasks again instead of assigning new ones.
type ClaimRequest struct {
	ClaimID string    `json:"claim_id"`
	Free    Resources `json:"free"`
	Max     int       `json:"max"`    // 1..16
	WaitS   int       `json:"wait_s"` // long-poll seconds (0-30)
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
	ID            string            `json:"id"`
	Name          string            `json:"name"`
	ShortCode     string            `json:"short_code"`
	Roles         []Role            `json:"roles"`
	Labels        map[string]string `json:"labels,omitempty"`        // effective: config labels overridden by admin labels
	ConfigLabels  map[string]string `json:"config_labels,omitempty"` // from the node's savior.conf
	AdminLabels   map[string]string `json:"admin_labels,omitempty"`  // set with NodePatch
	Liveness      NodeLiveness      `json:"liveness"`
	Approved      bool              `json:"approved"`
	Addr          string            `json:"addr"` // remote IP as seen by the hive
	Version       string            `json:"version"`
	FirstSeen     time.Time         `json:"first_seen"`
	LastSeen      time.Time         `json:"last_seen"`
	Drain         bool              `json:"drain"`
	Inventory     Inventory         `json:"inventory"`
	Status        NodeStatus        `json:"status"`
	Display       *DisplaySpec      `json:"display,omitempty"` // desired
	DisplayRotate int               `json:"display_rotate"`
	WallID        string            `json:"wall_id,omitempty"`
	ScratchInRAM  bool              `json:"scratch_in_ram"`
	Sandbox       string            `json:"sandbox"`
	SandboxCaps   []string          `json:"sandbox_caps,omitempty"`
	FullIsolation bool              `json:"full_isolation"`
	Allocated     Resources         `json:"allocated"`              // hive-side accounting of assigned tasks
	RunningTasks  []string          `json:"running_tasks"`          // hive-side assignment
	Quarantine    string            `json:"quarantine,omitempty"`   // why the hive stopped sending tasks (cleared by NodePatch)
	ReservedFor   string            `json:"reserved_for,omitempty"` // task ID the node is being drained for
	BootID        string            `json:"boot_id,omitempty"`
	Warnings      []string          `json:"warnings,omitempty"`
}

// NodePatch updates a node. Nil fields are unchanged.
type NodePatch struct {
	Name            *string            `json:"name,omitempty"`
	Labels          *map[string]string `json:"labels,omitempty"` // replaces all admin labels
	Drain           *bool              `json:"drain,omitempty"`
	Approved        *bool              `json:"approved,omitempty"`
	ClearQuarantine bool               `json:"clear_quarantine,omitempty"`
	Display         *DisplaySpec       `json:"display,omitempty"` // rev assigned by the hive; mode=wall rejected
	DisplayRotate   *int               `json:"display_rotate,omitempty"`
}

// NodeAction requests a one-shot action on a node.
type NodeAction struct {
	Action  string `json:"action"`            // identify | reboot | poweroff
	Seconds int    `json:"seconds,omitempty"` // identify duration (default 30)
}

// IdentifyRequest identifies many nodes at once (POST /api/v1/admin/identify).
type IdentifyRequest struct {
	Nodes   []string `json:"nodes,omitempty"` // empty = all online nodes
	Seconds int      `json:"seconds,omitempty"`
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
	Seq        uint64     `json:"seq"` // submission order (monotonic, persisted)
	Name       string     `json:"name,omitempty"`
	Count      int        `json:"count"`
	Priority   int        `json:"priority"`
	State      JobState   `json:"state"`
	Counts     TaskCounts `json:"counts"`
	CreatedAt  time.Time  `json:"created_at"` // hive clock, display only
	StartedAt  *time.Time `json:"started_at,omitempty"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	Warning    string     `json:"warning,omitempty"`
}

// TaskView is the admin view of a task.
type TaskView struct {
	ID            string        `json:"id"`
	JobID         string        `json:"job_id"`
	Index         int           `json:"index"`
	State         TaskState     `json:"state"`
	Node          string        `json:"node,omitempty"` // node ID currently/last assigned
	Attempt       int           `json:"attempt"`        // dispatch number (Task.Attempt)
	Failures      int           `json:"failures"`       // attempts consumed (exit/timeout)
	NodeErrors    int           `json:"node_errors"`    // input/sandbox/output/internal failures
	Interruptions int           `json:"interruptions"`  // lost/preempted requeues
	ExitCode      *int          `json:"exit_code,omitempty"`
	ErrorKind     string        `json:"error_kind,omitempty"`
	Error         string        `json:"error,omitempty"`
	WaitReason    string        `json:"wait_reason,omitempty"` // why a pending task isn't running
	Outputs       []Output      `json:"outputs,omitempty"`
	AssignedAt    *time.Time    `json:"assigned_at,omitempty"` // hive clock
	StartedAt     *time.Time    `json:"started_at,omitempty"`
	FinishedAt    *time.Time    `json:"finished_at,omitempty"`
	RunS          float64       `json:"run_s,omitempty"`
	CPUSeconds    float64       `json:"cpu_seconds,omitempty"`
	MaxMemMB      int           `json:"max_mem_mb,omitempty"`
	FailedNodes   []string      `json:"failed_nodes,omitempty"`
	History       []AttemptView `json:"history,omitempty"` // last 10 dispatches
}

// AttemptView records how one dispatch of a task ended.
type AttemptView struct {
	Attempt    int       `json:"attempt"`
	Node       string    `json:"node"`
	AssignedAt time.Time `json:"assigned_at"`
	FinishedAt time.Time `json:"finished_at,omitzero"`
	Outcome    string    `json:"outcome"` // succeeded, failed, timeout, node_error, lost, preempted, canceled
	ExitCode   int       `json:"exit_code"`
	Error      string    `json:"error,omitempty"`
}

// JobDetail is a job with its normalized spec (defaults applied). Tasks are
// listed separately: GET /api/v1/admin/jobs/{id}/tasks.
type JobDetail struct {
	JobView
	Spec JobSpec `json:"spec"`
}

// TaskPage is one page of GET /api/v1/admin/jobs/{id}/tasks.
// Tasks that were never dispatched have no record; Undispatched counts them.
type TaskPage struct {
	Tasks        []TaskView `json:"tasks"`
	Total        int        `json:"total"`
	Undispatched int        `json:"undispatched"`
	NextOffset   int        `json:"next_offset,omitempty"` // 0 = no more
}

// OutputEntry is one line of GET /api/v1/admin/jobs/{id}/outputs.
type OutputEntry struct {
	TaskID string `json:"task_id"`
	Index  int    `json:"index"`
	Name   string `json:"name"`
	Blob   string `json:"blob"`
	Size   int64  `json:"size"`
}

// BlobInfo describes a stored blob.
type BlobInfo struct {
	SHA256      string    `json:"sha256"`
	Size        int64     `json:"size"`
	CreatedAt   time.Time `json:"created_at"`
	LastTouched time.Time `json:"last_touched"`
	Referenced  bool      `json:"referenced"`
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
	URLs        []string  `json:"urls"` // https://<each address>:<port>
	DataDir     string    `json:"data_dir"`
	Persistent  bool      `json:"persistent"` // false = state lives in RAM and is lost on reboot
	JoinPolicy  string    `json:"join_policy"`
	TimeSynced  bool      `json:"time_synced"`
	TimeSource  string    `json:"time_source"`
	Warnings    []string  `json:"warnings,omitempty"`
}

// SessionRequest exchanges a pairing code or the admin token for a session
// (POST /api/v1/admin/session). Exactly one field is set.
type SessionRequest struct {
	PairCode string `json:"pair_code,omitempty"`
	Token    string `json:"token,omitempty"`
}

// SessionResponse is returned by the session and login endpoints.
type SessionResponse struct {
	Session   string    `json:"session"` // also set as the savior_admin cookie for browsers
	ExpiresAt time.Time `json:"expires_at"`
	HiveProof string    `json:"hive_proof,omitempty"` // login only
}

// LoginRequest is the proof-based admin login used by `savior ctl`
// (POST /api/v1/admin/login), so the admin token is never sent.
type LoginRequest struct {
	HiveNonce   string `json:"hive_nonce"`
	ClientNonce string `json:"client_nonce"`
	Proof       string `json:"proof"` // auth.AdminProof(admin_token, hive_nonce, client_nonce, FP_seen)
}

// PairCode is returned by POST /api/v1/admin/pair (and shown on the hive's screen).
type PairCode struct {
	Code      string    `json:"code"` // 8 chars Crockford base32, single use
	ExpiresAt time.Time `json:"expires_at"`
}

// ErrorResponse is the body of every non-2xx JSON response.
type ErrorResponse struct {
	Error string `json:"error"`
}
