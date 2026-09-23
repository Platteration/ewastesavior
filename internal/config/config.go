// Package config parses savior.conf files and the kernel command line into a
// typed Config. See docs/DESIGN.md section 5.
package config

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata" // timezone names work on a node without /usr/share/zoneinfo

	"github.com/platteration/ewastesavior/internal/proto"
)

// Default locations.
const (
	ImageConfigFile  = "/etc/savior/savior.conf"
	BakedConfigFile  = "/etc/savior/baked.conf"
	StickConfigFile  = "/media/savior/savior.conf"
	MergedConfigFile = "/run/savior/savior.conf"
	CmdlineFile      = "/proc/cmdline"
	CmdlinePrefix    = "savior."
	MinSwarmKeyLen   = 16
	MinAdminTokenLen = 32
)

// DefaultFiles is the config file search list used when no --file is given.
var DefaultFiles = []string{ImageConfigFile, BakedConfigFile, StickConfigFile}

// Config holds every savior.conf key. Field comments name the key.
type Config struct {
	Name   string            // name
	NodeID string            // node_id
	Roles  []string          // roles
	Labels map[string]string // labels

	Hive            string // hive
	SwarmKey        string // swarm_key
	HiveFingerprint string // hive_fingerprint
	Join            string // join

	Net         string   // net
	IP          string   // ip
	Gateway     string   // gateway
	DNS         []string // dns
	WifiSSID    string   // wifi_ssid
	WifiPSK     string   // wifi_psk
	WifiCountry string   // wifi_country
	DHCPServer  bool     // dhcp_server
	DHCPRange   string   // dhcp_range
	Netboot     bool     // netboot
	NTP         []string // ntp

	SSHKeys      []string // ssh_key
	ConsoleShell bool     // console_shell

	MaxCPUPercent int    // max_cpu_percent
	MaxMemPercent int    // max_mem_percent
	Sandbox       string // sandbox
	Scratch       string // scratch
	ScratchWipe   bool   // scratch_wipe

	RunOnBattery      bool   // run_on_battery
	BatteryMinPercent int    // battery_min_percent
	MaxTempC          int    // max_temp_c
	CPUGovernor       string // cpu_governor

	DisplayMode    string // display_mode
	DisplayText    string // display_text
	DisplayRotate  int    // display_rotate
	DisplayDevice  string // display_device
	DisplayIdleOff int    // display_idle_off_min

	HiveListen string // hive_listen
	HiveData   string // hive_data
	AdminToken string // admin_token
	JoinPolicy string // join_policy
	Beacon     bool   // beacon

	LogLevel string // log_level
	Timezone string // timezone
}

// KeyInfo documents one key.
type KeyInfo struct {
	Name    string
	Type    string // str, int, bool, list
	Default string
	Help    string
	Secret  bool
}

type keyDef struct {
	KeyInfo
	get func(c *Config) []string        // canonical values (one element unless list)
	set func(c *Config, v string) error // parse and assign one value (replace)
	add func(c *Config, v string) error // list keys only: parse and append one value
	rst func(c *Config)                 // list keys only: clear
}

