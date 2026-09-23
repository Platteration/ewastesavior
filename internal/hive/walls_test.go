package hive

import (
	"net/http"
	"testing"
	"time"

	"github.com/platteration/ewastesavior/internal/proto"
)

func fixedSize(w, h int) func(proto.WallCell) (int, int) {
	return func(c proto.WallCell) (int, int) {
		if c.WidthMM > 0 {
			return c.WidthMM, c.HeightMM
		}
		return w, h
	}
}

func TestWallLayoutEqualScreens(t *testing.T) {
	t.Parallel()
	w := &proto.WallSpec{Rows: 2, Cols: 2, Cells: []proto.WallCell{
		{Node: "a", Row: 0, Col: 0}, {Node: "b", Row: 0, Col: 1}, {Node: "c", Row: 1, Col: 0}, {Node: "d", Row: 1, Col: 1}}}
	cw, ch, r := wallLayout(w, fixedSize(400, 300))
	want := []proto.Rect{rc(0, 0, 400, 300), rc(400, 0, 400, 300), rc(0, 300, 400, 300), rc(400, 300, 400, 300)}
	if cw != 800 || ch != 600 {
		t.Fatalf("canvas %dx%d", cw, ch)
	}
	for i := range want {
		if r[i] != want[i] {
			t.Fatalf("cell %d: %+v want %+v", i, r[i], want[i])
		}
	}
	// Bezels: gaps between visible areas.
	w.GapXMM, w.GapYMM = 20, 10
	cw, ch, r = wallLayout(w, fixedSize(400, 300))
	want = []proto.Rect{rc(0, 0, 400, 300), rc(420, 0, 400, 300), rc(0, 310, 400, 300), rc(420, 310, 400, 300)}
	if cw != 820 || ch != 610 {
		t.Fatalf("canvas with gaps %dx%d", cw, ch)
	}
	for i := range want {
		if r[i] != want[i] {
			t.Fatalf("gap cell %d: %+v want %+v", i, r[i], want[i])
		}
	}
}

func TestWallLayoutMixedSizesAndRect(t *testing.T) {
	t.Parallel()
	// Column 0 holds a 400x300 and a 500x400 screen: the column is 500 wide
	// and the smaller one is centered in its slot.
	w := &proto.WallSpec{Rows: 2, Cols: 2, GapXMM: 10, Cells: []proto.WallCell{
		{Node: "a", Row: 0, Col: 0, WidthMM: 400, HeightMM: 300},
		{Node: "b", Row: 1, Col: 0, WidthMM: 500, HeightMM: 400},
		{Node: "c", Row: 0, Col: 1, WidthMM: 600, HeightMM: 350},
	}}
	cw, ch, r := wallLayout(w, fixedSize(0, 0))
	// colW = [500, 600], rowH = [350, 400]
	want := []proto.Rect{rc(50, 25, 400, 300), rc(0, 350, 500, 400), rc(510, 0, 600, 350)}
	for i := range want {
		if r[i] != want[i] {
			t.Fatalf("mixed cell %d: %+v want %+v", i, r[i], want[i])
		}
	}
	if cw != 1110 || ch != 750 {
		t.Fatalf("mixed canvas %dx%d", cw, ch)
	}
	// Rect overrides placement; the canvas is the bounding box from 0,0.
	w = &proto.WallSpec{Rows: 1, Cols: 2, Cells: []proto.WallCell{
		{Node: "a", Row: 0, Col: 0, Rect: &proto.Rect{X: 100, Y: 50, W: 300, H: 200}},
		{Node: "b", Row: 0, Col: 1, Rect: &proto.Rect{X: 450, Y: 80, W: 300, H: 200}},
	}}
	cw, ch, r = wallLayout(w, fixedSize(400, 300))
	if cw != 650 || ch != 230 || r[0] != rc(0, 0, 300, 200) || r[1] != rc(350, 30, 300, 200) {
		t.Fatalf("rects: %dx%d %+v", cw, ch, r)
	}
}

