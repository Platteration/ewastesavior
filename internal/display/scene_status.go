package display

import (
	"fmt"
	"image"
	"image/color"
	"math"
	"strings"
	"time"

	"github.com/platteration/ewastesavior/internal/proto"
)

// statusView is everything the status screen shows, already formatted.
// Its printed form is the frame key, so values are rounded to what is
// displayed and an unchanged view skips rendering entirely.
type statusView struct {
	name, code       string
	link, linkDetail string
	linkCol          color.RGBA
	hint, errLine    string
	message          string
	lines            [][2]string // label, value
	bars             []barView
	hive             *hiveView
}

type barView struct {
	label, value string
	frac         float64
	col          color.RGBA
}

type hiveView struct {
	urls    []string
	fp      string // "sha256:" + 64 hex, or whatever the hive reported
	pair    string
	nodes   string
	warning string
}

func buildStatusView(in StatusInfo) statusView {
	v := statusView{
		name: cleanText(in.Name, 64, false),
		code: cleanText(in.ShortCode, 8, false),
	}
	if v.name == "" {
		v.name = "savior"
	}
	if v.code == "" && in.NodeID != "" {
		v.code = proto.ShortCode(in.NodeID)
	}
	v.link = linkLabel(in.Link)
	switch in.Link {
	case proto.LinkConnected:
		v.linkCol = colGood
		if h, _ := splitHiveAddr(in.HiveAddr); h != "" {
			v.linkDetail = "hive " + cleanText(h, 64, false)
		}
	case proto.LinkSearching, proto.LinkPending, "":
		v.linkCol = colWarn
	default:
		v.linkCol = colBad
	}
	v.hint = LinkHint(in.Link, in.HiveAddr, in.HiveError)
	if in.Link != proto.LinkConnected && in.HiveError != "" && in.Link != "" {
		v.errLine = cleanText(in.HiveError, 200, false)
		if v.errLine == v.hint {
			v.errLine = ""
		}
	}
	v.message = cleanText(in.Message, 300, false)

	addrs := make([]string, 0, len(in.Addrs))
	for _, a := range in.Addrs {
		if a = cleanText(a, 64, false); a != "" {
			addrs = append(addrs, strings.SplitN(a, "/", 2)[0])
		}
	}
	ip := strings.Join(addrs, "   ")
	if ip == "" {
		ip = "no IP address"
	}
	v.lines = append(v.lines, [2]string{"IP", ip})
	roles := make([]string, 0, len(in.Roles))
	for _, r := range in.Roles {
		roles = append(roles, cleanText(string(r), 16, false))
	}
	if len(roles) > 0 {
		v.lines = append(v.lines, [2]string{"Roles", strings.Join(roles, ", ")})
	}
	tasks := fmt.Sprintf("%d running", in.RunningTasks)
	if in.RunningTasks == 1 {
		tasks = "1 running"
	}
	if in.PowerReason != "" {
		tasks += "  ·  paused: " + cleanText(in.PowerReason, 80, false)
	}
	v.lines = append(v.lines, [2]string{"Tasks", tasks})
	inv := in.Inventory
	var hw []string
	if inv.CPUModel != "" {
		hw = append(hw, cleanText(inv.CPUModel, 64, false))
	}
	if inv.Cores > 0 {
		hw = append(hw, fmt.Sprintf("%d CPU", inv.Cores))
	}
	if inv.MemTotalMB > 0 {
		hw = append(hw, fmt.Sprintf("%d MB", inv.MemTotalMB))
	}
	if len(hw) > 0 {
		v.lines = append(v.lines, [2]string{"Machine", strings.Join(hw, " · ")})
	}
	ver := "SaviorOS " + cleanText(in.Version, 32, false)
	if in.NodeID != "" {
		ver += " · " + cleanText(in.NodeID, 64, false)
	}
	v.lines = append(v.lines, [2]string{"Node", ver})
	v.bars = statusBars(in)
	if h := in.Hive; h != nil {
		hv := &hiveView{
			fp:    cleanText(h.Fingerprint, 80, false),
			pair:  cleanText(h.PairCode, 16, false),
			nodes: fmt.Sprintf("%d", h.NodesOnline),
		}
		for i, u := range h.URLs {
			if i == 3 {
				break
			}
			hv.urls = append(hv.urls, cleanText(u, 80, false))
		}
		if !h.Persistent {
			hv.warning = "Hive data is in RAM and is lost on reboot. Use a stick with room for a data partition."
		}
		v.hive = hv
	}
	return v
}

