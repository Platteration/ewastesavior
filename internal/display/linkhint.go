package display

import (
	"net"
	"strconv"
	"strings"

	"github.com/platteration/ewastesavior/internal/proto"
)

// LinkHint returns a one-line fix for a hive link state (DESIGN 10.6), for
// the status scene and the text console. hiveAddr is "host:port" (or a
// URL, or just a host) when known; hiveErr is the last error. It returns ""
// when nothing needs fixing.
func LinkHint(link proto.HiveLink, hiveAddr, hiveErr string) string {
	host, port := splitHiveAddr(hiveAddr)
	at := ""
	if host != "" {
		at = " at " + host
	}
	switch link {
	case proto.LinkConnected:
		return ""
	case proto.LinkNoNetwork:
		return "No network. Plug in a network cable, or set wifi_ssid and wifi_psk in savior.conf."
	case proto.LinkNoSwarmKey:
		return "Put swarm_key = ... in savior.conf on the stick."
	case proto.LinkSearching:
		return "Looking for a hive on this network. Start one with 'savior hive', or set hive = <address> in savior.conf."
	case proto.LinkKeyMismatch:
		return "A hive is on the network but its swarm key differs. Check swarm_key in savior.conf."
	case proto.LinkUnreachable:
		if host == "" {
			return "The hive can't be reached. Check the network and the hive computer's firewall."
		}
		return "Hive found at " + host + " but port " + port + " is blocked. Allow savior through that computer's firewall."
	case proto.LinkFingerprintMismatch:
		return "The hive" + at + " has a different certificate than hive_fingerprint in savior.conf. Fix the pin, or check for an impostor."
	case proto.LinkVersionMismatch:
		return "The hive" + at + " speaks a different protocol version. Update this stick or the hive to the same SaviorOS release."
	case proto.LinkRateLimited:
		return "The hive is refusing this node for a minute after failed attempts. It will retry automatically."
	case proto.LinkRejected:
		if strings.Contains(hiveErr, "node_id") {
			return "The hive" + at + " refused this node's ID. Fix node_id in savior.conf (lowercase letters, digits and dashes), or remove it."
		}
		return "The hive rejected this node. Check swarm_key in savior.conf, or approve the node on the hive."
	case proto.LinkPending:
		return "Waiting for approval. Approve this node in the hive dashboard."
	case proto.LinkDuplicate:
		return "Another running node has the same node ID. Set a different node_id in savior.conf on one of them."
	}
	if hiveErr != "" {
		return proto.Sanitize(hiveErr, 160, false)
	}
	return ""
}

// splitHiveAddr extracts host and port from "host:port", "[v6]:port", a
// URL or a bare host; the port defaults to proto.DefaultPort.
func splitHiveAddr(a string) (host, port string) {
	a = strings.TrimSpace(a)
	if a == "" {
		return "", ""
	}
	if i := strings.Index(a, "://"); i >= 0 {
		a = a[i+3:]
	}
	if i := strings.IndexByte(a, '/'); i >= 0 {
		a = a[:i]
	}
	if h, p, err := net.SplitHostPort(a); err == nil {
		return h, p
	}
	return strings.Trim(a, "[]"), strconv.Itoa(proto.DefaultPort)
}

// linkLabel is the short human name of a link state.
func linkLabel(l proto.HiveLink) string {
	switch l {
	case proto.LinkConnected:
		return "Connected"
	case proto.LinkNoNetwork:
		return "No network"
	case proto.LinkNoSwarmKey:
		return "No swarm key"
	case proto.LinkSearching:
		return "Searching for hive"
	case proto.LinkKeyMismatch:
		return "Swarm key mismatch"
	case proto.LinkUnreachable:
		return "Hive unreachable"
	case proto.LinkFingerprintMismatch:
		return "Hive fingerprint mismatch"
	case proto.LinkVersionMismatch:
		return "Version mismatch"
	case proto.LinkRateLimited:
		return "Rate limited"
	case proto.LinkRejected:
		return "Rejected by hive"
	case proto.LinkPending:
		return "Waiting for approval"
	case proto.LinkDuplicate:
		return "Duplicate node ID"
	case "":
		return "Starting"
	}
	return proto.Sanitize(string(l), 40, false)
}
