// Package storage implements `savior storage`, the SaviorOS boot helpers:
// finding and mounting the config medium, creating the hive data partition,
// picking the 386 binary and computing certificate fingerprints. See
// docs/DESIGN.md sections 3, 13.3 and 13.4.
package storage

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
)

// Main implements `savior storage`.
func Main(args []string) int {
	return run(args, os.Stdout, os.Stderr, platformOps())
}

const usageText = `usage: savior storage <command> [flags]

SaviorOS boot helpers (used by /etc/init.d scripts):
  find-media   find the SaviorOS stick (or another medium with savior.conf) and mount it read-only
  init-data    create/mount the SAVIOR-DATA partition for the hive (DESIGN 13.4)
  pick-binary  keep the savior 386 build that suits this CPU (sse2 or softfloat)
  fingerprint  print sha256:<hex> of a PEM certificate
  probe        print the filesystem type, label and UUID of block devices
  find-label   print the block device holding a filesystem label

Run 'savior storage <command> -h' for flags.`

func run(args []string, stdout, stderr io.Writer, ops sysOps) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, usageText)
		return 2
	}
	cmd, rest := args[0], args[1:]
	fs := flag.NewFlagSet("storage "+cmd, flag.ContinueOnError)
	fs.SetOutput(stderr)
	switch cmd {
	case "-h", "--help", "help":
		fmt.Fprintln(stdout, usageText)
		return 0

	case "find-media":
		o := findMediaOpts{wait: 20 * time.Second}
		fs.StringVar(&o.cmdlineFile, "cmdline-file", "/proc/cmdline", "kernel command line (for savior.media=)")
		fs.Var((*seconds)(&o.wait), "wait", "seconds to wait for the medium")
		fs.StringVar(&o.mountpoint, "mount", "/media/savior", `mount point ("" = don't mount)`)
		fs.StringVar(&o.sysRoot, "sys-root", "/", "root for /sys, /dev and /proc (tests)")
		fs.Var((*seconds)(&o.settle), "settle", "seconds to wait for better candidates once a fallback medium is found (default 2)")
		fs.BoolVar(&o.dryRun, "dry-run", false, "only print the chosen device")
		if fs.Parse(rest) != nil || fs.NArg() != 0 {
			return 2
		}
		return findMedia(o, ops, stdout, stderr)

	case "init-data":
		o := initDataOpts{partWait: 10 * time.Second}
		minFreeMB := fs.Uint64("min-free-mb", 256, "minimum unallocated space after the boot partition (MiB)")
		fs.StringVar(&o.sysRoot, "sys-root", "/", "root for /sys, /dev and /proc (tests)")
		fs.StringVar(&o.dev, "dev", "", "disk to use instead of the one holding --media")
		fs.StringVar(&o.media, "media", "/media/savior", "mount point of the SaviorOS boot medium")
		fs.StringVar(&o.mountpoint, "mountpoint", "/var/lib/savior/data", "where to mount SAVIOR-DATA")
		fs.BoolVar(&o.dryRun, "dry-run", false, "print the plan, write nothing")
		fs.BoolVar(&o.force, "force", false, "allow a fixed (non-removable, non-USB) disk")
		if fs.Parse(rest) != nil || fs.NArg() != 0 {
			return 2
		}
		o.minFree = *minFreeMB << 20
		return initData(o, ops, stdout, stderr)

	case "pick-binary":
		cpuinfo := fs.String("cpuinfo", "/proc/cpuinfo", "cpuinfo file")
		dir := fs.String("dir", "/usr/bin", "directory holding savior, savior-sse2, savior-softfloat")
		if fs.Parse(rest) != nil || fs.NArg() != 0 {
			return 2
		}
		return pickBinary(*cpuinfo, *dir, stdout, stderr)

	case "fingerprint":
		if fs.Parse(rest) != nil || fs.NArg() != 1 {
			fmt.Fprintln(stderr, "usage: savior storage fingerprint <cert.pem>")
			return 2
		}
		fp, err := certFingerprint(fs.Arg(0))
		if err != nil {
			fmt.Fprintf(stderr, "savior storage: fingerprint: %v\n", err)
			return 1
		}
		fmt.Fprintln(stdout, fp)
		return 0

	case "probe":
		field := fs.String("field", "", "print only this field: type, label or uuid")
		if fs.Parse(rest) != nil || fs.NArg() == 0 {
			fmt.Fprintln(stderr, "usage: savior storage probe [--field type|label|uuid] DEVICE...")
			return 2
		}
		rc := 0
		for _, dev := range fs.Args() {
			fi, err := ProbeFile(dev)
			if err != nil {
				fmt.Fprintf(stderr, "savior storage: probe %s: %v\n", dev, err)
				rc = 1
				continue
			}
			switch *field {
			case "":
				fmt.Fprintf(stdout, "%s: TYPE=%q LABEL=%q UUID=%q\n", dev, fi.Type, fi.Label, fi.UUID)
			case "type":
				fmt.Fprintln(stdout, fi.Type)
			case "label":
				fmt.Fprintln(stdout, fi.Label)
			case "uuid":
				fmt.Fprintln(stdout, fi.UUID)
			default:
				fmt.Fprintf(stderr, "savior storage: probe: unknown field %q\n", *field)
				return 2
			}
			if fi.Type == "" {
				rc = 1
			}
		}
		return rc

	case "find-label":
		sysRoot := fs.String("sys-root", "/", "root for /sys and /dev (tests)")
		if fs.Parse(rest) != nil || fs.NArg() != 1 {
			fmt.Fprintln(stderr, "usage: savior storage find-label LABEL")
			return 2
		}
		devs, err := ListBlockDevices(*sysRoot)
		if err != nil {
			fmt.Fprintf(stderr, "savior storage: find-label: %v\n", err)
			return 1
		}
		for _, d := range devs {
			if fi, err := ProbeFile(d.Path); err == nil && fi.Type != "" && fi.Label == fs.Arg(0) {
				fmt.Fprintln(stdout, d.Path)
				return 0
			}
		}
		return 1
	}
	fmt.Fprintf(stderr, "savior storage: unknown command %q\n%s\n", cmd, usageText)
	return 2
}

// seconds is a duration flag that accepts plain seconds ("20", "0.5") or a
// Go duration ("1m").
type seconds time.Duration

func (s *seconds) String() string { return time.Duration(*s).String() }

func (s *seconds) Set(v string) error {
	if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
		if f < 0 {
			return fmt.Errorf("negative duration")
		}
		*s = seconds(f * float64(time.Second))
		return nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < 0 {
		return fmt.Errorf("want seconds or a duration like 1m")
	}
	*s = seconds(d)
	return nil
}
