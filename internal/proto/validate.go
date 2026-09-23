package proto

import (
	"crypto/sha256"
	"fmt"
	"path"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// Limits shared by hive, node and ctl.
const (
	MaxTaskCount     = 100000
	MaxCores         = 256
	MaxMemMB         = 1 << 20
	MaxDiskMB        = 1 << 20
	MaxTimeoutS      = 7 * 24 * 3600
	MaxRetries       = 10
	MaxPriority      = 1000
	MaxInputs        = 256
	MaxOutputs       = 64
	MaxOutputFiles   = 1000
	MaxOutputBytes   = 1 << 30
	MaxLabels        = 32
	MaxImages        = 100
	MaxTextLen       = 4096
	MaxWallSize      = 16
	MaxBlobBytes     = 8 << 30
	MaxMediaBytes    = 64 << 20
	MaxMediaPixels   = 16 << 20 // 16 MP after DecodeConfig
	MaxMediaSide     = 8192
	DefaultCores     = 1
	DefaultMemMB     = 128
	DefaultDiskMB    = 64
	DefaultTimeoutS  = 3600
	DefaultRetries   = 1
	DefaultInterval  = 10
	MinInterval      = 3
	MaxInterruptions = 6 // extra attempts allowed for lost/preempted requeues
)

var (
	nodeNameRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$`)
	nodeIDRE   = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	idLikeRE   = regexp.MustCompile(`^n[0-9a-f]{12}$`)
	labelKeyRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_.\-/]{0,62}$`)
	archRE     = regexp.MustCompile(`^[a-z0-9]{1,16}$`)
	flagRE     = regexp.MustCompile(`^[a-z0-9_]{1,32}$`)
	envKeyRE   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)
)

