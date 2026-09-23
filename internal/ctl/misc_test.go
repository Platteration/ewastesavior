package ctl

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/platteration/ewastesavior/internal/discovery"
	"github.com/platteration/ewastesavior/internal/proto"
)

func TestGenkey(t *testing.T) {
	re := regexp.MustCompile(`^[0-9A-HJKMNP-TV-Z]{32}$`)
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		k, err := GenerateSwarmKey()
		if err != nil {
			t.Fatal(err)
		}
		if !re.MatchString(k) {
			t.Fatalf("key %q is not 32 Crockford base32 characters", k)
		}
		if seen[k] {
			t.Fatal("duplicate key")
		}
		seen[k] = true
	}
	e := newCtlEnv(t)
	code, stdout, _ := e.run("genkey")
	if code != 0 || !re.MatchString(strings.TrimSpace(stdout)) {
		t.Fatalf("genkey: %d %q", code, stdout)
	}
	if _, err := os.Stat(e.cfgPath); err == nil {
		t.Error("genkey touched ctl.json")
	}
}

func TestSanitize(t *testing.T) {
	cases := map[string]string{
		"plain text":                   "plain text",
		"tab\tand\nnewline":            "tab\tand\nnewline",
		"\x1b[31mred\x1b[0m":           "[31mred[0m",
		"bell\x07 back\bspace\rreturn": "bell backspacereturn",
		"c1 \u009b2J \u0085nel":        "c1 2J nel",
		"del\x7f":                      "del",
		"bidi \u202eevil\u202c \u2066": "bidi evil ",
		"bad utf8 \xff\xfe":            "bad utf8 \ufffd",
		"unicode ok: h\u00e9llo \u4e16\u754c \U0001f600": "unicode ok: h\u00e9llo \u4e16\u754c \U0001f600",
	}
	for in, want := range cases {
		if got := sanitize(in); got != want {
			t.Errorf("sanitize(%q) = %q, want %q", in, got, want)
		}
	}
	if got := sanitizeCell("a\tb\nc\x1bd"); got != "a b cd" {
		t.Errorf("sanitizeCell = %q", got)
	}
	if got := truncate("abcdefgh", 5); got != "ab..." {
		t.Errorf("truncate = %q", got)
	}
}

func TestPrintJSONEscapesControls(t *testing.T) {
	var out strings.Builder
	a := &app{stdout: &out, stderr: &out}
	a.printJSON(map[string]string{"name": "x\u009b2J\u202ey\x1b"})
	s := out.String()
	if strings.ContainsAny(s, "\u009b\u202e\x1b") {
		t.Fatalf("raw control characters in JSON output: %q", s)
	}
	var m map[string]string
	if err := json.Unmarshal([]byte(s), &m); err != nil || m["name"] != "x\u009b2J\u202ey\x1b" {
		t.Fatalf("JSON not lossless: %q (%v)", s, err)
	}
}

