package hwinfo

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/platteration/ewastesavior/internal/config"
	"github.com/platteration/ewastesavior/internal/proto"
	"github.com/platteration/ewastesavior/internal/version"
)

// Report is what `savior info --json` prints.
type Report struct {
	Inventory proto.Inventory `json:"inventory"`
	Identity  Identity        `json:"identity"`
	Metrics   proto.Metrics   `json:"metrics"`
}

// cpuSampleWait is how long `savior info` measures CPU usage on a live system.
const cpuSampleWait = 500 * time.Millisecond

// Main implements `savior info [--json] [--root DIR] [--bench]`.
func Main(args []string) int {
	return run(args, os.Stdout, os.Stderr)
}

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("savior info", flag.ContinueOnError)
	fs.SetOutput(stderr)
	asJSON := fs.Bool("json", false, "print JSON {inventory, identity, metrics}")
	root := fs.String("root", "/", "read /proc and /sys below `DIR` instead of the running system")
	bench := fs.Bool("bench", false, "also run a 2-second single-core CPU benchmark")
	maxTemp := fs.Int("max-temp", 0, "max_temp_c used for the temperature limit (default: savior.conf, else 85)")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: savior info [--json] [--root DIR] [--bench] [--max-temp C]\n\nShow this machine's hardware, identity, live metrics and suggested roles.\n\nflags:")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() > 0 {
		fs.Usage()
		return 2
	}

	inv, err := Collect(*root)
	if err != nil {
		fmt.Fprintf(stderr, "savior info: %v\n", err)
		return 1
	}
	id, err := GetIdentity(*root)
	if err != nil {
		fmt.Fprintf(stderr, "savior info: %v\n", err)
		return 1
	}
	if *bench {
		inv.BenchScore = Benchmark(2 * time.Second)
	}
	s := NewSampler(*root, float64(effectiveMaxTemp(*maxTemp, isLive(*root))))
	if isLive(*root) {
		time.Sleep(cpuSampleWait)
	}
	rep := Report{Inventory: inv, Identity: id, Metrics: s.Sample()}

	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rep); err != nil {
			fmt.Fprintf(stderr, "savior info: %v\n", err)
			return 1
		}
		return 0
	}
	writeText(stdout, rep, *root)
	return 0
}

// effectiveMaxTemp returns the flag value, else max_temp_c from the merged
// config of the running node, else the built-in default.
func effectiveMaxTemp(flagValue int, live bool) int {
	if flagValue > 0 {
		return flagValue
	}
	if live {
		if c, _, err := config.LoadDefault(""); err == nil && c.MaxTempC > 0 {
			return c.MaxTempC
		}
	}
	return config.Default().MaxTempC
}

