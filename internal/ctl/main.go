package ctl

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"slices"
	"sort"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"
	"unicode/utf8"

	"github.com/platteration/ewastesavior/internal/discovery"
)

// Main implements `savior ctl`.
func Main(args []string) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	a := &app{
		stdin:      os.Stdin,
		stdout:     os.Stdout,
		stderr:     os.Stderr,
		getenv:     os.Getenv,
		configPath: defaultConfigPath,
		stdinTTY:   isTerminal(os.Stdin),
		stdoutTTY:  isTerminal(os.Stdout),
		stderrTTY:  isTerminal(os.Stderr),
		readSecret: func(ctx context.Context) (string, error) { return readSecretLine(ctx, os.Stdin) },
		discover: func(ctx context.Context, window time.Duration) ([]discovery.Candidate, error) {
			return discovery.Collect(ctx, window, discovery.DiscoverOptions{ProbeInterval: time.Second})
		},
		now:          time.Now,
		pollInterval: 2 * time.Second,
	}
	return a.run(ctx, args)
}

// app is one invocation of `savior ctl`, with its environment injected so
// tests can drive it.
type app struct {
	stdin                          io.Reader
	stdout, stderr                 io.Writer
	getenv                         func(string) string
	configPath                     func() (string, error)
	stdinTTY, stdoutTTY, stderrTTY bool
	readSecret                     func(ctx context.Context) (string, error) // hidden terminal input
	discover                       func(ctx context.Context, window time.Duration) ([]discovery.Candidate, error)
	now                            func() time.Time
	pollInterval                   time.Duration

	g       globalFlags
	cmdName string

	// Loaded lazily by connect.
	cfgPath string
	file    fileConfig
	set     settings
	loaded  bool
	client  *Client
}

// globalFlags may be given before or after the command name.
type globalFlags struct {
	hive, fingerprint, token string
	config                   string
	json                     bool
	tofu                     bool
	timeout                  time.Duration
}

var globalNames = map[string]bool{"hive": true, "fingerprint": true, "token": true, "json": true,
	"insecure-tofu": true, "timeout": true, "config": true}

// register adds the global flags to fs, except those named in skip (a
// command may use the name for its own flag, like run's --timeout).
func (g *globalFlags) register(fs *flag.FlagSet, skip ...string) {
	add := func(name string, fn func()) {
		if !slices.Contains(skip, name) {
			fn()
		}
	}
	add("hive", func() {
		fs.StringVar(&g.hive, "hive", g.hive, "hive address: host, host:port or https://host:port (env "+EnvHive+")")
	})
	add("fingerprint", func() {
		fs.StringVar(&g.fingerprint, "fingerprint", g.fingerprint, "expected hive certificate fingerprint sha256:... (env "+EnvFingerprint+")")
	})
	add("token", func() {
		fs.StringVar(&g.token, "token", g.token, "admin token, or - to read it from stdin (env "+EnvAdminToken+"); never sent to the hive")
	})
	add("json", func() { fs.BoolVar(&g.json, "json", g.json, "print JSON") })
	add("insecure-tofu", func() {
		fs.BoolVar(&g.tofu, "insecure-tofu", g.tofu, "on first contact, trust the hive's certificate and pin it (verified by the login proof)")
	})
	add("timeout", func() {
		fs.DurationVar(&g.timeout, "timeout", g.timeout, "timeout for each request (transfers: longest stall)")
	})
	add("config", func() {
		fs.StringVar(&g.config, "config", g.config, "ctl.json path (env "+EnvConfig+"; default in the user config directory)")
	})
}

// command is one `savior ctl` subcommand.
type command struct {
	name  string
	args  string // synopsis after the name
	help  string // one line
	run   func(a *app, ctx context.Context, args []string) int
	group string
}

var commands []command