func TestConfigPrecedence(t *testing.T) {
	fpFile := "sha256:" + strings.Repeat("11", 32)
	fpEnv := "sha256:" + strings.Repeat("22", 32)
	fpFlag := "sha256:" + strings.Repeat("33", 32)
	session := strings.Repeat("s", 64)
	file := fileConfig{Hive: "https://10.0.0.1:7700", Fingerprint: fpFile, Session: session, SessionExpires: time.Now().Add(time.Hour)}
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

	// File only.
	s, err := resolveSettings("", "", "", env(nil), file)
	if err != nil || s.hive != file.Hive || s.fingerprint != fpFile || s.session != session || s.hiveSource != "file" || !s.sameAsFile {
		t.Fatalf("file only: %+v %v", s, err)
	}
	// Env beats file; a different hive gets neither the stored pin nor the session.
	s, _ = resolveSettings("", "", "", env(map[string]string{EnvHive: "10.0.0.2"}), file)
	if s.hive != "https://10.0.0.2:7700" || s.hiveSource != "env" || s.fingerprint != "" || s.session != "" {
		t.Fatalf("env hive: %+v", s)
	}
	// Flag beats env.
	s, _ = resolveSettings("10.0.0.3:9000", "", "", env(map[string]string{EnvHive: "10.0.0.2"}), file)
	if s.hive != "https://10.0.0.3:9000" || s.hiveSource != "flag" {
		t.Fatalf("flag hive: %+v", s)
	}
	// Same hive spelled differently still matches the file.
	s, _ = resolveSettings("10.0.0.1", "", "", env(nil), file)
	if !s.sameAsFile || s.fingerprint != fpFile || s.session != session {
		t.Fatalf("same hive by flag: %+v", s)
	}
	// Fingerprint: flag > env > file; a changed pin drops the stored session.
	s, _ = resolveSettings("", "", "", env(map[string]string{EnvFingerprint: fpEnv}), file)
	if s.fingerprint != fpEnv || s.fpSource != "env" || s.session != "" {
		t.Fatalf("env fp: %+v", s)
	}
	s, _ = resolveSettings("", strings.ToUpper(strings.ReplaceAll(fpFlag, "sha256:", "")), "", env(map[string]string{EnvFingerprint: fpEnv}), file)
	if s.fingerprint != fpFlag || s.fpSource != "flag" {
		t.Fatalf("flag fp: %+v", s)
	}
	// Token: flag > env, never from the file.
	s, _ = resolveSettings("", "", "flagtok", env(map[string]string{EnvAdminToken: "envtok"}), file)
	if s.token != "flagtok" || s.tokenSource != "flag" {
		t.Fatalf("flag token: %+v", s)
	}
	s, _ = resolveSettings("", "", "", env(map[string]string{EnvAdminToken: "envtok"}), file)
	if s.token != "envtok" {
		t.Fatalf("env token: %+v", s)
	}
	// Invalid values are errors naming their source.
	if _, err := resolveSettings("", "", "", env(map[string]string{EnvHive: "http://x"}), file); err == nil || !strings.Contains(err.Error(), "env") {
		t.Errorf("bad env hive: %v", err)
	}
	if _, err := resolveSettings("", "sha256:12", "", env(nil), file); err == nil {
		t.Error("short fingerprint accepted")
	}
}

func TestConfigPrecedenceEndToEnd(t *testing.T) {
	h := newFakeHive(t)
	e := loggedIn(t, h)
	// A bogus hive in the environment is used over the file (and gets
	// neither the stored pin nor the session)...
	e.env[EnvHive] = "127.0.0.1:1"
	if code, _, stderr := e.run("nodes"); code != 1 || !strings.Contains(stderr, "not logged in") {
		t.Fatalf("session offered to another hive: %d %s", code, stderr)
	}
	e.env[EnvAdminToken] = h.token
	if code, _, stderr := e.run("--timeout", "2s", "nodes"); code != 1 || !strings.Contains(stderr, "127.0.0.1:1") {
		t.Fatalf("env hive not used: %d %s", code, stderr)
	}
	// ...and the flag beats the environment.
	if code, _, stderr := e.run("--hive", h.url, "nodes"); code != 0 {
		t.Fatalf("flag hive not used: %s", stderr)
	}
	// Global flags work after the command name too.
	delete(e.env, EnvHive)
	code, stdout, stderr := e.run("nodes", "--json")
	if code != 0 {
		t.Fatalf("nodes --json: %s", stderr)
	}
	var nodes []proto.NodeView
	if err := json.Unmarshal([]byte(stdout), &nodes); err != nil || len(nodes) != 1 {
		t.Fatalf("json: %v %q", err, stdout)
	}
}

