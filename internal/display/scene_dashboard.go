package display

import (
	"errors"
	"fmt"
	"image"
	"strconv"
	"time"

	"github.com/platteration/ewastesavior/internal/proto"
)

// dashboardRefresh is how often the dashboard asks for new statistics.
const dashboardRefresh = 5 * time.Second

type dashTile struct {
	label, value, sub string
	frac              float64 // < 0: no bar
}

type dashView struct {
	tiles []dashTile
	err   string
}

func buildDashView(st *proto.SwarmStats, err error) dashView {
	var v dashView
	if err != nil && !errors.Is(err, errPending) {
		v.err = cleanText(err.Error(), 200, false)
	}
	if st == nil {
		return v
	}
	running := st.TasksByState[string(proto.TaskRunning)] + st.TasksByState[string(proto.TaskAssigned)]
	frac := -1.0
	if st.CoresAllocatable > 0 {
		frac = st.CoresInUse / st.CoresAllocatable
	}
	busy := st.NodesByState[string(proto.NodeBusy)]
	nodeFrac := -1.0
	if st.NodesOnline > 0 {
		nodeFrac = float64(busy) / float64(st.NodesOnline)
	}
	v.tiles = []dashTile{
		{"Nodes online", strconv.Itoa(st.NodesOnline), fmt.Sprintf("%d busy · %d offline", busy, st.NodesOffline), nodeFrac},
		{"Cores in use", fmt.Sprintf("%.1f", st.CoresInUse), fmt.Sprintf("of %.1f offered (%d CPUs)", st.CoresAllocatable, st.Cores), frac},
		{"Tasks running", strconv.Itoa(running), fmt.Sprintf("%d queued", st.TasksByState[string(proto.TaskPending)]), -1},
		{"Tasks done", strconv.FormatInt(st.TasksCompleted, 10), fmt.Sprintf("%.1f CPU hours", st.CPUSecondsTotal/3600), -1},
		{"Memory", fmt.Sprintf("%.1f GB", float64(st.MemMB)/1024), "in online nodes", -1},
		{"Jobs", strconv.Itoa(st.JobsByState[string(proto.JobRunning)] + st.JobsByState[string(proto.JobQueued)]),
			fmt.Sprintf("%d running · %d displays", st.JobsByState[string(proto.JobRunning)], st.Displays), -1},
	}
	return v
}

// dashboard renders swarm statistics, refreshed every 5 s.
func (s *scene) dashboard() {
	s.res.next = s.now.Add(dashboardRefresh)
	var st *proto.SwarmStats
	var err error
	if s.env.Stats == nil {
		err = errors.New("not connected to a hive")
	} else {
		st, err = s.env.Stats(s.ctx)
		if errors.Is(err, errPending) {
			s.res.pending = true
		}
	}
	v := buildDashView(st, err)
	if s.unchanged(fmt.Sprintf("dash|%dx%d|%+v", s.w, s.h, v)) {
		return
	}
	drawDashboard(s.dst, v, st == nil && err != nil && !errors.Is(err, errPending))
}

func drawDashboard(dst *image.RGBA, v dashView, failed bool) {
	W, H := dst.Rect.Dx(), dst.Rect.Dy()
	fill(dst, dst.Rect, colBG)
	m := max(6, min(W, H)/30)
	u := max(10, min(min(W*3/4, H)/24, 48))
	titleH := u * 2
	drawLeft(dst, image.Rect(m, m, W-m, m+titleH), styleBold, u*3/2, "SaviorOS swarm", colText)
	top := m + titleH + m/2
	bottom := H - m
	if v.err != "" {
		drawLeft(dst, image.Rect(m, H-m-u*3/2, W-m, H-m), styleRegular, u*4/5, "Stats unavailable: "+v.err, colWarn)
		bottom -= u * 3 / 2
	}
	if len(v.tiles) == 0 {
		msg := "Loading swarm statistics…"
		if failed {
			msg = "Swarm statistics unavailable"
		}
		drawCentered(dst, image.Rect(m, top, W-m, bottom), styleBold, u*2, msg, colMuted)
		return
	}
	cols, rows := 3, 2
	if W < H {
		cols, rows = 2, 3
	}
	gw := (W - 2*m - (cols-1)*m) / cols
	gh := (bottom - top - (rows-1)*m) / rows
	if gw <= 0 || gh <= 0 {
		return
	}
	for i, t := range v.tiles {
		c, r := i%cols, i/cols
		if r >= rows {
			break
		}
		x := m + c*(gw+m)
		y := top + r*(gh+m)
		tile := image.Rect(x, y, x+gw, y+gh)
		fill(dst, tile, colPanel)
		in := tile.Inset(max(4, m/2))
		lh := in.Dy() / 6
		drawLeft(dst, image.Rect(in.Min.X, in.Min.Y, in.Max.X, in.Min.Y+lh), styleRegular, lh*4/5, t.label, colMuted)
		vr := image.Rect(in.Min.X, in.Min.Y+lh, in.Max.X, in.Max.Y-lh*2)
		size := fitLineSize(styleBold, t.value, vr.Dx(), vr.Dy())
		drawLeft(dst, vr, styleBold, size, t.value, colText)
		sub := image.Rect(in.Min.X, in.Max.Y-lh*2, in.Max.X, in.Max.Y-lh)
		drawLeft(dst, sub, styleRegular, lh*4/5, t.sub, colMuted)
		if t.frac >= 0 {
			bar(dst, image.Rect(in.Min.X, in.Max.Y-lh*2/3, in.Max.X, in.Max.Y), t.frac, levelColor(t.frac))
		}
	}
}
