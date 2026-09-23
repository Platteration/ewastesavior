package ctl

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/platteration/ewastesavior/internal/config"
	"github.com/platteration/ewastesavior/internal/proto"
)

func cmdLogin(a *app, ctx context.Context, args []string) int {
	f := a.flagSet("login", "[--hive ADDR] [--token T] [--fingerprint FP]",
		`Log in to a hive and remember it, its certificate fingerprint and the
session in ctl.json. The admin token (flag, SAVIOR_ADMIN_TOKEN, or a hidden
prompt) is never sent: the hive gets a one-time proof bound to its
certificate and proves in return that it knows the token.`)
	if _, code, ok := a.parse(f, args, 0, 0); !ok {
		return code
	}
	c, err := a.connect(ctx)
	if err != nil {
		return a.fail(err)
	}
	res, err := c.Login(ctx)
	if err != nil {
		return a.fail(err)
	}
	if a.g.json {
		return a.printJSON(map[string]any{
			"hive": res.Hive, "hive_id": res.HiveID, "version": res.Version,
			"fingerprint": res.Fingerprint, "session_expires": res.Expires, "config": a.cfgPath,
		})
	}
	fmt.Fprintf(a.stdout, "Logged in to %s (hive %s, %s).\n", res.Hive, sanitizeCell(res.HiveID), sanitizeCell(res.Version))
	fmt.Fprintf(a.stdout, "Fingerprint %s is pinned", res.Fingerprint)
	if res.FirstContact {
		fmt.Fprint(a.stdout, " (verified by the hive's login proof)")
	}
	fmt.Fprintln(a.stdout, ".")
	if !res.Expires.IsZero() {
		fmt.Fprintf(a.stdout, "The session is valid until %s.\n", fmtTime(res.Expires))
	}
	return 0
}

func cmdLogout(a *app, ctx context.Context, args []string) int {
	f := a.flagSet("logout", "[--all]", "End the stored admin session. With --all, revoke every admin session on the hive (browsers too).")
	all := f.Bool("all", false, "revoke every admin session")
	if _, code, ok := a.parse(f, args, 0, 0); !ok {
		return code
	}
	c, err := a.connect(ctx)
	if err != nil {
		return a.fail(err)
	}
	if *all {
		err = c.RevokeAllSessions(ctx)
	} else if a.set.session == "" {
		fmt.Fprintln(a.stdout, "Not logged in.")
		return 0
	} else if lerr := c.Logout(ctx); lerr != nil && !errors.Is(lerr, ErrSessionExpired) {
		a.warn("the hive did not confirm the logout: %s", sanitize(lerr.Error()))
	}
	if err != nil {
		return a.fail(err)
	}
	if a.set.sameAsFile && a.file.Session != "" {
		fc := a.file
		fc.Session, fc.SessionExpires = "", time.Time{}
		if err := saveFileConfig(a.cfgPath, fc); err != nil {
			return a.fail(err)
		}
	}
	if *all {
		fmt.Fprintln(a.stdout, "All admin sessions were revoked.")
	} else {
		fmt.Fprintln(a.stdout, "Logged out.")
	}
	return 0
}

func cmdPair(a *app, ctx context.Context, args []string) int {
	f := a.flagSet("pair", "", "Print a single-use pairing code (valid 10 minutes) to log a browser in to the hive's dashboard.")
	if _, code, ok := a.parse(f, args, 0, 0); !ok {
		return code
	}
	c, err := a.connect(ctx)
	if err != nil {
		return a.fail(err)
	}
	pc, err := c.Pair(ctx)
	if err != nil {
		return a.fail(err)
	}
	if a.g.json {
		return a.printJSON(pc)
	}
	fmt.Fprintf(a.stdout, "Pairing code: %s\n", sanitizeCell(pc.Code))
	if !pc.ExpiresAt.IsZero() {
		fmt.Fprintf(a.stdout, "Single use, valid until %s (hive clock).\n", fmtTime(pc.ExpiresAt))
	}
	fmt.Fprintf(a.stdout, "Open %s/ in a browser and enter it.\n", c.Base())
	return 0
}