func TestConfigFileHandling(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "savior", "ctl.json")
	fc := fileConfig{Hive: "https://h:7700", Fingerprint: "sha256:" + strings.Repeat("ab", 32), Session: strings.Repeat("c", 64)}
	if err := saveFileConfig(path, fc); err != nil {
		t.Fatal(err)
	}
	got, warns, err := loadFileConfig(path)
	if err != nil || len(warns) != 0 || got.Hive != fc.Hive || got.Session != fc.Session {
		t.Fatalf("round trip: %+v %v %v", got, warns, err)
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(path, 0o644); err != nil {
			t.Fatal(err)
		}
		_, warns, _ = loadFileConfig(path)
		st, _ := os.Stat(path)
		if len(warns) == 0 || st.Mode().Perm() != 0o600 {
			t.Errorf("world-readable ctl.json: warnings %v, mode %v", warns, st.Mode().Perm())
		}
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, warns, err = loadFileConfig(path)
	if err != nil || len(warns) == 0 || got != (fileConfig{}) {
		t.Errorf("corrupt file: %+v %v %v", got, warns, err)
	}
	// A session without a pin is never used.
	if err := os.WriteFile(path, []byte(`{"hive":"h","session":"`+strings.Repeat("d", 64)+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	got, _, _ = loadFileConfig(path)
	if got.Session != "" || got.Hive != "https://h:7700" {
		t.Errorf("session without pin kept: %+v", got)
	}
	if got, _, err := loadFileConfig(filepath.Join(dir, "missing.json")); err != nil || got != (fileConfig{}) {
		t.Errorf("missing file: %+v %v", got, err)
	}
}

func TestInterleavedArgs(t *testing.T) {
	a := &app{stdout: &strings.Builder{}, stderr: &strings.Builder{}}
	f := a.flagSet("x", "", "")
	all := f.Bool("all", false, "")
	n := f.Int("n", 0, "")
	pos, code, ok := a.parse(f, []string{"one", "--all", "two", "-n", "5", "--", "--three", "-x"}, 0, -1)
	if !ok || code != 0 || !*all || *n != 5 || !reflect.DeepEqual(pos, []string{"one", "two", "--three", "-x"}) {
		t.Fatalf("pos %v all %v n %d ok %v", pos, *all, *n, ok)
	}
	f = a.flagSet("y", "", "", "timeout")
	var jf jobFlags
	jf.register(f)
	cmd, _, ok := a.parseCommand(f, []string{"--count", "2", "echo", "--count", "x"})
	if !ok || jf.count != 2 || !reflect.DeepEqual(cmd, []string{"echo", "--count", "x"}) {
		t.Fatalf("command %v count %d", cmd, jf.count)
	}
	f = a.flagSet("z", "", "", "timeout")
	jf = jobFlags{}
	jf.register(f)
	if _, code, ok := a.parseCommand(f, []string{"--count", "2", "stray", "--", "echo"}); ok || code != 2 {
		t.Fatalf("stray argument before -- accepted")
	}
}

func TestHelpAndUsage(t *testing.T) {
	e := newCtlEnv(t)
	if code, stdout, _ := e.run("-h"); code != 0 || !strings.Contains(stdout, "node-config") {
		t.Errorf("-h: %d", code)
	}
	if code, _, stderr := e.run(); code != 2 || !strings.Contains(stderr, "usage") {
		t.Errorf("no args: %d", code)
	}
	if code, _, _ := e.run("frobnicate"); code != 2 {
		t.Errorf("unknown command: %d", code)
	}
	code, stdout, _ := e.run("run", "-h")
	if code != 0 || !strings.Contains(stdout, "--input") || strings.Contains(stdout, "--fingerprint") {
		t.Errorf("run -h: %d\n%s", code, stdout)
	}
	if !strings.Contains(stdout, "task time limit") {
		t.Errorf("run -h lacks its own --timeout:\n%s", stdout)
	}
	if code, _, _ := e.run("help", "nodes"); code != 0 {
		t.Errorf("help nodes: %d", code)
	}
	if code, _, stderr := e.run("node"); code != 2 || !strings.Contains(stderr, "wrong number of arguments") {
		t.Errorf("node without ref: %d %s", code, stderr)
	}
}

func TestNotLoggedInWithoutHive(t *testing.T) {
	e := newCtlEnv(t)
	code, _, stderr := e.run("nodes")
	if code != 1 || !strings.Contains(stderr, "no hive configured") {
		t.Fatalf("%d %s", code, stderr)
	}
}

func TestFindListsHives(t *testing.T) {
	e := newCtlEnv(t)
	fp := "sha256:" + strings.Repeat("ab", 32)
	e.discover = func(ctx context.Context, window time.Duration) ([]discovery.Candidate, error) {
		if window != 2*time.Second {
			t.Errorf("window %v", window)
		}
		return []discovery.Candidate{
			{URL: "https://192.168.1.9:7700", Beacon: proto.Beacon{HiveID: "evil\x1b[2J", Fingerprint: "junk", SwarmHint: "deadbeef", Version: "v1"}},
			{URL: "https://192.168.1.5:7700", Beacon: proto.Beacon{HiveID: "h1", Fingerprint: fp, SwarmHint: "0badf00d", Version: "v0.2"}},
		}, nil
	}
	code, stdout, stderr := e.run("find", "--seconds", "2")
	if code != 0 {
		t.Fatalf("find: %d %s", code, stderr)
	}
	if strings.Contains(stdout, "\x1b") {
		t.Error("beacon escape sequence printed")
	}
	i1, i9 := strings.Index(stdout, "192.168.1.5"), strings.Index(stdout, "192.168.1.9")
	if i1 < 0 || i9 < 0 || i1 > i9 || !strings.Contains(stdout, fp) || !strings.Contains(stdout, "(invalid)") {
		t.Errorf("find output:\n%s", stdout)
	}
	code, stdout, _ = e.run("find", "--seconds", "2", "--json", "--fingerprint", fp)
	var found []map[string]any
	if code != 0 || json.Unmarshal([]byte(stdout), &found) != nil || len(found) != 2 || found[0]["pinned"] != true {
		t.Errorf("find --json: %d %s", code, stdout)
	}
	e.discover = func(context.Context, time.Duration) ([]discovery.Candidate, error) { return nil, nil }
	if code, _, stderr := e.run("find", "--seconds", "1"); code != 1 || !strings.Contains(stderr, "No hive") {
		t.Errorf("empty find: %d %s", code, stderr)
	}
	if code, _, _ := e.run("find", "--seconds", "0"); code != 2 {
		t.Error("--seconds 0 accepted")
	}
}

// The node-config command the savior.conf template on every stick shows must
// work as written: a shell redirect ("> savior.conf") truncates the target
// before ctl runs, and ctl then refuses or writes ./savior.conf instead.
func TestNodeConfigTemplateHint(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "os", "image", "savior.conf.template"))
	if err != nil {
		t.Fatal(err)
	}
	var hints []string
	for _, line := range strings.Split(string(b), "\n") {
		if i := strings.Index(line, "savior ctl node-config"); i >= 0 {
			hints = append(hints, strings.TrimSpace(line[i:]))
		}
	}
	if len(hints) == 0 {
		t.Fatal("the template shows no savior ctl node-config command")
	}
	h := newFakeHive(t)
	e := loggedIn(t, h)
	for _, hint := range hints {
		if strings.ContainsAny(hint, "<>|;&") {
			t.Fatalf("template hint %q uses the shell", hint)
		}
		dir := t.TempDir()
		t.Chdir(dir)
		args := strings.Fields(hint)[2:]
		if code, _, stderr := e.run(args...); code != 0 {
			t.Fatalf("%s: %d %s", hint, code, stderr)
		}
		out := filepath.Join(dir, "savior.conf")
		if b, err := os.ReadFile(out); err != nil || !strings.Contains(string(b), "hive_fingerprint = "+h.fingerprint()) {
			t.Fatalf("%s wrote %q (%v)", hint, b, err)
		}
		if runtime.GOOS != "windows" {
			if st, _ := os.Stat(out); st.Mode().Perm() != 0o600 {
				t.Errorf("%s: mode %v, want 0600", hint, st.Mode().Perm())
			}
		}
	}
}

