package node

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"net"
	"net/url"
	"os"
	"sort"
	"strconv"
	"time"

	"github.com/platteration/ewastesavior/internal/display"
	"github.com/platteration/ewastesavior/internal/proto"
	"github.com/platteration/ewastesavior/internal/version"
)

// newDisplay creates the display controller with the local default scene
// (and the URL media fetcher it uses). The caller stores it in a.disp.
func (a *Agent) newDisplay() *display.Controller {
	open := a.opt.OpenDisplay
	if open == nil {
		dev := a.cfg.DisplayDevice
		// OpenDevice re-selects the best framebuffer when "auto" and the
		// controller applies rotation itself.
		open = func() (display.Device, error) { return display.OpenDevice(dev, 0) }
	}
	loc, err := time.LoadLocation(a.cfg.Timezone)
	if err != nil {
		loc = time.UTC
	}
	cache := display.NewFrameCache(display.DefaultCacheBytes())
	a.urlf = display.NewURLFetcher()
	a.urlf.Cache = cache
	a.urlf.BytesCacheDir = "/tmp/savior-media"
	env := display.Env{
		Now:      a.now,
		Status:   a.statusInfo,
		Stats:    a.fetchStats,
		Fetch:    a.fetchMedia,
		Location: loc,
	}
	disp := display.NewController(open, env, a.log.With("component", "display"))
	disp.SetCache(cache)
	if a.cfg.DisplayIdleOff > 0 {
		disp.SetIdleOff(time.Duration(a.cfg.DisplayIdleOff) * time.Minute)
	}
	disp.SetRotate(a.cfg.DisplayRotate)
	disp.Apply(a.localDisplaySpec())
	return disp
}

// localDisplaySpec is the screen before the hive says otherwise.
func (a *Agent) localDisplaySpec() proto.DisplaySpec {
	switch a.cfg.DisplayMode {
	case proto.DisplayText:
		return proto.DisplaySpec{Mode: proto.DisplayText, Text: a.cfg.DisplayText}
	case proto.DisplayOff, proto.DisplayClock, proto.DisplayTest:
		return proto.DisplaySpec{Mode: a.cfg.DisplayMode}
	}
	return proto.DisplaySpec{Mode: proto.DisplayStatus}
}

func (a *Agent) fetchStats(ctx context.Context) (*proto.SwarmStats, error) {
	a.mu.Lock()
	hc := a.hc
	a.mu.Unlock()
	if hc == nil {
		return nil, fmt.Errorf("not connected to the hive")
	}
	return hc.stats(ctx)
}

// fetchMedia gets an image already fitted/cropped for this screen. Blob
// media are rendered by the hive (it's usually the faster machine); URL
// media, and blobs when the hive can't render, are processed locally with
// strict decode limits.
func (a *Agent) fetchMedia(ctx context.Context, m proto.Media, req display.FetchRequest) (image.Image, error) {
	if m.Blob == "" {
		return a.urlf.Fetch(ctx, m, req)
	}
	a.mu.Lock()
	hc := a.hc
	a.mu.Unlock()
	if hc == nil {
		return nil, fmt.Errorf("not connected to the hive")
	}
	fit := req.Fit
	if fit == "" {
		fit = "contain"
	}
	q := url.Values{}
	q.Set("cw", strconv.Itoa(req.CanvasW))
	q.Set("ch", strconv.Itoa(req.CanvasH))
	q.Set("x", strconv.Itoa(req.Rect.Min.X))
	q.Set("y", strconv.Itoa(req.Rect.Min.Y))
	q.Set("w", strconv.Itoa(req.Rect.Dx()))
	q.Set("h", strconv.Itoa(req.Rect.Dy()))
	q.Set("pw", strconv.Itoa(req.PixelW))
	q.Set("ph", strconv.Itoa(req.PixelH))
	q.Set("fit", fit)
	if body, err := hc.renderBlob(ctx, m.Blob, q); err == nil {
		defer body.Close()
		// The hive already fitted and cropped the image: take it pixel for pixel.
		exact := display.FetchRequest{CanvasW: req.PixelW, CanvasH: req.PixelH,
			Rect: image.Rect(0, 0, req.PixelW, req.PixelH), PixelW: req.PixelW, PixelH: req.PixelH, Fit: "stretch"}
		img, err := display.DecodeFrame(body, exact, nil)
		if err == nil {
			return img, nil
		}
		a.log.Debug("hive render unusable; decoding locally", "err", err)
	}
	body, err := hc.getBlob(ctx, m.Blob)
	if err != nil {
		return nil, err
	}
	defer body.Close()
	req.Fit = fit
	return display.DecodeFrame(body, req, nil)
}

