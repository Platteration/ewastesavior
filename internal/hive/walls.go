package hive

import (
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/platteration/ewastesavior/internal/auth"
	"github.com/platteration/ewastesavior/internal/proto"
)

// Default cell size when neither the wall nor EDID gives one.
const (
	defaultCellWMM = 400
	defaultCellHMM = 300
)

// wallLayout computes each cell's rectangle on the wall canvas in mm, per
// the proto.WallSpec doc: column widths and row heights are the maximum of
// their cells, gaps separate neighbors, cells are centered in their slot,
// Rect overrides, and the canvas is the bounding box (moved to 0,0).
// size returns a cell's visible width and height.
func wallLayout(w *proto.WallSpec, size func(c proto.WallCell) (int, int)) (cw, ch int, rects []proto.Rect) {
	colW := make([]int, w.Cols)
	rowH := make([]int, w.Rows)
	sizes := make([][2]int, len(w.Cells))
	for i, c := range w.Cells {
		cwid, chei := size(c)
		sizes[i] = [2]int{cwid, chei}
		if c.Rect != nil {
			continue
		}
		if cwid > colW[c.Col] {
			colW[c.Col] = cwid
		}
		if chei > rowH[c.Row] {
			rowH[c.Row] = chei
		}
	}
	// An empty row or column (a missing screen) keeps a default-sized slot
	// so the rest of the wall stays where it physically is.
	colX := make([]int, w.Cols)
	x := 0
	for c := range colW {
		if colW[c] == 0 {
			colW[c] = defaultCellWMM
		}
		colX[c] = x
		x += colW[c] + w.GapXMM
	}
	rowY := make([]int, w.Rows)
	y := 0
	for r := range rowH {
		if rowH[r] == 0 {
			rowH[r] = defaultCellHMM
		}
		rowY[r] = y
		y += rowH[r] + w.GapYMM
	}
	rects = make([]proto.Rect, len(w.Cells))
	for i, c := range w.Cells {
		if c.Rect != nil {
			rects[i] = *c.Rect
			continue
		}
		sw, sh := sizes[i][0], sizes[i][1]
		rects[i] = proto.Rect{ // centered, rounding half up
			X: colX[c.Col] + (colW[c.Col]-sw+1)/2,
			Y: rowY[c.Row] + (rowH[c.Row]-sh+1)/2,
			W: sw,
			H: sh,
		}
	}
	minX, minY, maxX, maxY := rects[0].X, rects[0].Y, rects[0].X+rects[0].W, rects[0].Y+rects[0].H
	for _, r := range rects[1:] {
		minX, minY = min(minX, r.X), min(minY, r.Y)
		maxX, maxY = max(maxX, r.X+r.W), max(maxY, r.Y+r.H)
	}
	for i := range rects {
		rects[i].X -= minX
		rects[i].Y -= minY
	}
	return maxX - minX, maxY - minY, rects
}

// cellSizeLocked is a cell's visible size: WidthMM/HeightMM, else the EDID
// size of the node's first connected connector that reports one (swapped
// for 90/270 rotation), else 400x300 mm.
func (s *Server) cellSizeLocked(c proto.WallCell) (int, int) {
	wmm, hmm := 0, 0
	if n := s.nodes[c.Node]; n != nil {
		for _, con := range n.Inventory.Connectors {
			if con.Status != "connected" || con.WidthMM <= 0 || con.HeightMM <= 0 {
				continue
			}
			wmm, hmm = con.WidthMM, con.HeightMM
			if r := n.effectiveRotate(); r == 90 || r == 270 {
				wmm, hmm = hmm, wmm
			}
			break
		}
	}
	if wmm == 0 {
		wmm, hmm = defaultCellWMM, defaultCellHMM
	}
	if c.WidthMM > 0 {
		wmm = c.WidthMM
	}
	if c.HeightMM > 0 {
		hmm = c.HeightMM
	}
	return wmm, hmm
}

// errNoSuchNode: a node reference matches no node.
var errNoSuchNode = errors.New("no such node")

// shortCodeLen is the length of proto.ShortCode.
const shortCodeLen = 3

// ambiguousCodeError: a short code shown by more than one node.
type ambiguousCodeError struct {
	code  string
	names []string // sorted
}

