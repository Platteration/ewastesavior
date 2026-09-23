package storage

import (
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// mountOpts are the portable mount flags plus filesystem-specific data.
type mountOpts struct {
	ReadOnly, NoSuid, NoDev, NoExec, NoAtime bool
	Data                                     string
}

// sysOps are the privileged operations, replaceable in tests.
type sysOps struct {
	mount    func(source, target, fstype string, o mountOpts) error
	unmount  func(target string) error
	rereadPT func(disk string) error                             // BLKRRPART
	addPart  func(disk string, p DataPlan, sectorSize int) error // BLKPG_ADD_PARTITION
	mknod    func(path, majMin string) error
	// loadModules runs modprobe for kernel modules, ignoring failures.
	loadModules func(names []string)
}

// Media is the label of the SaviorOS FAT partition (and ISO volume ID).
const MediaLabel = "SAVIOR"

// mediaSpec is the parsed savior.media= kernel argument.
type mediaSpec struct {
	Raw   string
	None  bool   // savior.media=none (netboot): don't look for a medium
	UUID  string // normalized (uppercase, no dashes)
	Label string
}

func (s mediaSpec) specific() bool { return s.UUID != "" || s.Label != "" }

// markerPrefix is the start of the build marker file name GRUB searches for
// (/boot/savior-<build id>.id). mkimage derives the FAT volume serial from
// the first 8 hex digits of the build ID, so a CD or ISO-hybrid stick of the
// same build can be recognized from the serial on the command line.
func (s mediaSpec) markerPrefix() string {
	if len(s.UUID) == 8 && isHex(s.UUID) {
		return "savior-" + strings.ToLower(s.UUID)
	}
	return ""
}

func isHex(s string) bool {
	for _, c := range s {
		if !strings.ContainsRune("0123456789abcdefABCDEF", c) {
			return false
		}
	}
	return s != ""
}

// parseMediaSpec extracts savior.media= from a kernel command line. Accepted
// values: UUID=XXXX-XXXX (vfat serial, ext UUID or iso9660 UUID), a bare
// UUID, LABEL=name, or none.
func parseMediaSpec(cmdline string) mediaSpec {
	var raw string
	for _, tok := range splitCmdline(cmdline) {
		if v, ok := strings.CutPrefix(tok, "savior.media="); ok {
			raw = v // the last one wins, like other cmdline keys
		}
	}
	if dv, err := url.PathUnescape(raw); err == nil {
		raw = dv
	}
	s := mediaSpec{Raw: raw}
	switch {
	case raw == "":
	case strings.EqualFold(raw, "none"):
		s.None = true
	case strings.HasPrefix(strings.ToUpper(raw), "UUID="):
		s.UUID = normUUID(raw[5:])
	case strings.HasPrefix(strings.ToUpper(raw), "LABEL="):
		s.Label = raw[6:]
	default:
		s.UUID = normUUID(raw)
	}
	return s
}

// splitCmdline splits a kernel command line on whitespace, honoring double
// quotes the way the kernel does.
func splitCmdline(s string) []string {
	var out []string
	var cur strings.Builder
	inQ, have := false, false
	for _, r := range s {
		switch {
		case r == '"':
			inQ, have = !inQ, true
		case !inQ && (r == ' ' || r == '\t' || r == '\n' || r == '\r'):
			if have {
				out = append(out, cur.String())
				cur.Reset()
				have = false
			}
		default:
			cur.WriteRune(r)
			have = true
		}
	}
	if have {
		out = append(out, cur.String())
	}
	return out
}

// Candidate ranks, best first.
const (
	rankNone    = 0
	rankExact   = 1 // savior.media matches a vfat/ext filesystem
	rankLabel   = 2 // LABEL=SAVIOR on vfat/ext: a SaviorOS stick
	rankPlain   = 3 // a removable vfat stick with /savior.conf
	rankOptical = 4 // an iso9660 labelled SAVIOR or with /savior.conf (the boot CD)
)

type candidate struct {
	Dev     BlockDev
	FS      FSInfo
	order   int // position in device search order
	checked bool
	fails   int  // failed trial mounts
	HasConf bool // /savior.conf exists (trial mount)
	Marker  bool // this is the medium we booted from (build marker / ISO UUID)
	Rank    int
}

func (c *candidate) String() string {
	s := fmt.Sprintf("%s %s", c.Dev.Path, c.FS.Type)
	if c.FS.Label != "" {
		s += " label=" + c.FS.Label
	}
	if c.FS.UUID != "" {
		s += " uuid=" + c.FS.UUID
	}
	return s
}

// finder implements find-media.
type finder struct {
	sysRoot  string
	spec     mediaSpec
	ops      sysOps
	probeDir string // parent directory for trial mounts
	settle   time.Duration
	poll     time.Duration
	now      func() time.Time
	sleep    func(time.Duration)
	logf     func(format string, args ...any)

	// inspect trial-mounts a device and reports whether /savior.conf and a
	// build marker matching the spec exist. Tests replace it.
	inspect func(c *candidate) (hasConf, marker bool, err error)

	cache map[string]*candidate
}

func newFinder(sysRoot string, spec mediaSpec, ops sysOps, logf func(string, ...any)) *finder {
	f := &finder{
		sysRoot: sysRoot, spec: spec, ops: ops, settle: 2 * time.Second, poll: 500 * time.Millisecond,
		now: time.Now, sleep: time.Sleep, logf: logf, cache: map[string]*candidate{},
	}
	f.inspect = f.trialInspect
	return f
}

// scan probes every block device once and returns the ranked candidates,
// best first.
func (f *finder) scan() []*candidate {
	devs, err := ListBlockDevices(f.sysRoot)
	if err != nil {
		f.logf("list block devices: %v", err)
		return nil
	}
	var out []*candidate
	for i, d := range devs {
		key := fmt.Sprintf("%s:%d", d.Name, d.Size)
		c := f.cache[key]
		if c == nil {
			fs, err := ProbeFile(d.Path)
			if err != nil {
				continue // not readable yet (e.g. a CD spinning up); retry next round
			}
			c = &candidate{Dev: d, FS: fs}
			f.cache[key] = c
		}
		c.order = i
		if c.Rank = f.rank(c); c.Rank != rankNone {
			out = append(out, c)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Rank != b.Rank {
			return a.Rank < b.Rank
		}
		if a.Marker != b.Marker {
			return a.Marker
		}
		if la, lb := strings.EqualFold(a.FS.Label, MediaLabel), strings.EqualFold(b.FS.Label, MediaLabel); la != lb {
			return la
		}
		return a.order < b.order
	})
	return out
}

func (f *finder) rank(c *candidate) int {
	fs := c.FS
	switch {
	case fs.Type == "vfat" || fs.IsExt():
		if f.spec.UUID != "" && fs.UUID != "" && normUUID(fs.UUID) == f.spec.UUID {
			return rankExact
		}
		if f.spec.Label != "" && strings.EqualFold(fs.Label, f.spec.Label) {
			return rankExact
		}
		if strings.EqualFold(fs.Label, MediaLabel) {
			return rankLabel
		}
		if fs.Type == "vfat" && c.Dev.Portable() {
			f.check(c)
			if c.HasConf {
				return rankPlain
			}
		}
	case fs.Type == "iso9660":
		if f.spec.UUID != "" && fs.UUID != "" && normUUID(fs.UUID) == f.spec.UUID {
			c.Marker = true
		}
		label := strings.EqualFold(fs.Label, MediaLabel)
		if label || c.Dev.Portable() {
			f.check(c)
		}
		if label || c.HasConf {
			return rankOptical
		}
	}
	return rankNone
}

// check runs the trial mount once per device.
func (f *finder) check(c *candidate) {
	if c.checked {
		return
	}
	hasConf, marker, err := f.inspect(c)
	if err != nil {
		f.logf("inspect %s: %v", c.Dev.Path, err)
		if c.fails++; c.fails >= 3 {
			c.checked = true // give up on this device
		}
		return // retried on the next scan
	}
	c.checked = true
	c.HasConf = hasConf
	c.Marker = c.Marker || marker
}

// find polls until the best possible medium is present or wait expires.
func (f *finder) find(wait time.Duration) *candidate {
	start := f.now()
	deadline := start.Add(wait)
	var settleSince time.Time
	var announced bool
	for {
		cands := f.scan()
		var best *candidate
		if len(cands) > 0 {
			best = cands[0]
		}
		now := f.now()
		if best != nil {
			if best.Rank == rankExact {
				return best
			}
			if !f.spec.specific() && best.Rank == rankLabel {
				return best
			}
			booted := false
			for _, c := range cands {
				booted = booted || c.Marker
			}
			// Without a specific savior.media, or once the medium we booted
			// from turned out to be a CD/ISO, the best candidate so far is
			// taken after a short settle time for slower USB devices.
			if !f.spec.specific() || booted {
				if settleSince.IsZero() {
					settleSince = now
				}
				if now.Sub(settleSince) >= f.settle {
					return best
				}
			}
		}
		if !now.Before(deadline) {
			return best
		}
		if !announced && now.Sub(start) >= 3*time.Second {
			announced = true
			what := "a SaviorOS stick"
			if f.spec.specific() {
				what = "the boot medium (" + f.spec.Raw + ")"
			}
			f.logf("waiting up to %ds for %s", int(wait.Seconds()), what)
		}
		f.sleep(f.poll)
	}
}

var (
	vfatBase = "uid=0,gid=0,fmask=0177,dmask=0077"
	vfatNLS  = ",iocharset=utf8,codepage=437"
)

type mountAttempt struct{ fstype, data string }

// mediaMountAttempts lists the read-only mount variants for a medium, most
// restrictive first (DESIGN 6.5). vfat always loads an I/O charset: the
// retry without iocharset/codepage uses the kernel's default (often
// iso8859-1, a module on distribution kernels), the last one only needs
// nls_cp437, which both SaviorOS kernels have built in (the files SaviorOS
// reads have ASCII names).
func mediaMountAttempts(fs FSInfo) []mountAttempt {
	switch {
	case fs.Type == "vfat":
		return []mountAttempt{
			{"vfat", vfatBase + vfatNLS},
			{"vfat", vfatBase},
			{"vfat", vfatBase + ",codepage=437,iocharset=cp437"},
		}
	case fs.Type == "iso9660":
		return []mountAttempt{
			{"iso9660", "uid=0,gid=0,mode=0600,dmode=0700,overriderockperms"},
			{"iso9660", "uid=0,gid=0,mode=0600,dmode=0700"},
			{"iso9660", ""},
		}
	case fs.IsExt():
		out := []mountAttempt{{"ext4", ""}}
		if fs.Type != "ext4" {
			out = append(out, mountAttempt{fs.Type, ""})
		}
		return out
	}
	return nil
}

// fsModules are the modules a medium may need, loaded best effort once per
// process (the SaviorOS kernels have them built in).
var (
	fsModules = map[string][]string{
		"vfat":    {"vfat", "nls_cp437", "nls_utf8", "nls_iso8859_1"},
		"iso9660": {"isofs"},
	}
	fsModulesTried = map[string]bool{}
)

// mountMedium mounts dev read-only, nosuid, nodev, noexec at dir.
func mountMedium(ops sysOps, dev string, fs FSInfo, dir string) (string, error) {
	attempts := mediaMountAttempts(fs)
	if len(attempts) == 0 {
		return "", fmt.Errorf("unsupported filesystem %q", fs.Type)
	}
	if mods := fsModules[fs.Type]; len(mods) > 0 && ops.loadModules != nil && !fsModulesTried[fs.Type] {
		fsModulesTried[fs.Type] = true
		ops.loadModules(mods)
	}
	var errs []error
	for _, a := range attempts {
		err := ops.mount(dev, dir, a.fstype, mountOpts{ReadOnly: true, NoSuid: true, NoDev: true, NoExec: true, Data: a.data})
		if err == nil {
			return a.data, nil
		}
		errs = append(errs, err)
	}
	return "", errors.Join(errs...)
}

// trialInspect mounts the candidate on a temporary directory and looks for
// /savior.conf and the build marker.
func (f *finder) trialInspect(c *candidate) (hasConf, marker bool, err error) {
	parent := f.probeDir
	if parent == "" {
		parent = os.TempDir()
	}
	dir, err := os.MkdirTemp(parent, "savior-probe-")
	if err != nil {
		return false, false, err
	}
	defer os.Remove(dir)
	if _, err := mountMedium(f.ops, c.Dev.Path, c.FS, dir); err != nil {
		return false, false, err
	}
	defer func() {
		if uerr := f.ops.unmount(dir); uerr != nil && err == nil {
			err = uerr
		}
	}()
	return inspectTree(dir, f.spec.markerPrefix())
}

// inspectTree reports whether root holds savior.conf and a marker file
// /boot/<markerPrefix>*.id.
func inspectTree(root, markerPrefix string) (hasConf, marker bool, err error) {
	if st, err := os.Stat(filepath.Join(root, "savior.conf")); err == nil && st.Mode().IsRegular() {
		hasConf = true
	}
	if markerPrefix != "" {
		entries, _ := os.ReadDir(filepath.Join(root, "boot"))
		for _, e := range entries {
			n := strings.ToLower(e.Name())
			if strings.HasPrefix(n, markerPrefix) && strings.HasSuffix(n, ".id") {
				marker = true
			}
		}
	}
	return hasConf, marker, nil
}

// findMediaOpts are the find-media flags.
type findMediaOpts struct {
	cmdlineFile string
	wait        time.Duration
	mountpoint  string
	sysRoot     string
	settle      time.Duration
	dryRun      bool
}

var safeMountpoint = regexp.MustCompile(`^/[A-Za-z0-9._/-]+$`)

func findMedia(o findMediaOpts, ops sysOps, stdout, stderr io.Writer) int {
	logf := func(format string, args ...any) { fmt.Fprintf(stderr, "savior storage: "+format+"\n", args...) }
	if o.mountpoint != "" && !safeMountpoint.MatchString(o.mountpoint) {
		logf("invalid --mount %q", o.mountpoint)
		return 2
	}
	var cmdline string
	if o.cmdlineFile != "" {
		b, err := os.ReadFile(o.cmdlineFile)
		if err != nil && !os.IsNotExist(err) {
			logf("read %s: %v", o.cmdlineFile, err)
		}
		cmdline = string(b)
	}
	spec := parseMediaSpec(cmdline)
	if spec.None {
		logf("savior.media=none: not looking for a config medium")
		return 1
	}

	// Already mounted (S08config restart): report it.
	if o.mountpoint != "" && !o.dryRun {
		if mi, err := ReadMountInfo(filepath.Join(o.sysRoot, "proc", "self", "mountinfo")); err == nil {
			if e, ok := findMount(mi, o.mountpoint); ok {
				logf("%s already mounted on %s", e.Source, o.mountpoint)
				fmt.Fprintf(stdout, "%s %s\n", e.Source, e.FSType)
				return 0
			}
		}
	}

	f := newFinder(o.sysRoot, spec, ops, logf)
	if o.settle > 0 {
		f.settle = o.settle
	}
	f.probeDir = filepath.Join(o.sysRoot, "run")
	if st, err := os.Stat(f.probeDir); err != nil || !st.IsDir() {
		f.probeDir = ""
	}
	c := f.find(o.wait)
	if c == nil {
		logf("no config medium found (savior.media=%q)", spec.Raw)
		return 1
	}
	why := map[int]string{rankExact: "savior.media match", rankLabel: "label " + MediaLabel,
		rankPlain: "removable stick with savior.conf", rankOptical: "SaviorOS CD/ISO"}[c.Rank]
	logf("config medium: %s (%s, %s)", c, c.Dev, why)
	if o.dryRun || o.mountpoint == "" {
		fmt.Fprintf(stdout, "%s %s\n", c.Dev.Path, c.FS.Type)
		return 0
	}
	if err := os.MkdirAll(o.mountpoint, 0o700); err != nil {
		logf("create %s: %v", o.mountpoint, err)
		return 1
	}
	data, err := mountMedium(ops, c.Dev.Path, c.FS, o.mountpoint)
	if err != nil {
		logf("mount %s on %s: %v", c.Dev.Path, o.mountpoint, err)
		return 1
	}
	if data != "" {
		data = " (" + data + ")"
	}
	logf("mounted %s read-only on %s%s", c.Dev.Path, o.mountpoint, data)
	fmt.Fprintf(stdout, "%s %s\n", c.Dev.Path, c.FS.Type)
	return 0
}
