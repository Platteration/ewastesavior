package config

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/platteration/ewastesavior/internal/proto"
)

func TestDefaults(t *testing.T) {
	c := Default()
	if c.Hive != "auto" || c.Net != "dhcp" || c.MaxMemPercent != 75 || !c.Beacon || c.DisplayDevice != "auto" || c.Join != "key" || c.JoinPolicy != "open" {
		t.Fatalf("unexpected defaults: %+v", c)
	}
	if !reflect.DeepEqual(c.Roles, []string{"auto"}) || !reflect.DeepEqual(c.NTP, []string{"pool.ntp.org"}) {
		t.Fatalf("list defaults: roles=%v ntp=%v", c.Roles, c.NTP)
	}
}

func TestParseFile(t *testing.T) {
	in := "\ufeff# comment\r\n" +
		"name = shelf-3\r\n" +
		"roles = compute, display\n" +
		"swarm_key = \"correct horse battery staple\"\n" +
		"ssh_key = ssh-ed25519 AAAAC3Nza key,with,commas\n" +
		"ssh_key = ssh-rsa AAAAB3 other\n" +
		"labels = room=lab, gpu=none\n" +
		"max_temp_c = 999\n" +
		"bogus = 1\n" +
		"no equals sign\n" +
		"hive_fingerprint = SHA256:AB" + strings.Repeat("cd", 31) + "\n" +
		"display_text = # not a comment\n"
	c := Default()
	warnings := ParseFile(strings.NewReader(in), "t.conf", &c)
	if c.Name != "shelf-3" {
		t.Errorf("name = %q", c.Name)
	}
	if !reflect.DeepEqual(c.Roles, []string{"compute", "display"}) {
		t.Errorf("roles = %v", c.Roles)
	}
	if c.SwarmKey != "correct horse battery staple" {
		t.Errorf("swarm_key = %q", c.SwarmKey)
	}
	if len(c.SSHKeys) != 2 || c.SSHKeys[0] != "ssh-ed25519 AAAAC3Nza key,with,commas" {
		t.Errorf("ssh keys = %q", c.SSHKeys)
	}
	if c.Labels["room"] != "lab" || c.Labels["gpu"] != "none" {
		t.Errorf("labels = %v", c.Labels)
	}
	if c.MaxTempC != 85 {
		t.Errorf("max_temp_c should keep default, got %d", c.MaxTempC)
	}
	if c.HiveFingerprint != "sha256:ab"+strings.Repeat("cd", 31) {
		t.Errorf("fingerprint = %q", c.HiveFingerprint)
	}
	if c.DisplayText != "# not a comment" {
		t.Errorf("display_text = %q", c.DisplayText)
	}
	if len(warnings) != 3 {
		t.Errorf("want 3 warnings (max_temp_c, bogus, no equals), got %q", warnings)
	}
}

func TestShortSwarmKeyRejected(t *testing.T) {
	c := Default()
	w := ParseFile(strings.NewReader("swarm_key = short\n"), "x", &c)
	if c.SwarmKey != "" || len(w) != 1 {
		t.Fatalf("short key accepted: %q %q", c.SwarmKey, w)
	}
	if strings.Contains(w[0], "short") && !strings.Contains(w[0], "too short") {
		t.Fatalf("warning leaks secret: %q", w[0])
	}
}

func TestListSourceSemantics(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.conf")
	b := filepath.Join(dir, "b.conf")
	os.WriteFile(a, []byte("dns = 1.1.1.1\ndns = 8.8.8.8\nntp = a.example\n"), 0o644)
	os.WriteFile(b, []byte("dns = 9.9.9.9\n"), 0o644)
	c, _, err := Load([]string{a, b, filepath.Join(dir, "missing.conf")}, "")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(c.DNS, []string{"9.9.9.9"}) {
		t.Errorf("later source should replace list: %v", c.DNS)
	}
	if !reflect.DeepEqual(c.NTP, []string{"a.example"}) {
		t.Errorf("file should replace default list: %v", c.NTP)
	}
}

func TestCmdline(t *testing.T) {
	c := Default()
	w := ParseCmdline(`BOOT_IMAGE=/boot/vmlinuz quiet savior.roles=display savior.display_text=Hello%20World savior.console_shell "savior.name=lab-1" savior.nothing savior.hive=10.0.0.5:7700`, &c)
	if !reflect.DeepEqual(c.Roles, []string{"display"}) || c.DisplayText != "Hello World" || !c.ConsoleShell || c.Name != "lab-1" || c.Hive != "10.0.0.5:7700" {
		t.Fatalf("cmdline not applied: %+v", c)
	}
	if len(w) != 1 {
		t.Fatalf("want 1 warning, got %q", w)
	}
}

