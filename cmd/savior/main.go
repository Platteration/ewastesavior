// Command savior is the single multi-call binary of SaviorOS: node agent,
// hive coordinator, operator CLI and helpers. See docs/DESIGN.md section 4.
package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/platteration/ewastesavior/internal/config"
	"github.com/platteration/ewastesavior/internal/ctl"
	"github.com/platteration/ewastesavior/internal/display"
	"github.com/platteration/ewastesavior/internal/hive"
	"github.com/platteration/ewastesavior/internal/hwinfo"
	"github.com/platteration/ewastesavior/internal/node"
	"github.com/platteration/ewastesavior/internal/runner"
	"github.com/platteration/ewastesavior/internal/storage"
	"github.com/platteration/ewastesavior/internal/version"
)

var commands = []struct {
	name, help string
	run        func([]string) int
	hidden     bool
}{
	{"node", "run the node agent", node.Main, false},
	{"hive", "run the hive (swarm coordinator)", hive.Main, false},
	{"ctl", "control a hive: nodes, jobs, displays, walls", ctl.Main, false},
	{"info", "show this machine's hardware inventory", hwinfo.Main, false},
	{"display", "test or drive the screen (test, render, show)", display.Main, false},
	{"config", "read the merged savior.conf (env, get, dump, keys, sample)", config.Main, false},
	{"console", "text status screen (runs on tty1)", node.ConsoleMain, false},
	{"storage", "boot media and data partition helpers (SaviorOS)", storage.Main, true},
	{"sandbox-exec", "internal: task sandbox shim", runner.SandboxExecMain, true},
}

func main() {
	os.Exit(run(os.Args, os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	// Multi-call: a symlink named savior-<cmd> (e.g. savior-ctl) runs <cmd>.
	// Other names (savior-sse2, savior-softfloat builds) are ordinary.
	if base := filepath.Base(args[0]); strings.HasPrefix(base, "savior-") {
		if name := strings.TrimPrefix(base, "savior-"); isCommand(name) {
			args = append([]string{args[0], name}, args[1:]...)
		}
	}
	if len(args) < 2 {
		usage(stderr)
		return 2
	}
	sub := args[1]
	switch sub {
	case "version", "--version", "-v":
		fmt.Fprintf(stdout, "savior %s\n", version.Version)
		return 0
	case "help", "--help", "-h":
		usage(stdout)
		return 0
	}
	for _, c := range commands {
		if c.name == sub {
			return c.run(args[2:])
		}
	}
	fmt.Fprintf(stderr, "savior: unknown command %q\n\n", sub)
	usage(stderr)
	return 2
}

func isCommand(name string) bool {
	for _, c := range commands {
		if c.name == name {
			return true
		}
	}
	return false
}

func usage(w io.Writer) {
	fmt.Fprintf(w, "SaviorOS %s - give old computers a new job.\n\nusage: savior <command> [arguments]\n\ncommands:\n", version.Version)
	for _, c := range commands {
		if !c.hidden {
			fmt.Fprintf(w, "  %-9s %s\n", c.name, c.help)
		}
	}
	fmt.Fprintf(w, "  %-9s %s\n\nRun 'savior <command> -h' for details.\n", "version", "print the version")
}