func TestNodeConfig(t *testing.T) {
	h := newFakeHive(t)
	e := loggedIn(t, h)
	out := filepath.Join(t.TempDir(), "savior.conf")
	code, _, stderr := e.run("node-config", "--hive-addr", "192.168.1.5", "-o", out)
	if code != 0 {
		t.Fatalf("node-config: %s", stderr)
	}
	b, err := os.ReadFile(out)
	if err != nil || !strings.Contains(string(b), "hive_fingerprint = "+h.fingerprint()) || !strings.Contains(string(b), "hive = 192.168.1.5") {
		t.Fatalf("savior.conf = %q (%v)", b, err)
	}
	if runtime.GOOS != "windows" {
		if st, _ := os.Stat(out); st.Mode().Perm() != 0o600 {
			t.Errorf("savior.conf mode %v, want 0600", st.Mode().Perm())
		}
	}
	if code, _, stderr := e.run("node-config", "-o", out); code != 1 || !strings.Contains(stderr, "--force") {
		t.Errorf("overwrite without --force: %d %s", code, stderr)
	}
	if code, _, stderr := e.run("node-config", "-o", out, "--force"); code != 0 {
		t.Errorf("--force: %s", stderr)
	}
	// auto (any case) is passed through: nodes find the hive on the LAN.
	for _, auto := range []string{"auto", "Auto"} {
		autoConf := filepath.Join(t.TempDir(), "auto.conf")
		if code, _, stderr := e.run("node-config", "--hive-addr", auto, "-o", autoConf); code != 0 {
			t.Fatalf("node-config --hive-addr %s: %d %s", auto, code, stderr)
		}
		if b, _ := os.ReadFile(autoConf); !strings.Contains(string(b), "hive = auto\n") {
			t.Errorf("--hive-addr %s: savior.conf = %q", auto, b)
		}
	}
	if code, _, stderr := e.run("node-config", "--hive-addr", "https://x/path", "-o", "-"); code != 2 || !strings.Contains(stderr, "invalid --hive-addr") {
		t.Errorf("bad --hive-addr: %d %s", code, stderr)
	}
	// A config pinning some other certificate is refused.
	h.nodeConf = "swarm_key = ABCDEFGHJKMNPQRSTVWXYZ0123456789\nhive_fingerprint = sha256:" + strings.Repeat("0", 64) + "\n"
	other := filepath.Join(t.TempDir(), "other.conf")
	if code, _, stderr := e.run("node-config", "-o", other); code != 1 || !strings.Contains(stderr, "not the verified fingerprint") {
		t.Errorf("mismatched pin: %d %s", code, stderr)
	}
	if _, err := os.Stat(other); !errors.Is(err, os.ErrNotExist) {
		t.Error("mismatched config written")
	}
}

