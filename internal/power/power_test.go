package power

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/platteration/ewastesavior/internal/proto"
)

var t0 = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

// base is a healthy desktop sample.
func base() proto.Metrics {
	return proto.Metrics{Time: t0, CPUTempC: 50, CPUTempLimitC: 85, BatteryPercent: -1}
}

var defPolicy = Policy{RunOnBattery: false, BatteryMinPercent: 40, MaxTempC: 85}

func TestEvaluateSingle(t *testing.T) {
	type want struct {
		accept, pause, preempt, blank bool
		reason                        string
	}
	cases := []struct {
		name   string
		p      Policy
		mod    func(m *proto.Metrics)
		ext    bool
		budget int
		want   want
	}{
		{"healthy", defPolicy, nil, false, 512, want{accept: true}},
		{"hot", defPolicy, func(m *proto.Metrics) { m.CPUTempC = 91 }, false, 0,
			want{pause: true, reason: "CPU 91°C ≥ 85°C limit"}},
		{"exactly at limit pauses", defPolicy, func(m *proto.Metrics) { m.CPUTempC = 85 }, false, 0,
			want{pause: true, reason: "CPU 85°C ≥ 85°C limit"}},
		{"just below limit", defPolicy, func(m *proto.Metrics) { m.CPUTempC = 84.9 }, false, 0, want{accept: true}},
		{"sensor limit below config", defPolicy, func(m *proto.Metrics) { m.CPUTempC = 71; m.CPUTempLimitC = 70 }, false, 0,
			want{pause: true, reason: "CPU 71°C ≥ 70°C limit"}},
		{"no sensor limit uses policy", Policy{BatteryMinPercent: 40, MaxTempC: 80},
			func(m *proto.Metrics) { m.CPUTempC = 81; m.CPUTempLimitC = 0 }, false, 0,
			want{pause: true, reason: "CPU 81°C ≥ 80°C limit"}},
		{"no limit at all uses default", Policy{},
			func(m *proto.Metrics) { m.CPUTempC = 86; m.CPUTempLimitC = 0 }, false, 0,
			want{pause: true, reason: "CPU 86°C ≥ 85°C limit"}},
		{"NaN limit falls back", Policy{MaxTempC: 60},
			func(m *proto.Metrics) { m.CPUTempC = 61; m.CPUTempLimitC = math.NaN() }, false, 0,
			want{pause: true, reason: "CPU 61°C ≥ 60°C limit"}},
		{"unknown temperature", defPolicy, func(m *proto.Metrics) { m.CPUTempC = 0 }, false, 0, want{accept: true}},
		{"NaN temperature", defPolicy, func(m *proto.Metrics) { m.CPUTempC = math.NaN() }, false, 0, want{accept: true}},

		{"on battery refuses", defPolicy, func(m *proto.Metrics) { m.OnBattery = true; m.BatteryPercent = 90 }, false, 0,
			want{reason: "on battery"}},
		{"on battery allowed", Policy{RunOnBattery: true, BatteryMinPercent: 40},
			func(m *proto.Metrics) { m.OnBattery = true; m.BatteryPercent = 90 }, false, 0, want{accept: true}},
		{"battery low preempts", defPolicy, func(m *proto.Metrics) { m.OnBattery = true; m.BatteryPercent = 12 }, false, 0,
			want{preempt: true, reason: "battery 12% < 40%"}},
		{"battery low preempts with run_on_battery", Policy{RunOnBattery: true, BatteryMinPercent: 40},
			func(m *proto.Metrics) { m.OnBattery = true; m.BatteryPercent = 12 }, false, 0,
			want{preempt: true, reason: "battery 12% < 40%"}},
		{"battery at minimum does not preempt", Policy{RunOnBattery: true, BatteryMinPercent: 40},
			func(m *proto.Metrics) { m.OnBattery = true; m.BatteryPercent = 40 }, false, 0, want{accept: true}},
		{"worn battery counts as absent", defPolicy,
			func(m *proto.Metrics) {
				m.OnBattery, m.BatteryPercent, m.BatteryHealthPercent = true, 12, 15
			}, false, 0, want{reason: "on battery"}},
		{"worn battery with run_on_battery", Policy{RunOnBattery: true, BatteryMinPercent: 40},
			func(m *proto.Metrics) {
				m.OnBattery, m.BatteryPercent, m.BatteryHealthPercent = true, 12, 19
			}, false, 0, want{accept: true}},
		{"battery at 20% health still counts", defPolicy,
			func(m *proto.Metrics) {
				m.OnBattery, m.BatteryPercent, m.BatteryHealthPercent = true, 12, 20
			}, false, 0, want{preempt: true, reason: "battery 12% < 40%"}},
		{"unknown charge on battery", defPolicy, func(m *proto.Metrics) { m.OnBattery = true; m.BatteryPercent = -1 }, false, 0,
			want{reason: "on battery"}},
		{"low but on AC", defPolicy, func(m *proto.Metrics) { m.BatteryPercent = 5; m.BatteryStatus = "Charging" }, false, 0,
			want{accept: true}},
		{"minimum 0 never preempts", Policy{RunOnBattery: true, BatteryMinPercent: 0},
			func(m *proto.Metrics) { m.OnBattery = true; m.BatteryPercent = 0 }, false, 0, want{accept: true}},

		{"PSI pressure", defPolicy, func(m *proto.Metrics) { m.MemPressure = 30 }, false, 512,
			want{reason: "memory pressure"}},
		{"PSI below threshold", defPolicy, func(m *proto.Metrics) { m.MemPressure = 29.9 }, false, 512, want{accept: true}},
		{"swap above half budget", defPolicy, func(m *proto.Metrics) { m.SwapUsedMB = 300 }, false, 512,
			want{reason: "memory pressure (swap)"}},
		{"swap at half budget", defPolicy, func(m *proto.Metrics) { m.SwapUsedMB = 256 }, false, 512, want{accept: true}},
		{"swap rule off without budget", defPolicy, func(m *proto.Metrics) { m.SwapUsedMB = 1000 }, false, 0, want{accept: true}},

		{"lid closed blanks", defPolicy, func(m *proto.Metrics) { m.LidClosed = true }, false, 0,
			want{accept: true, blank: true}},
		{"lid closed with external display", defPolicy, func(m *proto.Metrics) { m.LidClosed = true }, true, 0,
			want{accept: true}},
		{"lid closed on battery", defPolicy,
			func(m *proto.Metrics) { m.LidClosed, m.OnBattery, m.BatteryPercent = true, true, 80 }, false, 0,
			want{blank: true, reason: "on battery"}},

		{"preempt reason wins over heat and memory", defPolicy,
			func(m *proto.Metrics) {
				m.OnBattery, m.BatteryPercent, m.CPUTempC, m.MemPressure = true, 10, 95, 50
			}, false, 0, want{pause: true, preempt: true, reason: "battery 10% < 40%"}},
		{"heat reason wins over battery", defPolicy,
			func(m *proto.Metrics) { m.OnBattery, m.BatteryPercent, m.CPUTempC = true, 80, 95 }, false, 0,
			want{pause: true, reason: "CPU 95°C ≥ 85°C limit"}},
		{"battery reason wins over memory", defPolicy,
			func(m *proto.Metrics) { m.OnBattery, m.BatteryPercent, m.MemPressure = true, 80, 50 }, false, 0,
			want{reason: "on battery"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := base()
			if tc.mod != nil {
				tc.mod(&m)
			}
			d := Evaluate(tc.p, Input{Metrics: m, ExternalDisplay: tc.ext, MemBudgetMB: tc.budget}, Decision{})
			got := want{d.Accept, d.Pause, d.Preempt, d.BlankDisplay, d.Reason}
			if got != tc.want {
				t.Errorf("got %+v\nwant %+v", got, tc.want)
			}
			if d.Accept == (d.Reason != "") {
				t.Errorf("Accept=%v but Reason=%q", d.Accept, d.Reason)
			}
		})
	}
}

