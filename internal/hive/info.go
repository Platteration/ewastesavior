package hive

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/platteration/ewastesavior/internal/config"
	"github.com/platteration/ewastesavior/internal/proto"
)

// Info describes the hive (GET /api/v1/admin/info).
func (s *Server) Info() proto.HiveInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.infoLocked()
}

func (s *Server) infoLocked() proto.HiveInfo {
	info := proto.HiveInfo{
		HiveID:      s.hiveID,
		Version:     versionString(),
		APIVersion:  proto.APIVersion,
		Fingerprint: s.fp,
		SwarmHint:   s.hint,
		StartedAt:   s.startedAt,
		Listen:      s.listenString(),
		URLs:        s.urls(),
		DataDir:     s.data.path,
		Persistent:  s.data.persistent,
		JoinPolicy:  s.cfg.JoinPolicy,
		TimeSynced:  s.timeSynced,
		TimeSource:  s.timeSource,
	}
	for _, k := range sortedKeys(s.warnings) {
		info.Warnings = append(info.Warnings, s.warnings[k])
	}
	now := time.Now()
	for _, url := range sortedKeys(s.otherHives) {
		if now.Sub(s.otherHives[url].seen) < 10*time.Minute {
			info.Warnings = append(info.Warnings, "another hive with this swarm key at "+url)
		}
	}
	return info
}

func (s *Server) listenString() string {
	if a := s.Addr(); a != nil {
		return a.String()
	}
	return s.cfg.Listen
}

// listenPort is the TCP port the hive serves HTTPS on.
func (s *Server) listenPort() int {
	_, p, err := net.SplitHostPort(s.listenString())
	if err == nil {
		if n, err := strconv.Atoi(p); err == nil && n > 0 {
			return n
		}
	}
	return proto.DefaultPort
}

// urls lists https://<addr>:<port>/ for every usable interface address
// (or the explicit listen host).
func (s *Server) urls() []string {
	port := strconv.Itoa(s.listenPort())
	host, _, _ := net.SplitHostPort(s.listenString())
	if ip := net.ParseIP(host); host != "" && ip != nil && !ip.IsUnspecified() {
		return []string{"https://" + net.JoinHostPort(host, port) + "/"}
	}
	var out []string
	ifaces, _ := net.Interfaces()
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok || ipn.IP.IsLinkLocalUnicast() || ipn.IP.IsLoopback() {
				continue
			}
			out = append(out, "https://"+net.JoinHostPort(ipn.IP.String(), port)+"/")
		}
	}
	sort.SliceStable(out, func(i, k int) bool { return !strings.Contains(out[i], "[") && strings.Contains(out[k], "[") })
	if len(out) == 0 {
		out = []string{"https://" + net.JoinHostPort("127.0.0.1", port) + "/"}
	}
	return out
}

func (s *Server) handleInfo(w http.ResponseWriter, r *http.Request, _ adminCtx) {
	writeJSON(w, http.StatusOK, s.Info())
}

// Stats returns aggregate swarm numbers.
func (s *Server) Stats() proto.SwarmStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	st := proto.SwarmStats{
		Time:            s.now(),
		NodesByState:    map[string]int{},
		JobsByState:     map[string]int{},
		TasksByState:    map[string]int{},
		TasksCompleted:  s.completed,
		CPUSecondsTotal: s.cpuTotal,
	}
	for _, n := range s.nodes {
		if !n.isOnline(now, s.cfg.OfflineAfter) {
			st.NodesOffline++
			continue
		}
		st.NodesOnline++
		state := string(n.status.State)
		if !n.Approved {
			state = "pending"
		} else if n.Drain {
			state = string(proto.NodeDraining)
		}
		if state == "" {
			state = string(proto.NodeIdle)
		}
		st.NodesByState[state]++
		st.Cores += n.Inventory.Cores
		st.MemMB += n.Inventory.MemTotalMB
		st.BenchTotal += n.Inventory.BenchScore
		if n.hasRole(proto.RoleCompute) && n.Approved {
			st.CoresAllocatable += n.Total.Cores
			st.CoresInUse += n.allocated().Cores
		}
		if n.hasRole(proto.RoleDisplay) {
			st.Displays++
		}
	}
	for _, j := range s.jobs {
		st.JobsByState[string(j.state())]++
		c := j.counts
		for k, v := range map[proto.TaskState]int{proto.TaskPending: c.Pending, proto.TaskAssigned: c.Assigned,
			proto.TaskRunning: c.Running, proto.TaskSucceeded: c.Succeeded, proto.TaskFailed: c.Failed, proto.TaskCanceled: c.Canceled} {
			if v > 0 {
				st.TasksByState[string(k)] += v
			}
		}
	}
	return st
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request, who requester) {
	if !who.isAdmin() {
		s.mu.Lock()
		n := s.nodeByTokenLocked(who.nodeToken)
		approved := n != nil && n.Approved
		s.mu.Unlock()
		if n == nil {
			writeErr(w, http.StatusUnauthorized, "unknown node token; register again")
			return
		}
		if !approved {
			writeErr(w, http.StatusForbidden, "node is not approved")
			return
		}
	}
	writeJSON(w, http.StatusOK, s.Stats())
}

