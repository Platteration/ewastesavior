package node

import (
	"context"
	"os/exec"
	"runtime"
	"time"

	"github.com/platteration/ewastesavior/internal/hwinfo"
	"github.com/platteration/ewastesavior/internal/power"
	"github.com/platteration/ewastesavior/internal/proto"
	"github.com/platteration/ewastesavior/internal/version"
)

// Consecutive heartbeat failures (other than 401) before we rediscover.
const maxHeartbeatFailures = 6

func (a *Agent) heartbeatLoop(ctx context.Context, hc *hiveClient, endSession context.CancelFunc) {
	failures := 0
	for ctx.Err() == nil {
		a.mu.Lock()
		interval := a.hbInterval
		a.mu.Unlock()

		req := proto.HeartbeatRequest{Status: a.nodeStatus()}
		sent := time.Now()
		hctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		resp, err := hc.heartbeat(hctx, &req)
		cancel()
		recv := time.Now()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			code := statusOf(err)
			if code == 401 || code == 403 {
				a.log.Info("hive no longer knows this node; re-registering", "status", code)
				a.clearSession(hc)
				endSession()
				return
			}
			failures++
			a.log.Warn("heartbeat failed", "err", err, "failures", failures)
			if failures >= maxHeartbeatFailures {
				a.setLink(proto.LinkUnreachable, hostOf(hc.base), err.Error())
				a.clearSession(hc)
				endSession()
				return
			}
			sleep(ctx, interval)
			continue
		}
		if failures > 0 {
			a.setLink(proto.LinkConnected, hostOf(hc.base), "")
		}
		failures = 0
		a.clearAcked(req.Status.AckedActions)
		a.observeHiveTime(resp.Time, resp.TimeSynced, sent, recv)
		a.applyDirectives(resp.Directives)
		a.writeStatus()
		sleep(ctx, interval)
	}
}

func (a *Agent) clearSession(hc *hiveClient) {
	a.mu.Lock()
	if a.hc == hc {
		a.hc = nil
	}
	a.mu.Unlock()
}

