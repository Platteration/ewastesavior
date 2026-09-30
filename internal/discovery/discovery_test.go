package discovery

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/platteration/ewastesavior/internal/proto"
)

const (
	hintA = "0a0a0a0a"
	hintB = "0b0b0b0b"
)

var testFP = "sha256:" + strings.Repeat("ab", 32)

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

	b := proto.Beacon{HiveID: "h1", Port: 7700, Fingerprint: testFP, SwarmHint: hintA, Version: "t"}
	go Announce(ctx, b, AnnounceOptions{
		Port:       hivePort,
		Interval:   time.Hour, // rely on probe answers
		ListenAddr: "127.0.0.1:" + strconv.Itoa(hivePort),
		Targets:    []string{"127.0.0.1:" + strconv.Itoa(nodePort)},
	})

	addr, got, err := Discover(ctx, hintA, DiscoverOptions{
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
	go Announce(ctx, proto.Beacon{HiveID: "other", Port: 7700, SwarmHint: hintB}, AnnounceOptions{
		Port: hivePort, Interval: 50 * time.Millisecond,
		ListenAddr: "127.0.0.1:" + strconv.Itoa(hivePort),
		Targets:    []string{"127.0.0.1:" + strconv.Itoa(nodePort)},
	})
	dctx, dcancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer dcancel()
	_, _, err := Discover(dctx, hintA, DiscoverOptions{
		Port: hivePort, ProbeInterval: 50 * time.Millisecond,
		ListenAddr: "127.0.0.1:" + strconv.Itoa(nodePort),
		Targets:    []string{"127.0.0.1:" + strconv.Itoa(hivePort)},
	})
	if err == nil {
		t.Fatal("discovered a hive from another swarm")
	}
}

func TestParseBeacon(t *testing.T) {
	for _, b := range []proto.Beacon{
		{Svc: proto.BeaconService, V: proto.APIVersion, Port: 7700},
		{Svc: proto.BeaconService, V: proto.APIVersion, Port: 7700, HiveID: "h0123456789abcdef", Fingerprint: testFP,
			SwarmHint: hintA, Version: "v0.3.1-14-g0123abcd-dirty"},
	} {
		good, _ := json.Marshal(b)
		if _, ok := ParseBeacon(good); !ok {
			t.Fatalf("good beacon rejected: %s", good)
		}
	}
	for _, bad := range []string{
		`{"svc":"savior-hive","v":1,"port":0}`,
		`{"svc":"other","v":1,"port":7700}`,
		`{"svc":"savior-hive","v":99,"port":7700}`,
		`not json`,
		`{"svc":"savior-hive","v":1,"port":7700,"pad":"` + strings.Repeat("x", 1500) + `"}`,
		// Fields longer or other than the protocol's (DISPLAY-HW-4).
		`{"svc":"savior-hive","v":1,"port":7700,"hive_id":"` + strings.Repeat("h", 65) + `"}`,
		`{"svc":"savior-hive","v":1,"port":7700,"version":"` + strings.Repeat("v", 65) + `"}`,
		`{"svc":"savior-hive","v":1,"port":7700,"swarm_hint":"` + strings.Repeat("0", 9) + `"}`,
		`{"svc":"savior-hive","v":1,"port":7700,"swarm_hint":"hint-a!!"}`,
		`{"svc":"savior-hive","v":1,"port":7700,"fp":"sha256:ab"}`,
		`{"svc":"savior-hive","v":1,"port":7700,"fp":"md5:` + strings.Repeat("a", 64) + `"}`,
		`{"svc":"savior-hive","v":1,"port":7700,"fp":"sha256:` + strings.Repeat("g", 64) + `"}`,
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
	go Announce(ctx, proto.Beacon{HiveID: "x", Port: 7700, SwarmHint: hintB}, AnnounceOptions{
		Port: hivePort, Interval: 50 * time.Millisecond,
		ListenAddr: "127.0.0.1:" + strconv.Itoa(hivePort),
		Targets:    []string{"127.0.0.1:" + strconv.Itoa(nodePort)},
	})
	got, err := Collect(ctx, 400*time.Millisecond, DiscoverOptions{
		Port: hivePort, ProbeInterval: 50 * time.Millisecond,
		ListenAddr: "127.0.0.1:" + strconv.Itoa(nodePort),
		Targets:    []string{"127.0.0.1:" + strconv.Itoa(hivePort)},
	})
	if err != nil || len(got) != 1 || got[0].Beacon.SwarmHint != hintB {
		t.Fatalf("collect: %v %+v", err, got)
	}
}

// One host sending many distinct beacons can make a searching node keep
// neither unbounded memory nor more than a few of the candidates, so the
// real hive (another address) is still returned (DISPLAY-HW-4).
func TestCollectIsBounded(t *testing.T) {
	nodePort := freeUDPPort(t)
	flood, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer flood.Close()
	real, err := net.ListenPacket("udp4", "127.0.0.2:0")
	if err != nil {
		t.Skipf("no second loopback address: %v", err)
	}
	defer real.Close()
	dst := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: nodePort}
	beacon := func(id string, port int) []byte {
		b, _ := json.Marshal(proto.Beacon{Svc: proto.BeaconService, V: proto.APIVersion, HiveID: id, Port: port,
			SwarmHint: hintA, Fingerprint: testFP, Version: strings.Repeat("v", 60)})
		return b
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sent := make(chan int, 1)
	go func() {
		n := 0
		defer func() { sent <- n }()
		for ctx.Err() == nil {
			for i := 0; i < 50; i++ {
				flood.WriteTo(beacon(fmt.Sprintf("h%07d", n), 1+n%65535), dst)
				n++
			}
			real.WriteTo(beacon("hreal", 7700), dst)
			time.Sleep(time.Millisecond)
		}
	}()
	got, err := Collect(ctx, 800*time.Millisecond, DiscoverOptions{
		Port: freeUDPPort(t), ProbeInterval: time.Hour,
		ListenAddr: "127.0.0.1:" + strconv.Itoa(nodePort),
		Targets:    []string{"127.0.0.1:9"},
	})
	cancel()
	n := <-sent
	if err != nil {
		t.Fatal(err)
	}
	perSource := map[string]int{}
	foundReal := false
	for _, c := range got {
		perSource[strings.TrimPrefix(c.URL[:strings.LastIndex(c.URL, ":")], "https://")]++
		foundReal = foundReal || c.URL == "https://127.0.0.2:7700"
	}
	t.Logf("sent %d flood beacons; collected %d candidates %v", n, len(got), perSource)
	if len(got) > MaxCandidates || perSource["127.0.0.1"] > maxPerSource {
		t.Fatalf("collected %d candidates (%d from the flooding host), want at most %d (%d per host)",
			len(got), perSource["127.0.0.1"], MaxCandidates, maxPerSource)
	}
	if !foundReal {
		t.Fatalf("the real hive was crowded out: %v", perSource)
	}
}