func cmdInfo(a *app, ctx context.Context, args []string) int {
	f := a.flagSet("info", "", "Show the hive's ID, version, fingerprint, addresses, storage and clock.")
	if _, code, ok := a.parse(f, args, 0, 0); !ok {
		return code
	}
	c, err := a.connect(ctx)
	if err != nil {
		return a.fail(err)
	}
	hi, err := c.Info(ctx)
	if err != nil {
		return a.fail(err)
	}
	if hi.Fingerprint != "" && hi.Fingerprint != c.Fingerprint() {
		a.warn("the hive reports fingerprint %s but the connection is pinned to %s", sanitizeCell(hi.Fingerprint), c.Fingerprint())
	}
	if a.g.json {
		return a.printJSON(hi)
	}
	k := newKV(a.stdout)
	k.add("Hive", hi.HiveID)
	k.add("Version", fmt.Sprintf("%s (API %d)", hi.Version, hi.APIVersion))
	k.add("Address", c.Base())
	k.add("Fingerprint", hi.Fingerprint)
	k.add("Swarm hint", hi.SwarmHint)
	k.add("URLs", strings.Join(hi.URLs, " "))
	k.add("Listen", hi.Listen)
	storage := hi.DataDir
	if !hi.Persistent {
		storage += " (NOT persistent: state is lost when the hive restarts)"
	}
	k.add("Data", storage)
	k.add("Join policy", hi.JoinPolicy)
	k.add("Started", fmtTime(hi.StartedAt))
	k.add("Clock", clockState(hi.TimeSynced, hi.TimeSource))
	k.flush()
	for _, w := range hi.Warnings {
		fmt.Fprintf(a.stdout, "Warning: %s\n", sanitizeCell(w))
	}
	return 0
}

func clockState(synced bool, source string) string {
	if synced {
		return "synchronized (" + source + ")"
	}
	if source == "" {
		return "not synchronized"
	}
	return "not synchronized (" + source + ")"
}

func cmdTime(a *app, ctx context.Context, args []string) int {
	f := a.flagSet("time", "", `Compare this computer's clock with the hive's. Every admin request sends
this computer's time; a SaviorOS hive without NTP sets its clock from it
when they differ by more than a minute.`)
	if _, code, ok := a.parse(f, args, 0, 0); !ok {
		return code
	}
	c, err := a.connect(ctx)
	if err != nil {
		return a.fail(err)
	}
	hi, err := c.Info(ctx) // logs in if needed and hands over our clock
	if err != nil {
		return a.fail(err)
	}
	t0 := a.now()
	h, err := c.Hello(ctx)
	if err != nil {
		return a.fail(err)
	}
	rtt := a.now().Sub(t0)
	local := t0.Add(rtt / 2)
	offset := h.Time.Sub(local)
	if a.g.json {
		return a.printJSON(map[string]any{
			"local_time": local, "hive_time": h.Time, "offset_seconds": offset.Seconds(),
			"rtt_ms": rtt.Milliseconds(), "time_synced": hi.TimeSynced, "time_source": hi.TimeSource,
		})
	}
	k := newKV(a.stdout)
	k.add("This computer", local.Format("2006-01-02 15:04:05.000 MST"))
	k.add("Hive", h.Time.In(local.Location()).Format("2006-01-02 15:04:05.000 MST"))
	dir := "ahead of"
	if offset < 0 {
		dir = "behind"
	}
	k.add("Offset", fmt.Sprintf("the hive is %s %s this computer (round trip %s)", fmtDuration(offset.Abs()), dir, rtt.Round(time.Millisecond)))
	k.add("Hive clock", clockState(hi.TimeSynced, hi.TimeSource))
	k.flush()
	if offset.Abs() > time.Minute && !hi.TimeSynced {
		a.warn("the hive's clock is off and not synchronized; it can only set it from this computer on SaviorOS as root")
	}
	return 0
}

func cmdFind(a *app, ctx context.Context, args []string) int {
	f := a.flagSet("find", "[--seconds N]", `List hives announcing themselves on the local network (UDP 7701). Beacons
are unauthenticated: always compare the fingerprint with the hive's screen.`)
	seconds := f.Int("seconds", 3, "how long to listen (1-60)")
	if _, code, ok := a.parse(f, args, 0, 0); !ok {
		return code
	}
	if *seconds < 1 || *seconds > 60 {
		return a.usageError("--seconds must be 1..60")
	}
	pinned := ""
	if err := a.loadSettings(); err == nil {
		pinned = a.set.fingerprint
	}
	fmt.Fprintf(a.stderr, "Listening for hives for %d s...\n", *seconds)
	cands, err := a.discover(ctx, time.Duration(*seconds)*time.Second)
	if err != nil {
		return a.fail(fmt.Errorf("discovery: %w", err))
	}
	type found struct {
		Address     string `json:"address"`
		HiveID      string `json:"hive_id"`
		Fingerprint string `json:"fingerprint"`
		SwarmHint   string `json:"swarm_hint"`
		Version     string `json:"version"`
		Pinned      bool   `json:"pinned"`
	}
	out := make([]found, 0, len(cands))
	for _, cd := range cands {
		fp, err := NormalizeFingerprint(cd.Beacon.Fingerprint)
		if err != nil {
			fp = "(invalid)"
		}
		out = append(out, found{
			Address:     sanitizeCell(cd.URL),
			HiveID:      truncate(sanitizeCell(cd.Beacon.HiveID), 40),
			Fingerprint: fp,
			SwarmHint:   truncate(sanitizeCell(cd.Beacon.SwarmHint), 16),
			Version:     truncate(sanitizeCell(cd.Beacon.Version), 32),
			Pinned:      pinned != "" && fp == pinned,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Address < out[j].Address })
	if a.g.json {
		return a.printJSON(out)
	}
	if len(out) == 0 {
		fmt.Fprintln(a.stderr, "No hive answered. Check that the hive is running on this network and that its firewall allows UDP 7701 and TCP 7700.")
		return 1
	}
	t := newTable(a.stdout, "ADDRESS", "HIVE ID", "SWARM", "VERSION", "FINGERPRINT")
	for _, h := range out {
		fp := h.Fingerprint
		if h.Pinned {
			fp += " (pinned)"
		}
		t.row(h.Address, h.HiveID, h.SwarmHint, h.Version, fp)
	}
	t.flush()
	fmt.Fprintln(a.stderr, "\nNodes join the hive whose swarm hint matches their swarm key. To log in:\n  savior ctl login --hive ADDRESS\nand compare the fingerprint it prints with the hive's screen.")
	return 0
}

// crockford is the Crockford base32 alphabet (no I, L, O, U).
const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// GenerateSwarmKey returns 32 random Crockford base32 characters (160 bits).
func GenerateSwarmKey() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("random: %w", err)
	}
	for i := range b {
		b[i] = crockford[b[i]&31] // 256 is a multiple of 32: unbiased
	}
	return string(b[:]), nil
}