func init() {
	commands = []command{
		{name: "login", args: "[--hive ADDR] [--token T]", help: "log in to a hive (proves the admin token without sending it)", run: cmdLogin, group: "session"},
		{name: "logout", args: "[--all]", help: "end this session (--all: every admin session)", run: cmdLogout, group: "session"},
		{name: "pair", help: "print a pairing code for the web dashboard", run: cmdPair, group: "session"},
		{name: "info", help: "show the hive's details", run: cmdInfo, group: "session"},
		{name: "time", help: "compare this computer's clock with the hive's", run: cmdTime, group: "session"},
		{name: "find", args: "[--seconds N]", help: "list hives announcing themselves on the LAN (no key needed)", run: cmdFind, group: "setup"},
		{name: "genkey", help: "print a new random swarm key", run: cmdGenkey, group: "setup"},
		{name: "node-config", args: "[--hive-addr ADDR] [-o savior.conf] [--force]", help: "write a savior.conf for nodes (swarm key + fingerprint pin)", run: cmdNodeConfig, group: "setup"},
		{name: "nodes", args: "[--all]", help: "list nodes", run: cmdNodes, group: "nodes"},
		{name: "node", args: "<ref>", help: "show one node (ref = ID or name)", run: cmdNode, group: "nodes"},
		{name: "rename", args: "<ref> <name>", help: "rename a node", run: cmdRename, group: "nodes"},
		{name: "label", args: "<ref> k=v|k-... [--clear]", help: "set (k=v) or remove (k-) admin labels", run: cmdLabel, group: "nodes"},
		{name: "drain", args: "<ref>", help: "stop giving a node new tasks", run: nodePatchCmd("drain"), group: "nodes"},
		{name: "undrain", args: "<ref>", help: "let a drained node take tasks again", run: nodePatchCmd("undrain"), group: "nodes"},
		{name: "approve", args: "<ref>", help: "approve a pending node", run: nodePatchCmd("approve"), group: "nodes"},
		{name: "unquarantine", args: "<ref>", help: "clear a node's quarantine", run: nodePatchCmd("unquarantine"), group: "nodes"},
		{name: "rotate", args: "<ref> <0|90|180|270>", help: "rotate a node's screen", run: cmdRotate, group: "nodes"},
		{name: "identify", args: "[<ref>...|--all] [--seconds N]", help: "flash nodes' screens and beep", run: cmdIdentify, group: "nodes"},
		{name: "reboot", args: "<ref>", help: "reboot a node", run: nodeActionCmd("reboot"), group: "nodes"},
		{name: "poweroff", args: "<ref>", help: "power a node off", run: nodeActionCmd("poweroff"), group: "nodes"},
		{name: "forget", args: "<ref>", help: "remove an offline node", run: cmdForget, group: "nodes"},
		{name: "display", args: "<ref> <mode> [flags]", help: "set what a node's screen shows", run: cmdDisplay, group: "displays"},
		{name: "wall", args: "create|ls|show|rm|test|content ...", help: "manage video walls", run: cmdWall, group: "displays"},
		{name: "run", args: "[flags] -- command args...", help: "run a command on the swarm", run: cmdRun, group: "jobs"},
		{name: "script", args: "FILE [flags]", help: "run a shell script on the swarm", run: cmdScript, group: "jobs"},
		{name: "submit", args: "JOB.json [--wait] [--fetch DIR]", help: "submit a job spec (JSON)", run: cmdSubmit, group: "jobs"},
		{name: "jobs", args: "[--state S] [--limit N]", help: "list jobs", run: cmdJobs, group: "jobs"},
		{name: "job", args: "<id>", help: "show a job", run: cmdJob, group: "jobs"},
		{name: "tasks", args: "<job> [--state S]", help: "list a job's tasks", run: cmdTasks, group: "jobs"},
		{name: "task", args: "<task>", help: "show a task", run: cmdTask, group: "jobs"},
		{name: "cancel", args: "<job>", help: "cancel a job", run: cmdCancel, group: "jobs"},
		{name: "rm", args: "<job>", help: "delete a finished job", run: cmdRm, group: "jobs"},
		{name: "wait", args: "<job> [--fetch DIR]", help: "wait for a job to finish", run: cmdWait, group: "jobs"},
		{name: "logs", args: "[-f] <task>", help: "print a task's output (-f: follow)", run: cmdLogs, group: "jobs"},
		{name: "outputs", args: "<job> [-o DIR] [--zip]", help: "list or download a job's output files", run: cmdOutputs, group: "jobs"},
		{name: "fetch", args: "<blob> -o FILE", help: "download a blob", run: cmdFetch, group: "blobs"},
		{name: "upload", args: "FILE", help: "upload a file as a blob", run: cmdUpload, group: "blobs"},
		{name: "blobs", args: "[rm <sha256>]", help: "list (or delete) blobs", run: cmdBlobs, group: "blobs"},
		{name: "gc", help: "delete unreferenced blobs", run: cmdGC, group: "blobs"},
		{name: "stats", help: "show swarm totals", run: cmdStats, group: "blobs"},
	}
}