// writeText prints the human-readable report.
func writeText(w io.Writer, r Report, root string) {
	inv, m := r.Inventory, r.Metrics
	line := func(label, format string, a ...any) {
		fmt.Fprintf(w, "%-10s %s\n", label, fmt.Sprintf(format, a...))
	}
	more := func(format string, a ...any) { line("", format, a...) }

	fmt.Fprintf(w, "SaviorOS hardware report (savior %s)\n\n", version.Version)
	machine := joinNonEmpty(" ", inv.Vendor, inv.Product)
	if machine == "" {
		machine = "unknown machine"
	}
	kind := "desktop"
	switch {
	case inv.Virtualized:
		kind = "virtual machine"
	case inv.IsLaptop:
		kind = "laptop"
	}
	line("Machine", "%s", joinNonEmpty(", ", machine, kind, prefixed("BIOS ", inv.BIOSDate)))
	line("System", "%s", joinNonEmpty(", ",
		prefixed("host ", inv.Hostname),
		fmt.Sprintf("%s binary on %s kernel %s", inv.Arch, orUnknown(inv.MachineArch), orUnknown(inv.Kernel)),
		inv.OSVersion))
	cores := fmt.Sprintf("%d logical", inv.Cores)
	if inv.PhysicalCores > 0 {
		cores += fmt.Sprintf(" / %d physical", inv.PhysicalCores)
	}
	line("CPU", "%s", joinNonEmpty(", ", orUnknown(inv.CPUModel), cores+" cores", mhz(inv.CPUMHz)))
	if len(inv.CPUFlags) > 0 {
		more("flags: %s", strings.Join(inv.CPUFlags, " "))
	}
	if inv.BenchScore > 0 {
		more("benchmark: %d MiB/s SHA-256 (one core)", inv.BenchScore)
	}
	line("Memory", "%d MB RAM, %d MB swap", inv.MemTotalMB, inv.SwapTotalMB)

	label := "Disks"
	if len(inv.Disks) == 0 {
		line(label, "none")
	}
	for _, d := range inv.Disks {
		media := "HDD"
		if !d.Rotational {
			media = "SSD/flash"
		}
		line(label, "%s", joinNonEmpty(" ", d.Name, fmt.Sprintf("%d MB", d.SizeMB), d.Transport, media,
			quoted(d.Model), ifTrue(d.Removable, "removable")))
		label = ""
	}

	label = "Network"
	if len(inv.NICs) == 0 {
		line(label, "none")
	}
	for _, n := range inv.NICs {
		link, state := "wired", "down"
		if n.Wireless {
			link = "wireless"
		}
		if n.Up {
			state = "up"
			if n.SpeedMb > 0 {
				state += fmt.Sprintf(" %d Mb/s", n.SpeedMb)
			}
		} else if n.Carrier {
			state = "down, carrier"
		}
		line(label, "%s", joinNonEmpty(" ", n.Name, n.MAC, n.Bus, n.Driver, link, state, strings.Join(n.Addrs, " ")))
		label = ""
	}

	label = "Display"
	if len(inv.GPUs) == 0 && len(inv.Framebuffers) == 0 {
		line(label, "none (headless)")
	}
	for _, g := range inv.GPUs {
		line(label, "%s", joinNonEmpty(" ", g.Card, orUnknown(g.Driver), pciID(g.Vendor, g.Device)))
		label = ""
	}
	for _, c := range inv.Connectors {
		desc := joinNonEmpty(" ", c.Name, c.Status, c.Preferred)
		if c.WidthMM > 0 {
			desc += fmt.Sprintf(" %dx%d mm", c.WidthMM, c.HeightMM)
		}
		if InternalConnector(c.Name) {
			desc += " (built-in)"
		}
		line(label, "%s", desc)
		label = ""
	}
	for _, fb := range inv.Framebuffers {
		line(label, "%s %s %dx%d %d bpp", fb.Name, orUnknown(fb.Driver), fb.Width, fb.Height, fb.BPP)
		label = ""
	}

	switch {
	case m.BatteryPercent >= 0:
		power := "on AC"
		if m.OnBattery {
			power = "ON BATTERY"
		}
		line("Battery", "%s", joinNonEmpty(", ", fmt.Sprintf("%d%%", m.BatteryPercent), m.BatteryStatus,
			percentOf("health", m.BatteryHealthPercent), power))
	case inv.HasBattery:
		line("Battery", "present, charge unknown")
	}
	temp := "no CPU temperature sensor"
	if m.CPUTempC > 0 {
		temp = fmt.Sprintf("CPU %.0f°C via %s", m.CPUTempC, orUnknown(inv.TempSensor))
	}
	if m.CPUTempLimitC > 0 {
		temp += fmt.Sprintf(", pause at %.0f°C", m.CPUTempLimitC)
	}
	if m.ThrottleEvents > 0 {
		temp += fmt.Sprintf(", %d thermal throttle events", m.ThrottleEvents)
	}
	line("Sensors", "%s", temp)

	fmt.Fprintln(w)
	line("Node ID", "%s (from %s)", r.Identity.NodeID, r.Identity.Source)
	if !r.Identity.Stable {
		more("WARNING: no stable hardware ID; this node gets a new ID on every start")
	}
	line("Name", "%s, identify code %s", DefaultName(r.Identity), proto.ShortCode(r.Identity.NodeID))
	if len(r.Identity.HWIDs) > 0 {
		line("HW IDs", "%s", strings.Join(r.Identity.HWIDs, " "))
	}
	if b := BootID(root); b != "" {
		line("Boot ID", "%s", b)
	}

	fmt.Fprintln(w)
	line("Load", "CPU %.1f%%, load %.2f, up %s", m.CPUPercent, m.Load1, (time.Duration(m.UptimeS) * time.Second).String())
	line("Free mem", "%d MB available, %d MB swap used, pressure %.1f%%", m.MemAvailableMB, m.SwapUsedMB, m.MemPressure)
	line("Traffic", "%s received, %s sent", humanBytes(m.NetRxBytes), humanBytes(m.NetTxBytes))
	if inv.IsLaptop {
		lid := "open"
		if m.LidClosed {
			lid = "closed"
		}
		line("Lid", "%s", lid)
	}

	fmt.Fprintln(w)
	roles, notes := suggestRoles(inv, HasDisplay(root))
	line("Roles", "%s (suggested)", strings.Join(roles, ", "))
	for _, n := range notes {
		more("- %s", n)
	}
}

