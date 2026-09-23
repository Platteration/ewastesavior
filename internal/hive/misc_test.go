package hive

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/platteration/ewastesavior/internal/config"
	"github.com/platteration/ewastesavior/internal/discovery"
	"github.com/platteration/ewastesavior/internal/proto"
)

func TestGenKey(t *testing.T) {
	t.Parallel()
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		k := GenKey()
		if len(k) != 32 || strings.Trim(k, strings.ToLower(crockfordUpper)) != "" || seen[k] {
			t.Fatalf("bad key %q", k)
		}
		seen[k] = true
	}
}

func TestPairCodeNormalization(t *testing.T) {
	t.Parallel()
	if got := normalizePairCode(" ab1o-il2z "); got != "AB10112Z" {
		t.Fatalf("normalize: %q", got)
	}
}

func TestParseClientTime(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]int64{
		"1790000000":           1790000000,
		"1790000000123":        1790000000,
		"2026-09-23T10:00:00Z": 1790157600,
	} {
		got, ok := parseClientTime(in)
		if !ok || got.Unix() != want {
			t.Fatalf("%s: %v %v", in, got, ok)
		}
	}
	if _, ok := parseClientTime("yesterday"); ok {
		t.Fatal("garbage accepted")
	}
}

func TestSourceKeyAndLimiter(t *testing.T) {
	t.Parallel()
	if sourceKey("10.0.0.7:1234") != "10.0.0.7" || sourceKey("[::ffff:10.0.0.7]:1") != "10.0.0.7" {
		t.Fatal("ipv4 key")
	}
	if a, b := sourceKey("[2001:db8:1:2:aaaa::1]:5"), sourceKey("[2001:db8:1:2:bbbb::9]:6"); a != b || a != "2001:db8:1:2::/64" {
		t.Fatalf("ipv6 /64 grouping: %q %q", a, b)
	}
	now := time.Unix(1000, 0)
	l := newLimiter()
	l.now = func() time.Time { return now }
	for i := 0; i < 4; i++ {
		l.fail(bucketRegister, "a")
	}
	if l.blocked(bucketRegister, "a") {
		t.Fatal("blocked after 4")
	}
	l.fail(bucketRegister, "a")
	if !l.blocked(bucketRegister, "a") || l.blocked(bucketAdmin, "a") || l.blocked(bucketRegister, "b") {
		t.Fatal("buckets/sources not separate")
	}
	now = now.Add(61 * time.Second)
	if l.blocked(bucketRegister, "a") {
		t.Fatal("block did not expire")
	}
	// Failures older than a minute don't count.
	for i := 0; i < 4; i++ {
		l.fail(bucketPair, "c")
	}
	now = now.Add(2 * time.Minute)
	l.fail(bucketPair, "c")
	if l.blocked(bucketPair, "c") {
		t.Fatal("old failures counted")
	}
	// /hello: 20 per second.
	allowed := 0
	for i := 0; i < 50; i++ {
		if l.allowHello("h") {
			allowed++
		}
	}
	if allowed != 20 {
		t.Fatalf("hello burst: %d", allowed)
	}
	now = now.Add(100 * time.Millisecond)
	if !l.allowHello("h") || !l.allowHello("h") || l.allowHello("h") {
		t.Fatal("hello refill")
	}
	// The maps are capped.
	for i := 0; i < limiterMaxEntries+100; i++ {
		l.fail(bucketAdmin, net.IPv4(10, byte(i>>16), byte(i>>8), byte(i)).String())
	}
	if n := l.fails[bucketAdmin].len(); n != limiterMaxEntries {
		t.Fatalf("limiter map size %d", n)
	}
}

// TestDataDirResolution changes package variables, so it is not parallel.
func TestDataDirResolution(t *testing.T) {
	root := t.TempDir()
	oldRel, oldMount, oldRAM := saviorReleaseFile, saviorDataMount, saviorRAMDir
	defer func() { saviorReleaseFile, saviorDataMount, saviorRAMDir = oldRel, oldMount, oldRAM }()
	saviorReleaseFile = filepath.Join(root, "savior-release")
	saviorDataMount = filepath.Join(root, "data")
	saviorRAMDir = filepath.Join(root, "ram-hive")

	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "xdg"))
	d, err := resolveDataDir("auto")
	if err != nil || d.path != filepath.Join(root, "xdg", "savior", "hive") || d.warning != "" && !d.ram {
		t.Fatalf("elsewhere: %+v %v", d, err)
	}
	if fi, err := os.Stat(d.path); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("data dir mode: %v %v", fi, err)
	}
	// On SaviorOS without the data partition: RAM with a warning.
	os.WriteFile(saviorReleaseFile, []byte("SaviorOS test\n"), 0o644)
	d, err = resolveDataDir("")
	if err != nil || d.path != saviorRAMDir || d.persistent || !strings.Contains(d.warning, "RAM") {
		t.Fatalf("saviorOS without partition: %+v %v", d, err)
	}
	// An explicit directory is used as is.
	explicit := filepath.Join(root, "explicit")
	if d, err = resolveDataDir(explicit); err != nil || d.path != explicit {
		t.Fatalf("explicit: %+v %v", d, err)
	}
}

