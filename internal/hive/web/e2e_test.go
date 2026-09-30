//go:build linux

package web_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/platteration/ewastesavior/internal/auth"
	"github.com/platteration/ewastesavior/internal/config"
	"github.com/platteration/ewastesavior/internal/display"
	"github.com/platteration/ewastesavior/internal/hive"
	"github.com/platteration/ewastesavior/internal/node"
	"github.com/platteration/ewastesavior/internal/proto"
	"github.com/platteration/ewastesavior/internal/runner"
)

// TestMain lets the test binary act as the node's sandbox shim: the runner
// re-executes its own binary as "<exe> sandbox-exec ...".
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "sandbox-exec" {
		os.Exit(runner.SandboxExecMain(os.Args[2:]))
	}
	os.Exit(m.Run())
}

// TestDashboardE2E drives the dashboard in headless Chromium against a real
// hive (over TLS on loopback) with real node agents: two approved nodes
// (sandbox off, fixture hardware, memory display devices) and one waiting
// for approval. testdata/e2e.mjs (playwright-core) signs in with a pairing
// code and walks the user flows; see its header for the list.
//
// It runs only when SAVIOR_WEB_E2E is set (make test-web-e2e sets it) and
// skips when Node.js 22+, playwright-core or a Chromium it can launch is
// missing; SAVIOR_WEB_E2E=require turns those skips into failures (CI).
// SAVIOR_PLAYWRIGHT_DIR names a directory whose node_modules has
// playwright-core; SAVIOR_E2E_CHROMIUM a Chromium or Chrome executable
// (default: the browser playwright-core installed).
func TestDashboardE2E(t *testing.T) {
	mode := os.Getenv("SAVIOR_WEB_E2E")
	if mode == "" || mode == "0" {
		t.Skip("browser test: set SAVIOR_WEB_E2E=1 or run make test-web-e2e")
	}
	if testing.Short() {
		t.Skip("browser test skipped in -short mode")
	}
	missing := func(format string, args ...any) {
		t.Helper()
		if mode == "require" {
			t.Fatalf(format, args...)
		}
		t.Skipf(format, args...)
	}
	nodeJS := findNode()
	if nodeJS == "" {
		missing("Node.js 22+ not found (set SAVIOR_NODE)")
	}
	script, err := filepath.Abs("testdata/e2e.mjs")
	if err != nil {
		t.Fatal(err)
	}
	outDir := os.Getenv("SAVIOR_E2E_OUT") // keeps screenshots of a failed step
	if outDir == "" {
		outDir = t.TempDir()
	}

	// Some node start-up warnings (fixture hardware) go to the default logger.
	defer slog.SetDefault(slog.Default())
	slog.SetDefault(quietLog("default"))
	sw := startSwarm(t)
	defer sw.stop()

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, nodeJS, script)
	cmd.Env = append(os.Environ(),
		"E2E_HIVE="+sw.url,
		"E2E_ADMIN_TOKEN="+sw.srv.AdminToken(),
		"E2E_NODES="+strings.Join(sw.approved, ","),
		"E2E_PENDING="+sw.pending,
		"E2E_OUT="+outDir,
	)
	var out bytes.Buffer
	cmd.Stdout = io.MultiWriter(&out, testWriter{t})
	cmd.Stderr = cmd.Stdout
	err = cmd.Run()
	var ee *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &ee) && ee.ExitCode() == 77:
		missing("browser test prerequisites missing: %s", lastLine(out.String()))
	default:
		t.Fatalf("testdata/e2e.mjs: %v", err)
	}
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	for _, line := range strings.Split(strings.TrimRight(string(p), "\n"), "\n") {
		w.t.Log(line)
	}
	return len(p), nil
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return lines[len(lines)-1]
}

// findNode finds Node.js 22 or newer, like nodeBinary in web_test.go.
func findNode() string {
	candidates := []string{os.Getenv("SAVIOR_NODE")}
	if p, err := exec.LookPath("node"); err == nil {
		candidates = append(candidates, p)
	}
	candidates = append(candidates, "/opt/node22/bin/node")
	for _, c := range candidates {
		if c == "" {
			continue
		}
		out, err := exec.Command(c, "--version").Output()
		if err != nil {
			continue
		}
		v := strings.TrimPrefix(strings.TrimSpace(string(out)), "v")
		if major, _ := strconv.Atoi(strings.SplitN(v, ".", 2)[0]); major >= 22 {
			return c
		}
	}
	return ""
}

const e2eKey = "dashboard-e2e-swarm-key-0123456789abcdef"

type swarm struct {
	t        *testing.T
	srv      *hive.Server
	url      string
	admin    *http.Client
	stopFns  []func()
	approved []string
	pending  string
}