// nodeStatus builds the heartbeat payload.
func (a *Agent) nodeStatus() proto.NodeStatus {
	m := a.metrics()
	// Never call into the display controller while holding a.mu: its render
	// loop calls back into statusInfo, which takes a.mu.
	var ds proto.DisplayState
	if a.disp != nil {
		ds = a.disp.State()
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	st := proto.NodeStatus{
		Metrics:      m,
		Total:        a.total,
		Free:         a.freeLocked(),
		RunningTasks: a.runningLocked(),
		Addrs:        localAddrs(),
		AckedActions: append([]string(nil), a.ackPending...),
	}
	switch {
	case a.directives.Drain:
		st.State = proto.NodeDraining
	case a.decision.Pause || !a.decision.Accept && a.decision.Reason != "":
		st.State = proto.NodePaused
		st.Reason = a.decision.Reason
	case len(st.RunningTasks) > 0:
		st.State = proto.NodeBusy
	default:
		st.State = proto.NodeIdle
	}
	st.Display = ds
	return st
}

// clearAcked forgets acks the hive has now seen.
func (a *Agent) clearAcked(sent []string) {
	if len(sent) == 0 {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	drop := map[string]bool{}
	for _, id := range sent {
		drop[id] = true
	}
	var keep []string
	for _, id := range a.ackPending {
		if !drop[id] {
			keep = append(keep, id)
		}
	}
	a.ackPending = keep
}

// observeHiveTime updates the hive clock offset from a verified response
// and steps the system clock when DESIGN 10.2 allows it.
func (a *Agent) observeHiveTime(hiveTime time.Time, synced bool, sent, recv time.Time) {
	if hiveTime.IsZero() {
		return
	}
	mid := sent.Add(recv.Sub(sent) / 2)
	offset := hiveTime.Sub(mid)
	a.mu.Lock()
	a.clockOffset = offset
	a.hiveSynced = synced
	manage := a.opt.ManageSystem
	a.mu.Unlock()
	if !manage || runtime.GOOS != "linux" {
		return
	}
	if offset < 5*time.Second && offset > -5*time.Second {
		return
	}
	beforeBuild := time.Now().Before(version.BuildTime())
	if !synced && !beforeBuild {
		return
	}
	if localNTPSynced() {
		return
	}
	target := time.Now().Add(offset)
	if err := setSystemClock(target); err != nil {
		a.log.Warn("cannot set the system clock", "err", err)
		return
	}
	a.log.Info("system clock corrected from the hive", "offset", offset.Round(time.Second))
	a.mu.Lock()
	a.clockOffset = 0
	a.mu.Unlock()
}

// applyDirectives acts on the hive's desired state.
func (a *Agent) applyDirectives(d proto.Directives) {
	a.mu.Lock()
	prev := a.directives
	a.directives = d
	if d.Name != "" && d.Name != a.name {
		a.name = d.Name
		if a.opt.ManageSystem {
			setHostname(d.Name)
		}
	}
	wasPending := a.pending
	a.pending = d.Pending
	disp := a.disp
	newRev := d.Display != nil && d.Display.Rev != a.displayRev
	if d.Display != nil {
		a.displayRev = d.Display.Rev
	}
	rotateChanged := d.DisplayRotate != a.rotate
	a.rotate = d.DisplayRotate
	a.mu.Unlock()

	if wasPending != d.Pending {
		if d.Pending {
			a.setLink(proto.LinkPending, "", "waiting for an admin to approve this machine")
		} else {
			a.setLink(proto.LinkConnected, "", "")
		}
	}
	if disp != nil {
		if newRev {
			disp.Apply(*d.Display)
		}
		if rotateChanged {
			disp.SetRotate(d.DisplayRotate)
		}
	}
	if d.Drain != prev.Drain {
		a.log.Info("drain", "on", d.Drain)
	}
	for _, ref := range d.CancelTasks {
		a.cancelTask(ref, "canceled by the hive")
	}
	for _, act := range d.Actions {
		a.runAction(act)
	}
	a.poke()
}

// runAction executes a one-shot action at most once per agent process.
func (a *Agent) runAction(act proto.ActionDirective) {
	a.mu.Lock()
	if a.actionsDone[act.ID] {
		a.mu.Unlock()
		return
	}
	a.actionsDone[act.ID] = true
	a.ackPending = append(a.ackPending, act.ID)
	disp := a.disp
	code := a.shortCode
	manage := a.opt.ManageSystem
	a.mu.Unlock()

	switch act.Action {
	case proto.ActionIdentify:
		secs := act.Seconds
		if secs <= 0 {
			secs = 30
		}
		if secs > 600 {
			secs = 600
		}
		a.log.Info("identify", "seconds", secs)
		if disp != nil {
			// The controller draws the name from StatusInfo itself; an empty
			// label lets it show the wall position instead.
			disp.Identify(time.Duration(secs)*time.Second, code, "")
		}
	case proto.ActionReboot, proto.ActionPoweroff:
		a.log.Warn("hive requested "+act.Action, "id", act.ID)
		if !manage {
			return
		}
		// Ack in a heartbeat first so the hive drops the action, then act.
		go func() {
			a.sendAckNow()
			a.shutdownTasks()
			cmd := "/sbin/reboot"
			if act.Action == proto.ActionPoweroff {
				cmd = "/sbin/poweroff"
			}
			if err := exec.Command(cmd).Run(); err != nil {
				a.log.Error(act.Action+" failed", "err", err)
			}
		}()
	default:
		a.log.Warn("unknown action", "action", act.Action)
	}
}

// sendAckNow sends one immediate heartbeat carrying pending acks.
func (a *Agent) sendAckNow() {
	a.mu.Lock()
	hc := a.hc
	a.mu.Unlock()
	if hc == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req := proto.HeartbeatRequest{Status: a.nodeStatus()}
	if _, err := hc.heartbeat(ctx, &req); err == nil {
		a.clearAcked(req.Status.AckedActions)
	}
}

// powerLoop evaluates the power/thermal policy every few seconds and
// freezes, thaws or preempts tasks accordingly.
func (a *Agent) powerLoop(ctx context.Context) {
	pol := power.Policy{
		RunOnBattery:      a.cfg.RunOnBattery,
		BatteryMinPercent: a.cfg.BatteryMinPercent,
		MaxTempC:          float64(a.cfg.MaxTempC),
	}
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		m := a.sample()
		ext := hwinfo.ExternalDisplayConnected(a.opt.SysRoot)
		a.mu.Lock()
		prev := a.decision
		d := power.Evaluate(pol, power.Input{Metrics: m, ExternalDisplay: ext, MemBudgetMB: a.memBudgetMB}, prev)
		a.decision = d
		a.lastMetrics = m
		disp := a.disp
		a.mu.Unlock()

		if d.Accept != prev.Accept || d.Pause != prev.Pause || d.Preempt != prev.Preempt {
			a.log.Info("power policy", "accept", d.Accept, "pause", d.Pause, "preempt", d.Preempt, "reason", d.Reason)
		}
		if d.Pause != prev.Pause {
			a.freezeAll(d.Pause)
		}
		if d.Preempt && !prev.Preempt {
			a.preemptAll(d.Reason)
		}
		if disp != nil && d.BlankDisplay != prev.BlankDisplay {
			disp.SetBlank("lid", d.BlankDisplay)
		}
		if d.Accept != prev.Accept {
			a.poke()
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// poke wakes the claim loop.
func (a *Agent) poke() {
	select {
	case a.wake <- struct{}{}:
	default:
	}
}

// sample takes a fresh metrics sample (the power loop owns the cadence, so
// CPU% deltas cover a steady 5 s window).
func (a *Agent) sample() proto.Metrics {
	a.sampleMu.Lock()
	m := a.sampler.Sample()
	a.sampleMu.Unlock()
	a.mu.Lock()
	a.lastMetrics = m
	a.mu.Unlock()
	return m
}

// metrics returns the latest sample, sampling once if there is none yet.
func (a *Agent) metrics() proto.Metrics {
	a.mu.Lock()
	m := a.lastMetrics
	a.mu.Unlock()
	if m.Time.IsZero() {
		return a.sample()
	}
	return m
}
