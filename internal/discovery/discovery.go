// Package discovery lets nodes find the hive on the local network with UDP
// broadcast beacons and probes. See docs/DESIGN.md section 7.3.
//
// Beacons are unauthenticated hints: the join handshake (internal/auth)
// is what actually proves the hive is genuine.
package discovery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/platteration/ewastesavior/internal/proto"
)

// MaxDatagram is the largest datagram we send or accept.
const MaxDatagram = 1400

// AnnounceOptions configures the hive side.
type AnnounceOptions struct {
	Port     int           // UDP port to send to and listen on for probes (default proto.DiscoveryPort)
	Interval time.Duration // beacon period (default 5 s)
	// Targets overrides the broadcast destinations ("ip:port"). Empty =
	// 255.255.255.255 plus each interface's directed broadcast address.
	Targets []string
	// ListenAddr overrides the probe listen address (default ":<Port>").
	ListenAddr string
	// AllowAnySource answers probes from any address (tests); by default only
	// probes from directly connected subnets are answered.
	AllowAnySource bool
	Log            *slog.Logger
}

// Announce broadcasts b every Interval and answers probes with a unicast
// beacon until ctx is done. A failure to bind the probe port is logged and
// the hive keeps broadcasting.
func Announce(ctx context.Context, b proto.Beacon, o AnnounceOptions) error {
	if o.Port == 0 {
		o.Port = proto.DiscoveryPort
	}
	if o.Interval <= 0 {
		o.Interval = 5 * time.Second
	}
	log := o.Log
	if log == nil {
		log = slog.Default()
	}
	b.Svc = proto.BeaconService
	b.V = proto.APIVersion
	payload, err := json.Marshal(b)
	if err != nil {
		return err
	}
	if len(payload) > MaxDatagram {
		return fmt.Errorf("beacon too large (%d bytes)", len(payload))
	}

	// Sending socket: an unbound UDP socket with SO_BROADCAST.
	send, err := listenBroadcast(":0")
	if err != nil {
		return fmt.Errorf("beacon socket: %w", err)
	}
	defer send.Close()

	// Probe listener (shares nothing with the sender so either can fail).
	listenAddr := o.ListenAddr
	if listenAddr == "" {
		listenAddr = ":" + strconv.Itoa(o.Port)
	}
	if probe, err := listenBroadcast(listenAddr); err != nil {
		log.Warn("discovery: cannot listen for probes; broadcasting only", "addr", listenAddr, "err", err)
	} else {
		defer probe.Close()
		go func() {
			<-ctx.Done()
			probe.Close()
		}()
		go answerProbes(probe, payload, log, o.AllowAnySource)
	}

	t := time.NewTicker(o.Interval)
	defer t.Stop()
	for {
		targets := o.Targets
		if len(targets) == 0 {
			targets = BroadcastTargets(o.Port)
		}
		for _, dst := range targets {
			addr, err := net.ResolveUDPAddr("udp4", dst)
			if err != nil {
				continue
			}
			if _, err := send.WriteTo(payload, addr); err != nil {
				log.Debug("discovery: beacon send failed", "dst", dst, "err", err)
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

func answerProbes(conn net.PacketConn, payload []byte, log *slog.Logger, allowAny bool) {
	buf := make([]byte, MaxDatagram+1)
	last := map[string]time.Time{}
	for {
		n, from, err := conn.ReadFrom(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}
		// Probes are padded to MinProbeSize so the reply is never larger
		// than the request (no amplification).
		if n > MaxDatagram || n < proto.MinProbeSize {
			continue
		}
		var p proto.Probe
		if json.Unmarshal(buf[:n], &p) != nil || p.Svc != proto.ProbeService {
			continue
		}
		udp, ok := from.(*net.UDPAddr)
		if !ok || (!allowAny && !onLocalSubnet(udp.IP)) {
			continue
		}
		key := udp.IP.String()
		now := time.Now()
		if t, ok := last[key]; ok && now.Sub(t) < time.Second {
			continue
		}
		if len(last) > 4096 {
			last = map[string]time.Time{}
		}
		last[key] = now
		if _, err := conn.WriteTo(payload, from); err != nil {
			log.Debug("discovery: probe reply failed", "to", from, "err", err)
		}
	}
}

// onLocalSubnet reports whether ip is inside a subnet of one of our up
// interfaces (probes from elsewhere are ignored).
func onLocalSubnet(ip net.IP) bool {
	if ip.IsLoopback() {
		return true
	}
	ifaces, err := net.Interfaces()
	if err != nil {
		return false
	}
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && ipn.Contains(ip) {
				return true
			}
		}
	}
	return false
}

// NewProbe returns a padded probe datagram.
func NewProbe() []byte {
	p := proto.Probe{Svc: proto.ProbeService, V: proto.APIVersion}
	b, _ := json.Marshal(p)
	p.Pad = strings.Repeat("0", proto.MinProbeSize-len(b))
	b, _ = json.Marshal(p)
	return b
}

// DiscoverOptions configures the node side.
type DiscoverOptions struct {
	Port          int           // hive discovery port (default proto.DiscoveryPort)
	ProbeInterval time.Duration // default 5 s
	Targets       []string      // probe destinations; empty = broadcast targets
	// ListenAddr is where beacons are received (default ":<Port>"). Probe
	// replies arrive on the same socket because probes are sent from it.
	ListenAddr string
}

// Candidate is a hive seen on the network.
type Candidate struct {
	URL    string // https://ip:port
	Beacon proto.Beacon
}

// Discover waits for the first beacon whose swarm hint matches and returns
// the hive base URL and the beacon.
func Discover(ctx context.Context, swarmHint string, o DiscoverOptions) (string, proto.Beacon, error) {
	var first Candidate
	err := Listen(ctx, o, func(c Candidate) bool {
		if c.Beacon.SwarmHint != swarmHint {
			return true
		}
		first = c
		return false
	})
	if first.URL != "" {
		return first.URL, first.Beacon, nil
	}
	return "", proto.Beacon{}, err
}

// Collect gathers every distinct hive (by URL) seen within window. Beacons
// of any swarm are returned so callers can tell "wrong key" from "no hive".
func Collect(ctx context.Context, window time.Duration, o DiscoverOptions) ([]Candidate, error) {
	ctx, cancel := context.WithTimeout(ctx, window)
	defer cancel()
	var mu sync.Mutex
	var out []Candidate
	seen := map[string]bool{}
	err := Listen(ctx, o, func(c Candidate) bool {
		mu.Lock()
		defer mu.Unlock()
		if !seen[c.URL+"|"+c.Beacon.HiveID] {
			seen[c.URL+"|"+c.Beacon.HiveID] = true
			out = append(out, c)
		}
		return true
	})
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		err = nil
	}
	return out, err
}

// Listen sends probes and calls fn for every valid beacon until fn returns
// false or ctx is done.
func Listen(ctx context.Context, o DiscoverOptions, fn func(Candidate) bool) error {
	if o.Port == 0 {
		o.Port = proto.DiscoveryPort
	}
	if o.ProbeInterval <= 0 {
		o.ProbeInterval = 5 * time.Second
	}
	listenAddr := o.ListenAddr
	if listenAddr == "" {
		listenAddr = ":" + strconv.Itoa(o.Port)
	}
	conn, err := listenBroadcast(listenAddr)
	if err != nil {
		// Port busy (e.g. the hive runs on this machine): fall back to an
		// ephemeral port; we will still get unicast probe replies.
		conn, err = listenBroadcast(":0")
		if err != nil {
			return err
		}
	}
	defer conn.Close()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		<-ctx.Done()
		conn.Close()
	}()

	probe := NewProbe()
	sendProbes := func() {
		targets := o.Targets
		if len(targets) == 0 {
			targets = BroadcastTargets(o.Port)
		}
		for _, dst := range targets {
			if addr, err := net.ResolveUDPAddr("udp4", dst); err == nil {
				conn.WriteTo(probe, addr)
			}
		}
	}
	sendProbes()
	go func() {
		t := time.NewTicker(o.ProbeInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				sendProbes()
			}
		}
	}()

	buf := make([]byte, MaxDatagram+1)
	for {
		n, from, err := conn.ReadFrom(buf)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if errors.Is(err, net.ErrClosed) {
				return err
			}
			continue
		}
		b, ok := ParseBeacon(buf[:n])
		if !ok {
			continue
		}
		udp, ok := from.(*net.UDPAddr)
		if !ok {
			continue
		}
		c := Candidate{URL: "https://" + net.JoinHostPort(udp.IP.String(), strconv.Itoa(b.Port)), Beacon: b}
		if !fn(c) {
			return nil
		}
	}
}