func TestSecretsGeneratedAndStored(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfg := testConfig(dir)
	cfg.SwarmKey, cfg.AdminToken = "", ""
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	key, tok := s.SwarmKey(), s.AdminToken()
	s.Close()
	if len(key) != 32 || len(tok) < 32 || s.AdminTokenFile() != filepath.Join(dir, "admin_token") {
		t.Fatalf("generated: %q %q %q", key, tok, s.AdminTokenFile())
	}
	for _, f := range []string{"swarm_key", "admin_token", "hive_id", "tls/key.pem"} {
		fi, err := os.Stat(filepath.Join(dir, f))
		if err != nil || fi.Mode().Perm() != 0o600 {
			t.Fatalf("%s: %v %v", f, fi, err)
		}
	}
	found := false
	for _, w := range s.Info().Warnings {
		found = found || strings.Contains(w, "swarm_key")
	}
	if !found {
		t.Fatal("no warning about the generated swarm key")
	}
	// The next start reuses them.
	s2, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if s2.SwarmKey() != key || s2.AdminToken() != tok {
		t.Fatal("secrets not reused")
	}
	// Short configured secrets are refused.
	bad := testConfig(t.TempDir())
	bad.SwarmKey = "short"
	if _, err := New(bad); err == nil {
		t.Fatal("short swarm key accepted")
	}
	bad = testConfig(t.TempDir())
	bad.JoinPolicy = "whatever"
	if _, err := New(bad); err == nil {
		t.Fatal("bad join policy accepted")
	}
}

func TestRunServesAndShutsDown(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	nb := t.TempDir()
	os.WriteFile(filepath.Join(nb, "vmlinuz"), []byte("kernel"), 0o644)
	os.WriteFile(filepath.Join(nb, "savior.conf"), []byte("swarm_key = leak"), 0o644)
	os.WriteFile(filepath.Join(nb, ".hidden"), []byte("x"), 0o644)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	statusFile := filepath.Join(t.TempDir(), "hive-status.json")
	cfg := testConfig(dir)
	cfg.Listener = ln
	cfg.NetbootDir = nb
	cfg.NetbootListen = "127.0.0.1:0"
	cfg.StatusFile = statusFile
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	eventually(t, "listening", func() bool { return s.Addr() != nil })
	c := pinnedClient(s.Fingerprint())
	var hl proto.Hello
	if st, _ := do(t, c, "GET", "https://"+s.Addr().String()+"/api/v1/hello", "", nil, &hl); st != 200 || hl.HiveID != s.HiveID() {
		t.Fatalf("hello over Run: %d", st)
	}
	if info := s.Info(); info.Listen != ln.Addr().String() || !strings.HasSuffix(info.URLs[0], "/") {
		t.Fatalf("info: %+v", info)
	}
	eventually(t, "status file", func() bool {
		data, err := os.ReadFile(statusFile)
		var p StatusPanel
		return err == nil && json.Unmarshal(data, &p) == nil && p.Fingerprint == s.Fingerprint() && len(p.PairCode) == 8
	})
	if fi, _ := os.Stat(statusFile); fi.Mode().Perm() != 0o600 {
		t.Fatalf("status file mode %v", fi.Mode())
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return")
	}
	if _, err := os.Stat(filepath.Join(dir, "state.json")); err != nil {
		t.Fatal("state not saved on shutdown")
	}
}

func TestNetbootHandler(t *testing.T) {
	t.Parallel()
	nb := t.TempDir()
	os.MkdirAll(filepath.Join(nb, "boot", "x86_64"), 0o755)
	os.WriteFile(filepath.Join(nb, "boot", "x86_64", "vmlinuz"), []byte("kernel"), 0o644)
	os.WriteFile(filepath.Join(nb, "savior.conf"), []byte("swarm_key = leak"), 0o644)
	os.WriteFile(filepath.Join(nb, ".hidden"), []byte("x"), 0o644)
	h := netbootHandler(nb)
	get := func(method, p string) (int, string) {
		rec := &recorder{hdr: http.Header{}}
		req, _ := http.NewRequest(method, "http://x"+p, nil)
		h.ServeHTTP(rec, req)
		return rec.code, rec.body.String()
	}
	if code, body := get("GET", "/boot/x86_64/vmlinuz"); code != 200 || body != "kernel" {
		t.Fatalf("payload: %d %q", code, body)
	}
	for _, p := range []string{"/savior.conf", "/SAVIOR.CONF", "/.hidden", "/", "/boot/", "/../etc/passwd"} {
		if code, _ := get("GET", p); code != http.StatusNotFound {
			t.Fatalf("%s: %d", p, code)
		}
	}
	if code, _ := get("POST", "/boot/x86_64/vmlinuz"); code != http.StatusMethodNotAllowed {
		t.Fatalf("POST: %d", code)
	}
}