func (a *app) usage(w io.Writer) {
	fmt.Fprint(w, `usage: savior ctl [global flags] <command> [arguments]

Control a SaviorOS hive: nodes, displays, video walls and jobs.
Start with 'savior ctl find' and 'savior ctl login --hive ADDRESS'.
`)
	groups := []struct{ id, title string }{
		{"session", "session"}, {"setup", "setup"}, {"nodes", "nodes"}, {"displays", "displays"},
		{"jobs", "jobs"}, {"blobs", "blobs and stats"},
	}
	for _, g := range groups {
		fmt.Fprintf(w, "\n%s:\n", g.title)
		tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
		for _, c := range commands {
			if c.group == g.id {
				fmt.Fprintf(tw, "  %s %s\t%s\n", c.name, c.args, c.help)
			}
		}
		tw.Flush()
	}
	fmt.Fprint(w, `
global flags (before or after the command):
  --hive ADDR          hive address (env SAVIOR_HIVE; default: the one you logged in to)
  --fingerprint FP     expected certificate fingerprint (env SAVIOR_FINGERPRINT)
  --token T            admin token, or - to read it from stdin (env SAVIOR_ADMIN_TOKEN)
  --json               print JSON
  --insecure-tofu      trust the certificate on first contact (default true; the login
                       proof then verifies the hive); --insecure-tofu=false requires a pin
  --timeout D          per-request timeout (default 30s)
  --config FILE        ctl.json path (env SAVIOR_CTL_CONFIG)

Run 'savior ctl <command> -h' for a command's flags.
`)
}

// run executes one invocation and returns the exit code.
func (a *app) run(ctx context.Context, args []string) int {
	a.g.tofu = true
	a.g.timeout = defaultTimeout
	fs := flag.NewFlagSet("savior ctl", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	a.g.register(fs)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			a.usage(a.stdout)
			return 0
		}
		fmt.Fprintf(a.stderr, "savior ctl: %v\n\n", err)
		a.usage(a.stderr)
		return 2
	}
	if fs.NArg() == 0 {
		a.usage(a.stderr)
		return 2
	}
	name, rest := fs.Arg(0), fs.Args()[1:]
	if name == "help" {
		if len(rest) > 0 {
			return a.run(ctx, []string{rest[0], "-h"})
		}
		a.usage(a.stdout)
		return 0
	}
	for _, c := range commands {
		if c.name == name {
			a.cmdName = name
			code := c.run(a, ctx, rest)
			if a.client != nil {
				a.client.Close()
			}
			return code
		}
	}
	fmt.Fprintf(a.stderr, "savior ctl: unknown command %q\n\n", sanitizeCell(name))
	a.usage(a.stderr)
	return 2
}

// flags is a subcommand's FlagSet. It also accepts the global flags, but
// its usage lists only the command's own.
type flags struct {
	*flag.FlagSet
	cmd, synopsis, help string
	skip                []string // global flags this command redefines
}