// step is one sample in a sequence test.
type step struct {
	temp     float64
	throttle uint64
	battery  int  // -1 = no battery
	onBatt   bool // on battery
	pause    bool
	accept   bool
	preempt  bool
	reason   string // substring; "" = must be empty
}

func runSequence(t *testing.T, p Policy, steps []step) {
	t.Helper()
	var d Decision
	for i, s := range steps {
		m := base()
		m.Time = t0.Add(time.Duration(i) * 5 * time.Second)
		m.CPUTempC, m.ThrottleEvents, m.BatteryPercent, m.OnBattery = s.temp, s.throttle, s.battery, s.onBatt
		d = Evaluate(p, Input{Metrics: m}, d)
		if d.Pause != s.pause || d.Accept != s.accept || d.Preempt != s.preempt {
			t.Fatalf("step %d (%+v): pause=%v accept=%v preempt=%v reason=%q", i, s, d.Pause, d.Accept, d.Preempt, d.Reason)
		}
		if (s.reason == "") != (d.Reason == "") || !strings.Contains(d.Reason, s.reason) {
			t.Fatalf("step %d: reason %q, want %q", i, d.Reason, s.reason)
		}
	}
}

func TestThermalHysteresis(t *testing.T) {
	runSequence(t, defPolicy, []step{
		{temp: 80, battery: -1, accept: true},
		{temp: 86, battery: -1, pause: true, reason: "CPU 86°C ≥ 85°C limit"},
		{temp: 84, battery: -1, pause: true, reason: "cooling down: resuming at ≤ 75°C"},
		{temp: 76, battery: -1, pause: true, reason: "cooling down"},
		{temp: 75, battery: -1, accept: true},
		{temp: 84, battery: -1, accept: true},
		{temp: 85, battery: -1, pause: true, reason: "≥ 85°C"},
		{temp: 0, battery: -1, accept: true}, // sensor lost: nothing to wait for
	})
}

