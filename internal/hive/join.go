package hive

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/platteration/ewastesavior/internal/auth"
	"github.com/platteration/ewastesavior/internal/proto"
)

func (s *Server) handleHello(w http.ResponseWriter, r *http.Request) {
	if !s.limits.allowHello(sourceKey(r.RemoteAddr)) {
		writeErr(w, http.StatusTooManyRequests, "slow down")
		return
	}
	writeJSON(w, http.StatusOK, proto.Hello{
		HiveID:     s.hiveID,
		APIVersion: proto.APIVersion,
		Version:    versionString(),
		Nonce:      s.nonces.Issue(time.Now()),
		Time:       s.now(),
	})
}

// consumeNonce checks a hive nonce's MAC and age and marks it used. The
// used set only needs entries for the nonce TTL, so it stays bounded.
func (s *Server) consumeNonce(nonce string) bool {
	now := time.Now()
	ttl := s.cfg.tune.nonceTTL
	if !s.nonces.Check(nonce, now, ttl) {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, used := s.usedNonces[nonce]; used {
		return false
	}
	if len(s.usedNonces) >= maxUsedNonces {
		s.purgeNoncesLocked(now)
		if len(s.usedNonces) >= maxUsedNonces {
			return false
		}
	}
	s.usedNonces[nonce] = now.Add(ttl + 10*time.Second)
	return true
}

func (s *Server) purgeNoncesLocked(now time.Time) {
	for k, exp := range s.usedNonces {
		if now.After(exp) {
			delete(s.usedNonces, k)
		}
	}
}

func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	src := sourceKey(r.RemoteAddr)
	if s.limits.blocked(bucketRegister, src) {
		writeErr(w, http.StatusTooManyRequests, "too many failed join attempts; try again in a minute")
		return
	}
	var req proto.RegisterRequest
	if !decodeJSON(w, r, &req, false) {
		return
	}
	if v := r.Header.Get(APIVersionHeader); v != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err != nil || n != proto.APIVersion {
			writeErr(w, http.StatusUpgradeRequired, "API version mismatch: this hive speaks v%d", proto.APIVersion)
			return
		}
	}
	// 0 = a client that predates the field; treat it as v1.
	if req.APIVersion != 0 && req.APIVersion != proto.APIVersion {
		writeErr(w, http.StatusUpgradeRequired, "API version mismatch: this hive speaks v%d", proto.APIVersion)
		return
	}
	if !proto.ValidNodeID(req.NodeID) {
		writeErr(w, http.StatusBadRequest, "invalid node_id")
		return
	}
	if req.NodeNonce == "" || len(req.NodeNonce) > 128 || len(req.Proof) > 128 {
		writeErr(w, http.StatusBadRequest, "invalid node_nonce or proof")
		return
	}
	if !s.consumeNonce(req.HiveNonce) {
		s.limits.fail(bucketRegister, src)
		writeErr(w, http.StatusForbidden, "invalid, expired or already used hive nonce")
		return
	}
	keyless := req.Proof == ""
	var hiveProof string
	if !keyless {
		// Recompute with our own fingerprint: a proof made for any other
		// certificate (a man in the middle) never verifies.
		want := s.swarm.NodeProof(req.HiveNonce, req.NodeNonce, req.NodeID, s.fp)
		if !auth.VerifyProof(want, req.Proof) {
			s.limits.fail(bucketRegister, src)
			s.log.Warn("join rejected: bad proof", "node_id", req.NodeID, "addr", remoteIP(r))
			writeErr(w, http.StatusForbidden, "join rejected: wrong swarm key or certificate mismatch")
			return
		}
		hiveProof = s.swarm.HiveProof(req.HiveNonce, req.NodeNonce, req.NodeID, s.fp)
	}
	resp, status, msg := s.register(req, keyless, remoteIP(r))
	if status != http.StatusOK {
		writeErr(w, status, "%s", msg)
		return
	}
	resp.HiveProof = hiveProof
	writeJSON(w, http.StatusOK, resp)
}

