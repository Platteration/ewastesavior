package discovery

import (
	"context"
	"encoding/json"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/platteration/ewastesavior/internal/proto"
)

func freeUDPPort(t *testing.T) int {
	t.Helper()
	c, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	return c.LocalAddr().(*net.UDPAddr).Port
}

func TestProbeAndBeacon(t *testing.T) {
	hivePort := freeUDPPort(t)
	nodePort := freeUDPPort(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	b := proto.Beacon{HiveID: "h1", Port: 7700, Fingerprint: "sha256:ab", SwarmHint: "hint-a", Version: "t"}
	go Announce(ctx, b, AnnounceOptions{
		Port:       hivePort,
		Interval:   time.Hour, // rely on probe answers
		ListenAddr: "127.0.0.1:" + strconv.Itoa(hivePort),
		Targets:    []string{"127.0.0.1:" + strconv.Itoa(nodePort)},
	})

	addr, got, err := Discover(ctx, "hint-a", DiscoverOptions{
		Port:          hivePort,
		ProbeInterval: 100 * time.Millisecond,
		ListenAddr:    "127.0.0.1:" + strconv.Itoa(nodePort),
		Targets:       []string{"127.0.0.1:" + strconv.Itoa(hivePort)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if addr != "https://127.0.0.1:7700" || got.HiveID != "h1" || got.Svc != proto.BeaconService {
		t.Fatalf("got %s %+v", addr, got)
	}
}

func TestWrongSwarmIgnored(t *testing.T) {
	hivePort := freeUDPPort(t)
	nodePort := freeUDPPort(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Announce(ctx, proto.Beacon{HiveID: "other", Port: 7700, SwarmHint: "hint-b"}, AnnounceOptions{
		Port: hivePort, Interval: 50 * time.Millisecond,
		ListenAddr: "127.0.0.1:" + strconv.Itoa(hivePort),
		Targets:    []string{"127.0.0.1:" + strconv.Itoa(nodePort)},
	})
	dctx, dcancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer dcancel()
	_, _, err := Discover(dctx, "hint-a", DiscoverOptions{
		Port: hivePort, ProbeInterval: 50 * time.Millisecond,
		ListenAddr: "127.0.0.1:" + strconv.Itoa(nodePort),
		Targets:    []string{"127.0.0.1:" + strconv.Itoa(hivePort)},
	})
	if err == nil {
		t.Fatal("discovered a hive from another swarm")
	}
}

func TestParseBeacon(t *testing.T) {
	good, _ := json.Marshal(proto.Beacon{Svc: proto.BeaconService, V: proto.APIVersion, Port: 7700})
	if _, ok := ParseBeacon(good); !ok {
		t.Fatal("good beacon rejected")
	}
	for _, bad := range []string{
		`{"svc":"savior-hive","v":1,"port":0}`,
		`{"svc":"other","v":1,"port":7700}`,
		`{"svc":"savior-hive","v":99,"port":7700}`,
		`not json`,
		`{"svc":"savior-hive","v":1,"port":7700,"pad":"` + strings.Repeat("x", 1500) + `"}`,
	} {
		if _, ok := ParseBeacon([]byte(bad)); ok {
			t.Errorf("accepted %.60s", bad)
		}
	}
}

func TestBroadcastTargets(t *testing.T) {
	ts := BroadcastTargets(7701)
	if len(ts) == 0 || ts[0] != "255.255.255.255:7701" {
		t.Fatalf("targets %v", ts)
	}
}

func TestProbePadding(t *testing.T) {
	if n := len(NewProbe()); n < proto.MinProbeSize || n > proto.MinProbeSize+8 {
		t.Fatalf("probe size %d", n)
	}
}

func TestCollectSeesOtherSwarms(t *testing.T) {
	hivePort := freeUDPPort(t)
	nodePort := freeUDPPort(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Announce(ctx, proto.Beacon{HiveID: "x", Port: 7700, SwarmHint: "other"}, AnnounceOptions{
		Port: hivePort, Interval: 50 * time.Millisecond,
		ListenAddr: "127.0.0.1:" + strconv.Itoa(hivePort),
		Targets:    []string{"127.0.0.1:" + strconv.Itoa(nodePort)},
	})
	got, err := Collect(ctx, 400*time.Millisecond, DiscoverOptions{
		Port: hivePort, ProbeInterval: 50 * time.Millisecond,
		ListenAddr: "127.0.0.1:" + strconv.Itoa(nodePort),
		Targets:    []string{"127.0.0.1:" + strconv.Itoa(hivePort)},
	})
	if err != nil || len(got) != 1 || got[0].Beacon.SwarmHint != "other" {
		t.Fatalf("collect: %v %+v", err, got)
	}
}