var (
	nameRE   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,62}$`)
	labelRE  = regexp.MustCompile(`^[a-z0-9][a-z0-9_.\-/]{0,62}$`)
	fpRE     = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	countryR = regexp.MustCompile(`^[A-Z]{2}$`)
)

func str(name, def, help string, secret bool, get func(*Config) *string, check func(string) (string, error)) keyDef {
	return keyDef{
		KeyInfo: KeyInfo{Name: name, Type: "str", Default: def, Help: help, Secret: secret},
		get:     func(c *Config) []string { return []string{*get(c)} },
		set: func(c *Config, v string) error {
			if check != nil {
				nv, err := check(v)
				if err != nil {
					return err
				}
				v = nv
			}
			*get(c) = v
			return nil
		},
	}
}

func integer(name string, def, lo, hi int, help string, get func(*Config) *int, allowed ...int) keyDef {
	return keyDef{
		KeyInfo: KeyInfo{Name: name, Type: "int", Default: strconv.Itoa(def), Help: help},
		get:     func(c *Config) []string { return []string{strconv.Itoa(*get(c))} },
		set: func(c *Config, v string) error {
			n, err := strconv.Atoi(strings.TrimSpace(v))
			if err != nil {
				return fmt.Errorf("not an integer")
			}
			if len(allowed) > 0 {
				ok := false
				for _, a := range allowed {
					ok = ok || a == n
				}
				if !ok {
					return fmt.Errorf("must be one of %v", allowed)
				}
			} else if n < lo || n > hi {
				return fmt.Errorf("must be between %d and %d", lo, hi)
			}
			*get(c) = n
			return nil
		},
	}
}

func boolean(name string, def bool, help string, get func(*Config) *bool) keyDef {
	return keyDef{
		KeyInfo: KeyInfo{Name: name, Type: "bool", Default: fmtBool(def), Help: help},
		get:     func(c *Config) []string { return []string{fmtBool(*get(c))} },
		set: func(c *Config, v string) error {
			b, err := ParseBool(v)
			if err != nil {
				return err
			}
			*get(c) = b
			return nil
		},
	}
}

func list(name, def, help string, get func(*Config) *[]string, check func(string) (string, error)) keyDef {
	add := func(c *Config, v string) error {
		for _, part := range splitList(name, v) {
			if check != nil {
				nv, err := check(part)
				if err != nil {
					return fmt.Errorf("%q: %w", part, err)
				}
				part = nv
			}
			*get(c) = append(*get(c), part)
		}
		return nil
	}
	return keyDef{
		KeyInfo: KeyInfo{Name: name, Type: "list", Default: def, Help: help},
		get:     func(c *Config) []string { return append([]string(nil), *get(c)...) },
		set: func(c *Config, v string) error {
			old := *get(c)
			*get(c) = nil
			if err := add(c, v); err != nil {
				*get(c) = old
				return err
			}
			return nil
		},
		add: add,
		rst: func(c *Config) { *get(c) = nil },
	}
}

// splitList splits a list value. ssh_key values contain spaces and may
// contain commas in the comment, so they are never comma-split.
func splitList(name, v string) []string {
	if name == "ssh_key" {
		v = strings.TrimSpace(v)
		if v == "" {
			return nil
		}
		return []string{v}
	}
	var out []string
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func oneOf(vals ...string) func(string) (string, error) {
	return func(v string) (string, error) {
		v = strings.ToLower(strings.TrimSpace(v))
		for _, x := range vals {
			if v == x {
				return v, nil
			}
		}
		return "", fmt.Errorf("must be one of %s", strings.Join(vals, ", "))
	}
}

func ipCheck(v string) (string, error) {
	if net.ParseIP(v) == nil {
		return "", fmt.Errorf("not an IP address")
	}
	return v, nil
}

var defs = []keyDef{
	str("name", "", "Node display name (letters, digits, dashes). Empty = savior-<MAC suffix>.", false,
		func(c *Config) *string { return &c.Name },
		func(v string) (string, error) {
			if v != "" && !nameRE.MatchString(v) {
				return "", fmt.Errorf("use letters, digits and dashes (max 63)")
			}
			return v, nil
		}),
	str("node_id", "", "Override the hardware-derived node ID (lowercase letters, digits and dashes).", false,
		func(c *Config) *string { return &c.NodeID },
		func(v string) (string, error) {
			// The hive only accepts proto.ValidNodeID; lowercase what we can.
			v = strings.ToLower(strings.TrimSpace(v))
			if v != "" && !proto.ValidNodeID(v) {
				return "", fmt.Errorf("use letters, digits and dashes, starting with a letter or digit (max 63)")
			}
			return v, nil
		}),
	list("roles", "auto", "auto, compute, display, hive (comma-separated). auto = compute + display when a screen exists.",
		func(c *Config) *[]string { return &c.Roles }, oneOf("auto", "compute", "display", "hive")),
	{
		KeyInfo: KeyInfo{Name: "labels", Type: "list", Default: "", Help: "key=value labels used by job requirements."},
		get: func(c *Config) []string {
			var out []string
			for k, v := range c.Labels {
				out = append(out, k+"="+v)
			}
			sort.Strings(out)
			return out
		},
		set: func(c *Config, v string) error {
			m := map[string]string{}
			if err := addLabels(m, v); err != nil {
				return err
			}
			c.Labels = m
			return nil
		},
		add: func(c *Config, v string) error {
			if c.Labels == nil {
				c.Labels = map[string]string{}
			}
			return addLabels(c.Labels, v)
		},
		rst: func(c *Config) { c.Labels = map[string]string{} },
	},

	str("hive", "auto", "auto (find on the LAN), host, host:port or https://host:port.", false,
		func(c *Config) *string { return &c.Hive },
		func(v string) (string, error) {
			v = strings.TrimSpace(v)
			if v == "" || v == "auto" {
				return "auto", nil
			}
			if _, err := HiveURL(v); err != nil {
				return "", err
			}
			return v, nil
		}),
	str("swarm_key", "", "Shared join secret (min 16 chars). Generate one with: savior ctl genkey", true,
		func(c *Config) *string { return &c.SwarmKey },
		func(v string) (string, error) {
			if v != "" && len(v) < MinSwarmKeyLen {
				return "", fmt.Errorf("too short (min %d characters)", MinSwarmKeyLen)
			}
			return v, nil
		}),
	str("hive_fingerprint", "", "Optional sha256:<hex> pin of the hive TLS certificate.", false,
		func(c *Config) *string { return &c.HiveFingerprint },
		func(v string) (string, error) {
			if v == "" {
				return v, nil
			}
			n := NormalizeFingerprint(v)
			if !fpRE.MatchString(n) {
				return "", fmt.Errorf("want sha256:<64 hex digits>")
			}
			return n, nil
		}),

	str("join", "key", "key = join with swarm_key; keyless = join without a key and wait for admin approval (netboot).", false,
		func(c *Config) *string { return &c.Join }, oneOf("key", "keyless")),
	str("net", "dhcp", "Wired/wireless addressing: dhcp, static or off.", false,
		func(c *Config) *string { return &c.Net }, oneOf("dhcp", "static", "off")),
	str("ip", "", "Static address in CIDR form, e.g. 10.77.0.1/24 (net=static).", false,
		func(c *Config) *string { return &c.IP },
		func(v string) (string, error) {
			if v == "" {
				return v, nil
			}
			ip, _, err := net.ParseCIDR(v)
			if err != nil || ip.To4() == nil {
				return "", fmt.Errorf("want IPv4 CIDR like 10.77.0.1/24")
			}
			return v, nil
		}),
	str("gateway", "", "Static default gateway.", false,
		func(c *Config) *string { return &c.Gateway },
		func(v string) (string, error) {
			if v == "" {
				return v, nil
			}
			return ipCheck(v)
		}),
	list("dns", "", "Static DNS servers.", func(c *Config) *[]string { return &c.DNS }, ipCheck),
	str("wifi_ssid", "", "Wi-Fi network name to join.", false, func(c *Config) *string { return &c.WifiSSID },
		func(v string) (string, error) {
			if len(v) > 32 {
				return "", fmt.Errorf("SSID longer than 32 bytes")
			}
			return v, nil
		}),
	str("wifi_psk", "", "Wi-Fi passphrase (8-63 chars; empty = open network).", true, func(c *Config) *string { return &c.WifiPSK },
		func(v string) (string, error) {
			if v != "" && (len(v) < 8 || len(v) > 63) {
				return "", fmt.Errorf("WPA passphrase must be 8-63 characters")
			}
			return v, nil
		}),
	str("wifi_country", "US", "Wi-Fi regulatory country code.", false, func(c *Config) *string { return &c.WifiCountry },
		func(v string) (string, error) {
			v = strings.ToUpper(strings.TrimSpace(v))
			if !countryR.MatchString(v) {
				return "", fmt.Errorf("want a two-letter country code")
			}
			return v, nil
		}),
	boolean("dhcp_server", false, "Serve DHCP on the first wired interface (needs net=static).", func(c *Config) *bool { return &c.DHCPServer }),
	str("dhcp_range", "", "first-last IPv4 range for dhcp_server. Empty = .100-.200 of the static subnet.", false,
		func(c *Config) *string { return &c.DHCPRange },
		func(v string) (string, error) {
			if v == "" {
				return v, nil
			}
			a, b, ok := strings.Cut(v, "-")
			if !ok || net.ParseIP(strings.TrimSpace(a)).To4() == nil || net.ParseIP(strings.TrimSpace(b)).To4() == nil {
				return "", fmt.Errorf("want first-last, e.g. 10.77.0.100-10.77.0.200")
			}
			return strings.TrimSpace(a) + "-" + strings.TrimSpace(b), nil
		}),
	boolean("netboot", false, "Serve PXE network boot to other machines (hive role).", func(c *Config) *bool { return &c.Netboot }),
	list("ntp", "pool.ntp.org", "NTP servers, or off.", func(c *Config) *[]string { return &c.NTP }, nil),

	list("ssh_key", "", "Authorized SSH public key for root (repeat the line for more keys). No keys = no SSH server.",
		func(c *Config) *[]string { return &c.SSHKeys },
		func(v string) (string, error) {
			f := strings.Fields(v)
			if len(f) < 2 || !(strings.HasPrefix(f[0], "ssh-") || strings.HasPrefix(f[0], "ecdsa-") || strings.HasPrefix(f[0], "sk-")) {
				return "", fmt.Errorf("does not look like an SSH public key")
			}
			return v, nil
		}),
	boolean("console_shell", false, "Root shell on tty2 without a password.", func(c *Config) *bool { return &c.ConsoleShell }),

	integer("max_cpu_percent", 100, 1, 100, "Percent of logical CPUs offered to the swarm.", func(c *Config) *int { return &c.MaxCPUPercent }),
	integer("max_mem_percent", 75, 10, 95, "Percent of RAM offered to tasks.", func(c *Config) *int { return &c.MaxMemPercent }),
	str("sandbox", "auto", "Task isolation: auto, strict or none.", false, func(c *Config) *string { return &c.Sandbox }, oneOf("auto", "strict", "none")),
	str("scratch", "ram", "Task scratch space: ram, a block device (/dev/sda2) or LABEL=name.", false,
		func(c *Config) *string { return &c.Scratch },
		func(v string) (string, error) {
			v = strings.TrimSpace(v)
			if v == "ram" || strings.HasPrefix(v, "/dev/") || (strings.HasPrefix(v, "LABEL=") && len(v) > 6) {
				return v, nil
			}
			return "", fmt.Errorf("want ram, /dev/<device> or LABEL=<label>")
		}),
	boolean("scratch_wipe", false, "Allow formatting the scratch device if it has no usable filesystem. DESTRUCTIVE.", func(c *Config) *bool { return &c.ScratchWipe }),

	boolean("run_on_battery", false, "Laptops: keep taking tasks on battery power.", func(c *Config) *bool { return &c.RunOnBattery }),
	integer("battery_min_percent", 40, 0, 100, "On battery below this, running tasks are handed back to the hive.", func(c *Config) *int { return &c.BatteryMinPercent }),
	integer("max_temp_c", 85, 40, 110, "Pause tasks above this CPU temperature (resume 10 C lower).", func(c *Config) *int { return &c.MaxTempC }),
	str("cpu_governor", "auto", "cpufreq governor: auto (schedutil, else ondemand), ondemand, schedutil, performance, powersave, conservative.", false,
		func(c *Config) *string { return &c.CPUGovernor },
		oneOf("auto", "ondemand", "schedutil", "performance", "powersave", "conservative")),

	str("display_mode", "status", "Local screen mode until the hive sends one: status, off, text, clock, test.", false,
		func(c *Config) *string { return &c.DisplayMode }, oneOf("status", "off", "text", "clock", "test")),
	str("display_text", "", "Text shown when display_mode=text.", false, func(c *Config) *string { return &c.DisplayText }, nil),
	integer("display_rotate", 0, 0, 0, "Rotate the screen: 0, 90, 180 or 270.", func(c *Config) *int { return &c.DisplayRotate }, 0, 90, 180, 270),
	str("display_device", "auto", "Framebuffer device: auto (first connected display) or /dev/fbN.", false, func(c *Config) *string { return &c.DisplayDevice },
		func(v string) (string, error) {
			v = strings.TrimSpace(v)
			if v != "auto" && !strings.HasPrefix(v, "/dev/") {
				return "", fmt.Errorf("must be auto or a /dev path")
			}
			return v, nil
		}),
	integer("display_idle_off_min", 15, 0, 1440, "Blank the screen after N idle minutes in the local status/clock modes (0 = never).", func(c *Config) *int { return &c.DisplayIdleOff }),

	str("hive_listen", ":7700", "Hive HTTPS listen address.", false, func(c *Config) *string { return &c.HiveListen },
		func(v string) (string, error) {
			_, port, err := net.SplitHostPort(strings.TrimSpace(v))
			if err != nil {
				return "", fmt.Errorf("want host:port or :port")
			}
			if p, err := strconv.Atoi(port); err != nil || p < 1 || p > 65535 {
				return "", fmt.Errorf("port must be a number from 1 to 65535")
			}
			return strings.TrimSpace(v), nil
		}),
	str("hive_data", "auto", "Hive state directory (auto = on the stick when writable).", false, func(c *Config) *string { return &c.HiveData },
		func(v string) (string, error) {
			if v == "" {
				return "auto", nil
			}
			return v, nil
		}),
	str("admin_token", "", "Admin API token (min 32 chars). Empty = generated on first start (stored in hive_data/admin_token).", true,
		func(c *Config) *string { return &c.AdminToken },
		func(v string) (string, error) {
			if v != "" && len(v) < MinAdminTokenLen {
				return "", fmt.Errorf("too short (min %d characters)", MinAdminTokenLen)
			}
			return v, nil
		}),
	str("join_policy", "open", "open = nodes with the swarm key join directly; approve = new nodes wait for admin approval.", false,
		func(c *Config) *string { return &c.JoinPolicy }, oneOf("open", "approve")),
	boolean("beacon", true, "Hive announces itself on the LAN.", func(c *Config) *bool { return &c.Beacon }),

	str("log_level", "info", "debug, info, warn or error.", false, func(c *Config) *string { return &c.LogLevel }, oneOf("debug", "info", "warn", "error")),
	str("timezone", "UTC", "IANA time zone for clocks and logs, e.g. Europe/Berlin.", false, func(c *Config) *string { return &c.Timezone },
		func(v string) (string, error) {
			v = strings.TrimSpace(v)
			if _, err := time.LoadLocation(v); err != nil {
				return "", fmt.Errorf("unknown time zone")
			}
			return v, nil
		}),
}

var defIndex = func() map[string]*keyDef {
	m := map[string]*keyDef{}
	for i := range defs {
		m[defs[i].Name] = &defs[i]
	}
	return m
}()

func addLabels(m map[string]string, v string) error {
	for _, part := range splitList("labels", v) {
		k, val, ok := strings.Cut(part, "=")
		k, val = strings.TrimSpace(k), strings.TrimSpace(val)
		if !ok || !labelRE.MatchString(k) || len(val) > 128 {
			return fmt.Errorf("label %q: want key=value", part)
		}
		m[k] = val
	}
	return nil
}

// Keys returns documentation for every key, in file order.
func Keys() []KeyInfo {
	out := make([]KeyInfo, len(defs))
	for i, d := range defs {
		out[i] = d.KeyInfo
	}
	return out
}

// Default returns a Config with every key at its default.
func Default() Config {
	var c Config
	c.Labels = map[string]string{}
	for i := range defs {
		d := &defs[i]
		if d.Type == "list" {
			d.rst(&c)
			if d.Default != "" {
				_ = d.add(&c, d.Default)
			}
			continue
		}
		if err := d.set(&c, d.Default); err != nil {
			panic("config: bad default for " + d.Name + ": " + err.Error())
		}
	}
	return c
}

// Set parses value into key, replacing the current value (lists included).
func (c *Config) Set(key, value string) error {
	d, ok := defIndex[key]
	if !ok {
		return fmt.Errorf("unknown key %q", key)
	}
	if err := d.set(c, value); err != nil {
		return fmt.Errorf("%s: %w", key, err)
	}
	return nil
}

// Get returns the canonical string form of key. Lists are joined with ",",
// except ssh_key which is joined with newlines.
func (c Config) Get(key string) (string, bool) {
	vals, ok := c.Values(key)
	if !ok {
		return "", false
	}
	sep := ","
	if key == "ssh_key" {
		sep = "\n"
	}
	return strings.Join(vals, sep), true
}

// Values returns the canonical values of key (one element unless a list).
func (c Config) Values(key string) ([]string, bool) {
	d, ok := defIndex[key]
	if !ok {
		return nil, false
	}
	return d.get(&c), true
}

// source applies one config source; list keys set in this source replace the
// value from earlier sources, repeated lines within the source append.
type source struct {
	c        *Config
	seenList map[string]bool
	warn     func(string)
}

func (s *source) apply(key, value, where string) {
	d, ok := defIndex[key]
	if !ok {
		s.warn(fmt.Sprintf("%s: unknown key %q (ignored)", where, key))
		return
	}
	var err error
	if d.Type == "list" {
		if !s.seenList[key] {
			s.seenList[key] = true
			backup := d.get(s.c)
			d.rst(s.c)
			if err = d.add(s.c, value); err != nil {
				d.rst(s.c)
				for _, b := range backup {
					_ = d.add(s.c, b)
				}
				s.seenList[key] = false
			}
		} else {
			err = d.add(s.c, value)
		}
	} else {
		err = d.set(s.c, value)
	}
	if err != nil {
		s.warn(fmt.Sprintf("%s: %s: %v (keeping %q)", where, key, err, redact(d, strings.Join(d.get(s.c), ","))))
	}
}

func redact(d *keyDef, v string) string {
	if d.Secret && v != "" {
		return "<secret>"
	}
	return v
}

// ParseFile applies a savior.conf stream onto c and returns warnings.
func ParseFile(r io.Reader, name string, c *Config) []string {
	var warnings []string
	s := &source{c: c, seenList: map[string]bool{}, warn: func(w string) { warnings = append(warnings, w) }}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := sc.Text()
		if lineNo == 1 {
			line = strings.TrimPrefix(line, "\ufeff")
		}
		line = strings.TrimRight(line, "\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, ";") {
			continue
		}
		where := fmt.Sprintf("%s:%d", name, lineNo)
		k, v, ok := strings.Cut(trimmed, "=")
		if !ok {
			warnings = append(warnings, where+": expected key = value")
			continue
		}
		k = strings.ToLower(strings.TrimSpace(k))
		v = unquote(strings.TrimSpace(v))
		s.apply(k, v, where)
	}
	if err := sc.Err(); err != nil {
		warnings = append(warnings, fmt.Sprintf("%s: read error: %v", name, err))
	}
	return warnings
}

// bootOnlyKeys are savior.* cmdline parameters used by the boot scripts
// (e.g. savior.media for S08config), not configuration keys.
var bootOnlyKeys = map[string]bool{"media": true}

// ParseCmdline applies savior.<key>=<value> tokens from a kernel command line.
func ParseCmdline(cmdline string, c *Config) []string {
	var warnings []string
	s := &source{c: c, seenList: map[string]bool{}, warn: func(w string) { warnings = append(warnings, w) }}
	for _, tok := range splitCmdline(cmdline) {
		if !strings.HasPrefix(tok, CmdlinePrefix) {
			continue
		}
		kv := strings.TrimPrefix(tok, CmdlinePrefix)
		k, v, hasVal := strings.Cut(kv, "=")
		k = strings.ToLower(k)
		if bootOnlyKeys[k] {
			continue
		}
		d, known := defIndex[k]
		if !hasVal {
			if known && d.Type == "bool" {
				v = "yes"
			} else {
				warnings = append(warnings, fmt.Sprintf("cmdline: %s needs a value", tok))
				continue
			}
		}
		if dv, err := url.PathUnescape(v); err == nil {
			v = dv
		} else {
			warnings = append(warnings, fmt.Sprintf("cmdline: %s: bad %%-escape (used literally)", k))
		}
		s.apply(k, v, "cmdline")
	}
	return warnings
}

// splitCmdline splits on whitespace, honoring double quotes the way the
// kernel does ("a b" groups; quotes are removed).
func splitCmdline(s string) []string {
	var out []string
	var cur strings.Builder
	inQ, have := false, false
	for _, r := range s {
		switch {
		case r == '"':
			inQ = !inQ
			have = true
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

func unquote(v string) string {
	if len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"' {
		inner := v[1 : len(v)-1]
		var b strings.Builder
		for i := 0; i < len(inner); i++ {
			if inner[i] == '\\' && i+1 < len(inner) && (inner[i+1] == '"' || inner[i+1] == '\\') {
				i++
			}
			b.WriteByte(inner[i])
		}
		return b.String()
	}
	return v
}

func quote(v string) string {
	if v == "" {
		return ""
	}
	if strings.TrimSpace(v) != v || strings.HasPrefix(v, "\"") || strings.HasSuffix(v, "\"") {
		r := strings.NewReplacer(`\`, `\\`, `"`, `\"`)
		return `"` + r.Replace(v) + `"`
	}
	return v
}

