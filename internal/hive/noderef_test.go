package hive

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/platteration/ewastesavior/internal/proto"
)

// refTestIDs returns node IDs for the short-code tests: twins share a short
// code, others[i] have codes distinct from each other and from the twins'.
func refTestIDs(seed string, n int) (twins [2]string, others []string) {
	first := map[string]string{} // code -> first ID seen with it
	var ids []string
	for i := 0; twins[0] == ""; i++ {
		sum := sha256.Sum256([]byte(fmt.Sprintf("%s-%d", seed, i)))
		id := "n" + hex.EncodeToString(sum[:6])
		c := proto.ShortCode(id)
		if prev, ok := first[c]; ok {
			twins = [2]string{prev, id}
			break
		}
		first[c] = id
		ids = append(ids, id)
	}
	used := map[string]bool{proto.ShortCode(twins[0]): true}
	for _, id := range ids {
		if c := proto.ShortCode(id); !used[c] && len(others) < n {
			used[c] = true
			others = append(others, id)
		}
	}
	return twins, others
}

// TOOLING-7: every node reference (GET/PATCH/DELETE nodes/{ref}, actions,
// identify, wall cells) also accepts the 3-character short code a node's
// screen shows, in any case, after the ID and the name. A code shared by
// several nodes is refused with a clear error rather than guessed.
func TestNodeRefShortCode(t *testing.T) {
	t.Parallel()
	if len(proto.ShortCode("x")) != shortCodeLen {
		t.Fatalf("shortCodeLen %d, proto.ShortCode is %d characters", shortCodeLen, len(proto.ShortCode("x")))
	}
	h := newHive(t, nil)
	twins, others := refTestIDs(t.Name(), 3)
	reg := func(id, name string) *testNode {
		n := h.newNode(func(r *proto.RegisterRequest) { r.NodeID = id; r.Name = name })
		n.register()
		return n
	}
	a := reg(others[0], "")
	reg(twins[0], "twin-a")
	reg(twins[1], "twin-b")
	shadowed := reg(others[1], "")
	// A node named like the other node's short code: the name wins.
	named := reg(others[2], strings.ToLower(proto.ShortCode(shadowed.req.NodeID)))

	raw := func(method, path string, in any) (int, string) {
		t.Helper()
		st, b := do(t, h.hc, method, h.url+"/api/v1/admin/"+path, testAdmin, in, nil)
		return st, string(b)
	}

	// The identify-then-rename workflow of the user guide.
	code := proto.ShortCode(a.req.NodeID)
	for _, ref := range []string{code, strings.ToLower(code)} {
		if v := h.nodeView(ref); v.ID != a.req.NodeID {
			t.Fatalf("GET nodes/%s = %s, want %s", ref, v.ID, a.req.NodeID)
		}
	}
	h.mustAdmin("POST", "identify", proto.IdentifyRequest{Nodes: []string{strings.ToLower(code)}}, nil)
	if acts := a.heartbeat().Directives.Actions; len(acts) != 1 || acts[0].Action != proto.ActionIdentify {
		t.Fatalf("identify by code: %+v", acts)
	}
	var v proto.NodeView
	name := "lab-shelf-3"
	h.mustAdmin("PATCH", "nodes/"+strings.ToLower(code), proto.NodePatch{Name: &name}, &v)
	if v.ID != a.req.NodeID || v.Name != name {
		t.Fatalf("rename by code: %+v", v)
	}
	yes := true
	h.mustAdmin("PATCH", "nodes/"+code, proto.NodePatch{Drain: &yes}, &v)
	if v.ID != a.req.NodeID || !v.Drain {
		t.Fatalf("drain by code: %+v", v)
	}
	h.mustAdmin("POST", "nodes/"+code+"/action", proto.NodeAction{Action: proto.ActionIdentify}, nil)
	if acts := a.heartbeat().Directives.Actions; len(acts) != 2 {
		t.Fatalf("action by code: %+v", acts)
	}
	wall := proto.WallSpec{Rows: 1, Cols: 1, Cells: []proto.WallCell{{Node: strings.ToLower(code)}},
		Content: proto.DisplaySpec{Mode: proto.DisplayTest}}
	var saved proto.WallSpec
	h.mustAdmin("POST", "walls", wall, &saved)
	if saved.Cells[0].Node != a.req.NodeID {
		t.Fatalf("wall cell by code: %+v", saved.Cells)
	}
	h.mustAdmin("DELETE", "walls/"+saved.ID, nil, nil)

	// The name beats another node's short code; that node keeps its ID.
	shadow := proto.ShortCode(shadowed.req.NodeID)
	if v := h.nodeView(shadow); v.ID != named.req.NodeID {
		t.Fatalf("GET nodes/%s = %s, want the node named %s", shadow, v.ID, named.req.Name)
	}
	if v := h.nodeView(shadowed.req.NodeID); v.ID != shadowed.req.NodeID {
		t.Fatal("lookup by ID")
	}

	// A shared code is refused everywhere and changes nothing.
	tc := strings.ToLower(proto.ShortCode(twins[0]))
	want := "ambiguous short code " + strings.ToUpper(tc) + ": nodes twin-a, twin-b"
	for _, c := range []struct {
		method, path string
		in           any
	}{
		{"GET", "nodes/" + tc, nil},
		{"PATCH", "nodes/" + tc, proto.NodePatch{Drain: &yes}},
		{"DELETE", "nodes/" + tc, nil},
		{"POST", "nodes/" + tc + "/action", proto.NodeAction{Action: proto.ActionReboot}},
		{"POST", "identify", proto.IdentifyRequest{Nodes: []string{tc}}},
	} {
		if st, body := raw(c.method, c.path, c.in); st != http.StatusConflict || !strings.Contains(body, want) {
			t.Errorf("%s %s: %d %s, want 409 %q", c.method, c.path, st, body, want)
		}
	}
	wall.Cells[0].Node = tc
	if st, body := raw("POST", "walls", wall); st != http.StatusBadRequest || !strings.Contains(body, want) {
		t.Errorf("wall with a shared code: %d %s", st, body)
	}
	for _, id := range twins {
		if v := h.nodeView(id); v.Drain {
			t.Errorf("%s drained through an ambiguous code", v.Name)
		}
	}

	// A code nobody shows, and a 3-character ref that is no code at all.
	inUse := map[string]bool{}
	for _, id := range append(others, twins[:]...) {
		inUse[proto.ShortCode(id)] = true
	}
	free := "ZZZ"
	for _, c := range []string{"ZZZ", "ZZY", "ZZX", "ZZW"} {
		if !inUse[c] {
			free = c
			break
		}
	}
	for _, ref := range []string{free, "i-o"} {
		if st, body := raw("GET", "nodes/"+ref, nil); st != http.StatusNotFound || !strings.Contains(body, "no such node") {
			t.Errorf("GET nodes/%s: %d %s", ref, st, body)
		}
	}
	if st, body := raw("POST", "identify", proto.IdentifyRequest{Nodes: []string{free}}); st != http.StatusNotFound {
		t.Errorf("identify %s: %d %s", free, st, body)
	}
}
