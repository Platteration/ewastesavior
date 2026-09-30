package node

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/platteration/ewastesavior/internal/discovery"
	"github.com/platteration/ewastesavior/internal/proto"
)

func TestSortBeacons(t *testing.T) {
	fpA := "sha256:aa"
	fpB := "sha256:bb"
	seen := []discovery.Candidate{
		{URL: "https://10.0.0.1:7700", Beacon: proto.Beacon{SwarmHint: "h1", Fingerprint: fpA}},
		{URL: "https://10.0.0.2:7700", Beacon: proto.Beacon{SwarmHint: "h1", Fingerprint: fpB}},
		{URL: "https://10.0.0.3:7700", Beacon: proto.Beacon{SwarmHint: "h1"}}, // no fingerprint announced
		{URL: "https://10.0.0.4:7700", Beacon: proto.Beacon{SwarmHint: "h2", Fingerprint: fpA}},
	}
	for _, tc := range []struct {
		name, hint, pin         string
		mine, wrongCert, others []string
	}{
		{"key, not pinned yet", "h1", "",
			[]string{"https://10.0.0.1:7700", "https://10.0.0.2:7700", "https://10.0.0.3:7700"}, nil, []string{"https://10.0.0.4:7700"}},
		{"key, pinned to A", "h1", fpA,
			[]string{"https://10.0.0.1:7700", "https://10.0.0.3:7700"}, []string{"https://10.0.0.2:7700"}, []string{"https://10.0.0.4:7700"}},
		{"keyless, pinned to A", "", fpA,
			[]string{"https://10.0.0.1:7700", "https://10.0.0.4:7700"}, nil, []string{"https://10.0.0.2:7700", "https://10.0.0.3:7700"}},
	} {
		mine, wrongCert, others := sortBeacons(seen, tc.hint, tc.pin)
		if !reflect.DeepEqual(mine, tc.mine) || !reflect.DeepEqual(wrongCert, tc.wrongCert) || !reflect.DeepEqual(others, tc.others) {
			t.Errorf("%s: mine %v wrongCert %v others %v", tc.name, mine, wrongCert, others)
		}
	}
}

func TestFirstCandidatesBounded(t *testing.T) {
	var urls []string
	for i := range 40 {
		u := fmt.Sprintf("https://10.0.0.%d:7700", i%20) // duplicates too
		urls = append(urls, u)
	}
	got := firstCandidates(urls)
	if len(got) != maxCandidates || got[0] != "https://10.0.0.0:7700" || got[maxCandidates-1] != fmt.Sprintf("https://10.0.0.%d:7700", maxCandidates-1) {
		t.Fatalf("firstCandidates kept %d: %v", len(got), got)
	}
}
