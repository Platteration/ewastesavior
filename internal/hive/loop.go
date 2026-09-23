package hive

import (
	"context"
	"runtime/debug"
	"time"

	"github.com/platteration/ewastesavior/internal/proto"
)

// loop is the 1 s background loop (DESIGN 9): liveness, lost/offline
// requeue, deadlines, reservations, the recovery window, action, session
// and pairing-code expiry, blob GC and persistence.
func (s *Server) loop(ctx context.Context) {
	defer close(s.bgDone)
	t := time.NewTicker(s.cfg.tune.loopInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		now := time.Now()
		s.checkStorage()
		s.tick(now)
		s.writeStatusFile(now)
		s.maybePersist()
	}
}

// tick runs one pass of the loop. A panic (a bug) is logged instead of
// killing the hive, and the state mutex is always released.
func (s *Server) tick(now time.Time) {
	defer func() {
		if r := recover(); r != nil {
			s.log.Error("background loop panic", "panic", r, "stack", string(debug.Stack()))
		}
	}()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tickLocked(now)
}

func (s *Server) tickLocked(now time.Time) {
	s.livenessLocked(now)
	s.deadlinesLocked(now)
	if s.recovering && !now.Before(s.recoveryUntil) {
		s.endRecoveryLocked()
	}
	s.updateReservationLocked(now)
	for _, n := range s.nodes {
		if len(n.actions) == 0 {
			continue
		}
		kept := n.actions[:0:0]
		for _, a := range n.actions {
			if now.Sub(a.created) < s.cfg.tune.actionExpiry {
				kept = append(kept, a)
			}
		}
		n.actions = kept
	}
	s.expireSessionsLocked(now)
	s.expirePairCodesLocked(now)
	s.purgeNoncesLocked(now)
	if now.Sub(s.lastBlobGC) >= s.cfg.tune.blobGCEvery {
		s.lastBlobGC = now
		if n, freed := s.gcLocked(true, now); n > 0 {
			s.log.Info("blob gc", "deleted", n, "freed_bytes", freed)
		}
	}
	if now.Sub(s.lastNTPCheck) >= time.Minute {
		s.lastNTPCheck = now
		s.checkNTPLocked()
	}
}

// livenessLocked marks nodes offline after OfflineAfter without a
// heartbeat and requeues their tasks as lost after a further LostAfter
// (DESIGN 7.4). Restored, unconfirmed tasks wait for the recovery window.
func (s *Server) livenessLocked(now time.Time) {
	for _, n := range s.nodes {
		online := n.isOnline(now, s.cfg.OfflineAfter)
		if n.online && !online {
			n.online = false
			s.liveDirty = true
			s.log.Info("node offline", "node_id", n.ID, "name", n.Name)
			if s.reservation != nil && s.reservation.nodeID == n.ID {
				s.clearReservationLocked()
			}
			// Assignments that are no longer wanted stop being charged.
			for id, h := range n.held {
				t := s.tasks[id]
				switch {
				case t != nil && t.liveFor(n.ID, h.lease):
				case t != nil && t.CancelRequested && t.active() && t.Lease == h.lease:
					s.settleCanceledLocked(t)
				default:
					s.releaseLocked(n, id)
				}
			}
		}
		if online || n.lastHB.IsZero() || now.Sub(n.lastHB) < s.cfg.OfflineAfter+s.cfg.LostAfter {
			continue
		}
		for id, h := range n.held {
			if t := s.tasks[id]; t != nil && t.liveFor(n.ID, h.lease) && !t.unconfirmed {
				s.requeueLocked(t, requeueOpts{outcome: "lost", err: "node went offline", interrupt: t.seen})
			}
		}
	}
}

// deadlinesLocked fails tasks whose transfers stalled (DESIGN 7.4); the
// run_s deadline is checked on every heartbeat.
func (s *Server) deadlinesLocked(now time.Time) {
	for _, n := range s.nodes {
		for id, h := range n.held {
			t := s.tasks[id]
			if t == nil || !t.liveFor(n.ID, h.lease) || t.unconfirmed || !t.seen {
				continue
			}
			if t.phase != proto.PhaseFetching && t.phase != proto.PhaseUploading {
				continue
			}
			if stalled := now.Sub(t.xferMono); stalled >= s.cfg.tune.xferStall {
				s.requeueLocked(t, requeueOpts{outcome: "timeout", errKind: proto.ErrTimeout, consume: true, keepHeld: true,
					err: "hive deadline: no transfer progress for " + stalled.Round(time.Second).String() + " while " + t.phase})
			}
		}
	}
}

// endRecoveryLocked requeues restored assignments that no node re-adopted
// within the recovery window (DESIGN 8.6).
func (s *Server) endRecoveryLocked() {
	s.recovering = false
	for _, t := range s.tasks {
		if t.unconfirmed && t.active() {
			s.requeueLocked(t, requeueOpts{outcome: "lost", err: "not re-adopted after a hive restart"})
		}
	}
}