func (a *app) flagSet(name, synopsis, help string, skipGlobals ...string) *flags {
	fs := flag.NewFlagSet("savior ctl "+name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {} // printed by parse
	a.g.register(fs, skipGlobals...)
	return &flags{FlagSet: fs, cmd: name, synopsis: synopsis, help: help, skip: skipGlobals}
}

func (f *flags) printUsage(w io.Writer) {
	fmt.Fprintf(w, "usage: savior ctl %s %s\n\n%s\n", f.cmd, f.synopsis, f.help)
	var own []*flag.Flag
	f.VisitAll(func(fl *flag.Flag) {
		if !globalNames[fl.Name] || slices.Contains(f.skip, fl.Name) {
			own = append(own, fl)
		}
	})
	if len(own) > 0 {
		fmt.Fprintln(w, "\nflags:")
		tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
		for _, fl := range own {
			typ, usage := flag.UnquoteUsage(fl)
			def := ""
			if fl.DefValue != "" && fl.DefValue != "false" && fl.DefValue != "0" && fl.DefValue != "[]" {
				def = " (default " + fl.DefValue + ")"
			}
			fmt.Fprintf(tw, "  --%s %s\t%s%s\n", fl.Name, typ, usage, def)
		}
		tw.Flush()
	}
	fmt.Fprintln(w, "\nGlobal flags (--hive, --json, ...) are listed by 'savior ctl -h'.")
}

// parse parses flags interleaved with positional arguments; everything
// after a literal "--" is positional. ok=false means return code.
func (a *app) parse(f *flags, args []string, minPos, maxPos int) (pos []string, code int, ok bool) {
	var tail []string
	for i, s := range args {
		if s == "--" {
			tail = append(tail, args[i+1:]...)
			args = args[:i]
			break
		}
	}
	for {
		if err := f.Parse(args); err != nil {
			return nil, a.flagError(f, err), false
		}
		rest := f.Args()
		if len(rest) == 0 {
			break
		}
		pos = append(pos, rest[0])
		args = rest[1:]
	}
	pos = append(pos, tail...)
	if len(pos) < minPos || (maxPos >= 0 && len(pos) > maxPos) {
		fmt.Fprintf(a.stderr, "savior ctl %s: wrong number of arguments\n\n", f.cmd)
		f.printUsage(a.stderr)
		return nil, 2, false
	}
	return pos, 0, true
}

// parseCommand parses flags up to "--" (or up to the first non-flag) and
// returns the rest untouched: the command line of a job.
func (a *app) parseCommand(f *flags, args []string) (cmd []string, code int, ok bool) {
	for i, s := range args {
		if s == "--" {
			if err := f.Parse(args[:i]); err != nil {
				return nil, a.flagError(f, err), false
			}
			if f.NArg() > 0 {
				return nil, a.usageError("unexpected %q before --", sanitizeCell(f.Arg(0))), false
			}
			return args[i+1:], 0, true
		}
	}
	if err := f.Parse(args); err != nil {
		return nil, a.flagError(f, err), false
	}
	return f.Args(), 0, true
}

func (a *app) flagError(f *flags, err error) int {
	if errors.Is(err, flag.ErrHelp) {
		f.printUsage(a.stdout)
		return 0
	}
	fmt.Fprintf(a.stderr, "savior ctl %s: %v\n\n", f.cmd, err)
	f.printUsage(a.stderr)
	return 2
}

// usageError reports a bad argument and returns exit code 2.
func (a *app) usageError(format string, args ...any) int {
	fmt.Fprintf(a.stderr, "savior ctl %s: %s\n", a.cmdName, fmt.Sprintf(format, args...))
	return 2
}

// fail prints err with a hint for the common cases and returns exit code 1
// (130 when interrupted).
func (a *app) fail(err error) int {
	if errors.Is(err, context.Canceled) {
		fmt.Fprintln(a.stderr, "savior ctl: interrupted")
		return 130
	}
	msg := sanitize(err.Error())
	fmt.Fprintf(a.stderr, "savior ctl %s: %s\n", a.cmdName, msg)
	var fe *FingerprintError
	var ae *APIError
	switch {
	case errors.As(err, &fe):
		fmt.Fprintf(a.stderr, `
The hive's certificate does not match the pinned fingerprint. Either another
machine is posing as the hive, or the hive's certificate was replaced.
Nothing was sent to it. Compare with the fingerprint on the hive's screen;
if it shows %s, run:
  savior ctl login --hive %s --fingerprint %s
`, fe.Seen, fe.Hive, fe.Seen)
	case errors.Is(err, ErrHiveProof):
		fmt.Fprintln(a.stderr, "Nothing was saved. Check that --hive points at your hive and compare its fingerprint with the hive's screen.")
	case errors.As(err, &ae) && ae.Status == 429:
		fmt.Fprintln(a.stderr, "Too many failed attempts from this computer; wait a minute and try again.")
	case errors.Is(err, context.DeadlineExceeded):
		fmt.Fprintf(a.stderr, "The hive did not answer within %s; check the address or raise --timeout.\n", a.g.timeout)
	}
	return 1
}

// warn prints a warning to stderr.
func (a *app) warn(format string, args ...any) {
	fmt.Fprintf(a.stderr, "warning: %s\n", fmt.Sprintf(format, args...))
}

// loadSettings reads ctl.json and resolves the effective settings once.
func (a *app) loadSettings() error {
	if a.loaded {
		return nil
	}
	path := a.g.config
	if path == "" {
		path = a.getenv(EnvConfig)
	}
	if path == "" {
		p, err := a.configPath()
		if err != nil {
			return err
		}
		path = p
	}
	fc, warnings, err := loadFileConfig(path)
	if err != nil {
		return err
	}
	for _, w := range warnings {
		a.warn("%s", w)
	}
	token := a.g.token
	if token == "-" {
		t, err := readLine(a.stdin)
		if err != nil {
			return fmt.Errorf("read the admin token from stdin: %w", err)
		}
		token = t
	}
	s, err := resolveSettings(a.g.hive, a.g.fingerprint, strings.TrimSpace(token), func(k string) string {
		return strings.TrimSpace(a.getenv(k))
	}, fc)
	if err != nil {
		return err
	}
	a.cfgPath, a.file, a.set, a.loaded = path, fc, s, true
	return nil
}

// tokenSource supplies the admin token for a login: flag or environment,
// else a hidden prompt when stdin is a terminal. The login command also
// accepts a line from a non-terminal stdin.
func (a *app) tokenSource(ctx context.Context) func() (string, error) {
	return func() (string, error) {
		if a.set.token != "" {
			return a.set.token, nil
		}
		switch {
		case a.stdinTTY && a.readSecret != nil:
			fmt.Fprintf(a.stderr, "Admin token for %s: ", a.set.hive)
			t, err := a.readSecret(ctx)
			fmt.Fprintln(a.stderr)
			if err != nil {
				return "", fmt.Errorf("read the admin token: %w", err)
			}
			return strings.TrimSpace(t), nil
		case a.cmdName == "login":
			fmt.Fprintln(a.stderr, "Reading the admin token from standard input (not a terminal).")
			t, err := readLine(a.stdin)
			if err != nil {
				return "", fmt.Errorf("read the admin token from stdin: %w", err)
			}
			return strings.TrimSpace(t), nil
		}
		return "", ErrNotLoggedIn
	}
}

// connect returns the client for the effective hive.
func (a *app) connect(ctx context.Context) (*Client, error) {
	if a.client != nil {
		return a.client, nil
	}
	if err := a.loadSettings(); err != nil {
		return nil, err
	}
	s := a.set
	if s.hive == "" {
		return nil, errors.New("no hive configured: run 'savior ctl login --hive ADDRESS' (find hives with 'savior ctl find')")
	}
	opts := []Option{
		WithTimeout(a.g.timeout),
		WithTOFU(a.g.tofu),
		WithTokenSource(a.tokenSource(ctx)),
		WithFirstContact(a.firstContact),
		WithLoginCallback(a.saveLogin),
	}
	if s.session != "" {
		opts = append(opts, WithSession(s.session, s.expires))
	}
	c, err := NewClient(s.hive, s.fingerprint, opts...)
	if err != nil {
		return nil, err
	}
	a.client = c
	return c, nil
}

func (a *app) firstContact(fp string) {
	fmt.Fprintf(a.stderr, `WARNING: first contact with %s. Its certificate fingerprint is
  %s
Compare it with the fingerprint shown on the hive's screen. It will be pinned
once the hive proves it knows the admin token, and checked on every later
connection.
`, a.set.hive, fp)
}

// saveLogin persists a verified login. A login to a hive other than the
// remembered one replaces it only for the login command (or when nothing
// is remembered), so a one-off --hive does not clobber ctl.json.
func (a *app) saveLogin(res LoginResult) {
	if !(a.cmdName == "login" || a.file.Hive == "" || a.set.sameAsFile) {
		return
	}
	fc := fileConfig{Hive: res.Hive, Fingerprint: res.Fingerprint, Session: res.Session, SessionExpires: res.Expires}
	if err := saveFileConfig(a.cfgPath, fc); err != nil {
		a.warn("could not save the session: %v", err)
		return
	}
	a.file = fc
	a.set.sameAsFile = true
}

// printJSON writes v as indented JSON. C1 control characters, which JSON
// does not escape, are escaped so the output is safe on a terminal.
func (a *app) printJSON(v any) int {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return a.fail(err)
	}
	if _, err := a.stdout.Write(escapeJSONControls(buf.Bytes())); err != nil {
		return 1
	}
	return 0
}