func TestWallsAPI(t *testing.T) {
	t.Parallel()
	h := newHive(t, nil)
	names := []string{"w1", "w2", "w3", "w4"}
	nodes := map[string]*testNode{}
	for _, name := range names {
		name := name
		n := h.newNode(func(r *proto.RegisterRequest) {
			r.Name = name
			r.Inventory.Connectors = nil // no EDID: 400x300 default
		})
		n.register()
		nodes[name] = n
	}
	compute := h.newNode(func(r *proto.RegisterRequest) { r.Name = "cpu"; r.Roles = []proto.Role{proto.RoleCompute} })
	compute.register()
	img := h.putBlob(testPNG(t, 32, 18))
	spec := proto.WallSpec{Name: "lobby", Rows: 2, Cols: 2, GapXMM: 20, Cells: []proto.WallCell{
		{Node: "w1", Row: 0, Col: 0}, {Node: "w2", Row: 0, Col: 1}, {Node: nodes["w3"].req.NodeID, Row: 1, Col: 0}, {Node: "w4", Row: 1, Col: 1}},
		Content: proto.DisplaySpec{Mode: proto.DisplayImage, Image: &proto.Media{Blob: img}, Fit: "cover"}}
	var saved proto.WallSpec
	h.mustAdmin("POST", "walls", spec, &saved)
	if saved.ID == "" || saved.Cells[0].Node != nodes["w1"].req.NodeID || saved.Content.Image.Width != 32 {
		t.Fatalf("saved: %+v", saved)
	}
	d := nodes["w2"].heartbeat().Directives.Display
	if d == nil || d.Mode != proto.DisplayWall || d.Rev == 0 || d.Wall == nil {
		t.Fatalf("directive: %+v", d)
	}
	tile := *d.Wall
	if tile.WallID != saved.ID || tile.CanvasW != 820 || tile.CanvasH != 600 || tile.X != 420 || tile.Y != 0 ||
		tile.W != 400 || tile.H != 300 || tile.Label != "R1C2 w2" || tile.Content == nil || tile.Content.Image.Blob != img {
		t.Fatalf("tile: %+v", tile)
	}
	if err := proto.ValidateDisplaySpec(d); err != nil {
		t.Fatalf("pushed spec invalid: %v", err)
	}
	// Wall members may fetch the content blob.
	if code, _ := nodes["w4"].api("GET", "blobs/"+img, nil, nil); code != 200 {
		t.Fatalf("wall member blob access: %d", code)
	}

	// Conflict rules.
	if st := h.admin("PATCH", "nodes/w1", proto.NodePatch{Display: &proto.DisplaySpec{Mode: proto.DisplayClock}}, nil); st != http.StatusConflict {
		t.Fatalf("display patch of a wall member: %d", st)
	}
	if st := h.admin("PATCH", "nodes/cpu", proto.NodePatch{Display: &proto.DisplaySpec{Mode: proto.DisplayWall,
		Wall: &proto.WallTile{CanvasW: 1, CanvasH: 1, W: 1, H: 1, Content: &proto.DisplaySpec{Mode: proto.DisplayTest}}}}, nil); st != http.StatusBadRequest {
		t.Fatalf("mode wall via patch: %d", st)
	}
	second := proto.WallSpec{Rows: 1, Cols: 1, Cells: []proto.WallCell{{Node: "w1"}}, Content: proto.DisplaySpec{Mode: proto.DisplayTest}}
	if st := h.admin("POST", "walls", second, nil); st != http.StatusBadRequest {
		t.Fatalf("node in two walls: %d", st)
	}
	second.Cells[0].Node = "cpu"
	if st := h.admin("POST", "walls", second, nil); st != http.StatusBadRequest {
		t.Fatalf("node without the display role: %d", st)
	}
	second.Cells[0].Node = "ghost"
	if st := h.admin("POST", "walls", second, nil); st != http.StatusBadRequest {
		t.Fatalf("unknown node: %d", st)
	}
	if st := h.admin("DELETE", "nodes/w1", nil, nil); st != http.StatusConflict {
		t.Fatalf("delete of an online wall member: %d", st)
	}

	// Rotating a member recomputes its tile (EDID-less: explicit sizes stay).
	h.mustAdmin("PATCH", "nodes/w2", proto.NodePatch{DisplayRotate: ptr(180)}, nil)
	hb := nodes["w2"].heartbeat()
	if hb.Directives.DisplayRotate != 180 || hb.Directives.Display.Rev <= d.Rev {
		t.Fatalf("rotate: %+v", hb.Directives)
	}

	// Shrinking the wall frees the removed nodes.
	spec.Rows, spec.Cells = 1, spec.Cells[:2]
	h.mustAdmin("PUT", "walls/"+saved.ID, spec, nil)
	if d := nodes["w3"].heartbeat().Directives.Display; d == nil || d.Mode != proto.DisplayStatus {
		t.Fatalf("removed member: %+v", d)
	}
	if v := h.nodeView("w3"); v.WallID != "" {
		t.Fatal("wall id kept")
	}
	var walls []proto.WallSpec
	h.mustAdmin("GET", "walls", nil, &walls)
	if len(walls) != 1 || len(walls[0].Cells) != 2 {
		t.Fatalf("walls: %+v", walls)
	}
	h.mustAdmin("DELETE", "walls/"+saved.ID, nil, nil)
	if d := nodes["w1"].heartbeat().Directives.Display; d == nil || d.Mode != proto.DisplayStatus {
		t.Fatalf("after delete: %+v", d)
	}
	if st := h.admin("GET", "walls/"+saved.ID, nil, nil); st != http.StatusNotFound {
		t.Fatalf("deleted wall: %d", st)
	}
	// Now w1 can get its own display again.
	h.mustAdmin("PATCH", "nodes/w1", proto.NodePatch{Display: &proto.DisplaySpec{Mode: proto.DisplayText, Text: "hi"}}, nil)
}