type recorder struct {
	code int
	hdr  http.Header
	body bytes.Buffer
}

func (r *recorder) Header() http.Header { return r.hdr }
func (r *recorder) Write(b []byte) (int, error) {
	if r.code == 0 {
		r.code = 200
	}
	return r.body.Write(b)
}
func (r *recorder) WriteHeader(c int) {
	if r.code == 0 {
		r.code = c
	}
}

func TestMainFlags(t *testing.T) {
	t.Parallel()
	var out, errb bytes.Buffer
	if rc := run([]string{"--help"}, &out, &errb); rc != 0 || !strings.Contains(errb.String(), "--listen") && !strings.Contains(errb.String(), "-listen") {
		t.Fatalf("help: %d %q", rc, errb.String())
	}
	if rc := run([]string{"--bogus"}, io.Discard, io.Discard); rc != 2 {
		t.Fatalf("bad flag: %d", rc)
	}
	if rc := run([]string{"extra"}, io.Discard, io.Discard); rc != 2 {
		t.Fatalf("extra arg: %d", rc)
	}
}

func TestPrintBanner(t *testing.T) {
	t.Parallel()
	h := newHive(t, nil)
	var b bytes.Buffer
	printBanner(&b, h.s)
	out := b.String()
	for _, want := range []string{h.s.Fingerprint(), "Pairing code", "Admin token", "https://"} {
		if !strings.Contains(out, want) {
			t.Fatalf("banner lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, testAdmin) || strings.Contains(out, testKey) {
		t.Fatal("banner prints a secret")
	}
}

func TestOtherHiveWarning(t *testing.T) {
	t.Parallel()
	h := newHive(t, nil)
	pc, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := pc.LocalAddr().(*net.UDPAddr).Port
	pc.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	addr := "127.0.0.1:" + itoa(uint64(port))
	// A rogue hive using our swarm key answers probes on the loopback.
	go discovery.Announce(ctx, proto.Beacon{HiveID: "hrogue", Port: 7700, SwarmHint: swarmSecret().SwarmHint()},
		discovery.AnnounceOptions{Port: port, Interval: time.Hour, ListenAddr: addr, Targets: []string{"127.0.0.1:9"}})
	time.Sleep(100 * time.Millisecond)
	h.s.scanOtherHives(ctx, []string{addr})
	found := false
	for _, w := range h.s.Info().Warnings {
		found = found || strings.Contains(w, "another hive with this swarm key at https://127.0.0.1:7700")
	}
	if !found {
		t.Fatalf("no warning: %v", h.s.Info().Warnings)
	}
}

func TestConfigFromFile(t *testing.T) {
	t.Parallel()
	c := config.Default()
	c.HiveListen, c.HiveData, c.SwarmKey, c.AdminToken, c.JoinPolicy, c.Beacon, c.Netboot =
		":8800", "/srv/hive", "k", "t", "approve", false, true
	cfg := ConfigFromFile(c)
	if cfg.Listen != ":8800" || cfg.DataDir != "/srv/hive" || cfg.SwarmKey != "k" || cfg.AdminToken != "t" ||
		cfg.JoinPolicy != "approve" || cfg.Beacon || cfg.NetbootDir == "" {
		t.Fatalf("%+v", cfg)
	}
	cfg.setDefaults()
	if cfg.NetbootListen != DefaultNetbootListen || cfg.OfflineAfter != DefaultOfflineAfter || cfg.BeaconPort != proto.DiscoveryPort {
		t.Fatalf("defaults: %+v", cfg)
	}
}

func TestRenderCacheEviction(t *testing.T) {
	t.Parallel()
	c, err := newRenderCache(t.TempDir(), 100)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	render := func(n int) func() ([]byte, error) {
		return func() ([]byte, error) { calls++; return bytes.Repeat([]byte("x"), n), nil }
	}
	ctx := context.Background()
	c.do(ctx, "a", render(40))
	c.do(ctx, "b", render(40))
	if _, ok := c.get("a"); !ok { // a is now most recent
		t.Fatal("a missing")
	}
	c.do(ctx, "c", render(40)) // evicts b
	if _, ok := c.get("b"); ok {
		t.Fatal("LRU did not evict b")
	}
	if _, ok := c.get("a"); !ok {
		t.Fatal("a evicted")
	}
	if c.size > 100 {
		t.Fatalf("cache size %d", c.size)
	}
	if _, err := os.Stat(c.path("b")); !os.IsNotExist(err) {
		t.Fatal("evicted file kept")
	}
	c.do(ctx, "huge", render(200)) // larger than the cache: not stored
	if _, ok := c.get("huge"); ok || calls != 4 {
		t.Fatalf("oversized entry cached (calls %d)", calls)
	}
}