// statusBars builds the meters. Fractions are rounded to whole percent so
// the frame only changes when something visible does.
func statusBars(in StatusInfo) []barView {
	pct := func(f float64) float64 { return math.Round(clampf(f, 0, 1)*100) / 100 }
	m := in.Metrics
	cpu := math.Round(clampf(m.CPUPercent, 0, 100))
	out := []barView{{label: "CPU", value: fmt.Sprintf("%.0f%%", cpu), frac: cpu / 100, col: levelColor(cpu / 100)}}
	if total := in.Inventory.MemTotalMB; total > 0 {
		used := total - m.MemAvailableMB
		used = max(0, min(used, total))
		f := pct(float64(used) / float64(total))
		out = append(out, barView{label: "RAM", value: fmt.Sprintf("%d / %d MB", used, total), frac: f, col: levelColor(f)})
	}
	if t := math.Round(m.CPUTempC); t > 0 {
		limit := m.CPUTempLimitC
		if limit <= 0 {
			limit = 90
		}
		f := pct(t / limit)
		out = append(out, barView{label: "Temp", value: fmt.Sprintf("%.0f°C", t), frac: f, col: levelColor(f)})
	}
	if p := m.BatteryPercent; p >= 0 && (in.Inventory.HasBattery || p > 0) {
		p = min(p, 100)
		f := float64(p) / 100
		state := "on AC"
		if m.OnBattery {
			state = "on battery"
		} else if s := strings.ToLower(m.BatteryStatus); s == "charging" {
			state = "charging"
		}
		out = append(out, barView{label: "Battery", value: fmt.Sprintf("%d%% %s", p, state), frac: f, col: levelColor(1 - f)})
	}
	return out
}

