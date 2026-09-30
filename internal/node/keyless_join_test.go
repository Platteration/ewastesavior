//go:build linux

package node

import (
	"context"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/platteration/ewastesavior/internal/auth"
	"github.com/platteration/ewastesavior/internal/config"
	"github.com/platteration/ewastesavior/internal/hive"
	"github.com/platteration/ewastesavior/internal/proto"
)

// startKeylessHive is startHive for a hive that accepts keyless joins (as
// with netboot = yes).
func startKeylessHive(t *testing.T) *testHive {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	cfg := hiveConfig(dir, ln)
	cfg.KeylessJoin = true
	srv, err := hive.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	h := &testHive{t: t, srv: srv, addr: ln.Addr().String(), dir: dir, cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(h.done)
		srv.Run(ctx)
	}()
	h.admin = &http.Client{Transport: &http.Transport{TLSClientConfig: auth.ClientTLSConfig(srv.Fingerprint(), nil)}, Timeout: 30 * time.Second}
	return h
}

// keylessNode makes a node join like a netbooted machine: no swarm key,
// join = keyless, the hive pinned by fingerprint, and fixture hardware with
// stable hardware IDs.
func keylessNode(fp string) func(*config.Config, *Options) {
	return func(c *config.Config, o *Options) {
		c.SwarmKey = ""
		c.Join = "keyless"
		c.HiveFingerprint = fp
		o.SysRoot = "../hwinfo/testdata/thinkpad-t60"
	}
}

func (n *testNode) linkDetail() string {
	n.agent.mu.Lock()
	defer n.agent.mu.Unlock()
	return n.agent.linkErr
}

func (h *testHive) adminNodeView(id string) (proto.NodeView, int) {
	var v proto.NodeView
	code := h.api(http.MethodGet, "/api/v1/admin/nodes/"+id, nil, &v)
	return v, code
}

// The node side of a keyless (netboot) join: pending with no work until an
// admin approves it, then connected; the same machine re-joining later is
// re-approved without the admin (DESIGN 6.2).
func TestKeylessJoinApproval(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	h := startKeylessHive(t)
	defer h.stop()
	n := startNode(t, h, "netbooted", keylessNode(h.srv.Fingerprint()))
	waitFor(t, "keyless node pending", 30*time.Second, func() bool { return n.link() == proto.LinkPending })

	id := h.submit(proto.JobSpec{Name: "after-approval", Script: "echo ok > out.txt", Outputs: []string{"out.txt"}, Count: 1})
	time.Sleep(3 * time.Second)
	if c := h.job(id).Counts; c.Assigned+c.Running+c.Succeeded != 0 {
		t.Fatalf("a pending keyless node got work: %+v", c)
	}
	if v, _ := h.adminNodeView("netbooted"); v.Approved || v.Liveness != proto.NodeOnline {
		t.Fatalf("pending node view: %+v", v)
	}

	if code := h.api(http.MethodPatch, "/api/v1/admin/nodes/netbooted", approvePatch(), nil); code != http.StatusOK {
		t.Fatalf("approve: %d", code)
	}
	waitFor(t, "approved node connected", 30*time.Second, func() bool { return n.link() == proto.LinkConnected })
	waitFor(t, "job succeeded on the keyless node", 60*time.Second, func() bool { return h.job(id).State == proto.JobSucceeded })

	// The machine reboots: a new agent (new boot ID) with the same identity.
	n.stop()
	waitFor(t, "node offline", 30*time.Second, func() bool {
		v, _ := h.adminNodeView("netbooted")
		return v.Liveness != proto.NodeOnline
	})
	again := startNode(t, h, "netbooted", keylessNode(h.srv.Fingerprint()))
	waitFor(t, "re-approved without the admin", 30*time.Second, func() bool { return again.link() == proto.LinkConnected })
	id2 := h.submit(proto.JobSpec{Name: "after-reboot", Script: "true", Count: 1})
	waitFor(t, "job after reboot", 60*time.Second, func() bool { return h.job(id2).State == proto.JobSucceeded })
}

// A keyless node needs the hive pinned, and a hive without keyless joins
// refuses it without creating a record.
func TestKeylessJoinRefused(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	h := startHive(t, t.TempDir(), "") // keyless joins off (no netboot)
	defer h.stop()
	unpinned := startNode(t, h, "unpinned", keylessNode(""))
	refused := startNode(t, h, "refused", keylessNode(h.srv.Fingerprint()))
	waitFor(t, "unpinned keyless node rejected", 30*time.Second, func() bool { return unpinned.link() == proto.LinkRejected })
	waitFor(t, "keyless node refused", 30*time.Second, func() bool { return refused.link() == proto.LinkRejected })
	if e := refused.linkDetail(); !strings.Contains(e, "keyless joins are disabled") {
		t.Fatalf("refusal detail %q", e)
	}
	var views []proto.NodeView
	h.api(http.MethodGet, "/api/v1/admin/nodes", nil, &views)
	if len(views) != 0 {
		t.Fatalf("refused keyless nodes were registered: %+v", views)
	}
}

// A node without hive_fingerprint pins the certificate of the hive it first
// joined for the rest of the process (DESIGN 6.2 step 5): another server
// with the swarm key taking over the hive's address doesn't get it.
func TestTOFUPinKeptAcrossRejoins(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	a := startHive(t, t.TempDir(), "")
	addr := a.addr
	n := startNode(t, a, "tofu", nil) // no hive_fingerprint
	waitFor(t, "joined hive A", 30*time.Second, func() bool { return n.link() == proto.LinkConnected })
	a.stop()

	b := startHive(t, t.TempDir(), addr) // same swarm key and address, new certificate
	defer b.stop()
	if b.srv.Fingerprint() == a.srv.Fingerprint() {
		t.Fatal("hive B reused hive A's certificate")
	}
	waitFor(t, "fingerprint mismatch with hive B", 60*time.Second, func() bool { return n.link() == proto.LinkFingerprintMismatch })
	if e := n.linkDetail(); !strings.Contains(e, "until it restarts") {
		t.Fatalf("mismatch detail %q", e)
	}
	time.Sleep(2 * time.Second)
	if _, code := b.adminNodeView("tofu"); code != http.StatusNotFound {
		t.Fatalf("the node registered with the impostor hive: %d", code)
	}
	if l := n.link(); l == proto.LinkConnected || l == proto.LinkPending {
		t.Fatalf("node link %s", l)
	}
}

func approvePatch() proto.NodePatch {
	yes := true
	return proto.NodePatch{Approved: &yes}
}