func TestRoundTrip(t *testing.T) {
	c := Default()
	c.Name = "x"
	c.SwarmKey = ` spaces and "quotes" \ `
	c.SSHKeys = []string{"ssh-ed25519 AAAA a,b", "ssh-rsa BBBB"}
	c.Labels = map[string]string{"a": "1", "b": "two"}
	c.DNS = nil
	c.DisplayText = `"quoted"`
	var buf bytes.Buffer
	if err := c.WriteFile(&buf); err != nil {
		t.Fatal(err)
	}
	d := Default()
	if w := ParseFile(&buf, "rt", &d); len(w) != 0 {
		t.Fatalf("warnings: %q\n%s", w, buf.String())
	}
	if !reflect.DeepEqual(c, d) {
		t.Fatalf("round trip mismatch:\n%+v\n%+v", c, d)
	}
}

func TestShellEnv(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	c := Default()
	c.DisplayText = `it's "fun" $HOME`
	c.Roles = []string{"hive", "display"}
	c.SSHKeys = []string{"ssh-ed25519 AAA one", "ssh-ed25519 BBB two"}
	script := c.ShellEnv() + `printf '%s|%s|%s|%s' "$SAVIOR_DISPLAY_TEXT" "$SAVIOR_ROLE_HIVE" "$SAVIOR_ROLE_COMPUTE" "$SAVIOR_SSH_KEY"`
	out, err := exec.Command("sh", "-c", script).Output()
	if err != nil {
		t.Fatal(err)
	}
	want := `it's "fun" $HOME|yes|no|ssh-ed25519 AAA one` + "\n" + `ssh-ed25519 BBB two`
	if string(out) != want {
		t.Fatalf("got %q want %q", out, want)
	}
}

func TestEffectiveRoles(t *testing.T) {
	c := Default()
	if got := c.EffectiveRoles(true); !reflect.DeepEqual(got, []proto.Role{proto.RoleCompute, proto.RoleDisplay}) {
		t.Errorf("auto+fb: %v", got)
	}
	if got := c.EffectiveRoles(false); !reflect.DeepEqual(got, []proto.Role{proto.RoleCompute}) {
		t.Errorf("auto: %v", got)
	}
	c.Roles = []string{"hive", "auto"}
	if got := c.EffectiveRoles(false); !reflect.DeepEqual(got, []proto.Role{proto.RoleCompute, proto.RoleHive}) {
		t.Errorf("hive+auto: %v", got)
	}
}

func TestHiveURL(t *testing.T) {
	cases := map[string]string{
		"10.0.0.5":             "https://10.0.0.5:7700",
		"hive.lan:8443":        "https://hive.lan:8443",
		"https://hive.lan":     "https://hive.lan:7700",
		"https://10.1.1.1:99/": "https://10.1.1.1:99",
		"[fe80::1]:7700":       "https://[fe80::1]:7700",
	}
	for in, want := range cases {
		got, err := HiveURL(in)
		if err != nil || got != want {
			t.Errorf("HiveURL(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"auto", "", "http://x", "x:0", "https://x/path"} {
		if _, err := HiveURL(bad); err == nil {
			t.Errorf("HiveURL(%q) should fail", bad)
		}
	}
}

func TestCLI(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "s.conf")
	os.WriteFile(f, []byte("name = abc\nssh_key = ssh-ed25519 K1 c\nssh_key = ssh-ed25519 K2 d\n"), 0o644)
	var out, errb bytes.Buffer
	if rc := run([]string{"get", "--file", f, "--cmdline-file", "", "name", "ssh_key"}, &out, &errb); rc != 0 {
		t.Fatalf("rc=%d %s", rc, errb.String())
	}
	if out.String() != "abc\nssh-ed25519 K1 c\nssh-ed25519 K2 d\n" {
		t.Fatalf("get output %q", out.String())
	}
	dst := filepath.Join(dir, "run", "merged.conf")
	out.Reset()
	if rc := run([]string{"dump", "--file", f, "--cmdline-file", "", "--out", dst}, &out, &errb); rc != 0 {
		t.Fatalf("dump rc=%d %s", rc, errb.String())
	}
	st, err := os.Stat(dst)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("dump file: %v %v", st, err)
	}
	out.Reset()
	if rc := run([]string{"sample"}, &out, &errb); rc != 0 || !strings.Contains(out.String(), "#swarm_key =") {
		t.Fatalf("sample: %d %q", rc, out.String())
	}
}