// ValidSHA256 reports whether s is 64 lowercase hex characters.
func ValidSHA256(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// ValidNodeName reports whether s is a valid node name: lowercase letters,
// digits and inner dashes, 1-32 chars, not shaped like a node ID.
func ValidNodeName(s string) bool {
	return nodeNameRE.MatchString(s) && !idLikeRE.MatchString(s)
}

// ValidNodeID reports whether s is a valid node ID.
func ValidNodeID(s string) bool { return nodeIDRE.MatchString(s) }

// ValidLabelKey reports whether k is a valid label key.
func ValidLabelKey(k string) bool { return labelKeyRE.MatchString(k) }

// ValidRelPath reports whether p is a safe relative path for use inside a
// task working directory on any OS: segments separated by "/", no empty,
// "." or ".." segments, no control characters, no backslash or colon, no
// segment starting with "-" or ending in "." or " ", not starting with
// ".savior" (reserved for the runner), at most 255 bytes, valid UTF-8.
func ValidRelPath(p string) bool {
	if p == "" || len(p) > 255 || !utf8.ValidString(p) || strings.HasPrefix(p, "/") {
		return false
	}
	for i := 0; i < len(p); i++ {
		c := p[i]
		if c < 0x20 || c == 0x7f || c == '\\' || c == ':' {
			return false
		}
	}
	segs := strings.Split(p, "/")
	if strings.HasPrefix(segs[0], ".savior") {
		return false
	}
	for _, s := range segs {
		if s == "" || s == "." || s == ".." || strings.HasPrefix(s, "-") ||
			strings.HasSuffix(s, ".") || strings.HasSuffix(s, " ") {
			return false
		}
	}
	return true
}

// ValidOutputPattern reports whether pat is an acceptable outputs glob: a
// ValidRelPath (glob metacharacters allowed) with valid path.Match syntax.
func ValidOutputPattern(pat string) bool {
	if !ValidRelPath(pat) {
		return false
	}
	_, err := path.Match(pat, "")
	return err == nil
}

// Sanitize strips C0/C1 control characters (except, when keepNewlines, \n
// and \t), replaces invalid UTF-8, and caps the result at max bytes.
func Sanitize(s string, max int, keepNewlines bool) string {
	var b strings.Builder
	for _, r := range strings.ToValidUTF8(s, "�") {
		if (r < 0x20 || (r >= 0x7f && r < 0xa0)) && !(keepNewlines && (r == '\n' || r == '\t')) {
			continue
		}
		if b.Len()+utf8.RuneLen(r) > max {
			break
		}
		b.WriteRune(r)
	}
	return b.String()
}

// ShortCode derives the 3-character identify code of a node ID (Crockford
// base32 without I, L, O, U so it can be read off a screen).
func ShortCode(nodeID string) string {
	const alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
	h := sha256.Sum256([]byte(nodeID))
	v := uint32(h[0])<<16 | uint32(h[1])<<8 | uint32(h[2])
	return string([]byte{alphabet[(v>>10)&31], alphabet[(v>>5)&31], alphabet[v&31]})
}

// ParseColor parses "#rrggbb" or "#rgb". It returns ok=false for anything else.
func ParseColor(s string) (r, g, b uint8, ok bool) {
	if len(s) == 0 || s[0] != '#' {
		return 0, 0, 0, false
	}
	h := s[1:]
	hexv := func(c byte) (uint8, bool) {
		switch {
		case c >= '0' && c <= '9':
			return c - '0', true
		case c >= 'a' && c <= 'f':
			return c - 'a' + 10, true
		case c >= 'A' && c <= 'F':
			return c - 'A' + 10, true
		}
		return 0, false
	}
	var v [3]uint8
	switch len(h) {
	case 3:
		for i := 0; i < 3; i++ {
			x, ok := hexv(h[i])
			if !ok {
				return 0, 0, 0, false
			}
			v[i] = x*16 + x
		}
	case 6:
		for i := 0; i < 3; i++ {
			a, ok1 := hexv(h[2*i])
			c, ok2 := hexv(h[2*i+1])
			if !ok1 || !ok2 {
				return 0, 0, 0, false
			}
			v[i] = a*16 + c
		}
	default:
		return 0, 0, 0, false
	}
	return v[0], v[1], v[2], true
}

func validMedia(m Media) error {
	if (m.Blob == "") == (m.URL == "") {
		return fmt.Errorf("media needs exactly one of blob or url")
	}
	if m.Blob != "" && !ValidSHA256(m.Blob) {
		return fmt.Errorf("invalid blob hash %q", m.Blob)
	}
	if m.URL != "" {
		if len(m.URL) > 2048 || !(strings.HasPrefix(m.URL, "http://") || strings.HasPrefix(m.URL, "https://")) {
			return fmt.Errorf("media url must be http(s) and at most 2048 chars")
		}
		if m.SHA256 != "" && !ValidSHA256(m.SHA256) {
			return fmt.Errorf("invalid media sha256")
		}
	}
	return nil
}

// ValidateDisplaySpec checks a display spec for structural errors and limits.
func ValidateDisplaySpec(s *DisplaySpec) error {
	return validateDisplaySpec(s, false)
}

func validateDisplaySpec(s *DisplaySpec, inWall bool) error {
	ok := false
	for _, m := range DisplayModes {
		ok = ok || s.Mode == m
	}
	if !ok {
		return fmt.Errorf("unknown display mode %q", s.Mode)
	}
	for _, c := range []string{s.FG, s.BG} {
		if c != "" {
			if _, _, _, ok := ParseColor(c); !ok {
				return fmt.Errorf("invalid color %q (want #rrggbb)", c)
			}
		}
	}
	switch s.Fit {
	case "", "contain", "cover", "stretch":
	default:
		return fmt.Errorf("fit must be contain, cover or stretch")
	}
	if len(s.Text) > MaxTextLen || len(s.Title) > 256 || len(s.ClockFormat) > 64 {
		return fmt.Errorf("text (%d), title (256) or clock_format (64) too long", MaxTextLen)
	}
	if s.Timezone != "" {
		if _, err := time.LoadLocation(s.Timezone); err != nil {
			return fmt.Errorf("unknown timezone %q", s.Timezone)
		}
	}
	if s.IntervalS != 0 && (s.IntervalS < MinInterval || s.IntervalS > 86400) {
		return fmt.Errorf("interval_s must be 0 (default) or %d..86400", MinInterval)
	}
	if s.Image != nil {
		if err := validMedia(*s.Image); err != nil {
			return err
		}
	}
	if len(s.Images) > MaxImages {
		return fmt.Errorf("at most %d images", MaxImages)
	}
	for _, m := range s.Images {
		if err := validMedia(m); err != nil {
			return err
		}
	}
	switch s.Mode {
	case DisplayImage:
		if s.Image == nil {
			return fmt.Errorf("image mode needs image")
		}
	case DisplaySlideshow:
		if len(s.Images) == 0 {
			return fmt.Errorf("slideshow mode needs images")
		}
	case DisplayWall:
		if inWall {
			return fmt.Errorf("walls can't be nested")
		}
		w := s.Wall
		if w == nil || w.Content == nil {
			return fmt.Errorf("wall mode needs wall.content")
		}
		if w.CanvasW <= 0 || w.CanvasH <= 0 || w.CanvasW > 1000000 || w.CanvasH > 1000000 ||
			w.W <= 0 || w.H <= 0 || w.X < 0 || w.Y < 0 || w.X+w.W > w.CanvasW || w.Y+w.H > w.CanvasH {
			return fmt.Errorf("invalid wall tile geometry")
		}
		if err := validWallContent(w.Content); err != nil {
			return err
		}
	}
	if s.Mode != DisplayWall && s.Wall != nil {
		return fmt.Errorf("wall is only valid with mode wall")
	}
	return nil
}

func validWallContent(c *DisplaySpec) error {
	switch c.Mode {
	case DisplayImage, DisplaySlideshow, DisplayText, DisplayColor, DisplayTest:
	default:
		return fmt.Errorf("wall content mode must be image, slideshow, text, color or test")
	}
	if err := validateDisplaySpec(c, true); err != nil {
		return fmt.Errorf("wall content: %w", err)
	}
	return nil
}

// ValidateWallSpec checks a wall definition (node existence and roles are
// checked by the hive).
func ValidateWallSpec(w *WallSpec) error {
	if w.Rows < 1 || w.Cols < 1 || w.Rows > MaxWallSize || w.Cols > MaxWallSize {
		return fmt.Errorf("rows and cols must be 1..%d", MaxWallSize)
	}
	if len(w.Name) > 64 {
		return fmt.Errorf("name too long")
	}
	if w.GapXMM < 0 || w.GapYMM < 0 || w.GapXMM > 500 || w.GapYMM > 500 {
		return fmt.Errorf("gaps must be 0..500 mm")
	}
	if len(w.Cells) == 0 {
		return fmt.Errorf("wall needs at least one cell")
	}
	pos := map[[2]int]bool{}
	nodes := map[string]bool{}
	for _, c := range w.Cells {
		if c.Row < 0 || c.Col < 0 || c.Row >= w.Rows || c.Col >= w.Cols {
			return fmt.Errorf("cell %s at %d,%d is outside the %dx%d grid", c.Node, c.Row, c.Col, w.Rows, w.Cols)
		}
		if pos[[2]int{c.Row, c.Col}] {
			return fmt.Errorf("two cells at row %d col %d", c.Row, c.Col)
		}
		pos[[2]int{c.Row, c.Col}] = true
		if c.Node == "" || nodes[c.Node] {
			return fmt.Errorf("each cell needs a distinct node")
		}
		nodes[c.Node] = true
		if c.WidthMM < 0 || c.HeightMM < 0 || c.WidthMM > 10000 || c.HeightMM > 10000 {
			return fmt.Errorf("cell size must be 0..10000 mm")
		}
		if r := c.Rect; r != nil && (r.X < 0 || r.Y < 0 || r.W <= 0 || r.H <= 0 || r.X+r.W > 1000000 || r.Y+r.H > 1000000) {
			return fmt.Errorf("invalid cell rect")
		}
	}
	return validWallContent(&w.Content)
}

// ApplyJobDefaults fills in defaults for omitted JobSpec fields.
func ApplyJobDefaults(s *JobSpec) {
	if s.Kind == "" {
		if len(s.Command) > 0 {
			s.Kind = KindExec
		} else {
			s.Kind = KindScript
		}
	}
	if s.Resources.Cores == 0 {
		s.Resources.Cores = DefaultCores
	}
	if s.Resources.MemMB == 0 {
		s.Resources.MemMB = DefaultMemMB
	}
	if s.Resources.DiskMB == 0 {
		s.Resources.DiskMB = DefaultDiskMB
	}
	if s.TimeoutS == 0 {
		s.TimeoutS = DefaultTimeoutS
	}
	if s.Retries == nil {
		r := DefaultRetries
		s.Retries = &r
	}
	if s.Count == 0 {
		s.Count = 1
	}
	if s.Requirements.Isolation == "" {
		s.Requirements.Isolation = IsolationFull
	}
}

// ValidateJobSpec checks a job spec after ApplyJobDefaults.
func ValidateJobSpec(s *JobSpec) error {
	if len(s.Name) > 128 {
		return fmt.Errorf("name too long")
	}
	switch s.Kind {
	case KindExec:
		if len(s.Command) == 0 || s.Command[0] == "" {
			return fmt.Errorf("exec jobs need a command")
		}
		if s.Script != "" {
			return fmt.Errorf("exec jobs can't have a script")
		}
	case KindScript:
		if strings.TrimSpace(s.Script) == "" {
			return fmt.Errorf("script jobs need a script")
		}
		if len(s.Command) > 0 {
			return fmt.Errorf("script jobs can't have a command")
		}
	default:
		return fmt.Errorf("kind must be exec or script")
	}
	if len(s.Command) > 4096 || len(s.Script) > 1<<20 {
		return fmt.Errorf("command or script too large")
	}
	for _, a := range s.Command {
		if strings.IndexByte(a, 0) >= 0 {
			return fmt.Errorf("command arguments can't contain NUL")
		}
	}
	if len(s.Env) > 256 {
		return fmt.Errorf("too many env vars")
	}
	for k, v := range s.Env {
		if !envKeyRE.MatchString(k) || strings.HasPrefix(k, "SAVIOR_") || strings.IndexByte(v, 0) >= 0 || len(v) > 32768 {
			return fmt.Errorf("invalid env var %q (SAVIOR_* is reserved)", k)
		}
	}
	r := s.Resources
	if r.Cores <= 0 || r.Cores > MaxCores || r.MemMB < 1 || r.MemMB > MaxMemMB || r.DiskMB < 0 || r.DiskMB > MaxDiskMB {
		return fmt.Errorf("resources out of range (cores 0-%d, mem_mb 1-%d, disk_mb 0-%d)", MaxCores, MaxMemMB, MaxDiskMB)
	}
	if s.Count < 1 || s.Count > MaxTaskCount {
		return fmt.Errorf("count must be 1..%d", MaxTaskCount)
	}
	if s.TimeoutS < 1 || s.TimeoutS > MaxTimeoutS {
		return fmt.Errorf("timeout_s must be 1..%d", MaxTimeoutS)
	}
	if s.Retries != nil && (*s.Retries < 0 || *s.Retries > MaxRetries) {
		return fmt.Errorf("retries must be 0..%d", MaxRetries)
	}
	if s.Priority < -MaxPriority || s.Priority > MaxPriority {
		return fmt.Errorf("priority must be -%d..%d", MaxPriority, MaxPriority)
	}
	if len(s.Inputs) > MaxInputs {
		return fmt.Errorf("at most %d inputs", MaxInputs)
	}
	names := map[string]bool{}
	var total int64
	for _, in := range s.Inputs {
		if !ValidRelPath(in.Name) {
			return fmt.Errorf("input name %q is not a safe relative path", in.Name)
		}
		if names[in.Name] {
			return fmt.Errorf("duplicate input %q", in.Name)
		}
		names[in.Name] = true
		switch {
		case in.Blob != "" && in.URL == "":
			if !ValidSHA256(in.Blob) {
				return fmt.Errorf("input %q: invalid blob hash", in.Name)
			}
		case in.URL != "" && in.Blob == "":
			if !(strings.HasPrefix(in.URL, "http://") || strings.HasPrefix(in.URL, "https://")) || len(in.URL) > 2048 {
				return fmt.Errorf("input %q: url must be http(s)", in.Name)
			}
			if !ValidSHA256(in.SHA256) || in.Size <= 0 {
				return fmt.Errorf("input %q: url inputs need sha256 and size", in.Name)
			}
			total += in.Size
		default:
			return fmt.Errorf("input %q needs exactly one of blob or url", in.Name)
		}
	}
	if r.DiskMB > 0 && total > int64(r.DiskMB)<<20 {
		return fmt.Errorf("url inputs (%d bytes) exceed disk_mb", total)
	}
	if len(s.Outputs) > MaxOutputs {
		return fmt.Errorf("at most %d output patterns", MaxOutputs)
	}
	for _, o := range s.Outputs {
		if !ValidOutputPattern(o) {
			return fmt.Errorf("output pattern %q is not a safe relative glob", o)
		}
	}
	q := s.Requirements
	if len(q.Arch) > 8 || len(q.CPUFlags) > 32 || len(q.Labels) > MaxLabels || len(q.Nodes) > 1024 {
		return fmt.Errorf("requirements too large")
	}
	for _, a := range q.Arch {
		if !archRE.MatchString(a) {
			return fmt.Errorf("invalid arch %q", a)
		}
	}
	for _, f := range q.CPUFlags {
		if !flagRE.MatchString(f) {
			return fmt.Errorf("invalid cpu flag %q", f)
		}
	}
	for k, v := range q.Labels {
		if !ValidLabelKey(k) || len(v) > 128 {
			return fmt.Errorf("invalid label requirement %q", k)
		}
	}
	switch q.Isolation {
	case "", IsolationFull, IsolationAny:
	default:
		return fmt.Errorf("isolation must be full or any")
	}
	if q.MinMemMB < 0 {
		return fmt.Errorf("min_mem_mb must be >= 0")
	}
	return nil
}

// ExpandTemplate replaces {{index}} and {{count}} in s.
func ExpandTemplate(s string, index, count int) string {
	if !strings.Contains(s, "{{") {
		return s
	}
	return strings.NewReplacer("{{index}}", fmt.Sprint(index), "{{count}}", fmt.Sprint(count)).Replace(s)
}
