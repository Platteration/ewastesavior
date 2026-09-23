package hwinfo

import (
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/platteration/ewastesavior/internal/proto"
)

var sampleTime = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

// fakeClock is a settable clock for Sampler.SetClock.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newTestSampler(root string, maxTempC float64) (*Sampler, *fakeClock) {
	clk := &fakeClock{t: sampleTime}
	s := NewSampler(root, maxTempC)
	s.SetClock(clk.now)
	return s, clk
}

func TestSampleFixtures(t *testing.T) {
	cases := map[string]proto.Metrics{
		"thinkpad-t60": {
			UptimeS: 3600, Load1: 0.35, MemAvailableMB: 1760, MemPressure: 1.5,
			CPUTempC: 52, CPUTempLimitC: 85, ThrottleEvents: 5,
			BatteryPercent: 84, BatteryHealthPercent: 80, BatteryStatus: "Charging",
			NetRxBytes: 1500000, NetTxBytes: 250000,
		},
		"p4-desktop": {
			// The 127 °C zone is ignored; the valid zone's critical trip
			// (85 °C) gives a limit of 80.
			UptimeS: 86400, Load1: 1.02, MemAvailableMB: 391, SwapUsedMB: 53,
			CPUTempC: 49, CPUTempLimitC: 80, ThrottleEvents: 24, BatteryPercent: -1,
			NetRxBytes: 700000, NetTxBytes: 90000,
		},
		"atom-netbook": {
			// No Mains supply and the battery discharges: on battery.
			UptimeS: 7200, Load1: 0.8, MemAvailableMB: 597, MemPressure: 4.1,
			CPUTempC: 58, CPUTempLimitC: 85, OnBattery: true,
			BatteryPercent: 55, BatteryHealthPercent: 91, BatteryStatus: "Discharging",
			LidClosed: true, NetRxBytes: 3000000, NetTxBytes: 400000,
		},
		"amd-desktop": {
			// drivetemp (45 °C) is ignored; k10temp temp1_max is the limit;
			// the mouse battery is not a system battery.
			UptimeS: 120, MemAvailableMB: 3417, CPUTempC: 38.25, CPUTempLimitC: 70,
			BatteryPercent: -1, NetRxBytes: 42, NetTxBytes: 24,
		},
		"qemu": {
			UptimeS: 60, Load1: 0.1, MemAvailableMB: 371, CPUTempLimitC: 85, BatteryPercent: -1,
			NetRxBytes: 9000, NetTxBytes: 8000,
		},
	}
	for _, name := range machines {
		t.Run(name, func(t *testing.T) {
			s, _ := newTestSampler(fixture(t, name), 85)
			got := s.Sample()
			want := cases[name]
			want.Time = sampleTime
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("got  %+v\nwant %+v", got, want)
			}
		})
	}
}

func TestCPUPercent(t *testing.T) {
	root := copyFixture(t, "qemu")
	stat := func(line string) { writeFile(t, root, "proc/stat", line+"\nintr 1\n") }

	stat("cpu  100 0 100 800 0 0 0 0 0 0")
	s, _ := newTestSampler(root, 85)
	steps := []struct {
		line string
		want float64
	}{
		// +300 user, +100 sys, +500 idle, +100 iowait: 400 busy of 1000.
		{"cpu  400 0 200 1300 100 0 0 0 0 0", 40},
		{"cpu  400 0 200 1300 100 0 0 0 0 0", 40}, // no progress: repeat
		{"cpu  400 0 200 1400 100 0 0 0 0 0", 0},  // fully idle
		// guest time is part of user and must not be counted twice.
		{"cpu  500 0 200 1400 100 0 0 0 100 0", 100},
		{"cpu  10 0 10 10 0 0 0 0 0 0", 100},  // counters went backwards: repeat, re-baseline
		{"cpu  15 0 10 25 0 5 5 0 0 0", 50},   // 15 busy (5 user, 5 irq, 5 softirq) of 30
		{"cpu  30 0 10 25 0 5 5 15 0 0", 100}, // steal counts as busy
		{"cpu  30 0 10", 0},                   // unreadable: 0 (unknown), baseline kept
		{"garbage", 0},
		{"cpu  80 0 10 75 0 5 5 15 0 0", 50}, // measured against the last readable sample
	}
	for i, st := range steps {
		stat(st.line)
		if got := s.Sample().CPUPercent; got != st.want {
			t.Fatalf("step %d (%q): CPUPercent = %v, want %v", i, st.line, got, st.want)
		}
	}
}

