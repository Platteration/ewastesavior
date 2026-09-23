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
	Log        *slog.Logger
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
		go answerProbes(probe, payload, log)
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

func answerProbes(conn net.PacketConn, payload []byte, log *slog.Logger) {
	buf := make([]byte, MaxDatagram+1)
	for {
		n, from, err := conn.ReadFrom(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}
		if n > MaxDatagram {
			continue
		}
		var p proto.Probe
		if json.Unmarshal(buf[:n], &p) != nil || p.Svc != proto.ProbeService {
			continue
		}
		if _, err := conn.WriteTo(payload, from); err != nil {
			log.Debug("discovery: probe reply failed", "to", from, "err", err)
		}
	}
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

// Discover waits for a beacon whose swarm hint matches and returns the hive
// base URL ("https://ip:port") and the beacon. It sends probes so a hive
// answers immediately instead of at its next beacon.
func Discover(ctx context.Context, swarmHint string, o DiscoverOptions) (string, proto.Beacon, error) {
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
			return "", proto.Beacon{}, err
		}
	}
	defer conn.Close()
	go func() {
		<-ctx.Done()
		conn.Close()
	}()

	probe, _ := json.Marshal(proto.Probe{Svc: proto.ProbeService, V: proto.APIVersion})
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
				return "", proto.Beacon{}, ctx.Err()
			}
			if errors.Is(err, net.ErrClosed) {
				return "", proto.Beacon{}, err
			}
			continue
		}
		b, ok := ParseBeacon(buf[:n])
		if !ok || b.SwarmHint != swarmHint {
			continue
		}
		udp, ok := from.(*net.UDPAddr)
		if !ok {
			continue
		}
		return "https://" + net.JoinHostPort(udp.IP.String(), strconv.Itoa(b.Port)), b, nil
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