func clampf(v, lo, hi float64) float64 {
	if v != v || v < lo { // NaN too
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// status renders the "which machine is this?" screen. It re-checks the
// status once a second but redraws only when a displayed value changed.
func (s *scene) status() {
	var in StatusInfo
	if s.env.Status != nil {
		in = s.env.Status()
	}
	v := buildStatusView(in)
	s.res.next = s.now.Truncate(time.Second).Add(time.Second)
	if s.unchanged(fmt.Sprintf("status|%dx%d|%+v", s.w, s.h, v)) {
		return
	}
	drawStatus(s.dst, v)
}

// statusLayout holds the unit sizes derived from the screen.
type statusLayout struct {
	W, H, m, u int
}

func drawStatus(dst *image.RGBA, v statusView) {
	W, H := dst.Rect.Dx(), dst.Rect.Dy()
	L := statusLayout{W: W, H: H}
	L.m = max(6, min(W, H)/36)
	L.u = max(10, min(min(W*3/4, H)/26, 44))
	u, m := L.u, L.m
	fill(dst, dst.Rect, colBG)

	// Header: name and the identify code badge.
	y := m
	headH := max(u*2, min(H*15/100, u*4))
	bw := 0
	if v.code != "" {
		bw = min(W/4, headH*5/2)
		badge := image.Rect(W-m-bw, y, W-m, y+headH)
		fill(dst, badge, colPanel)
		lab := u * 7 / 10
		drawCentered(dst, image.Rect(badge.Min.X, badge.Min.Y+lab/4, badge.Max.X, badge.Min.Y+lab/4+lab), styleRegular, lab, "CODE", colMuted)
		drawCentered(dst, image.Rect(badge.Min.X+m/2, badge.Min.Y+lab+lab/4, badge.Max.X-m/2, badge.Max.Y-lab/4), styleBold, 0, v.code, colText)
		bw += m
	}
	nameBox := image.Rect(m, y, W-m-bw, y+headH)
	size := min(fitLineSize(styleBold, v.name, nameBox.Dx(), nameBox.Dy()), headH)
	drawLeft(dst, nameBox, styleBold, size, v.name, colText)
	y += headH + m

	// Link state, fix hint, last error, message.
	lh := u * 3 / 2
	dot := u * 3 / 4
	fill(dst, image.Rect(m, y+(lh-dot)/2, m+dot, y+(lh-dot)/2+dot), v.linkCol)
	text := v.link
	if v.linkDetail != "" {
		text += "  ·  " + v.linkDetail
	}
	drawLeft(dst, image.Rect(m+dot+u/2, y, W-m, y+lh), styleBold, u*6/5, text, colText)
	y += lh
	if v.hint != "" {
		for i, l := range wrapText(styleRegular, u, v.hint, W-2*m) {
			if i == 2 {
				break
			}
			drawLeft(dst, image.Rect(m, y, W-m, y+u*13/10), styleRegular, u, l, colWarn)
			y += u * 13 / 10
		}
	}
	if v.errLine != "" {
		drawLeft(dst, image.Rect(m, y, W-m, y+u), styleRegular, u*4/5, v.errLine, colMuted)
		y += u
	}
	if v.message != "" {
		drawLeft(dst, image.Rect(m, y, W-m, y+u*3/2), styleBold, u, v.message, colAccent)
		y += u * 3 / 2
	}
	y += m / 2

	// Body: details and meters, with the hive panel beside (landscape) or
	// below (portrait).
	left := image.Rect(m, y, W-m, H-m)
	var right image.Rectangle
	if v.hive != nil && W >= H {
		split := W * 56 / 100
		left.Max.X = split - m/2
		right = image.Rect(split+m/2, y, W-m, H-m)
	}
	y = drawStatusDetails(dst, left, L, v)
	if v.hive != nil {
		if right.Empty() {
			right = image.Rect(m, y+m/2, W-m, H-m)
		}
		drawHivePanel(dst, right, L, v.hive)
	}
}

// drawStatusDetails draws label/value lines and meter bars into r and
// returns the y below them.
func drawStatusDetails(dst *image.RGBA, r image.Rectangle, L statusLayout, v statusView) int {
	u := L.u
	y := r.Min.Y
	labelW := min(u*5, r.Dx()/4)
	row := u * 7 / 5
	for _, l := range v.lines {
		if y+row > r.Max.Y {
			return y
		}
		drawLeft(dst, image.Rect(r.Min.X, y, r.Min.X+labelW, y+row), styleRegular, u*4/5, l[0], colMuted)
		drawLeft(dst, image.Rect(r.Min.X+labelW, y, r.Max.X, y+row), styleRegular, u, l[1], colText)
		y += row
	}
	y += u / 2
	valueW := min(u*8, r.Dx()/3)
	for _, b := range v.bars {
		if y+row > r.Max.Y {
			return y
		}
		drawLeft(dst, image.Rect(r.Min.X, y, r.Min.X+labelW, y+row), styleBold, u*4/5, b.label, colMuted)
		track := image.Rect(r.Min.X+labelW, y+row/4, r.Max.X-valueW-u/2, y+row-row/4)
		bar(dst, track, b.frac, b.col)
		drawRight(dst, image.Rect(r.Max.X-valueW, y, r.Max.X, y+row), styleRegular, u*9/10, b.value, colText)
		y += row
	}
	return y
}

// fingerprintLines formats "sha256:<hex>" as groups of four hex digits:
// two lines of eight groups, else four lines of four, else shortened
// (head…tail) when even that does not fit maxW.
func fingerprintLines(fp string, size, maxW int) []string {
	hex := strings.TrimPrefix(fp, "sha256:")
	if len(hex) != 64 {
		return []string{ellipsize(styleMono, size, fp, maxW)}
	}
	group := func(s string) string {
		var b strings.Builder
		for i := 0; i < len(s); i += 4 {
			if i > 0 {
				b.WriteByte(' ')
			}
			b.WriteString(s[i:min(i+4, len(s))])
		}
		return b.String()
	}
	for _, n := range []int{2, 4} {
		if maxW <= 0 {
			break
		}
		per := 64 / n
		lines := []string{"sha256:"}
		for i := 0; i < n; i++ {
			lines = append(lines, group(hex[i*per:(i+1)*per]))
		}
		if textWidth(styleMono, size, lines[1]) <= maxW {
			return lines
		}
	}
	short := "sha256:" + hex[:16] + "…" + hex[56:]
	if maxW <= 0 {
		return []string{short}
	}
	return []string{ellipsize(styleMono, size, short, maxW)}
}

// drawHivePanel draws the hive panel into r: once to measure, then for
// real on a background sized to the content.
func drawHivePanel(dst *image.RGBA, r image.Rectangle, L statusLayout, h *hiveView) {
	if r.Dy() < L.u*3 {
		return
	}
	used := hivePanelContent(dst, r, L, h, false)
	r.Max.Y = min(r.Max.Y, used+L.m/2)
	fill(dst, r, colPanel)
	hivePanelContent(dst, r, L, h, true)
}

func hivePanelContent(dst *image.RGBA, r image.Rectangle, L statusLayout, h *hiveView, draw bool) int {
	u, m := L.u, L.m
	in := r.Inset(m / 2)
	y := in.Min.Y
	row := u * 13 / 10
	line := func(st fontStyle, size int, s string, c color.RGBA, height int) {
		if draw && y+height <= in.Max.Y && s != "" {
			drawLeft(dst, image.Rect(in.Min.X, y, in.Max.X, y+height), st, size, s, c)
		}
		if y+height <= in.Max.Y {
			y += height
		}
	}
	line(styleBold, u*6/5, "This computer is the hive", colText, row)
	line(styleRegular, u, h.nodes+" nodes online", colText, row)
	for _, url := range h.urls {
		line(styleMono, u*9/10, url, colAccent, row)
	}
	if h.pair != "" {
		y += u / 3
		line(styleRegular, u*4/5, "Pairing code for the dashboard", colMuted, row)
		code := h.pair
		if len(code) == 8 {
			code = code[:4] + "-" + code[4:]
		}
		line(styleMono, u*2, code, colText, u*5/2)
	}
	if h.warning != "" {
		y += u / 3
		for _, l := range wrapText(styleBold, u*4/5, h.warning, in.Dx()) {
			line(styleBold, u*4/5, l, colWarn, u*11/10)
		}
	}
	if h.fp != "" {
		y += u / 3
		line(styleRegular, u*4/5, "Fingerprint (hive_fingerprint)", colMuted, row)
		lines := fingerprintLines(h.fp, u*4/5, in.Dx())
		if y+len(lines)*(u*11/10) > in.Max.Y && len(lines) > 1 {
			lines = fingerprintLines(h.fp, u*4/5, 0) // too tall: one shortened line
		}
		for _, l := range lines {
			line(styleMono, u*4/5, l, colText, u*11/10)
		}
	}
	return y
}