// ParseBeacon decodes and sanity-checks a beacon datagram.
func ParseBeacon(data []byte) (proto.Beacon, bool) {
	var b proto.Beacon
	if len(data) > MaxDatagram || json.Unmarshal(data, &b) != nil {
		return b, false
	}
	if b.Svc != proto.BeaconService || b.V != proto.APIVersion || b.Port < 1 || b.Port > 65535 {
		return b, false
	}
	return b, true
}

// BroadcastTargets returns 255.255.255.255:port plus the directed broadcast
// address of every up, non-loopback IPv4 interface.
func BroadcastTargets(port int) []string {
	ps := strconv.Itoa(port)
	out := []string{net.JoinHostPort("255.255.255.255", ps)}
	seen := map[string]bool{out[0]: true}
	ifaces, err := net.Interfaces()
	if err != nil {
		return out
	}
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip4 := ipn.IP.To4()
			if ip4 == nil || len(ipn.Mask) != net.IPv4len {
				continue
			}
			bc := make(net.IP, 4)
			for i := range bc {
				bc[i] = ip4[i] | ^ipn.Mask[i]
			}
			t := net.JoinHostPort(bc.String(), ps)
			if !seen[t] {
				seen[t] = true
				out = append(out, t)
			}
		}
	}
	return out
}