// register creates or updates the node record and issues a token.
func (s *Server) register(req proto.RegisterRequest, keyless bool, addr string) (proto.RegisterResponse, int, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	wall := s.now()
	hwids := sanitizeList(req.HWIDs, maxHWIDs, 128)
	bootID := proto.Sanitize(req.BootID, 256, false)

	n := s.nodes[req.NodeID]
	if n != nil && n.isOnline(now, s.cfg.OfflineAfter) && n.BootID != bootID {
		return proto.RegisterResponse{}, http.StatusConflict, "duplicate node id: another online machine uses " + req.NodeID
	}
	var takeover *node
	if n == nil {
		if len(s.nodes) >= maxNodes {
			return proto.RegisterResponse{}, http.StatusServiceUnavailable, "too many nodes"
		}
		takeover = s.takeoverCandidateLocked(hwids, now)
		if keyless && takeover == nil && s.pendingCountLocked() >= maxPendingNodes {
			return proto.RegisterResponse{}, http.StatusServiceUnavailable, "too many nodes waiting for approval"
		}
	}

	// Approval (DESIGN 6.2, 5.3 join_policy).
	prior := n
	if prior == nil {
		prior = takeover
	}
	var approved bool
	switch {
	case keyless:
		approved = s.hwidApprovedLocked(hwids) && (prior == nil || !prior.Denied)
	case s.cfg.JoinPolicy == "open":
		approved = prior == nil || !prior.Denied
	default: // approve
		approved = prior != nil && prior.Approved && !prior.Denied
	}

	if n == nil {
		if takeover != nil {
			n = s.takeOverLocked(takeover, req.NodeID)
		} else {
			n = newNode(nodeRecord{ID: req.NodeID, FirstSeen: wall})
		}
		s.nodes[n.ID] = n
	}

	var warnings []string
	n.HWIDs = hwids
	n.BootID = bootID
	n.Version = proto.Sanitize(req.Version, 64, false)
	n.Inventory = sanitizeInventory(req.Inventory)
	n.Roles = sanitizeRoles(req.Roles)
	n.ScratchInRAM = req.ScratchInRAM
	n.Sandbox = proto.Sanitize(req.Sandbox, 16, false)
	n.SandboxCaps = sanitizeList(req.SandboxCaps, 32, 32)
	n.NodeRotate = validRotate(req.DisplayRotate)
	n.Total = clampTotal(req.Total, n.Inventory)
	n.Addr = addr
	n.LastSeen = wall
	n.Keyless = keyless
	n.Approved = approved
	labels, lw := sanitizeLabels(req.Labels)
	n.ConfigLabels = labels
	warnings = append(warnings, lw...)
	reqName := strings.ToLower(proto.Sanitize(req.Name, 64, false))
	keepName := takeover != nil && reqName == "" // a takeover keeps the record's name
	if !keepName {
		n.RequestedName = reqName
	}
	if !n.AdminName && !keepName {
		name, warn := s.pickNameLocked(n.RequestedName, n.ID)
		n.Name = name
		if warn != "" {
			warnings = append(warnings, warn)
		}
	}
	if n.Sandbox == "none" || !n.fullIsolation() {
		warnings = append(warnings, "sandbox without full isolation: only jobs with isolation=any run here")
	}
	n.Warnings = warnings

	token := auth.NewToken()
	if n.tokenHash != "" {
		delete(s.tokens, n.tokenHash)
	}
	n.tokenHash = auth.HashToken(token)
	s.tokens[n.tokenHash] = n.ID
	n.lastHB = now
	n.online = true
	n.claimID, n.claimTasks = "", nil
	n.status = proto.NodeStatus{State: proto.NodeIdle}

	adopted := s.adoptLocked(n, req.RunningTasks, now)
	s.dirty = true
	s.notifyLocked()
	s.log.Info("node registered", "node_id", n.ID, "name", n.Name, "addr", addr, "keyless", keyless,
		"approved", n.Approved, "adopted", len(adopted))

	return proto.RegisterResponse{
		NodeID:             n.ID,
		Name:               n.Name,
		ShortCode:          proto.ShortCode(n.ID),
		Token:              token,
		HeartbeatIntervalS: heartbeatSeconds(s.cfg.HeartbeatInterval),
		Time:               wall,
		TimeSynced:         s.timeSynced,
		Pending:            !n.Approved,
		Directives:         s.directivesLocked(n, nil, now),
		AdoptedTasks:       adopted,
	}, http.StatusOK, ""
}