// statusInfo feeds the status scene and the console.
func (a *Agent) statusInfo() display.StatusInfo {
	a.mu.Lock()
	defer a.mu.Unlock()
	return display.StatusInfo{
		Name:         a.name,
		ShortCode:    a.shortCode,
		NodeID:       a.id.NodeID,
		Version:      version.Version,
		Link:         a.link,
		HiveAddr:     a.hiveAddr,
		HiveError:    a.linkErr,
		Addrs:        localAddrs(),
		Roles:        a.roles,
		Metrics:      a.lastMetrics,
		Inventory:    a.inv,
		RunningTasks: len(a.tasks),
		PowerReason:  a.decision.Reason,
		Hive:         readHivePanel(a.hivePanelPath()),
	}
}

// consoleStatus is what `savior console` reads from the status file.
type consoleStatus struct {
	display.StatusInfo
	Hint      string    `json:"hint"`
	Pending   bool      `json:"pending"`
	Drain     bool      `json:"drain"`
	Updated   time.Time `json:"updated"`
	Total     proto.Resources
	Free      proto.Resources
	Display   proto.DisplayState
	TaskNames []string `json:"tasks"`
}

// writeStatus atomically writes the status file. It is 0644, so it holds
// no secrets: the hive's pairing code is left out (HivePanel.PairCode is
// never serialized, and it is cleared here too); `savior console` reads the
// code from the hive's own 0600 file.
func (a *Agent) writeStatus() {
	path := a.statusPath()
	if path == "" {
		return
	}
	si := a.statusInfo()
	if si.Hive != nil {
		h := *si.Hive
		h.PairCode = ""
		si.Hive = &h
	}
	a.mu.Lock()
	cs := consoleStatus{
		StatusInfo: si,
		Hint:       display.LinkHint(si.Link, si.HiveAddr, si.HiveError),
		Pending:    a.pending,
		Drain:      a.directives.Drain,
		Updated:    time.Now(),
		Total:      a.total,
		Free:       a.freeLocked(),
	}
	for _, t := range a.tasks {
		name := t.t.JobName
		if name == "" {
			name = t.t.JobID
		}
		cs.TaskNames = append(cs.TaskNames, fmt.Sprintf("%s #%d", name, t.t.Index))
	}
	disp := a.disp
	a.mu.Unlock()
	sort.Strings(cs.TaskNames)
	if disp != nil {
		cs.Display = disp.State()
	}
	b, err := json.MarshalIndent(cs, "", " ")
	if err != nil {
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err == nil {
		os.Rename(tmp, path)
	}
}

// hivePanelFile is written by a hive running on this machine (roles=hive;
// see hive.DefaultStatusFile) so the status screen and console can show how
// to reach it. It is 0600: it holds the dashboard pairing code.
const hivePanelFile = "/run/savior/hive-status.json"

func (a *Agent) hivePanelPath() string { return orDefault(a.opt.HivePanelFile, hivePanelFile) }

// readHivePanel reads a hive's status panel file (nil when there is none
// or it is unreadable).
func readHivePanel(path string) *display.HivePanel {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var p struct {
		URLs        []string `json:"urls"`
		Fingerprint string   `json:"fingerprint"`
		PairCode    string   `json:"pair_code"`
		NodesOnline int      `json:"nodes_online"`
		Persistent  bool     `json:"persistent"`
	}
	if json.Unmarshal(bytes.TrimSpace(b), &p) != nil {
		return nil
	}
	return &display.HivePanel{URLs: p.URLs, Fingerprint: p.Fingerprint, PairCode: p.PairCode, NodesOnline: p.NodesOnline, Persistent: p.Persistent}
}

// localAddrs lists this machine's non-loopback addresses (CIDR).
func localAddrs() []string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}
	var out []string
	for _, a := range addrs {
		ipn, ok := a.(*net.IPNet)
		if !ok || ipn.IP.IsLoopback() || (ipn.IP.To4() == nil && ipn.IP.IsLinkLocalUnicast()) {
			continue
		}
		out = append(out, ipn.String())
	}
	return out
}
