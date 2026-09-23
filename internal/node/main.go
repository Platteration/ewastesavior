package node

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"github.com/platteration/ewastesavior/internal/config"
	"github.com/platteration/ewastesavior/internal/hwinfo"
	"github.com/platteration/ewastesavior/internal/logging"
	"github.com/platteration/ewastesavior/internal/proto"
	"github.com/platteration/ewastesavior/internal/version"
)

// DefaultStatusFile is read by `savior console`.
const DefaultStatusFile = "/run/savior/status.json"

// Main implements `savior node`.
func Main(args []string) int {
	fs := flag.NewFlagSet("node", flag.ContinueOnError)
	cfgFile := fs.String("config", "", "merged config file (default /run/savior/savior.conf, else the standard search)")
	logFile := fs.String("log-file", "", "also log to this size-rotated file")
	logLevel := fs.String("log-level", "", "debug, info, warn or error (default: config log_level)")
	statusFile := fs.String("status-file", DefaultStatusFile, "status file for savior console")
	sysRoot := fs.String("sys-root", "", "read /proc and /sys below this directory (testing)")
	manage := fs.Bool("manage-system", true, "allow setting the clock and hostname and rebooting on request")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg, warnings, err := config.LoadDefault(*cfgFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, "savior node:", err)
		return 1
	}
	level := cfg.LogLevel
	if *logLevel != "" {
		level = *logLevel
	}
	log, closer, err := logging.Setup(level, *logFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, "savior node:", err)
		return 1
	}
	defer closer.Close()
	for _, w := range warnings {
		log.Warn("config: " + w)
	}
	if bt := version.BuildTime(); *manage && !bt.IsZero() && time.Now().Before(bt) {
		if err := setSystemClock(bt); err == nil {
			log.Warn("system clock was before the build date; raised it", "now", bt)
		}
	}
	if mt := hwinfo.MemTotalMB(*sysRoot); mt > 0 {
		limit := int64(mt) << 20 / 4
		if limit < 48<<20 {
			limit = 48 << 20
		}
		debug.SetMemoryLimit(limit)
	}
	if *manage {
		protectFromOOM()
	}

	agent, err := New(Options{
		Config:       cfg,
		Log:          log,
		SysRoot:      *sysRoot,
		StatusFile:   *statusFile,
		ManageSystem: *manage,
	})
	if err != nil {
		log.Error("cannot start the node agent", "err", err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	log.Info("savior node starting", "version", version.Version, "node_id", agent.id.NodeID, "name", agent.name, "roles", fmt.Sprint(agent.roles))
	if err := agent.Run(ctx); err != nil {
		log.Error("node agent stopped", "err", err)
		return 1
	}
	return 0
}

// protectFromOOM lowers the agent's OOM score so the kernel kills a
// runaway task before the agent that supervises it.
func protectFromOOM() {
	os.WriteFile("/proc/self/oom_score_adj", []byte("-900"), 0o644)
}

// ConsoleMain implements `savior console`: a text status screen for tty1
// that works without a framebuffer.
func ConsoleMain(args []string) int {
	fs := flag.NewFlagSet("console", flag.ContinueOnError)
	statusFile := fs.String("status-file", DefaultStatusFile, "status file written by savior node")
	interval := fs.Duration("interval", 2*time.Second, "refresh interval")
	once := fs.Bool("once", false, "print once and exit")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	out := os.Stdout
	for {
		var b strings.Builder
		renderConsole(&b, *statusFile, time.Now())
		if *once {
			fmt.Fprint(out, b.String())
			return 0
		}
		// Clear screen + home, then draw; keeps the old tty flicker-free enough.
		fmt.Fprint(out, "\033[H\033[2J"+b.String())
		time.Sleep(*interval)
	}
}

// renderConsole writes the console text for the given status file.
func renderConsole(w io.Writer, statusFile string, now time.Time) {
	fmt.Fprintf(w, "SaviorOS %s\n\n", version.Version)
	raw, err := os.ReadFile(statusFile)
	if err != nil {
		fmt.Fprintln(w, "  Starting the node agent...")
		if cfg, _, err := config.LoadDefault(""); err == nil && cfg.SwarmKey == "" && cfg.Join != "keyless" {
			fmt.Fprintln(w, "\n  No swarm_key configured. Put savior.conf with swarm_key = ... on the stick.")
		}
		fmt.Fprintln(w, "\n  Alt+F2: shell (if console_shell = yes)   Alt+F7: display")
		return
	}
	var st consoleStatus
	if err := json.Unmarshal(raw, &st); err != nil {
		fmt.Fprintln(w, "  status unreadable:", err)
		return
	}
	san := func(s string) string { return proto.Sanitize(s, 200, false) }
	fmt.Fprintf(w, "  Name      %s   [%s]\n", san(st.Name), san(st.ShortCode))
	fmt.Fprintf(w, "  Node ID   %s\n", san(st.NodeID))
	fmt.Fprintf(w, "  Address   %s\n", san(strings.Join(st.Addrs, "  ")))
	roles := make([]string, len(st.Roles))
	for i, r := range st.Roles {
		roles[i] = string(r)
	}
	fmt.Fprintf(w, "  Roles     %s\n", strings.Join(roles, ", "))
	link := string(st.Link)
	if st.HiveAddr != "" {
		link += " (" + san(st.HiveAddr) + ")"
	}
	fmt.Fprintf(w, "  Hive      %s\n", link)
	if st.Hint != "" {
		fmt.Fprintf(w, "            %s\n", san(st.Hint))
	}
	if st.Hive != nil {
		fmt.Fprintf(w, "\n  This machine is the hive:\n")
		for _, u := range st.Hive.URLs {
			fmt.Fprintf(w, "    %s\n", san(u))
		}
		fmt.Fprintf(w, "    fingerprint %s\n", san(st.Hive.Fingerprint))
		if st.Hive.PairCode != "" {
			fmt.Fprintf(w, "    pairing code %s (enter it in the web dashboard)\n", san(st.Hive.PairCode))
		}
		fmt.Fprintf(w, "    nodes online %d\n", st.Hive.NodesOnline)
		if !st.Hive.Persistent {
			fmt.Fprintln(w, "    WARNING: hive data is in RAM and is lost on reboot")
		}
	}
	m := st.Metrics
	fmt.Fprintf(w, "\n  CPU       %s  %d cores  %.0f%% busy\n", san(st.Inventory.CPUModel), st.Inventory.Cores, m.CPUPercent)
	fmt.Fprintf(w, "  Memory    %d MB total, %d MB available\n", st.Inventory.MemTotalMB, m.MemAvailableMB)
	if m.CPUTempC > 0 {
		fmt.Fprintf(w, "  Temp      %.0f°C (limit %.0f°C)\n", m.CPUTempC, m.CPUTempLimitC)
	}
	if m.BatteryPercent >= 0 && st.Inventory.HasBattery {
		src := "mains"
		if m.OnBattery {
			src = "battery"
		}
		fmt.Fprintf(w, "  Battery   %d%% (%s)\n", m.BatteryPercent, src)
	}
	state := fmt.Sprintf("%d task(s) running", len(st.TaskNames))
	if st.Drain {
		state += ", draining"
	}
	if st.Pending {
		state += ", waiting for approval"
	}
	if st.PowerReason != "" {
		state += ", paused: " + san(st.PowerReason)
	}
	fmt.Fprintf(w, "  Work      %s\n", state)
	for i, n := range st.TaskNames {
		if i == 8 {
			fmt.Fprintf(w, "            ... and %d more\n", len(st.TaskNames)-8)
			break
		}
		fmt.Fprintf(w, "            %s\n", san(n))
	}
	if st.Display.Active {
		fmt.Fprintf(w, "  Display   %s %dx%d %s (Alt+F7)\n", san(st.Display.Mode), st.Display.Width, st.Display.Height, san(st.Display.Format))
	} else if st.Display.Error != "" {
		fmt.Fprintf(w, "  Display   error: %s\n", san(st.Display.Error))
	}
	if age := now.Sub(st.Updated); age > 30*time.Second {
		fmt.Fprintf(w, "\n  (status is %s old — is savior node running?)\n", age.Round(time.Second))
	}
	fmt.Fprintln(w, "\n  Alt+F2: shell (if console_shell = yes)   Alt+F7: display")
}
