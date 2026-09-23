package node

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/platteration/ewastesavior/internal/auth"
	"github.com/platteration/ewastesavior/internal/config"
	"github.com/platteration/ewastesavior/internal/discovery"
	"github.com/platteration/ewastesavior/internal/hwinfo"
	"github.com/platteration/ewastesavior/internal/proto"
	"github.com/platteration/ewastesavior/internal/version"
)

const (
	// blacklistFor is how long a hive that answered wrongly (swarm key,
	// fingerprint, version) is skipped.
	blacklistFor = 5 * time.Minute
	// unreachableFirst and unreachableMax bound the backoff from a hive that
	// could not be reached at all (not listening yet, network down, 5xx).
	unreachableFirst = 2 * time.Second
	unreachableMax   = time.Minute
)

// join finds the hive and registers. It returns a client pinned to the
// hive's certificate with a valid node token.
func (a *Agent) join(ctx context.Context) (*hiveClient, error) {
	keyless := a.cfg.Join == "keyless"
	if a.secret.IsZero() && !keyless {
		a.setLink(proto.LinkNoSwarmKey, "", "")
		sleep(ctx, 30*time.Second)
		return nil, errors.New("no swarm key")
	}
	if keyless && a.cfg.HiveFingerprint == "" {
		a.setLink(proto.LinkRejected, "", "join=keyless needs hive_fingerprint")
		sleep(ctx, 60*time.Second)
		return nil, errors.New("keyless join without a pinned hive")
	}
	if !haveNetwork() {
		a.setLink(proto.LinkNoNetwork, "", "")
		return nil, errors.New("no network")
	}
	candidates, err := a.candidates(ctx)
	if err != nil {
		return nil, err
	}
	var lastErr error
	for _, base := range candidates {
		if until, ok := a.blacklisted(base); ok {
			lastErr = fmt.Errorf("%s blacklisted until %s", base, until.Format(time.TimeOnly))
			continue
		}
		hc, err := a.handshake(ctx, base)
		if err == nil {
			return hc, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}
	if lastErr == nil {
		lastErr = errors.New("no hive candidates")
	}
	return nil, lastErr
}

func haveNetwork() bool {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return true // can't tell; try anyway
	}
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok && !ipn.IP.IsLoopback() && !ipn.IP.IsLinkLocalUnicast() {
			return true
		}
	}
	// Link-local only still counts on an isolated switch with zcip.
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil && ipn.IP.IsLinkLocalUnicast() {
			return true
		}
	}
	return false
}