// Old kernels (2.6.x) print fewer than 10 counters; 4 fields are enough.
func TestCPUPercentShortStatLine(t *testing.T) {
	root := copyFixture(t, "qemu")
	writeFile(t, root, "proc/stat", "cpu  100 0 100 800\n")
	s, _ := newTestSampler(root, 85)
	writeFile(t, root, "proc/stat", "cpu  150 0 150 900\n")
	if got := s.Sample().CPUPercent; got != 50 {
		t.Fatalf("CPUPercent = %v, want 50", got)
	}
}

func TestReadCPUTimes(t *testing.T) {
	cases := []struct {
		content     string
		total, idle uint64
		ok          bool
	}{
		{"cpu  1 2 3 4 5 6 7 8 9 10\n", 36, 9, true},
		{"cpu  1 2 3 4\n", 10, 4, true},
		{"cpu  1 2 3\n", 0, 0, false},
		{"cpu0 1 2 3 4 5\n", 0, 0, false},
		{"cpu  1 2 x 4 5\n", 0, 0, false},
		{"cpu  1 2 3 -4 5\n", 0, 0, false},
		{"", 0, 0, false},
	}
	for _, c := range cases {
		root := t.TempDir()
		writeFile(t, root, "proc/stat", c.content)
		got, ok := readCPUTimes(root)
		if ok != c.ok || got.total != c.total || got.idle != c.idle {
			t.Errorf("readCPUTimes(%q) = %+v, %v; want total %d idle %d ok %v", c.content, got, ok, c.total, c.idle, c.ok)
		}
	}
}

// DESIGN 10.4: a reading unchanged for 10 minutes is ignored, and the CPU
// temperature falls back to the ACPI zones when every CPU sensor is invalid.
func TestStuckSensor(t *testing.T) {
	root := copyFixture(t, "atom-netbook")
	s, clk := newTestSampler(root, 85)
	check := func(step string, temp, limit float64) {
		t.Helper()
		m := s.Sample()
		if m.CPUTempC != temp || m.CPUTempLimitC != limit {
			t.Fatalf("%s: temp %v limit %v, want %v / %v", step, m.CPUTempC, m.CPUTempLimitC, temp, limit)
		}
	}
	check("t=0", 58, 85)
	clk.advance(5 * time.Minute)
	writeFile(t, root, "sys/class/thermal/thermal_zone0/temp", "62000\n")
	check("t=5m acpitz changed", 58, 85)
	clk.advance(5*time.Minute - time.Second)
	check("t=9m59s", 58, 85)
	clk.advance(time.Second)
	// coretemp is stuck at 58 since t=0: ignore it and use acpitz (limit
	// min(85, 98-5)).
	check("t=10m coretemp stuck", 62, 85)
	clk.advance(5 * time.Minute)
	check("t=15m everything stuck", 0, 85)
	writeFile(t, root, "sys/class/hwmon/hwmon0/temp2_input", "59000\n")
	check("coretemp moves again", 59, 85)
}

func TestStuckSensorForgetsVanishedInputs(t *testing.T) {
	root := copyFixture(t, "atom-netbook")
	s, clk := newTestSampler(root, 85)
	s.Sample()
	removeAll(t, root, "sys/class/hwmon/hwmon0")
	clk.advance(11 * time.Minute)
	s.Sample()
	if n := len(s.stuck.hist); n != 1 {
		t.Fatalf("history has %d entries, want only the acpitz zone", n)
	}
	// The module comes back (reloaded): its old history must not mark it stuck.
	writeFile(t, root, "sys/class/hwmon/hwmon0/name", "coretemp\n")
	writeFile(t, root, "sys/class/hwmon/hwmon0/temp2_input", "58000\n")
	if got := s.Sample().CPUTempC; got != 58 {
		t.Fatalf("CPUTempC = %v, want 58", got)
	}
}

func TestStuckFilterClockGoingBackwards(t *testing.T) {
	var f stuckFilter
	r := sensorReading{key: "k", milliC: 50000}
	g := [][]sensorReading{{r}}
	f.check(sampleTime, g)
	// A clock step backwards restarts the timer instead of underflowing.
	if !f.check(sampleTime.Add(-time.Hour), g)(r) {
		t.Fatal("reading marked stuck after clock went backwards")
	}
	if !f.check(sampleTime.Add(-time.Hour+9*time.Minute), g)(r) {
		t.Fatal("reading marked stuck too early")
	}
	if f.check(sampleTime.Add(-time.Hour+10*time.Minute), g)(r) {
		t.Fatal("reading not marked stuck after 10 minutes")
	}
}