func (e *ambiguousCodeError) Error() string {
	return fmt.Sprintf("ambiguous short code %s: nodes %s all show it; use the node's name or ID",
		e.code, strings.Join(e.names, ", "))
}

// resolveNodeRefLocked finds a node by ID, else by name, else by the
// 3-character short code its screen shows (identify), in any case. Short
// codes are 15 bits, so several nodes can share one: a code that matches
// more than one node is an *ambiguousCodeError, never a guess.
func (s *Server) resolveNodeRefLocked(ref string) (*node, error) {
	if n := s.nodes[ref]; n != nil {
		return n, nil
	}
	lower := strings.ToLower(ref)
	for _, n := range s.nodes {
		if n.Name == lower {
			return n, nil
		}
	}
	if len(ref) != shortCodeLen {
		return nil, errNoSuchNode
	}
	code := strings.ToUpper(ref)
	var names []string
	var match *node
	for _, n := range s.nodes {
		if n.shortCode == code {
			match = n
			names = append(names, n.Name)
		}
	}
	switch len(names) {
	case 0:
		return nil, errNoSuchNode
	case 1:
		return match, nil
	}
	sort.Strings(names)
	return nil, &ambiguousCodeError{code: code, names: names}
}

// writeNodeRefErr answers a failed resolveNodeRefLocked: 404 for no match,
// 409 for an ambiguous short code.
func writeNodeRefErr(w http.ResponseWriter, err error) {
	var amb *ambiguousCodeError
	if errors.As(err, &amb) {
		writeErr(w, http.StatusConflict, "%s", amb.Error())
		return
	}
	writeErr(w, http.StatusNotFound, "no such node")
}

// prepareWallLocked validates a wall for saving (DESIGN 9 "Walls"): node
// references resolve to IDs, nodes exist, have the display role and are in
// no other wall. It returns the normalized spec.
func (s *Server) prepareWallLocked(in proto.WallSpec, id string) (*proto.WallSpec, error) {
	w := in
	w.ID = id
	w.Name = proto.Sanitize(w.Name, 64, false)
	w.Cells = append([]proto.WallCell(nil), in.Cells...)
	for i := range w.Cells {
		n, err := s.resolveNodeRefLocked(w.Cells[i].Node)
		if errors.Is(err, errNoSuchNode) {
			return nil, fmt.Errorf("unknown node %q", proto.Sanitize(w.Cells[i].Node, 64, false))
		} else if err != nil {
			return nil, err
		}
		if !n.hasRole(proto.RoleDisplay) {
			return nil, fmt.Errorf("node %s does not have the display role", n.Name)
		}
		if n.WallID != "" && n.WallID != id {
			return nil, fmt.Errorf("node %s is already in wall %s", n.Name, n.WallID)
		}
		w.Cells[i].Node = n.ID
		if r := w.Cells[i].Rect; r != nil {
			rc := *r
			w.Cells[i].Rect = &rc
		}
	}
	if err := proto.ValidateWallSpec(&w); err != nil {
		return nil, err
	}
	w.Content.Wall = nil
	w.Content.Rev = 0
	return &w, nil
}

// applyWallLocked computes the geometry and pushes a wall tile to every
// member's display spec with a new Rev.
func (s *Server) applyWallLocked(w *proto.WallSpec) {
	cw, ch, rects := wallLayout(w, s.cellSizeLocked)
	content := w.Content
	for i, c := range w.Cells {
		n := s.nodes[c.Node]
		if n == nil {
			continue
		}
		r := rects[i]
		cc := content
		spec := &proto.DisplaySpec{
			Rev:  s.nextRevLocked(n),
			Mode: proto.DisplayWall,
			Wall: &proto.WallTile{
				WallID: w.ID, CanvasW: cw, CanvasH: ch,
				X: r.X, Y: r.Y, W: r.W, H: r.H,
				Row: c.Row, Col: c.Col,
				Label:   fmt.Sprintf("R%dC%d %s", c.Row+1, c.Col+1, n.Name),
				Content: &cc,
			},
		}
		n.Display = spec
		n.WallID = w.ID
	}
	s.walls[w.ID] = w
	s.dirty = true
}