func quietLog(name string) *slog.Logger {
	if os.Getenv("SAVIOR_TEST_LOG") != "" {
		return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug})).With("who", name)
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// startSwarm starts a hive with join_policy=approve, two node agents it
// approves and one it leaves waiting, and waits until all are known.
func startSwarm(t *testing.T) *swarm {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv, err := hive.New(hive.Config{
		DataDir:           t.TempDir(),
		SwarmKey:          e2eKey,
		AdminToken:        auth.NewToken(),
		JoinPolicy:        "approve",
		Listener:          ln,
		StatusFile:        "-",
		HeartbeatInterval: time.Second,
		OfflineAfter:      4 * time.Second,
		LostAfter:         3 * time.Second,
		MissingAfter:      6 * time.Second,
		RecoveryWindow:    8 * time.Second,
		Log:               quietLog("hive"),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = srv.Run(ctx)
	}()
	sw := &swarm{t: t, srv: srv, url: "https://" + ln.Addr().String(),
		admin: &http.Client{Transport: &http.Transport{TLSClientConfig: auth.ClientTLSConfig(srv.Fingerprint(), nil)}, Timeout: 30 * time.Second}}
	sw.stopFns = append(sw.stopFns, func() {
		cancel()
		<-done
	})
	for _, id := range []string{"e2e-node-1", "e2e-node-2", "e2e-new-box"} {
		sw.startNode(id)
	}
	deadline := time.Now().Add(60 * time.Second)
	for {
		var views []proto.NodeView
		sw.api(http.MethodGet, "/api/v1/admin/nodes", nil, &views)
		byName := map[string]proto.NodeView{}
		for _, v := range views {
			byName[v.Name] = v
		}
		if len(byName) == 3 {
			for _, name := range []string{"e2e-node-1", "e2e-node-2"} {
				if v := byName[name]; !v.Approved {
					sw.api(http.MethodPatch, "/api/v1/admin/nodes/"+v.ID, map[string]any{"approved": true}, nil)
				}
			}
			if a, b := byName["e2e-node-1"], byName["e2e-node-2"]; a.Approved && b.Approved &&
				a.Liveness == proto.NodeOnline && b.Liveness == proto.NodeOnline && byName["e2e-new-box"].Liveness == proto.NodeOnline {
				sw.approved = []string{a.ID, b.ID}
				sw.pending = byName["e2e-new-box"].ID
				return sw
			}
		}
		if time.Now().After(deadline) {
			sw.stop()
			t.Fatalf("nodes did not come online: %+v", views)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

func (sw *swarm) startNode(id string) {
	t := sw.t
	cfg := config.Default()
	cfg.NodeID = id
	cfg.Name = id
	cfg.SwarmKey = e2eKey
	cfg.Hive = sw.url
	cfg.Sandbox = "none"
	cfg.Roles = []string{"compute", "display"}
	dir := t.TempDir()
	// Tasks run as dropped uids with the sandbox off: they must be able
	// to reach their work directory.
	for d := dir; d != os.TempDir() && d != "/"; d = filepath.Dir(d) {
		_ = os.Chmod(d, 0o755)
	}
	mem := display.NewMemDevice(320, 240, 32, "", 0, 0)
	a, err := node.New(node.Options{
		Config:        cfg,
		Log:           quietLog(id),
		SysRoot:       "../../hwinfo/testdata/qemu",
		WorkRoot:      filepath.Join(dir, "work"),
		CacheDir:      filepath.Join(dir, "cache"),
		CgroupRoot:    filepath.Join(dir, "no-cgroup"),
		StatusFile:    filepath.Join(dir, "status.json"),
		HivePanelFile: filepath.Join(dir, "no-hive-panel.json"),
		OpenDisplay:   func() (display.Device, error) { return mem, nil },
		SkipBenchmark: true,
		MinHeartbeat:  time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		a.Run(ctx)
	}()
	sw.stopFns = append(sw.stopFns, func() {
		cancel()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
		}
	})
}

// stop stops the nodes, then the hive.
func (sw *swarm) stop() {
	for i := len(sw.stopFns) - 1; i >= 0; i-- {
		sw.stopFns[i]()
	}
	sw.stopFns = nil
}

func (sw *swarm) api(method, path string, in, out any) {
	sw.t.Helper()
	var body io.Reader
	if in != nil {
		b, _ := json.Marshal(in)
		body = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, sw.url+path, body)
	req.Header.Set("Authorization", "Bearer "+sw.srv.AdminToken())
	req.Header.Set("Content-Type", "application/json")
	resp, err := sw.admin.Do(req)
	if err != nil {
		sw.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		sw.t.Fatalf("%s %s: %d %s", method, path, resp.StatusCode, b)
	}
	if out != nil {
		if err := json.Unmarshal(b, out); err != nil {
			sw.t.Fatalf("%s %s: %v", method, path, err)
		}
	}
}