// handleNodeConfig returns a ready savior.conf for nodes: swarm key,
// fingerprint pin and hive address (DESIGN 7.2, 6).
func (s *Server) handleNodeConfig(w http.ResponseWriter, r *http.Request, _ adminCtx) {
	addr := r.URL.Query().Get("hive")
	if addr == "" {
		addr = r.Host
	}
	addr = strings.TrimSpace(addr)
	if strings.ContainsAny(addr, " \t\r\n\"#\\") || addr == "" {
		writeErr(w, http.StatusBadRequest, "invalid hive address")
		return
	}
	hostPort := "auto" // nodes find the hive by LAN broadcast; the pin still authenticates it
	if addr != "auto" {
		u, err := config.HiveURL(addr)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "invalid hive address: %s", err.Error())
			return
		}
		hostPort = strings.TrimPrefix(u, "https://")
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# savior.conf generated by SaviorOS hive %s (%s) on %s\n", s.hiveID, versionString(), s.now().UTC().Format(time.RFC3339))
	b.WriteString("# Copy this file to the root of the SAVIOR partition of each node's boot stick.\n")
	b.WriteString("# It contains the swarm key: anyone who has it can join machines to this swarm.\n")
	b.WriteString("# hive_fingerprint pins this hive's certificate so no other machine can pose as it.\n\n")
	fmt.Fprintf(&b, "swarm_key = %s\n", s.swarmKey)
	fmt.Fprintf(&b, "hive_fingerprint = %s\n", s.fp)
	fmt.Fprintf(&b, "hive = %s\n", hostPort)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="savior.conf"`)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(b.String()))
}

// StatusPanel is the hive panel of the local status screen (DESIGN 9,
// 11.3). It is also written as JSON to Config.StatusFile.
type StatusPanel struct {
	URLs        []string  `json:"urls"`
	Fingerprint string    `json:"fingerprint"`
	PairCode    string    `json:"pair_code"`
	PairExpires time.Time `json:"pair_expires"`
	NodesOnline int       `json:"nodes_online"`
	Persistent  bool      `json:"persistent"`
	Warnings    []string  `json:"warnings,omitempty"`
}

// Panel returns the current status panel data.
func (s *Server) Panel() StatusPanel {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.panelLocked(time.Now())
}

func (s *Server) panelLocked(now time.Time) StatusPanel {
	pc := s.screenPairCodeLocked()
	p := StatusPanel{
		URLs:        s.urls(),
		Fingerprint: s.fp,
		PairCode:    pc.Code,
		PairExpires: pc.ExpiresAt,
		Persistent:  s.data.persistent,
	}
	for _, n := range s.nodes {
		if n.isOnline(now, s.cfg.OfflineAfter) {
			p.NodesOnline++
		}
	}
	for _, k := range sortedKeys(s.warnings) {
		p.Warnings = append(p.Warnings, s.warnings[k])
	}
	return p
}

// writeStatusFile publishes the panel for the local console/status screen
// when it changed (0600: it holds a pairing code).
func (s *Server) writeStatusFile(now time.Time) {
	if s.statusFile == "" {
		return
	}
	s.mu.Lock()
	p := s.panelLocked(now)
	s.mu.Unlock()
	data, err := json.Marshal(p)
	if err != nil {
		return
	}
	if string(data) == s.lastStatus {
		return
	}
	if err := writeFileAtomic(s.statusFile, data, 0o600); err != nil {
		s.log.Debug("could not write the hive status file", "path", s.statusFile, "err", err)
		return
	}
	s.lastStatus = string(data)
}