func cmdGenkey(a *app, _ context.Context, args []string) int {
	f := a.flagSet("genkey", "", "Print a new random swarm key (32 Crockford base32 characters) for savior.conf. Works offline.")
	if _, code, ok := a.parse(f, args, 0, 0); !ok {
		return code
	}
	key, err := GenerateSwarmKey()
	if err != nil {
		return a.fail(err)
	}
	if a.g.json {
		return a.printJSON(map[string]string{"swarm_key": key})
	}
	fmt.Fprintln(a.stdout, key)
	return 0
}

func cmdNodeConfig(a *app, ctx context.Context, args []string) int {
	f := a.flagSet("node-config", "[--hive-addr ADDR] [-o savior.conf] [--force]",
		`Write a savior.conf for nodes: the swarm key, the hive's fingerprint pin and
its address. Copy it to the root of the SAVIOR partition of each node's stick.
It contains the swarm key, so keep it private.`)
	hiveAddr := f.String("hive-addr", "", "address nodes should use for the hive (default: the one this command uses)")
	out := f.String("o", "savior.conf", "output file, or - for standard output")
	force := f.Bool("force", false, "overwrite an existing file")
	if _, code, ok := a.parse(f, args, 0, 0); !ok {
		return code
	}
	if *hiveAddr != "" {
		if _, err := config.HiveURL(*hiveAddr); err != nil {
			return a.usageError("invalid --hive-addr: %v", err)
		}
	}
	c, err := a.connect(ctx)
	if err != nil {
		return a.fail(err)
	}
	conf, err := c.NodeConfig(ctx, *hiveAddr)
	if err != nil {
		return a.fail(err)
	}
	if err := checkNodeConfig(conf, c.Fingerprint()); err != nil {
		return a.fail(err)
	}
	if *out == "-" {
		fmt.Fprint(a.stdout, conf)
		return 0
	}
	if err := writeNewFile(*out, []byte(conf), 0o600, *force); err != nil {
		return a.fail(err)
	}
	fmt.Fprintf(a.stderr, "Wrote %s. Copy it to the root of the SAVIOR partition of each node's stick.\nIt contains the swarm key: anyone who has it can join machines to this swarm.\n", *out)
	return 0
}

// checkNodeConfig makes sure the generated file pins the certificate this
// client verified, so nodes will not trust anything else.
func checkNodeConfig(conf, verifiedFP string) error {
	cfg := config.Default()
	config.ParseFile(strings.NewReader(conf), "node-config", &cfg)
	if cfg.SwarmKey == "" {
		return errors.New("the hive's node config has no swarm_key")
	}
	if cfg.HiveFingerprint != verifiedFP {
		return fmt.Errorf("the hive's node config pins %q, not the verified fingerprint %s; not writing it",
			sanitizeCell(cfg.HiveFingerprint), verifiedFP)
	}
	return nil
}

// writeNewFile writes data to name. Without force an existing file is an
// error (O_EXCL); with force the file is replaced atomically.
func writeNewFile(name string, data []byte, perm os.FileMode, force bool) error {
	if !force {
		f, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
		if err != nil {
			if errors.Is(err, fs.ErrExist) {
				return fmt.Errorf("%s already exists (use --force to overwrite it)", name)
			}
			return err
		}
		_, err = f.Write(data)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			os.Remove(name)
		}
		return err
	}
	dir := filepath.Dir(name)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(name)+".*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(perm); err != nil && !isWindows() {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, name)
}

// describeNode is used by several commands to name a node in messages.
func describeNode(n proto.NodeView) string {
	if n.Name != "" {
		return sanitizeCell(n.Name)
	}
	return sanitizeCell(n.ID)
}