// hwmonChip writes one hwmon device with the given attributes (m°C strings).
func hwmonChip(t *testing.T, root string, n int, name string, attrs map[string]string) {
	t.Helper()
	dir := "sys/class/hwmon/hwmon" + strconv.Itoa(n) + "/"
	writeFile(t, root, dir+"name", name+"\n")
	for k, v := range attrs {
		writeFile(t, root, dir+k, v+"\n")
	}
}

func acpiZone(t *testing.T, root string, n int, temp string, trips ...string) {
	t.Helper()
	dir := "sys/class/thermal/thermal_zone" + strconv.Itoa(n) + "/"
	writeFile(t, root, dir+"type", "acpitz\n")
	writeFile(t, root, dir+"temp", temp+"\n")
	for i := 0; i+1 < len(trips); i += 2 {
		writeFile(t, root, dir+"trip_point_"+strconv.Itoa(i/2)+"_type", trips[i]+"\n")
		writeFile(t, root, dir+"trip_point_"+strconv.Itoa(i/2)+"_temp", trips[i+1]+"\n")
	}
}

func TestCPUTemperatureAndLimit(t *testing.T) {
	cases := []struct {
		name        string
		setup       func(t *testing.T, root string)
		maxTempC    float64
		temp, limit float64
		sensor      string
	}{
		{"coretemp max and crit", func(t *testing.T, r string) {
			hwmonChip(t, r, 0, "coretemp", map[string]string{"temp1_input": "61000", "temp1_max": "80000", "temp1_crit": "100000"})
		}, 85, 61, 80, "coretemp"},
		{"hottest core wins, lowest limit wins", func(t *testing.T, r string) {
			hwmonChip(t, r, 0, "coretemp", map[string]string{
				"temp1_input": "61000", "temp1_crit": "100000",
				"temp2_input": "67000", "temp2_crit": "90000",
			})
		}, 110, 67, 85, "coretemp"},
		{"k10temp uses temp1_max, not crit-5", func(t *testing.T, r string) {
			hwmonChip(t, r, 0, "k10temp", map[string]string{"temp1_input": "50125", "temp1_max": "70000", "temp1_crit": "73000"})
		}, 85, 50.125, 70, "k10temp"},
		{"k10temp without max uses crit-5", func(t *testing.T, r string) {
			hwmonChip(t, r, 0, "k10temp", map[string]string{"temp1_input": "50000", "temp1_crit": "80000"})
		}, 85, 50, 75, "k10temp"},
		{"k8temp and via_cputemp are CPU sensors", func(t *testing.T, r string) {
			hwmonChip(t, r, 0, "k8temp", map[string]string{"temp1_input": "44000"})
			hwmonChip(t, r, 1, "via_cputemp", map[string]string{"temp1_input": "47000"})
		}, 85, 47, 85, "via_cputemp"},
		{"implausible limits ignored", func(t *testing.T, r string) {
			hwmonChip(t, r, 0, "coretemp", map[string]string{
				"temp1_input": "50000", "temp1_max": "0", "temp1_crit": "200000",
				"temp2_input": "51000", "temp2_max": "20000", "temp2_crit": "garbage",
			})
		}, 85, 51, 85, "coretemp"},
		{"non-CPU sensors never used", func(t *testing.T, r string) {
			hwmonChip(t, r, 0, "drivetemp", map[string]string{"temp1_input": "45000"})
			hwmonChip(t, r, 1, "radeon", map[string]string{"temp1_input": "70000", "temp1_crit": "90000"})
			hwmonChip(t, r, 2, "nouveau", map[string]string{"temp1_input": "71000"})
			hwmonChip(t, r, 3, "iwlwifi_1", map[string]string{"temp1_input": "72000"})
			hwmonChip(t, r, 4, "acpitz", map[string]string{"temp1_input": "73000"})
			writeFile(t, r, "sys/class/thermal/thermal_zone0/type", "x86_pkg_temp\n")
			writeFile(t, r, "sys/class/thermal/thermal_zone0/temp", "74000\n")
		}, 85, 0, 85, ""},
		{"implausible readings ignored", func(t *testing.T, r string) {
			hwmonChip(t, r, 0, "coretemp", map[string]string{
				"temp1_input": "0", "temp2_input": "125000", "temp3_input": "-5000", "temp4_input": "124999",
			})
		}, 85, 124.999, 85, "coretemp"},
		{"acpitz fallback with critical and hot trips", func(t *testing.T, r string) {
			acpiZone(t, r, 0, "56000", "passive", "60000", "critical", "90000", "hot", "84000")
		}, 85, 56, 84, "acpitz"},
		{"a bogus zone doesn't lower the limit", func(t *testing.T, r string) {
			acpiZone(t, r, 0, "127000", "critical", "45000")
			acpiZone(t, r, 1, "41000", "critical", "100000")
		}, 85, 41, 85, "acpitz"},
		{"CPU hwmon preferred over hotter acpitz", func(t *testing.T, r string) {
			hwmonChip(t, r, 0, "coretemp", map[string]string{"temp1_input": "40000", "temp1_crit": "100000"})
			acpiZone(t, r, 0, "60000", "critical", "70000")
		}, 85, 40, 85, "coretemp"},
		{"invalid CPU hwmon falls back to acpitz", func(t *testing.T, r string) {
			hwmonChip(t, r, 0, "coretemp", map[string]string{"temp1_input": "255000"})
			acpiZone(t, r, 0, "60000", "critical", "70000")
		}, 85, 60, 65, "acpitz"},
		{"old kernel: attributes under hwmonN/device", func(t *testing.T, r string) {
			writeFile(t, r, "sys/class/hwmon/hwmon0/device/name", "coretemp\n")
			writeFile(t, r, "sys/class/hwmon/hwmon0/device/temp1_input", "48000\n")
			writeFile(t, r, "sys/class/hwmon/hwmon0/device/temp1_crit", "85000\n")
		}, 85, 48, 80, "coretemp"},
		{"no configured maximum", func(t *testing.T, r string) {
			hwmonChip(t, r, 0, "coretemp", map[string]string{"temp1_input": "48000", "temp1_crit": "100000"})
		}, 0, 48, 95, "coretemp"},
		{"nothing known", func(t *testing.T, r string) {}, 0, 0, 0, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			root := t.TempDir()
			c.setup(t, root)
			s, _ := newTestSampler(root, c.maxTempC)
			m := s.Sample()
			if m.CPUTempC != c.temp || m.CPUTempLimitC != c.limit {
				t.Errorf("temp %v limit %v, want %v / %v", m.CPUTempC, m.CPUTempLimitC, c.temp, c.limit)
			}
			groups := [][]sensorReading{readHwmonCPU(root), readACPIZones(root)}
			if got := pickCPUTemp(groups, sensorReading.plausible).chip; got != c.sensor {
				t.Errorf("sensor %q, want %q", got, c.sensor)
			}
		})
	}
}