// Load builds a Config from defaults, the given files and a kernel command
// line. Missing files are skipped. A file that exists but can't be read
// (permissions, I/O error, a directory) is skipped with a warning, like
// any other bad input, so one broken source never stops a service: the
// node still starts and shows what is missing. Warnings describe ignored
// input. The error is always nil; it is kept for API compatibility.
func Load(files []string, cmdline string) (Config, []string, error) {
	c := Default()
	var warnings []string
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			if !os.IsNotExist(err) {
				warnings = append(warnings, fmt.Sprintf("%s: cannot read (%v); ignoring this file", f, err))
			}
			continue
		}
		warnings = append(warnings, ParseFile(bytes.NewReader(data), f, &c)...)
	}
	warnings = append(warnings, ParseCmdline(cmdline, &c)...)
	return c, warnings, nil
}

// WriteFile writes c as a canonical savior.conf that Load reads back to an
// identical Config.
func (c Config) WriteFile(w io.Writer) error {
	bw := bufio.NewWriter(w)
	fmt.Fprintf(bw, "# SaviorOS configuration (generated by savior config dump)\n")
	for i := range defs {
		d := &defs[i]
		vals := d.get(&c)
		if d.Type == "list" {
			if len(vals) == 0 {
				fmt.Fprintf(bw, "%s =\n", d.Name)
			}
			for _, v := range vals {
				fmt.Fprintf(bw, "%s = %s\n", d.Name, quote(v))
			}
			continue
		}
		fmt.Fprintf(bw, "%s = %s\n", d.Name, quote(vals[0]))
	}
	return bw.Flush()
}

