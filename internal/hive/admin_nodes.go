package hive

import (
	"net/http"
	"sort"
	"time"

	"github.com/platteration/ewastesavior/internal/auth"
	"github.com/platteration/ewastesavior/internal/proto"
)

// nodeViewLocked builds the admin view of a node.
func (s *Server) nodeViewLocked(n *node, now time.Time) proto.NodeView {
	v := proto.NodeView{
		ID:            n.ID,
		Name:          n.Name,
		ShortCode:     proto.ShortCode(n.ID),
		Roles:         n.Roles,
		Labels:        n.effectiveLabels(),
		ConfigLabels:  n.ConfigLabels,
		AdminLabels:   n.AdminLabels,
		Liveness:      proto.NodeOffline,
		Approved:      n.Approved,
		Addr:          n.Addr,
		Version:       n.Version,
		FirstSeen:     n.FirstSeen,
		LastSeen:      n.LastSeen,
		Drain:         n.Drain,
		Inventory:     n.Inventory,
		Status:        n.status,
		Display:       n.Display,
		DisplayRotate: n.effectiveRotate(),
		WallID:        n.WallID,
		ScratchInRAM:  n.ScratchInRAM,
		Sandbox:       n.Sandbox,
		SandboxCaps:   n.SandboxCaps,
		FullIsolation: n.fullIsolation(),
		Allocated:     n.allocated(),
		RunningTasks:  []string{},
		Quarantine:    n.Quarantine,
		ReservedFor:   n.reservedFor,
		BootID:        n.BootID,
		Warnings:      n.Warnings,
	}
	if n.isOnline(now, s.cfg.OfflineAfter) {
		v.Liveness = proto.NodeOnline
	} else {
		v.Status.Metrics = n.Metrics
	}
	for id, h := range n.held {
		if t := s.tasks[id]; t != nil && t.liveFor(n.ID, h.lease) {
			v.RunningTasks = append(v.RunningTasks, id)
		}
	}
	sort.Strings(v.RunningTasks)
	return v
}

func (s *Server) handleNodes(w http.ResponseWriter, r *http.Request, _ adminCtx) {
	now := time.Now()
	s.mu.Lock()
	out := make([]proto.NodeView, 0, len(s.nodes))
	for _, id := range sortedKeys(s.nodes) {
		out = append(out, s.nodeViewLocked(s.nodes[id], now))
	}
	s.mu.Unlock()
	sort.SliceStable(out, func(i, k int) bool { return out[i].Name < out[k].Name })
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleNodeGet(w http.ResponseWriter, r *http.Request, _ adminCtx) {
	s.mu.Lock()
	n := s.resolveNodeRefLocked(r.PathValue("ref"))
	if n == nil {
		s.mu.Unlock()
		writeErr(w, http.StatusNotFound, "no such node")
		return
	}
	v := s.nodeViewLocked(n, time.Now())
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, v)
}

// handleNodePatch applies a NodePatch atomically: everything is validated
// before anything changes, and the result is saved before answering.
func (s *Server) handleNodePatch(w http.ResponseWriter, r *http.Request, _ adminCtx) {
	var p proto.NodePatch
	if !decodeJSON(w, r, &p, true) {
		return
	}
	var display *proto.DisplaySpec
	if p.Display != nil {
		d := *p.Display
		if d.Mode == proto.DisplayWall || d.Wall != nil {
			writeErr(w, http.StatusBadRequest, "mode wall is set by saving a wall, not per node")
			return
		}
		if err := proto.ValidateDisplaySpec(&d); err != nil {
			writeErr(w, http.StatusBadRequest, "%s", err.Error())
			return
		}
		if err := s.fillMediaDims(&d); err != nil {
			writeErr(w, http.StatusBadRequest, "%s", err.Error())
			return
		}
		display = &d
	}
	var labels map[string]string
	if p.Labels != nil {
		in := *p.Labels
		if len(in) > proto.MaxLabels {
			writeErr(w, http.StatusBadRequest, "at most %d labels", proto.MaxLabels)
			return
		}
		labels = map[string]string{}
		for k, v := range in {
			if !proto.ValidLabelKey(k) || len(v) > 128 || proto.Sanitize(v, 128, false) != v {
				writeErr(w, http.StatusBadRequest, "invalid label %q", proto.Sanitize(k, 64, false))
				return
			}
			labels[k] = v
		}
		if len(labels) == 0 {
			labels = nil
		}
	}
	if p.DisplayRotate != nil && validRotate(*p.DisplayRotate) != *p.DisplayRotate {
		writeErr(w, http.StatusBadRequest, "display_rotate must be 0, 90, 180 or 270")
		return
	}

	s.mu.Lock()
	n := s.resolveNodeRefLocked(r.PathValue("ref"))
	if n == nil {
		s.mu.Unlock()
		writeErr(w, http.StatusNotFound, "no such node")
		return
	}
	var newName string
	if p.Name != nil {
		newName = *p.Name
		if newName != "" && (!proto.ValidNodeName(newName)) {
			s.mu.Unlock()
			writeErr(w, http.StatusBadRequest, "invalid name: use 1-32 lowercase letters, digits and inner dashes, not shaped like a node ID")
			return
		}
		if newName != "" && s.nameTakenLocked(newName, n.ID) {
			s.mu.Unlock()
			writeErr(w, http.StatusConflict, "name %s is already taken", newName)
			return
		}
	}
	if display != nil && n.WallID != "" {
		s.mu.Unlock()
		writeErr(w, http.StatusConflict, "node %s is part of wall %s; edit or delete the wall instead", n.Name, n.WallID)
		return
	}

	// Validated; apply.
	if p.Name != nil {
		if newName == "" {
			n.AdminName = false
			n.Name, _ = s.pickNameLocked(n.RequestedName, n.ID)
		} else {
			n.AdminName = true
			n.Name = newName
		}
		if n.WallID != "" {
			s.recomputeWallLocked(n.WallID) // tile labels carry the name
		}
	}
	if p.Labels != nil {
		n.AdminLabels = labels
	}
	if p.Drain != nil {
		n.Drain = *p.Drain
		if n.Drain && s.reservation != nil && s.reservation.nodeID == n.ID {
			s.clearReservationLocked()
		}
	}
	if p.Approved != nil {
		n.Approved = *p.Approved
		n.Denied = !*p.Approved
	}
	if p.ClearQuarantine {
		n.Quarantine = ""
		n.nodeErrs = nil
		n.fastFails = map[string]time.Time{}
	}
	if display != nil {
		display.Rev = s.nextRevLocked(n)
		n.Display = display
	}
	if p.DisplayRotate != nil {
		rot := *p.DisplayRotate
		n.AdminRotate = &rot
		if n.WallID != "" {
			s.recomputeWallLocked(n.WallID)
		}
	}
	s.dirty = true
	s.notifyLocked()
	v := s.nodeViewLocked(n, time.Now())
	s.mu.Unlock()
	s.persistSync()
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) handleNodeDelete(w http.ResponseWriter, r *http.Request, _ adminCtx) {
	s.mu.Lock()
	n := s.resolveNodeRefLocked(r.PathValue("ref"))
	if n == nil {
		s.mu.Unlock()
		writeErr(w, http.StatusNotFound, "no such node")
		return
	}
	if n.isOnline(time.Now(), s.cfg.OfflineAfter) {
		s.mu.Unlock()
		writeErr(w, http.StatusConflict, "node %s is online; only offline nodes can be deleted", n.Name)
		return
	}
	if n.WallID != "" {
		s.mu.Unlock()
		writeErr(w, http.StatusConflict, "node %s is part of wall %s; remove it from the wall first", n.Name, n.WallID)
		return
	}
	s.removeNodeLocked(n, "deleted by admin")
	s.mu.Unlock()
	s.persistSync()
	writeOK(w)
}