// supplyDef is a synthetic /sys/class/power_supply entry.
type supplyDef struct {
	name  string
	attrs map[string]string
}

func TestBatteryRules(t *testing.T) {
	ac := func(online string) supplyDef {
		return supplyDef{"AC", map[string]string{"type": "Mains", "online": online}}
	}
	bat := func(name string, kv ...string) supplyDef {
		m := map[string]string{"type": "Battery"}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return supplyDef{name, m}
	}
	type want struct {
		onBattery      bool
		percent        int
		health         int
		status         string
		hasBattery     bool
		externalMarker bool
	}
	cases := []struct {
		name     string
		supplies []supplyDef
		want     want
	}{
		{"no supplies", nil, want{percent: -1}},
		{"AC online, battery claims discharging", []supplyDef{ac("1"), bat("BAT0", "status", "Discharging", "capacity", "70")},
			want{percent: 70, status: "Discharging", hasBattery: true}},
		{"AC offline", []supplyDef{ac("0"), bat("BAT0", "status", "Discharging", "capacity", "70")},
			want{onBattery: true, percent: 70, status: "Discharging", hasBattery: true}},
		{"AC offline but battery says charging", []supplyDef{ac("0"), bat("BAT0", "status", "Charging", "capacity", "70")},
			want{onBattery: true, percent: 70, status: "Charging", hasBattery: true}},
		{"two Mains, one online", []supplyDef{ac("0"), {"ADP1", map[string]string{"type": "Mains", "online": "1"}},
			bat("BAT0", "status", "Discharging", "capacity", "70")},
			want{percent: 70, status: "Discharging", hasBattery: true}},
		{"two Mains, both offline", []supplyDef{ac("0"), {"ADP1", map[string]string{"type": "Mains", "online": "0"}},
			bat("BAT0", "status", "Discharging", "capacity", "70")},
			want{onBattery: true, percent: 70, status: "Discharging", hasBattery: true}},
		{"Mains without online file is not offline", []supplyDef{{"AC", map[string]string{"type": "Mains"}},
			bat("BAT0", "status", "Discharging", "capacity", "70")},
			want{percent: 70, status: "Discharging", hasBattery: true}},
		{"no Mains, discharging", []supplyDef{bat("BAT1", "status", "Discharging", "capacity", "33")},
			want{onBattery: true, percent: 33, status: "Discharging", hasBattery: true}},
		{"no Mains, full", []supplyDef{bat("BAT1", "status", "Full", "capacity", "100")},
			want{percent: 100, status: "Full", hasBattery: true}},
		{"no Mains, unknown status", []supplyDef{bat("BAT1", "status", "Unknown", "capacity", "97")},
			want{percent: 97, status: "Unknown", hasBattery: true}},
		{"no battery at all: never on battery", []supplyDef{ac("0")}, want{percent: -1}},
		{"battery slot empty", []supplyDef{ac("0"), bat("BAT0", "present", "0", "status", "Unknown")}, want{percent: -1}},
		{"peripheral battery ignored", []supplyDef{bat("hid-mouse", "scope", "Device", "status", "Discharging", "capacity", "20")},
			want{percent: -1}},
		{"energy fallback", []supplyDef{ac("1"), bat("BAT0", "status", "Charging", "energy_now", "30000000",
			"energy_full", "40000000", "energy_full_design", "50000000")},
			want{percent: 75, health: 80, status: "Charging", hasBattery: true}},
		{"charge fallback", []supplyDef{ac("1"), bat("BAT0", "status", "Charging", "charge_now", "1000000",
			"charge_full", "4000000", "charge_full_design", "5200000")},
			want{percent: 25, health: 77, status: "Charging", hasBattery: true}},
		{"capacity wins over energy", []supplyDef{bat("BAT0", "status", "Full", "capacity", "99",
			"energy_now", "1", "energy_full", "2")},
			want{percent: 99, status: "Full", hasBattery: true}},
		{"capacity above 100 clamped, health above design clamped", []supplyDef{bat("BAT0", "status", "Full",
			"capacity", "104", "energy_full", "52000000", "energy_full_design", "50000000")},
			want{percent: 100, health: 100, status: "Full", hasBattery: true}},
		{"worn out battery", []supplyDef{bat("BAT0", "status", "Full", "capacity", "100",
			"energy_full", "4000000", "energy_full_design", "50000000")},
			want{percent: 100, health: 8, status: "Full", hasBattery: true}},
		{"charge unknown", []supplyDef{bat("BAT0", "status", "Discharging")},
			want{onBattery: true, percent: -1, status: "Discharging", hasBattery: true}},
		{"garbage values", []supplyDef{bat("BAT0", "status", "Discharging", "capacity", "-3",
			"energy_now", "x", "energy_full", "-1")},
			want{onBattery: true, percent: -1, status: "Discharging", hasBattery: true}},
		{"two batteries weighted by capacity", []supplyDef{ac("0"),
			bat("BAT0", "status", "Discharging", "energy_now", "20000000", "energy_full", "40000000", "energy_full_design", "50000000"),
			bat("BAT1", "status", "Unknown", "energy_now", "20000000", "energy_full", "20000000", "energy_full_design", "40000000")},
			want{onBattery: true, percent: 67, health: 65, status: "Discharging", hasBattery: true}},
		{"two batteries, mixed units: plain mean", []supplyDef{
			bat("BAT0", "status", "Charging", "capacity", "40"),
			bat("BAT1", "status", "Full", "charge_now", "2000000", "charge_full", "2000000")},
			want{percent: 70, status: "Charging", hasBattery: true}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			root := t.TempDir()
			writeFile(t, root, "proc/meminfo", "MemTotal: 1024 kB\n")
			for _, s := range c.supplies {
				for k, v := range s.attrs {
					writeFile(t, root, "sys/class/power_supply/"+s.name+"/"+k, v+"\n")
				}
			}
			s, _ := newTestSampler(root, 85)
			m := s.Sample()
			got := want{m.OnBattery, m.BatteryPercent, m.BatteryHealthPercent, m.BatteryStatus, false, false}
			inv, _ := Collect(root)
			got.hasBattery = inv.HasBattery
			if got != c.want {
				t.Errorf("got  %+v\nwant %+v", got, c.want)
			}
			if inv.IsLaptop != inv.HasBattery {
				t.Errorf("IsLaptop %v with HasBattery %v and no DMI", inv.IsLaptop, inv.HasBattery)
			}
		})
	}
}

