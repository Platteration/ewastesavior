package hive

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/platteration/ewastesavior/internal/config"
	"github.com/platteration/ewastesavior/internal/logging"
	"github.com/platteration/ewastesavior/internal/proto"
	"github.com/platteration/ewastesavior/internal/version"
)

// Main implements `savior hive`.
func Main(args []string) int {
	return run(args, os.Stdout, os.Stderr)
}

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("hive", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configFile := fs.String("config", "", "savior.conf to read (default: the merged boot config, else the default files)")
	listen := fs.String("listen", "", "HTTPS listen address (hive_listen, default :7700)")
	data := fs.String("data", "", "state directory (hive_data, default auto)")
	swarmKey := fs.String("swarm-key", "", "swarm key (swarm_key; default: stored in the data directory)")
	adminToken := fs.String("admin-token", "", "admin token (admin_token; default: generated in the data directory)")
	noBeacon := fs.Bool("no-beacon", false, "do not announce the hive on the LAN")
	logFile := fs.String("log-file", "", "also log to this file (rotated at 1 MiB)")
	logLevel := fs.String("log-level", "", "debug, info, warn or error")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: savior hive [flags]\n\nRuns the SaviorOS hive: the coordinator nodes join, plus the web dashboard.\n\nflags:")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "savior hive: unexpected argument %q\n", fs.Arg(0))
		return 2
	}
	conf, warnings, err := config.LoadDefault(*configFile)
	if err != nil {
		fmt.Fprintf(stderr, "savior hive: %v\n", err)
		return 1
	}
	level := conf.LogLevel
	if *logLevel != "" {
		level = *logLevel
	}
	log, closer, err := logging.Setup(level, *logFile)
	if err != nil {
		fmt.Fprintf(stderr, "savior hive: %v\n", err)
		return 1
	}
	defer closer.Close()
	for _, w := range warnings {
		log.Warn("config: " + w)
	}

	cfg := ConfigFromFile(conf)
	cfg.Log = log
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "listen":
			cfg.Listen = *listen
		case "data":
			cfg.DataDir = *data
		case "swarm-key":
			cfg.SwarmKey = *swarmKey
		case "admin-token":
			cfg.AdminToken = *adminToken
		case "no-beacon":
			cfg.Beacon = !*noBeacon
		}
	})
	cfg.tune.clockRaised = prepareProcess(log)

	s, err := New(cfg)
	if err != nil {
		log.Error("hive failed to start", "err", err)
		fmt.Fprintf(stderr, "savior hive: %v\n", err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errc := make(chan error, 1)
	go func() { errc <- s.Run(ctx) }()
	// Print the banner once the listener is up (or failed).
	for i := 0; i < 50 && s.Addr() == nil; i++ {
		select {
		case err := <-errc:
			fmt.Fprintf(stderr, "savior hive: %v\n", err)
			return 1
		case <-time.After(20 * time.Millisecond):
		}
	}
	printBanner(stdout, s)
	if err := <-errc; err != nil {
		log.Error("hive stopped", "err", err)
		fmt.Fprintf(stderr, "savior hive: %v\n", err)
		return 1
	}
	return 0
}

// printBanner shows how to reach and use the hive.
func printBanner(w io.Writer, s *Server) {
	info := s.Info()
	pc := s.PairCode()
	fmt.Fprintf(w, "SaviorOS hive %s  (hive id %s)\n\n", version.Version, info.HiveID)
	fmt.Fprintln(w, "Dashboard:")
	for _, u := range info.URLs {
		fmt.Fprintf(w, "  %s\n", u)
	}
	fmt.Fprintf(w, "\nCertificate fingerprint (check it when your browser or ctl asks):\n  %s\n\n", info.Fingerprint)
	fmt.Fprintf(w, "Pairing code for the dashboard: %s (single use, valid until %s)\n",
		pc.Code, pc.ExpiresAt.Local().Format("15:04"))
	if f := s.AdminTokenFile(); f != "" {
		fmt.Fprintf(w, "Admin token: stored in %s\n", f)
	} else {
		fmt.Fprintln(w, "Admin token: from the configuration (admin_token)")
	}
	fmt.Fprintf(w, "Data directory: %s\n", info.DataDir)
	fmt.Fprintln(w, "\nNode config: savior ctl node-config > savior.conf, then copy it to each boot stick.")
	if runtime.GOOS == "darwin" || runtime.GOOS == "windows" {
		fmt.Fprintf(w, "\nFirewall: allow savior to accept TCP %d (HTTPS) and UDP %d (discovery) from the local network.\n",
			s.listenPort(), proto.DiscoveryPort)
	}
	for _, warn := range info.Warnings {
		fmt.Fprintf(w, "\nWARNING: %s\n", warn)
	}
	fmt.Fprintln(w)
}

// prepareProcess applies DESIGN 4 process setup: on Linux as root protect
// the hive from the OOM killer, and raise a clock that is before the build
// time (dead CMOS batteries). It reports whether the clock was raised.
func prepareProcess(log interface{ Warn(string, ...any) }) bool {
	if runtime.GOOS != "linux" || os.Geteuid() != 0 {
		return false
	}
	if err := os.WriteFile("/proc/self/oom_score_adj", []byte("-900\n"), 0o644); err != nil {
		log.Warn("could not set oom_score_adj", "err", err)
	}
	if bt := version.BuildTime(); !bt.IsZero() && time.Now().Before(bt) {
		if err := setSystemClock(bt); err != nil {
			log.Warn("could not raise the clock to the build time", "err", err)
			return false
		}
		return true
	}
	return false
}