// suggestRoles proposes roles from the hardware alone, with explanations.
func suggestRoles(inv proto.Inventory, hasDisplay bool) (roles, notes []string) {
	roles = append(roles, string(proto.RoleCompute))
	notes = append(notes, fmt.Sprintf("compute: %d cores, %d MB RAM", inv.Cores, inv.MemTotalMB))
	if inv.MemTotalMB > 0 && inv.MemTotalMB < 384 {
		notes = append(notes, "little RAM: only small tasks will fit")
	}
	if len(inv.CPUFlags) > 0 && !inv.HasCPUFlag("sse2") {
		notes = append(notes, "no SSE2: the softfloat build runs (slower floating point)")
	}
	if hasDisplay {
		roles = append(roles, string(proto.RoleDisplay))
		screens := 0
		for _, c := range inv.Connectors {
			if c.Status == "connected" {
				screens++
			}
		}
		notes = append(notes, fmt.Sprintf("display: %d connected screen(s), %d framebuffer(s)", screens, len(inv.Framebuffers)))
	}
	if inv.IsLaptop && !inv.Virtualized {
		notes = append(notes, "laptop: stops taking tasks on battery unless run_on_battery = yes")
	}
	wired := false
	for _, n := range inv.NICs {
		wired = wired || (!n.Wireless && n.Bus != "virtual")
	}
	if inv.MemTotalMB >= 1024 && wired && !inv.IsLaptop {
		notes = append(notes, "hive: a good candidate (1 GB+ RAM, wired network)")
	}
	return roles, notes
}

func joinNonEmpty(sep string, parts ...string) string {
	out := parts[:0:0]
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, sep)
}

func prefixed(prefix, s string) string {
	if s == "" {
		return ""
	}
	return prefix + s
}

func quoted(s string) string {
	if s == "" {
		return ""
	}
	return fmt.Sprintf("%q", s)
}

func ifTrue(b bool, s string) string {
	if b {
		return s
	}
	return ""
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

func mhz(n int) string {
	if n <= 0 {
		return ""
	}
	return fmt.Sprintf("%d MHz", n)
}

func percentOf(label string, p int) string {
	if p <= 0 {
		return ""
	}
	return fmt.Sprintf("%s %d%%", label, p)
}

func pciID(vendor, device string) string {
	if vendor == "" {
		return ""
	}
	return "(" + vendor + ":" + device + ")"
}

func humanBytes(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := uint64(unit), 0
	for v := n / unit; v >= unit && exp < 4; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTP"[exp])
}