func TestLid(t *testing.T) {
	cases := []struct {
		states []string
		want   bool
	}{
		{nil, false},
		{[]string{"state:      open"}, false},
		{[]string{"state:      closed"}, true},
		{[]string{"state:      open", "state:      closed"}, true},
		{[]string{""}, false},
	}
	for _, c := range cases {
		root := t.TempDir()
		for i, st := range c.states {
			writeFile(t, root, "proc/acpi/button/lid/LID"+strconv.Itoa(i)+"/state", st+"\n")
		}
		if got := lidClosed(root); got != c.want {
			t.Errorf("lidClosed(%q) = %v, want %v", c.states, got, c.want)
		}
	}
}

func TestReadPSIMemory(t *testing.T) {
	cases := map[string]float64{
		"some avg10=12.34 avg60=1.00 avg300=0.00 total=1\nfull avg10=5.00 avg60=0 avg300=0 total=0\n": 12.34,
		"full avg10=5.00 avg60=0 avg300=0 total=0\nsome avg10=0.50 avg60=1 avg300=0 total=0\n":        0.5,
		"some avg10=abc avg60=1.00\n":   0,
		"some avg10=250.0 avg60=1.00\n": 0,
		"some avg10=-1 avg60=1.00\n":    0,
		"some avg60=3.00\n":             0,
		"":                              0,
	}
	for content, want := range cases {
		root := t.TempDir()
		writeFile(t, root, "proc/pressure/memory", content)
		if got := readPSIMemory(root); got != want {
			t.Errorf("readPSIMemory(%q) = %v, want %v", content, got, want)
		}
	}
	if got := readPSIMemory(t.TempDir()); got != 0 {
		t.Errorf("missing PSI = %v", got)
	}
}