// candidates returns hive base URLs to try, in order.
func (a *Agent) candidates(ctx context.Context) ([]string, error) {
	if a.cfg.Hive != "" && a.cfg.Hive != "auto" {
		u, err := config.HiveURL(a.cfg.Hive)
		if err != nil {
			return nil, err
		}
		return []string{u}, nil
	}
	a.setLink(proto.LinkSearching, "", "")
	opts := discovery.DiscoverOptions{Port: a.opt.DiscoveryPort, ProbeInterval: time.Second, Targets: a.opt.DiscoveryTargets}
	var hint string
	if !a.secret.IsZero() {
		hint = a.secret.SwarmHint()
	}
	for ctx.Err() == nil {
		seen, err := discovery.Collect(ctx, 2*time.Second, opts)
		if err != nil {
			a.log.Debug("discovery failed", "err", err)
		}
		var mine, others []string
		for _, c := range seen {
			switch {
			case hint != "" && c.Beacon.SwarmHint == hint:
				mine = append(mine, c.URL)
			case hint == "" && a.cfg.HiveFingerprint != "" && c.Beacon.Fingerprint == a.cfg.HiveFingerprint:
				// Keyless nodes recognize their pinned hive by fingerprint.
				mine = append(mine, c.URL)
			default:
				others = append(others, c.URL)
			}
		}
		if len(mine) > 0 {
			return dedupe(mine), nil
		}
		if len(others) > 0 {
			a.setLink(proto.LinkKeyMismatch, others[0], fmt.Sprintf("%d hive(s) with a different swarm key", len(others)))
		}
		sleep(ctx, 3*time.Second)
	}
	return nil, ctx.Err()
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

func (a *Agent) blacklisted(base string) (time.Time, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	until, ok := a.blacklist[base]
	if ok && time.Now().After(until) {
		delete(a.blacklist, base)
		return time.Time{}, false
	}
	return until, ok
}

func (a *Agent) ban(base string) {
	a.mu.Lock()
	a.blacklist[base] = time.Now().Add(blacklistFor)
	a.mu.Unlock()
}

// banUnreachable skips a hive that could not be reached for a short,
// growing time. Unreachable says nothing about it being the wrong hive: a
// hive machine's own node agent starts before the hive listens.
func (a *Agent) banUnreachable(base string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.unreachable == nil {
		a.unreachable = map[string]time.Duration{}
	}
	d := a.unreachable[base] * 2
	if d == 0 {
		d = unreachableFirst
	}
	d = min(d, unreachableMax)
	a.unreachable[base] = d
	a.blacklist[base] = time.Now().Add(d)
}

func hostOf(base string) string {
	if u, err := url.Parse(base); err == nil {
		return u.Host
	}
	return base
}

// handshake runs DESIGN 6.2 against one hive.
func (a *Agent) handshake(ctx context.Context, base string) (*hiveClient, error) {
	addr := hostOf(base)
	hctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()

	hello, fpSeen, err := probeHello(hctx, base, a.cfg.HiveFingerprint)
	if err != nil {
		if errors.Is(err, auth.ErrFingerprintMismatch) || strings.Contains(err.Error(), "fingerprint mismatch") {
			a.setLink(proto.LinkFingerprintMismatch, addr, "the hive's certificate doesn't match hive_fingerprint")
			a.ban(base)
			return nil, err
		}
		if code := statusOf(err); code == 429 {
			a.setLink(proto.LinkRateLimited, addr, "")
			return nil, err
		}
		a.setLink(proto.LinkUnreachable, addr, err.Error())
		a.banUnreachable(base)
		return nil, err
	}
	if hello.APIVersion != proto.APIVersion {
		a.setLink(proto.LinkVersionMismatch, addr, fmt.Sprintf("hive speaks API v%d, this node v%d", hello.APIVersion, proto.APIVersion))
		a.ban(base)
		return nil, fmt.Errorf("API version mismatch")
	}

	// Everything from here on goes only over connections pinned to fpSeen.
	hc := newHiveClient(base, fpSeen)
	nodeNonce := auth.NewNonce()
	req := a.registerRequest()
	req.HiveNonce, req.NodeNonce = hello.Nonce, nodeNonce
	keyless := a.secret.IsZero()
	if !keyless {
		req.Proof = a.secret.NodeProof(hello.Nonce, nodeNonce, req.NodeID, fpSeen)
	}
	sent := time.Now()
	resp, err := hc.register(hctx, req)
	recv := time.Now()
	if err != nil {
		hc.close()
		switch statusOf(err) {
		case 400:
			// The hive refuses the registration itself (e.g. a node_id it
			// can't accept): the hive is reachable, and retrying won't help
			// until savior.conf changes. display.LinkHint turns the detail
			// into a fix.
			msg := "the hive refused this node's registration"
			var he *HTTPError
			if errors.As(err, &he) && he.Msg != "" {
				msg += ": " + he.Msg
			}
			a.setLink(proto.LinkRejected, addr, msg)
			a.ban(base)
		case 403:
			a.setLink(proto.LinkRejected, addr, "the hive rejected our swarm key")
			a.ban(base)
		case 409:
			a.setLink(proto.LinkDuplicate, addr, "another online machine uses this node ID")
			sleep(ctx, 30*time.Second)
		case 426:
			a.setLink(proto.LinkVersionMismatch, addr, err.Error())
			a.ban(base)
		case 429:
			a.setLink(proto.LinkRateLimited, addr, "")
			sleep(ctx, 60*time.Second)
		default:
			a.setLink(proto.LinkUnreachable, addr, err.Error())
			a.banUnreachable(base)
		}
		return nil, err
	}
	if !keyless {
		want := a.secret.HiveProof(hello.Nonce, nodeNonce, req.NodeID, fpSeen)
		if !auth.VerifyProof(want, resp.HiveProof) {
			hc.close()
			a.setLink(proto.LinkRejected, addr, "the hive could not prove it knows the swarm key")
			a.ban(base)
			return nil, errors.New("invalid hive proof")
		}
	}
	hc.setToken(resp.Token)
	a.log.Info("registered with hive", "hive", base, "fingerprint", fpSeen, "node_id", resp.NodeID, "name", resp.Name, "pending", resp.Pending)

	a.mu.Lock()
	delete(a.unreachable, base)
	a.hc = hc
	a.hiveAddr = addr
	a.sessionGen++
	if resp.Name != "" {
		a.name = resp.Name
	}
	if resp.ShortCode != "" {
		a.shortCode = resp.ShortCode
	}
	if resp.HeartbeatIntervalS > 0 {
		a.hbInterval = time.Duration(resp.HeartbeatIntervalS) * time.Second
	}
	if a.opt.MinHeartbeat > 0 && a.hbInterval > a.opt.MinHeartbeat {
		a.hbInterval = a.opt.MinHeartbeat
	}
	a.pending = resp.Pending
	adopted := map[string]bool{}
	for _, id := range resp.AdoptedTasks {
		adopted[id] = true
	}
	a.mu.Unlock()

	a.dropUnadopted(adopted)
	a.observeHiveTime(resp.Time, resp.TimeSynced, sent, recv)
	a.applyDirectives(resp.Directives)
	if resp.Pending {
		a.setLink(proto.LinkPending, addr, "waiting for an admin to approve this machine")
	} else {
		a.setLink(proto.LinkConnected, addr, "")
	}
	return hc, nil
}

func (a *Agent) registerRequest() *proto.RegisterRequest {
	a.mu.Lock()
	defer a.mu.Unlock()
	labels := map[string]string{}
	for k, v := range a.cfg.Labels {
		labels[k] = v
	}
	inv := a.inv
	inv.NICs = nil
	if fresh, err := hwinfo.Collect(a.opt.SysRoot); err == nil {
		inv.NICs = fresh.NICs
		inv.Framebuffers = fresh.Framebuffers
		inv.Connectors = fresh.Connectors
	}
	return &proto.RegisterRequest{
		APIVersion:    proto.APIVersion,
		NodeID:        a.id.NodeID,
		HWIDs:         a.id.HWIDs,
		BootID:        a.bootID,
		Name:          a.cfgName(),
		Roles:         a.roles,
		Labels:        labels,
		Version:       version.Version,
		Inventory:     inv,
		Total:         a.total,
		ScratchInRAM:  a.scratchInRAM,
		Sandbox:       a.sandboxMode,
		SandboxCaps:   a.sandboxCaps,
		DisplayRotate: a.cfg.DisplayRotate,
		RunningTasks:  a.runningLocked(),
	}
}

// cfgName is the name we ask for: configured, else the hardware default.
func (a *Agent) cfgName() string {
	if a.cfg.Name != "" {
		return strings.ToLower(a.cfg.Name)
	}
	return hwinfo.DefaultName(a.id)
}