// ShellEnv renders c as POSIX shell assignments: SAVIOR_<KEY>='value'.
// List values are joined with newlines. Derived convenience variables:
// SAVIOR_ROLE_HIVE, SAVIOR_ROLE_COMPUTE, SAVIOR_ROLE_DISPLAY_EXPLICIT (yes/no).
func (c Config) ShellEnv() string {
	var b strings.Builder
	for i := range defs {
		d := &defs[i]
		fmt.Fprintf(&b, "SAVIOR_%s=%s\n", strings.ToUpper(d.Name), shellQuote(strings.Join(d.get(&c), "\n")))
	}
	has := func(r string) string {
		for _, x := range c.Roles {
			if x == r || (x == "auto" && r == "compute") {
				return "yes"
			}
		}
		return "no"
	}
	fmt.Fprintf(&b, "SAVIOR_ROLE_HIVE=%s\n", has("hive"))
	fmt.Fprintf(&b, "SAVIOR_ROLE_COMPUTE=%s\n", has("compute"))
	fmt.Fprintf(&b, "SAVIOR_ROLE_DISPLAY_EXPLICIT=%s\n", has("display"))
	return b.String()
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// EffectiveRoles resolves "auto": compute, plus display when a display
// device (DRM card or framebuffer) exists. The result is deduplicated and
// ordered compute, display, hive.
func (c Config) EffectiveRoles(hasDisplay bool) []proto.Role {
	set := map[proto.Role]bool{}
	roles := c.Roles
	if len(roles) == 0 {
		roles = []string{"auto"}
	}
	for _, r := range roles {
		switch r {
		case "auto":
			set[proto.RoleCompute] = true
			if hasDisplay {
				set[proto.RoleDisplay] = true
			}
		default:
			set[proto.Role(r)] = true
		}
	}
	var out []proto.Role
	for _, r := range []proto.Role{proto.RoleCompute, proto.RoleDisplay, proto.RoleHive} {
		if set[r] {
			out = append(out, r)
		}
	}
	return out
}

// HiveURL turns a hive= value (host, host:port, https://host:port) into a
// base URL like https://host:7700. It returns an error for "auto".
func HiveURL(v string) (string, error) {
	v = strings.TrimSpace(v)
	if v == "" || v == "auto" {
		return "", fmt.Errorf("hive address is auto")
	}
	if strings.Contains(v, "://") {
		u, err := url.Parse(v)
		if err != nil || u.Scheme != "https" || u.Host == "" || (u.Path != "" && u.Path != "/") {
			return "", fmt.Errorf("want https://host[:port]")
		}
		v = u.Host
	}
	host, port, err := net.SplitHostPort(v)
	if err != nil {
		host, port = strings.Trim(v, "[]"), strconv.Itoa(proto.DefaultPort)
	}
	if host == "" || strings.ContainsAny(host, "/ ") {
		return "", fmt.Errorf("invalid host")
	}
	if p, err := strconv.Atoi(port); err != nil || p < 1 || p > 65535 {
		return "", fmt.Errorf("invalid port")
	}
	return "https://" + net.JoinHostPort(host, port), nil
}

// NormalizeFingerprint lowercases a fingerprint, removes colons, and adds the
// sha256: prefix if it's missing.
func NormalizeFingerprint(v string) string {
	v = strings.ToLower(strings.TrimSpace(v))
	v = strings.TrimPrefix(v, "sha256:")
	v = strings.ReplaceAll(v, ":", "")
	return "sha256:" + v
}

// ParseBool accepts yes/no/true/false/1/0/on/off (case-insensitive).
func ParseBool(v string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "yes", "y", "true", "1", "on":
		return true, nil
	case "no", "n", "false", "0", "off", "":
		return false, nil
	}
	return false, fmt.Errorf("want yes or no")
}

func fmtBool(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}