func TestThrottleStreak(t *testing.T) {
	runSequence(t, defPolicy, []step{
		{temp: 60, throttle: 1000, battery: -1, accept: true}, // first sample is a baseline, not a rise
		{temp: 60, throttle: 1001, battery: -1, accept: true},
		{temp: 60, throttle: 1002, battery: -1, accept: true},
		{temp: 60, throttle: 1003, battery: -1, pause: true, reason: "throttling"},
		{temp: 60, throttle: 1010, battery: -1, pause: true, reason: "throttling"},
		{temp: 60, throttle: 1010, battery: -1, accept: true}, // no new events, cool enough
		{temp: 60, throttle: 1011, battery: -1, accept: true},
		{temp: 60, throttle: 1012, battery: -1, accept: true},
		{temp: 60, throttle: 1012, battery: -1, accept: true}, // streak broken
		{temp: 60, throttle: 1013, battery: -1, accept: true},
		{temp: 60, throttle: 1014, battery: -1, accept: true},
		{temp: 60, throttle: 1015, battery: -1, pause: true, reason: "throttling"},
	})
}

func TestThrottleCounterResetIsNotARise(t *testing.T) {
	runSequence(t, defPolicy, []step{
		{temp: 60, throttle: 50, battery: -1, accept: true},
		{temp: 60, throttle: 51, battery: -1, accept: true},
		{temp: 60, throttle: 52, battery: -1, accept: true},
		{temp: 60, throttle: 0, battery: -1, accept: true}, // unreadable/reset: streak ends
		{temp: 60, throttle: 1, battery: -1, accept: true},
	})
}

func TestResumeWaitsForThrottlingToStop(t *testing.T) {
	runSequence(t, defPolicy, []step{
		{temp: 90, throttle: 5, battery: -1, pause: true, reason: "CPU 90°C"},
		{temp: 60, throttle: 6, battery: -1, pause: true, reason: "still throttling"},
		{temp: 60, throttle: 6, battery: -1, accept: true},
	})
}

func TestThrottlePauseKeepsTemperatureHysteresis(t *testing.T) {
	runSequence(t, defPolicy, []step{
		{temp: 80, throttle: 0, battery: -1, accept: true},
		{temp: 80, throttle: 1, battery: -1, accept: true},
		{temp: 80, throttle: 2, battery: -1, accept: true},
		{temp: 80, throttle: 3, battery: -1, pause: true, reason: "throttling"},
		{temp: 80, throttle: 3, battery: -1, pause: true, reason: "resuming at ≤ 75°C"},
		{temp: 74, throttle: 3, battery: -1, accept: true},
	})
}

func TestBatteryHysteresis(t *testing.T) {
	p := Policy{RunOnBattery: true, BatteryMinPercent: 40, MaxTempC: 85}
	runSequence(t, p, []step{
		{temp: 50, battery: 45, onBatt: true, accept: true},
		{temp: 50, battery: 39, onBatt: true, preempt: true, reason: "battery 39% < 40%"},
		{temp: 50, battery: 41, onBatt: true, reason: "battery 41%, waiting for 45%"},
		{temp: 50, battery: 44, onBatt: true, reason: "waiting for 45%"},
		{temp: 50, battery: 45, onBatt: true, accept: true},
		{temp: 50, battery: 39, onBatt: true, preempt: true, reason: "battery 39%"},
		{temp: 50, battery: 39, onBatt: false, accept: true}, // plugged in
		{temp: 50, battery: 41, onBatt: true, accept: true},  // unplugged above the minimum
	})
}

func TestBatteryWithoutRunOnBattery(t *testing.T) {
	runSequence(t, defPolicy, []step{
		{temp: 50, battery: 100, onBatt: false, accept: true},
		{temp: 50, battery: 99, onBatt: true, reason: "on battery"},
		{temp: 50, battery: 39, onBatt: true, preempt: true, reason: "battery 39% < 40%"},
		{temp: 50, battery: 41, onBatt: true, reason: "on battery"},
		{temp: 50, battery: 41, onBatt: false, accept: true},
	})
}

func TestSinceTracksTransitions(t *testing.T) {
	var d Decision
	m := base()
	for i, temp := range []float64{50, 50, 90, 90, 60, 60} {
		m.Time = t0.Add(time.Duration(i) * time.Minute)
		m.CPUTempC = temp
		d = Evaluate(defPolicy, Input{Metrics: m}, d)
		want := map[int]int{0: 0, 1: 0, 2: 2, 3: 2, 4: 4, 5: 4}[i]
		if !d.Since.Equal(t0.Add(time.Duration(want) * time.Minute)) {
			t.Fatalf("sample %d: Since = %v, want minute %d", i, d.Since, want)
		}
	}
}

func TestZeroDecisionIsUsableAndPure(t *testing.T) {
	in := Input{Metrics: base(), MemBudgetMB: 400}
	a := Evaluate(defPolicy, in, Decision{})
	b := Evaluate(defPolicy, in, Decision{})
	if a != b {
		t.Fatalf("Evaluate is not deterministic: %+v vs %+v", a, b)
	}
	if !a.Accept || a.Pause || a.Preempt || a.BlankDisplay || a.Reason != "" {
		t.Fatalf("healthy first sample: %+v", a)
	}
}