func heartbeatSeconds(d time.Duration) int {
	sec := int((d + time.Second/2) / time.Second)
	if sec < 1 {
		sec = 1
	}
	return sec
}

// adoptLocked re-adopts the tasks a (re-)registering node still runs whose
// lease matches (DESIGN 8.6). Its other assignments are requeued as lost;
// the node kills everything not in the returned list.
func (s *Server) adoptLocked(n *node, running []proto.RunningTask, now time.Time) []string {
	listed := map[string]proto.RunningTask{}
	for i, rt := range running {
		if i >= maxRunningListed {
			break
		}
		listed[rt.ID] = rt
	}
	adopted := map[string]bool{}
	if n.Approved {
		for id, rt := range listed {
			t := s.tasks[id]
			if t == nil || !t.liveFor(n.ID, rt.Lease) {
				continue
			}
			adopted[id] = true
			t.unconfirmed = false
			t.seen = true
			s.noteProgressLocked(t, rt, now)
			if _, ok := n.held[id]; !ok {
				n.held[id] = heldTask{lease: t.Lease, charge: charge(t.job.Spec.Resources, n.ScratchInRAM)}
			}
		}
	}
	for id, h := range n.held {
		if adopted[id] {
			continue
		}
		t := s.tasks[id]
		switch {
		case t != nil && t.liveFor(n.ID, h.lease):
			s.requeueLocked(t, requeueOpts{outcome: "lost", err: "node re-registered without this task", interrupt: t.seen})
		case t != nil && t.CancelRequested && t.active() && t.Lease == h.lease:
			s.settleCanceledLocked(t)
		default:
			s.releaseLocked(n, id)
		}
	}
	out := make([]string, 0, len(adopted))
	for id := range adopted {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// takeoverCandidateLocked finds the most recently seen offline record
// sharing a HWID (DESIGN 9: a new ID takes over name, labels, display and
// wall cell).
func (s *Server) takeoverCandidateLocked(hwids []string, now time.Time) *node {
	if len(hwids) == 0 {
		return nil
	}
	var best *node
	for _, n := range s.nodes {
		if n.isOnline(now, s.cfg.OfflineAfter) || !intersects(n.HWIDs, hwids) {
			continue
		}
		if best == nil || n.LastSeen.After(best.LastSeen) {
			best = n
		}
	}
	return best
}

// takeOverLocked moves an old record's admin-managed settings to newID and
// removes the old record.
func (s *Server) takeOverLocked(old *node, newID string) *node {
	rec := nodeRecord{
		ID:            newID,
		Name:          old.Name,
		AdminName:     old.AdminName,
		RequestedName: old.RequestedName,
		AdminLabels:   old.AdminLabels,
		Display:       old.Display,
		AdminRotate:   old.AdminRotate,
		Drain:         old.Drain,
		Denied:        old.Denied,
		Quarantine:    old.Quarantine,
		WallID:        old.WallID,
		FirstSeen:     old.FirstSeen,
	}
	n := newNode(rec)
	if w := s.walls[old.WallID]; w != nil {
		nw := *w
		nw.Cells = append([]proto.WallCell(nil), w.Cells...)
		for i := range nw.Cells {
			if nw.Cells[i].Node == old.ID {
				nw.Cells[i].Node = newID
			}
		}
		s.walls[nw.ID] = &nw
	}
	if s.reservation != nil && s.reservation.nodeID == old.ID {
		s.clearReservationLocked()
	}
	old.WallID = "" // the wall now belongs to the new record
	s.removeNodeLocked(old, "taken over by "+newID)
	s.log.Info("node record taken over by hardware ID match", "old", old.ID, "new", newID, "name", rec.Name)
	return n
}

// removeNodeLocked deletes a node record, requeueing its live tasks.
func (s *Server) removeNodeLocked(n *node, why string) {
	for id, h := range n.held {
		if t := s.tasks[id]; t != nil && t.liveFor(n.ID, h.lease) {
			s.requeueLocked(t, requeueOpts{outcome: "lost", err: "node removed: " + why, interrupt: t.seen})
		} else if t != nil && t.CancelRequested && t.active() && t.Node == n.ID {
			s.settleCanceledLocked(t)
		}
	}
	if n.tokenHash != "" {
		delete(s.tokens, n.tokenHash)
	}
	if s.reservation != nil && s.reservation.nodeID == n.ID {
		s.clearReservationLocked()
	}
	delete(s.nodes, n.ID)
	s.dirty = true
}

func (s *Server) hwidApprovedLocked(hwids []string) bool {
	if len(hwids) == 0 {
		return false
	}
	for _, n := range s.nodes {
		if n.Approved && intersects(n.HWIDs, hwids) {
			return true
		}
	}
	return false
}

func (s *Server) pendingCountLocked() int {
	c := 0
	for _, n := range s.nodes {
		if !n.Approved {
			c++
		}
	}
	return c
}

func intersects(a, b []string) bool {
	for _, x := range a {
		if x != "" && containsStr(b, x) {
			return true
		}
	}
	return false
}

// pickNameLocked returns the requested name when valid and free, else the
// default name with a warning (DESIGN 5.3 name).
func (s *Server) pickNameLocked(requested, id string) (string, string) {
	if requested != "" {
		if proto.ValidNodeName(requested) && !s.nameTakenLocked(requested, id) {
			return requested, ""
		}
		def := s.defaultNameLocked(id)
		return def, fmt.Sprintf("requested name %q is invalid or already taken; using %s", requested, def)
	}
	return s.defaultNameLocked(id), ""
}

// defaultNameLocked is savior-<last 6 hex of the node identity>, made
// unique with a numeric suffix if needed.
func (s *Server) defaultNameLocked(id string) string {
	suffix := ""
	if len(id) >= 6 && isHex(id[len(id)-6:]) {
		suffix = id[len(id)-6:]
	} else {
		h := sha256.Sum256([]byte(id))
		suffix = hex.EncodeToString(h[:3])
	}
	base := "savior-" + suffix
	name := base
	for i := 2; s.nameTakenLocked(name, id); i++ {
		name = fmt.Sprintf("%s-%d", base, i)
	}
	return name
}

// nameTakenLocked reports whether another node uses name as its name or ID.
func (s *Server) nameTakenLocked(name, selfID string) bool {
	for _, n := range s.nodes {
		if n.ID != selfID && (n.Name == name || n.ID == name) {
			return true
		}
	}
	return false
}

func isHex(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func (s *Server) handleHeartbeat(w http.ResponseWriter, r *http.Request, tok string) {
	var req proto.HeartbeatRequest
	if !decodeJSON(w, r, &req, false) {
		return
	}
	resp, ok := s.heartbeat(tok, sanitizeStatus(req.Status), remoteIP(r))
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unknown node token; register again")
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// heartbeat records a node's status and returns its directives.
func (s *Server) heartbeat(tok string, st proto.NodeStatus, addr string) (proto.HeartbeatResponse, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := s.nodeByTokenLocked(tok)
	if n == nil {
		return proto.HeartbeatResponse{}, false
	}
	now := time.Now()
	if !n.online {
		s.log.Info("node online", "node_id", n.ID, "name", n.Name)
		s.notifyLocked()
	}
	n.lastHB = now
	n.online = true
	n.LastSeen = s.now()
	n.Addr = addr
	n.status = st
	n.Metrics = st.Metrics
	if st.Total != (proto.Resources{}) {
		if t := clampTotal(st.Total, n.Inventory); t != n.Total {
			n.Total = t
			s.notifyLocked()
		}
	}
	s.liveDirty = true
	if len(st.AckedActions) > 0 && len(n.actions) > 0 {
		kept := n.actions[:0:0]
		for _, a := range n.actions {
			if !containsStr(st.AckedActions, a.ID) {
				kept = append(kept, a)
			}
		}
		n.actions = kept
	}
	cancel := s.reconcileLocked(n, st.RunningTasks, now)
	return proto.HeartbeatResponse{Time: s.now(), TimeSynced: s.timeSynced, Directives: s.directivesLocked(n, cancel, now)}, true
}

// reconcileLocked compares a node's running_tasks with its assignments
// (DESIGN 8.4, level-triggered) and returns the tasks it must cancel.
func (s *Server) reconcileLocked(n *node, running []proto.RunningTask, now time.Time) []proto.TaskRef {
	listed := make(map[string]proto.RunningTask, len(running))
	for _, rt := range running {
		listed[rt.ID] = rt
	}
	var cancel []proto.TaskRef
	released := false
	for id, rt := range listed {
		t := s.tasks[id]
		if t != nil && t.liveFor(n.ID, rt.Lease) {
			t.seen = true
			t.unconfirmed = false
			s.noteProgressLocked(t, rt, now)
			if limit := float64(t.job.Spec.TimeoutS + 60); rt.RunS > limit {
				s.requeueLocked(t, requeueOpts{outcome: "timeout", errKind: proto.ErrTimeout, consume: true, keepHeld: true,
					err: fmt.Sprintf("hive deadline: ran %.0f s, timeout_s is %d", rt.RunS, t.job.Spec.TimeoutS)})
				cancel = append(cancel, proto.TaskRef{ID: id, Lease: rt.Lease})
			}
			continue
		}
		cancel = append(cancel, proto.TaskRef{ID: id, Lease: rt.Lease})
	}
	for id, h := range n.held {
		if rt, ok := listed[id]; ok && rt.Lease == h.lease {
			continue
		}
		t := s.tasks[id]
		switch {
		case t != nil && t.liveFor(n.ID, h.lease):
			if !t.unconfirmed && now.Sub(t.assignedMono) >= s.cfg.MissingAfter {
				s.requeueLocked(t, requeueOpts{outcome: "lost", err: "task missing from the node's running tasks", interrupt: t.seen})
				released = true
			}
		case t != nil && t.CancelRequested && t.active() && t.Lease == h.lease:
			s.settleCanceledLocked(t)
			released = true
		default:
			s.releaseLocked(n, id)
			released = true
		}
	}
	if released {
		s.notifyLocked()
	}
	sort.Slice(cancel, func(i, k int) bool { return cancel[i].ID < cancel[k].ID })
	return cancel
}

// noteProgressLocked records phase, run time and transfer progress.
func (s *Server) noteProgressLocked(t *task, rt proto.RunningTask, now time.Time) {
	if rt.XferBytes != t.xfer || rt.Phase != t.phase {
		t.xferMono = now
	}
	t.xfer, t.phase = rt.XferBytes, rt.Phase
	if rt.RunS > t.runS {
		t.runS = rt.RunS
	}
	switch rt.Phase {
	case proto.PhaseRunning, proto.PhaseFrozen, proto.PhaseUploading, proto.PhaseReporting:
		s.markRunningLocked(t)
	}
}

// markRunningLocked moves an assigned task to running.
func (s *Server) markRunningLocked(t *task) {
	if t.State != proto.TaskAssigned || t.CancelRequested {
		return
	}
	t.job.adjust(t.bucket(), -1)
	t.State = proto.TaskRunning
	t.StartedAt = timePtr(s.now())
	t.job.adjust(t.bucket(), 1)
	s.dirty = true
}

// directivesLocked builds the desired state for a node.
func (s *Server) directivesLocked(n *node, cancel []proto.TaskRef, now time.Time) proto.Directives {
	d := proto.Directives{
		Name:          n.Name,
		Labels:        n.effectiveLabels(),
		Drain:         n.Drain,
		Pending:       !n.Approved,
		CancelTasks:   cancel,
		DisplayRotate: n.effectiveRotate(),
	}
	if n.Approved {
		d.Display = n.Display
	}
	for _, a := range n.actions {
		if now.Sub(a.created) < s.cfg.tune.actionExpiry {
			d.Actions = append(d.Actions, a.ActionDirective)
		}
	}
	return d
}

// --- input hygiene (DESIGN 6.3) ---

func sanitizeList(in []string, maxItems, maxLen int) []string {
	var out []string
	for _, v := range in {
		if len(out) >= maxItems {
			break
		}
		if v = proto.Sanitize(v, maxLen, false); v != "" && !containsStr(out, v) {
			out = append(out, v)
		}
	}
	return out
}

func sanitizeRoles(in []proto.Role) []proto.Role {
	var out []proto.Role
	for _, r := range []proto.Role{proto.RoleCompute, proto.RoleDisplay, proto.RoleHive} {
		if proto.HasRole(in, r) {
			out = append(out, r)
		}
	}
	return out
}

// sanitizeLabels keeps at most 32 valid config labels (sorted by key).
func sanitizeLabels(in map[string]string) (map[string]string, []string) {
	var warnings []string
	out := map[string]string{}
	for _, k := range sortedKeys(in) {
		v := in[k]
		if !proto.ValidLabelKey(k) || len(v) > 128 {
			warnings = append(warnings, fmt.Sprintf("ignored invalid label %q", proto.Sanitize(k, 64, false)))
			continue
		}
		if len(out) >= proto.MaxLabels {
			warnings = append(warnings, fmt.Sprintf("more than %d labels; the rest are ignored", proto.MaxLabels))
			break
		}
		out[k] = proto.Sanitize(v, 128, false)
	}
	if len(out) == 0 {
		out = nil
	}
	return out, warnings
}

func validRotate(r int) int {
	switch r {
	case 0, 90, 180, 270:
		return r
	}
	return 0
}

// clampTotal limits advertised resources to the inventory (DESIGN 6.4).
func clampTotal(t proto.Resources, inv proto.Inventory) proto.Resources {
	if t.Cores < 0 || t.Cores != t.Cores {
		t.Cores = 0
	}
	if t.Cores > float64(inv.Cores) {
		t.Cores = float64(inv.Cores)
	}
	if t.Cores > proto.MaxCores {
		t.Cores = proto.MaxCores
	}
	if t.MemMB < 0 {
		t.MemMB = 0
	}
	if t.MemMB > inv.MemTotalMB {
		t.MemMB = inv.MemTotalMB
	}
	if t.DiskMB < 0 {
		t.DiskMB = 0
	}
	if t.DiskMB > proto.MaxDiskMB {
		t.DiskMB = proto.MaxDiskMB
	}
	return t
}

func nonNegative(r proto.Resources) proto.Resources {
	if r.Cores < 0 || r.Cores != r.Cores {
		r.Cores = 0
	}
	if r.MemMB < 0 {
		r.MemMB = 0
	}
	if r.DiskMB < 0 {
		r.DiskMB = 0
	}
	return r
}

func sanitizeInventory(in proto.Inventory) proto.Inventory {
	const m = 256
	sz := func(s string) string { return proto.Sanitize(s, m, false) }
	out := in
	out.Hostname, out.Arch, out.MachineArch, out.Kernel = sz(in.Hostname), sz(in.Arch), sz(in.MachineArch), sz(in.Kernel)
	out.OSVersion, out.CPUModel, out.CPUVendor = sz(in.OSVersion), sz(in.CPUModel), sz(in.CPUVendor)
	out.Vendor, out.Product, out.BIOSDate, out.TempSensor = sz(in.Vendor), sz(in.Product), sz(in.BIOSDate), sz(in.TempSensor)
	out.CPUFlags = sanitizeList(in.CPUFlags, 64, 32)
	if out.Cores < 0 {
		out.Cores = 0
	}
	if out.MemTotalMB < 0 {
		out.MemTotalMB = 0
	}
	out.Disks = nil
	for i, d := range in.Disks {
		if i >= 32 {
			break
		}
		d.Name, d.Model, d.Transport = sz(d.Name), sz(d.Model), sz(d.Transport)
		out.Disks = append(out.Disks, d)
	}
	out.NICs = nil
	for i, c := range in.NICs {
		if i >= 32 {
			break
		}
		c.Name, c.MAC, c.Bus, c.Driver, c.Error = sz(c.Name), sz(c.MAC), sz(c.Bus), sz(c.Driver), sz(c.Error)
		c.Addrs = sanitizeList(c.Addrs, 16, 64)
		out.NICs = append(out.NICs, c)
	}
	out.Framebuffers = nil
	for i, f := range in.Framebuffers {
		if i >= 16 {
			break
		}
		f.Name, f.Driver = sz(f.Name), sz(f.Driver)
		out.Framebuffers = append(out.Framebuffers, f)
	}
	out.GPUs = nil
	for i, g := range in.GPUs {
		if i >= 16 {
			break
		}
		g.Card, g.Driver, g.Vendor, g.Device = sz(g.Card), sz(g.Driver), sz(g.Vendor), sz(g.Device)
		out.GPUs = append(out.GPUs, g)
	}
	out.Connectors = nil
	for i, c := range in.Connectors {
		if i >= 32 {
			break
		}
		c.Name, c.Card, c.Status, c.Preferred = sz(c.Name), sz(c.Card), sz(c.Status), sz(c.Preferred)
		if c.WidthMM < 0 || c.WidthMM > 10000 {
			c.WidthMM = 0
		}
		if c.HeightMM < 0 || c.HeightMM > 10000 {
			c.HeightMM = 0
		}
		out.Connectors = append(out.Connectors, c)
	}
	return out
}

func sanitizeStatus(st proto.NodeStatus) proto.NodeStatus {
	switch st.State {
	case proto.NodeIdle, proto.NodeBusy, proto.NodePaused, proto.NodeDraining:
	default:
		st.State = proto.NodeIdle
	}
	st.Reason = proto.Sanitize(st.Reason, 256, false)
	st.Metrics.BatteryStatus = proto.Sanitize(st.Metrics.BatteryStatus, 32, false)
	st.Total = nonNegative(st.Total)
	st.Free = nonNegative(st.Free)
	if len(st.RunningTasks) > maxRunningListed {
		st.RunningTasks = st.RunningTasks[:maxRunningListed]
	}
	rts := make([]proto.RunningTask, 0, len(st.RunningTasks))
	for _, rt := range st.RunningTasks {
		rt.ID = proto.Sanitize(rt.ID, 64, false)
		rt.Lease = proto.Sanitize(rt.Lease, 64, false)
		rt.Phase = proto.Sanitize(rt.Phase, 16, false)
		if rt.RunS < 0 || rt.RunS != rt.RunS {
			rt.RunS = 0
		}
		rts = append(rts, rt)
	}
	st.RunningTasks = rts
	st.Addrs = sanitizeList(st.Addrs, 32, 64)
	st.AckedActions = sanitizeList(st.AckedActions, 64, 64)
	d := &st.Display
	sz := func(s string) string { return proto.Sanitize(s, 256, false) }
	d.Device, d.Driver, d.Format, d.Mode = sz(d.Device), sz(d.Driver), sz(d.Format), sz(d.Mode)
	d.BlankReason, d.BlankMethod, d.Warning, d.Error = sz(d.BlankReason), sz(d.BlankMethod), sz(d.Warning), sz(d.Error)
	d.MediaErrors = sanitizeList(d.MediaErrors, 16, 256)
	return st
}