// nextRevLocked returns a display Rev that is larger than the node's
// current one and, via the clock, than any Rev it may have seen before a
// state loss.
func (s *Server) nextRevLocked(n *node) int64 {
	rev := s.now().UnixMilli()
	if n.Display != nil && n.Display.Rev >= rev {
		rev = n.Display.Rev + 1
	}
	if rev < 1 {
		rev = 1
	}
	return rev
}

// resetDisplayLocked puts a node back to the status screen.
func (s *Server) resetDisplayLocked(n *node) {
	n.Display = &proto.DisplaySpec{Rev: s.nextRevLocked(n), Mode: proto.DisplayStatus}
	n.WallID = ""
	s.dirty = true
}

// recomputeWallLocked refreshes a wall's tiles (after a member's rotation
// changed).
func (s *Server) recomputeWallLocked(id string) {
	if w := s.walls[id]; w != nil {
		s.applyWallLocked(w)
	}
}

func (s *Server) handleWalls(w http.ResponseWriter, r *http.Request, _ adminCtx) {
	s.mu.Lock()
	out := make([]proto.WallSpec, 0, len(s.walls))
	for _, id := range sortedKeys(s.walls) {
		out = append(out, *s.walls[id])
	}
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleWallGet(w http.ResponseWriter, r *http.Request, _ adminCtx) {
	s.mu.Lock()
	wall := s.walls[r.PathValue("id")]
	var out proto.WallSpec
	if wall != nil {
		out = *wall
	}
	s.mu.Unlock()
	if wall == nil {
		writeErr(w, http.StatusNotFound, "no such wall")
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleWallCreate(w http.ResponseWriter, r *http.Request, _ adminCtx) {
	s.saveWall(w, r, "")
}

func (s *Server) handleWallPut(w http.ResponseWriter, r *http.Request, _ adminCtx) {
	s.saveWall(w, r, r.PathValue("id"))
}

// saveWall creates (id == "") or replaces a wall. Nodes that leave the
// wall go back to the status screen.
func (s *Server) saveWall(w http.ResponseWriter, r *http.Request, id string) {
	var in proto.WallSpec
	if !decodeJSON(w, r, &in, true) {
		return
	}
	if len(in.Cells) > proto.MaxWallSize*proto.MaxWallSize {
		writeErr(w, http.StatusBadRequest, "too many cells")
		return
	}
	// Media checks read blob files; do them before taking the lock.
	content := in.Content
	if err := s.fillMediaDims(&content); err != nil {
		writeErr(w, http.StatusBadRequest, "%s", err.Error())
		return
	}
	in.Content = content

	s.mu.Lock()
	var old *proto.WallSpec
	if id != "" {
		if old = s.walls[id]; old == nil {
			s.mu.Unlock()
			writeErr(w, http.StatusNotFound, "no such wall")
			return
		}
	} else {
		id = "w" + auth.NewID(6)
		for s.walls[id] != nil {
			id = "w" + auth.NewID(6)
		}
	}
	spec, err := s.prepareWallLocked(in, id)
	if err != nil {
		s.mu.Unlock()
		writeErr(w, http.StatusBadRequest, "%s", err.Error())
		return
	}
	if old != nil {
		for _, c := range old.Cells {
			if n := s.nodes[c.Node]; n != nil && !wallHasNode(spec, n.ID) {
				s.resetDisplayLocked(n)
			}
		}
	}
	s.applyWallLocked(spec)
	out := *spec
	s.mu.Unlock()
	s.persistSync()
	s.log.Info("wall saved", "wall", id, "cells", len(out.Cells))
	writeJSON(w, http.StatusOK, out)
}

func wallHasNode(w *proto.WallSpec, id string) bool {
	for _, c := range w.Cells {
		if c.Node == id {
			return true
		}
	}
	return false
}

func (s *Server) handleWallDelete(w http.ResponseWriter, r *http.Request, _ adminCtx) {
	id := r.PathValue("id")
	s.mu.Lock()
	wall := s.walls[id]
	if wall == nil {
		s.mu.Unlock()
		writeErr(w, http.StatusNotFound, "no such wall")
		return
	}
	for _, c := range wall.Cells {
		if n := s.nodes[c.Node]; n != nil && n.WallID == id {
			s.resetDisplayLocked(n)
		}
	}
	delete(s.walls, id)
	s.dirty = true
	s.mu.Unlock()
	s.persistSync()
	writeOK(w)
}