// ctl passes a node's short code to the hive unchanged (the hive resolves
// it), so "savior ctl identify --all" then "savior ctl rename <code> ..."
// works as the user guide describes.
func TestShortCodeRefsPassThrough(t *testing.T) {
	h := newFakeHive(t)
	e := loggedIn(t, h)
	lastPath := func() string {
		reqs := h.allRequests()
		return reqs[len(reqs)-1].Method + " " + reqs[len(reqs)-1].Path
	}
	if code, stdout, stderr := e.run("rename", "7kq", "lab-shelf-3"); code != 0 || !strings.Contains(stdout, "n0123456789ab is now named lab-shelf-3") {
		t.Fatalf("rename by code: %d %s %s", code, stdout, stderr)
	}
	if got := lastPath(); got != "PATCH /api/v1/admin/nodes/7kq" {
		t.Errorf("rename sent %s", got)
	}
	for _, c := range []struct{ args, want string }{
		{"node 7KQ", "GET /api/v1/admin/nodes/7KQ"},
		{"drain 7kq", "PATCH /api/v1/admin/nodes/7kq"},
		{"reboot 7KQ", "POST /api/v1/admin/nodes/7KQ/action"},
		{"identify 7kq", "POST /api/v1/admin/identify"},
	} {
		if code, _, stderr := e.run(strings.Fields(c.args)...); code != 0 {
			t.Errorf("%s: %d %s", c.args, code, stderr)
		}
		if got := lastPath(); got != c.want {
			t.Errorf("%s sent %s, want %s", c.args, got, c.want)
		}
	}
	h.mu.Lock()
	ids := h.identifies[len(h.identifies)-1].Nodes
	h.mu.Unlock()
	if len(ids) != 1 || ids[0] != "7kq" {
		t.Errorf("identify sent %q", ids)
	}
}