// escapeJSONControls replaces DEL, C1 controls and bidi overrides inside
// JSON output with \u escapes (JSON structure itself is ASCII, so this
// only touches string contents and keeps the data intact).
func escapeJSONControls(b []byte) []byte {
	need := false
	for _, r := range string(b) {
		if r >= 0x7f && dangerousRune(r) {
			need = true
			break
		}
	}
	if !need {
		return b
	}
	var out bytes.Buffer
	for len(b) > 0 {
		r, n := utf8.DecodeRune(b)
		if r >= 0x7f && dangerousRune(r) {
			fmt.Fprintf(&out, `\u%04x`, r)
		} else {
			out.Write(b[:n])
		}
		b = b[n:]
	}
	return out.Bytes()
}

// table writes aligned columns; every cell is sanitized.
type table struct {
	tw *tabwriter.Writer
}

func newTable(w io.Writer, headers ...string) *table {
	t := &table{tw: tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)}
	t.row(headers...)
	return t
}

func (t *table) row(cells ...string) {
	for i, c := range cells {
		if i > 0 {
			t.tw.Write([]byte{'\t'})
		}
		c = sanitizeCell(c)
		if c == "" {
			c = "-"
		}
		t.tw.Write([]byte(c))
	}
	t.tw.Write([]byte{'\n'})
}

func (t *table) flush() { t.tw.Flush() }

// kv prints aligned "key: value" lines, sanitizing values.
type kv struct {
	tw *tabwriter.Writer
}

func newKV(w io.Writer) *kv { return &kv{tw: tabwriter.NewWriter(w, 0, 4, 1, ' ', 0)} }

func (k *kv) add(key, value string) {
	if value == "" {
		return
	}
	fmt.Fprintf(k.tw, "%s:\t%s\n", key, sanitizeCell(value))
}

func (k *kv) flush() { k.tw.Flush() }

func sortedKV(m map[string]string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + "=" + m[k]
	}
	return strings.Join(parts, " ")
}