func TestNetBytes(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "proc/net/dev", `Inter-|   Receive                                                |  Transmit
 face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed
    lo: 999999      10    0    0    0     0          0         0   999999      10    0    0    0     0       0          0
  eth0:1000 5 0 0 0 0 0 0 2000 6 0 0 0 0 0 0
 wlan0:   300       3    0    0    0     0          0         0      400       4    0    0    0     0       0          0
 bogus: 1 2 3
  eth1: x 1 0 0 0 0 0 0 5 1 0 0 0 0 0 0
`)
	rx, tx := netBytes(root)
	if rx != 1300 || tx != 2400 {
		t.Fatalf("netBytes = %d, %d; want 1300, 2400", rx, tx)
	}
}

func TestThrottleEvents(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "sys/devices/system/cpu/cpu0/thermal_throttle/core_throttle_count", "3\n")
	writeFile(t, root, "sys/devices/system/cpu/cpu0/thermal_throttle/package_throttle_count", "7\n")
	writeFile(t, root, "sys/devices/system/cpu/cpu0/thermal_throttle/core_throttle_max_time_ms", "999\n")
	writeFile(t, root, "sys/devices/system/cpu/cpu1/thermal_throttle/core_throttle_count", "garbage\n")
	writeFile(t, root, "sys/devices/system/cpu/cpu10/thermal_throttle/core_throttle_count", "5\n")
	writeFile(t, root, "sys/devices/system/cpu/cpufreq/thermal_throttle/core_throttle_count", "100\n")
	if got := throttleEvents(root); got != 15 {
		t.Fatalf("throttleEvents = %d, want 15", got)
	}
}

func TestSampleEmptyRoot(t *testing.T) {
	s, _ := newTestSampler(t.TempDir(), 85)
	m := s.Sample()
	want := proto.Metrics{Time: sampleTime, BatteryPercent: -1, CPUTempLimitC: 85}
	if !reflect.DeepEqual(m, want) {
		t.Fatalf("got %+v, want %+v", m, want)
	}
}

func TestSampleConcurrent(t *testing.T) {
	s := NewSampler(fixture(t, "thinkpad-t60"), 85)
	done := make(chan proto.Metrics)
	for i := 0; i < 4; i++ {
		go func() { done <- s.Sample() }()
	}
	for i := 0; i < 4; i++ {
		if m := <-done; m.CPUTempC != 52 {
			t.Errorf("concurrent sample: %+v", m)
		}
	}
}
