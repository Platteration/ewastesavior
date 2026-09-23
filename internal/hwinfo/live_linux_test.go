//go:build linux

package hwinfo

import (
	"runtime"
	"testing"
	"time"
)

// These run against the machine executing the tests; they check only what
// every Linux system has.
func TestLiveCollect(t *testing.T) {
	inv, err := Collect("")
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if inv.Cores < 1 || inv.MemTotalMB < 1 || inv.Kernel == "" || inv.MachineArch == "" || inv.Arch != runtime.GOARCH {
		t.Fatalf("implausible live inventory: %+v", inv)
	}
	if MemTotalMB("/") != inv.MemTotalMB || MemAvailableMB("") < 1 {
		t.Fatalf("MemTotalMB/MemAvailableMB disagree with Collect")
	}
}

func TestLiveIdentity(t *testing.T) {
	id, err := GetIdentity("/")
	if err != nil {
		t.Fatal(err)
	}
	if !nodeIDRE.MatchString(id.NodeID) || id.NodeID != NodeIDFromSource(id.Source) {
		t.Fatalf("identity = %+v", id)
	}
	if BootID("") == "" {
		t.Fatal("no boot id")
	}
}

func TestLiveSampler(t *testing.T) {
	s := NewSampler("", 85)
	time.Sleep(20 * time.Millisecond)
	m := s.Sample()
	if m.UptimeS <= 0 || m.MemAvailableMB <= 0 || m.CPUPercent < 0 || m.CPUPercent > 100 || m.CPUTempLimitC <= 0 {
		t.Fatalf("implausible live metrics: %+v", m)
	}
}
