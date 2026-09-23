package hive

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/platteration/ewastesavior/internal/proto"
	"github.com/platteration/ewastesavior/internal/version"
)

// maxClientClockSkew is how far an admin client's clock may differ before
// an unsynchronized hive adopts it (DESIGN 9 "Clock").
const maxClientClockSkew = 60 * time.Second

// initClock determines TimeSynced/TimeSource at startup.
func (s *Server) initClock() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if synced, ok := ntpSynced(); ok && synced {
		s.timeSynced, s.timeSource = true, proto.TimeNTP
		return
	}
	s.timeSynced, s.timeSource = false, proto.TimeRTC
	if bt := version.BuildTime(); s.cfg.tune.clockRaised || (!bt.IsZero() && s.now().Before(bt)) {
		s.timeSource = proto.TimeBuildFloor
	}
}

// checkNTPLocked re-reads the kernel sync state (called from the loop).
func (s *Server) checkNTPLocked() {
	if synced, ok := ntpSynced(); ok && synced && s.timeSource != proto.TimeNTP {
		s.timeSynced, s.timeSource = true, proto.TimeNTP
		s.log.Info("system clock is NTP-synchronized")
	}
}

// parseClientTime accepts RFC 3339 or Unix seconds (milliseconds when the
// number is too large to be seconds).
func parseClientTime(v string) (time.Time, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return time.Time{}, false
	}
	if n, err := strconv.ParseInt(v, 10, 64); err == nil {
		if n > 1e12 {
			return time.UnixMilli(n), true
		}
		return time.Unix(n, 0), true
	}
	t, err := time.Parse(time.RFC3339Nano, v)
	return t, err == nil
}

// maybeAdoptClientTime implements the admin clock handover: on a hive whose
// clock is not synchronized, an authenticated admin request carrying
// X-Savior-Client-Time that differs by more than 60 s sets the clock.
func (s *Server) maybeAdoptClientTime(r *http.Request) {
	ct, ok := parseClientTime(r.Header.Get(ClientTimeHeader))
	if !ok {
		return
	}
	s.mu.Lock()
	synced := s.timeSynced
	s.mu.Unlock()
	if synced || !s.cfg.tune.canSetClock() {
		return
	}
	if bt := version.BuildTime(); !bt.IsZero() && ct.Before(bt) {
		return // a client clock before our build date is certainly wrong
	}
	diff := ct.Sub(s.now())
	if diff < 0 {
		diff = -diff
	}
	if diff <= maxClientClockSkew {
		return
	}
	if err := s.cfg.tune.setClock(ct); err != nil {
		s.log.Warn("could not set the clock from the admin client", "err", err)
		return
	}
	s.mu.Lock()
	s.timeSynced, s.timeSource = true, proto.TimeAdmin
	s.mu.Unlock()
	s.log.Info("system clock set from an admin client", "time", ct.UTC().Format(time.RFC3339), "skew", diff.Round(time.Second))
}