func TestWallUsesEDIDAndRotation(t *testing.T) {
	t.Parallel()
	h := newHive(t, nil)
	a := h.newNode(func(r *proto.RegisterRequest) {
		r.Name = "edid"
		r.DisplayRotate = 90
		r.Inventory.Connectors = []proto.Connector{
			{Name: "VGA-1", Status: "disconnected", WidthMM: 999, HeightMM: 999},
			{Name: "LVDS-1", Status: "connected", WidthMM: 300, HeightMM: 200},
		}
	})
	a.register()
	b := h.newNode(func(r *proto.RegisterRequest) { r.Name = "plain"; r.Inventory.Connectors = nil })
	b.register()
	h.mustAdmin("POST", "walls", proto.WallSpec{Rows: 1, Cols: 2, Cells: []proto.WallCell{{Node: "edid"}, {Node: "plain", Col: 1}},
		Content: proto.DisplaySpec{Mode: proto.DisplayColor, BG: "#ff0000"}}, nil)
	tile := a.heartbeat().Directives.Display.Wall
	// 300x200 panel rotated by 90 degrees is 200 wide, 300 tall.
	if tile.W != 200 || tile.H != 300 || tile.CanvasW != 600 || tile.CanvasH != 300 {
		t.Fatalf("EDID tile: %+v", tile)
	}
	if bt := b.heartbeat().Directives.Display.Wall; bt.X != 200 || bt.W != 400 || bt.H != 300 || bt.Label != "R1C2 plain" {
		t.Fatalf("default-size tile: %+v", bt)
	}
}

func rc(x, y, w, h int) proto.Rect { return proto.Rect{X: x, Y: y, W: w, H: h} }

func TestTakeoverKeepsWallCell(t *testing.T) {
	t.Parallel()
	h := newHive(t, func(c *Config) { c.OfflineAfter = 150 * time.Millisecond })
	a := h.newNode(func(r *proto.RegisterRequest) { r.Name = "left" })
	a.register()
	b := h.newNode(func(r *proto.RegisterRequest) { r.Name = "right" })
	b.register()
	var wall proto.WallSpec
	h.mustAdmin("POST", "walls", proto.WallSpec{Rows: 1, Cols: 2, Cells: []proto.WallCell{{Node: "left"}, {Node: "right", Col: 1}},
		Content: proto.DisplaySpec{Mode: proto.DisplayTest}}, &wall)
	time.Sleep(200 * time.Millisecond)
	// "left" comes back with a new node ID (new NIC) but the same UUID.
	nw := h.newNode(func(r *proto.RegisterRequest) { r.HWIDs = a.req.HWIDs })
	resp := nw.register()
	if resp.Name != "left" || resp.Directives.Display == nil || resp.Directives.Display.Wall == nil ||
		resp.Directives.Display.Wall.WallID != wall.ID || resp.Directives.Display.Wall.Col != 0 {
		t.Fatalf("takeover lost the wall cell: %+v %+v (wall %s)", resp.Name, resp.Directives.Display.Wall, wall.ID)
	}
	var got proto.WallSpec
	h.mustAdmin("GET", "walls/"+wall.ID, nil, &got)
	if got.Cells[0].Node != nw.req.NodeID {
		t.Fatalf("wall still references the old ID: %+v", got.Cells)
	}
	if v := h.nodeView("left"); v.WallID != wall.ID {
		t.Fatalf("wall id: %+v", v.WallID)
	}
}

func TestWallLayoutEmptySlotAndRounding(t *testing.T) {
	t.Parallel()
	// A missing screen in the middle keeps its slot (default 400 mm) so the
	// outer screens stay where they physically are.
	w := &proto.WallSpec{Rows: 1, Cols: 3, GapXMM: 10, Cells: []proto.WallCell{
		{Node: "a", Col: 0}, {Node: "c", Col: 2}}}
	cw, ch, r := wallLayout(w, fixedSize(400, 300))
	if cw != 1220 || ch != 300 || r[0] != rc(0, 0, 400, 300) || r[1] != rc(820, 0, 400, 300) {
		t.Fatalf("empty slot: %dx%d %+v", cw, ch, r)
	}
	// Centering rounds half up, like the dashboard preview.
	w = &proto.WallSpec{Rows: 2, Cols: 1, Cells: []proto.WallCell{
		{Node: "a", Row: 0, WidthMM: 501, HeightMM: 300}, {Node: "b", Row: 1, WidthMM: 400, HeightMM: 300}}}
	if _, _, r = wallLayout(w, fixedSize(0, 0)); r[1].X != 51 {
		t.Fatalf("rounding: %+v", r[1])
	}
}