func TestNodeCommands(t *testing.T) {
	h := newFakeHive(t)
	e := loggedIn(t, h)

	if code, stdout, stderr := e.run("label", "lobby-1", "c=3", "a-"); code != 0 || !strings.Contains(stdout, "b=2 c=3") {
		t.Fatalf("label: %d %s %s", code, stdout, stderr)
	}
	if p := lastPatch(t, h); p.Labels == nil || !reflect.DeepEqual(*p.Labels, map[string]string{"b": "2", "c": "3"}) {
		t.Errorf("label patch = %+v", p.Labels)
	}
	if code, _, _ := e.run("label", "lobby-1", "--clear"); code != 0 {
		t.Fatal("label --clear failed")
	}
	if p := lastPatch(t, h); p.Labels == nil || len(*p.Labels) != 0 {
		t.Errorf("clear patch = %+v", p.Labels)
	}
	if code, _, _ := e.run("label", "lobby-1", "Bad=1"); code != 2 {
		t.Error("bad label key accepted")
	}
	if code, _, _ := e.run("rename", "lobby-1", "Front-Desk"); code != 0 {
		t.Fatal("rename failed")
	}
	if p := lastPatch(t, h); p.Name == nil || *p.Name != "front-desk" {
		t.Errorf("rename patch = %+v", p)
	}
	for _, bad := range []string{"n0123456789ab", "-x", "has space", strings.Repeat("a", 40)} {
		if code, _, _ := e.run("rename", "front-desk", bad); code != 2 {
			t.Errorf("rename to %q accepted", bad)
		}
	}
	if code, _, _ := e.run("rotate", "front-desk", "90"); code != 0 {
		t.Fatal("rotate failed")
	}
	if p := lastPatch(t, h); p.DisplayRotate == nil || *p.DisplayRotate != 90 {
		t.Errorf("rotate patch = %+v", p)
	}
	if code, _, _ := e.run("rotate", "front-desk", "45"); code != 2 {
		t.Error("rotate 45 accepted")
	}
	for cmd, check := range map[string]func(proto.NodePatch) bool{
		"drain":        func(p proto.NodePatch) bool { return p.Drain != nil && *p.Drain },
		"undrain":      func(p proto.NodePatch) bool { return p.Drain != nil && !*p.Drain },
		"approve":      func(p proto.NodePatch) bool { return p.Approved != nil && *p.Approved },
		"unquarantine": func(p proto.NodePatch) bool { return p.ClearQuarantine },
	} {
		if code, _, stderr := e.run(cmd, "front-desk"); code != 0 || !check(lastPatch(t, h)) {
			t.Errorf("%s: %d %s %+v", cmd, code, stderr, lastPatch(t, h))
		}
	}
	if code, _, _ := e.run("identify"); code != 2 {
		t.Error("identify without refs or --all accepted")
	}
	if code, _, _ := e.run("identify", "--all", "--seconds", "20"); code != 0 {
		t.Error("identify --all failed")
	}
	if code, _, _ := e.run("identify", "a", "b", "--seconds", "5"); code != 0 {
		t.Error("identify refs failed")
	}
	h.mu.Lock()
	ids := append([]proto.IdentifyRequest(nil), h.identifies...)
	h.mu.Unlock()
	if len(ids) != 2 || len(ids[0].Nodes) != 0 || ids[0].Seconds != 20 || !reflect.DeepEqual(ids[1].Nodes, []string{"a", "b"}) {
		t.Errorf("identify requests = %+v", ids)
	}
	if code, _, _ := e.run("reboot", "front-desk"); code != 0 {
		t.Error("reboot failed")
	}
	if code, _, _ := e.run("poweroff", "front-desk"); code != 0 {
		t.Error("poweroff failed")
	}
	h.mu.Lock()
	acts := append([]proto.NodeAction(nil), h.actions...)
	h.mu.Unlock()
	if len(acts) != 2 || acts[0].Action != proto.ActionReboot || acts[1].Action != proto.ActionPoweroff {
		t.Errorf("actions = %+v", acts)
	}
	if code, _, _ := e.run("forget", "front-desk"); code != 0 {
		t.Error("forget failed")
	}
	if code, stdout, _ := e.run("node", "front-desk"); code != 0 || !strings.Contains(stdout, "front-desk") {
		t.Errorf("node: %d %s", code, stdout)
	}
	if code, _, stderr := e.run("node", "nobody"); code != 1 || !strings.Contains(stderr, "no such node") {
		t.Errorf("missing node: %d %s", code, stderr)
	}
	for _, cmd := range [][]string{{"info"}, {"stats"}, {"pair"}, {"time"}, {"blobs"}, {"wall", "ls"}} {
		if code, _, stderr := e.run(cmd...); code != 0 {
			t.Errorf("%v: %d %s", cmd, code, stderr)
		}
	}
	assertTokenNeverSent(t, h)
}

func TestCheckRef(t *testing.T) {
	for _, bad := range []string{"", ".", "..", "a/b", `a\b`, "a b", "a?b", "a#b", "a%2f", "x\x00", strings.Repeat("a", 129)} {
		if checkRef("node", bad) == nil {
			t.Errorf("checkRef(%q) accepted", bad)
		}
	}
	for _, good := range []string{"lobby-1", "n0123456789ab", "j0011223344556677", "..x"} {
		if err := checkRef("node", good); err != nil {
			t.Errorf("checkRef(%q): %v", good, err)
		}
	}
}

func TestNormalizeFingerprint(t *testing.T) {
	hex := strings.Repeat("ab", 32)
	for _, in := range []string{"sha256:" + hex, strings.ToUpper(hex), "SHA256:" + strings.ToUpper(hex), " sha256:" + hex + " "} {
		if got, err := NormalizeFingerprint(in); err != nil || got != "sha256:"+hex {
			t.Errorf("NormalizeFingerprint(%q) = %q, %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "sha256:", "md5:" + hex, "sha256:" + hex[:62], "sha256:" + hex + "00", "sha256:" + strings.Repeat("zz", 32)} {
		if _, err := NormalizeFingerprint(bad); err == nil {
			t.Errorf("NormalizeFingerprint(%q) accepted", bad)
		}
	}
}