// addActionLocked queues a one-shot action, repeated in heartbeats until
// acknowledged or 5 minutes old.
func (s *Server) addActionLocked(n *node, a proto.NodeAction) {
	d := proto.ActionDirective{ID: "a" + auth.NewID(8), Action: a.Action}
	if a.Action == proto.ActionIdentify {
		d.Seconds = a.Seconds
		if d.Seconds <= 0 {
			d.Seconds = identifyDefault
		}
		if d.Seconds > identifyMax {
			d.Seconds = identifyMax
		}
	}
	n.actions = append(n.actions, &action{ActionDirective: d, created: time.Now()})
	if len(n.actions) > 16 {
		n.actions = n.actions[len(n.actions)-16:]
	}
}

func validAction(a string) bool {
	return a == proto.ActionIdentify || a == proto.ActionReboot || a == proto.ActionPoweroff
}

func (s *Server) handleNodeAction(w http.ResponseWriter, r *http.Request, _ adminCtx) {
	var a proto.NodeAction
	if !decodeJSON(w, r, &a, true) {
		return
	}
	if !validAction(a.Action) {
		writeErr(w, http.StatusBadRequest, "action must be identify, reboot or poweroff")
		return
	}
	if a.Seconds < 0 || a.Seconds > identifyMax {
		writeErr(w, http.StatusBadRequest, "seconds must be 0..%d", identifyMax)
		return
	}
	s.mu.Lock()
	n := s.resolveNodeRefLocked(r.PathValue("ref"))
	if n == nil {
		s.mu.Unlock()
		writeErr(w, http.StatusNotFound, "no such node")
		return
	}
	s.addActionLocked(n, a)
	s.mu.Unlock()
	s.log.Info("node action queued", "node", n.ID, "action", a.Action)
	writeOK(w)
}

func (s *Server) handleIdentify(w http.ResponseWriter, r *http.Request, _ adminCtx) {
	var req proto.IdentifyRequest
	if !decodeJSON(w, r, &req, true) {
		return
	}
	if req.Seconds < 0 || req.Seconds > identifyMax {
		writeErr(w, http.StatusBadRequest, "seconds must be 0..%d", identifyMax)
		return
	}
	if len(req.Nodes) > maxNodes {
		writeErr(w, http.StatusBadRequest, "too many nodes")
		return
	}
	now := time.Now()
	s.mu.Lock()
	var targets []*node
	if len(req.Nodes) == 0 {
		for _, n := range s.nodes {
			if n.isOnline(now, s.cfg.OfflineAfter) {
				targets = append(targets, n)
			}
		}
	} else {
		for _, ref := range req.Nodes {
			n := s.resolveNodeRefLocked(ref)
			if n == nil {
				s.mu.Unlock()
				writeErr(w, http.StatusNotFound, "no such node %q", proto.Sanitize(ref, 64, false))
				return
			}
			targets = append(targets, n)
		}
	}
	for _, n := range targets {
		s.addActionLocked(n, proto.NodeAction{Action: proto.ActionIdentify, Seconds: req.Seconds})
	}
	s.mu.Unlock()
	writeOK(w)
}
