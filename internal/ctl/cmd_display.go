package ctl

import (
	"context"
	"errors"
	"fmt"
	"image"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/platteration/ewastesavior/internal/imaging"
	"github.com/platteration/ewastesavior/internal/proto"
)

// contentFlags are the display content options shared by `display` and
// `wall`.
type contentFlags struct {
	text, title, fg, bg        string
	image, images, imagesDir   string
	fit, clockFormat, timezone string
	interval                   int
	color                      string // wall: solid color content
	test                       bool   // wall: calibration pattern
}

func (cf *contentFlags) register(f *flags, wall bool) {
	f.StringVar(&cf.text, "text", "", "text to show (mode text)")
	f.StringVar(&cf.title, "title", "", "title above the text (mode text)")
	f.StringVar(&cf.fg, "fg", "", "text color #rrggbb (the # may be left out)")
	f.StringVar(&cf.bg, "bg", "", "background color #rrggbb")
	f.StringVar(&cf.image, "image", "", "image file (uploaded) or http(s) URL (mode image)")
	f.StringVar(&cf.images, "images", "", "comma-separated image files or URLs (mode slideshow)")
	f.StringVar(&cf.imagesDir, "images-dir", "", "every image in this directory, by name (mode slideshow)")
	f.StringVar(&cf.fit, "fit", "", "contain (default), cover or stretch")
	f.IntVar(&cf.interval, "interval", 0, "seconds per slide (default 10, minimum 3)")
	if wall {
		f.StringVar(&cf.color, "color", "", "fill the wall with one color #rrggbb")
		f.BoolVar(&cf.test, "test", false, "show the calibration test pattern (default content)")
		return
	}
	f.StringVar(&cf.clockFormat, "clock-format", "", "Go time layout for mode clock (default 15:04)")
	f.StringVar(&cf.timezone, "timezone", "", "IANA time zone for mode clock (default: the node's)")
}

// pendingUpload is a local image to upload before the spec is sent.
type pendingUpload struct {
	file string
	sha  string
	size int64
}

func normColor(s string) string {
	if s != "" && !strings.HasPrefix(s, "#") {
		return "#" + s
	}
	return s
}

// buildContent turns content flags into a DisplaySpec for mode. Local
// images are checked and hashed (so the spec can be validated before
// anything is uploaded) and returned for upload.
func (a *app) buildContent(mode string, cf *contentFlags) (proto.DisplaySpec, []*pendingUpload, error) {
	spec := proto.DisplaySpec{
		Mode: mode, Title: cf.title, Text: cf.text, FG: normColor(cf.fg), BG: normColor(cf.bg),
		IntervalS: cf.interval, Fit: cf.fit, ClockFormat: cf.clockFormat, Timezone: cf.timezone,
	}
	onlyFor := func(set bool, flag string, modes ...string) error {
		if set && !slices.Contains(modes, mode) {
			return fmt.Errorf("--%s does not apply to mode %s", flag, mode)
		}
		return nil
	}
	images := cf.images != "" || cf.imagesDir != ""
	for _, err := range []error{
		onlyFor(cf.image != "", "image", proto.DisplayImage),
		onlyFor(cf.images != "", "images", proto.DisplaySlideshow),
		onlyFor(cf.imagesDir != "", "images-dir", proto.DisplaySlideshow),
		onlyFor(cf.interval != 0, "interval", proto.DisplaySlideshow),
		onlyFor(cf.fit != "", "fit", proto.DisplayImage, proto.DisplaySlideshow),
		onlyFor(cf.text != "", "text", proto.DisplayText),
		onlyFor(cf.title != "", "title", proto.DisplayText),
		onlyFor(cf.clockFormat != "", "clock-format", proto.DisplayClock),
		onlyFor(cf.timezone != "", "timezone", proto.DisplayClock),
	} {
		if err != nil {
			return spec, nil, err
		}
	}
	var uploads []*pendingUpload
	if cf.image != "" {
		m, up, err := resolveMedia(cf.image)
		if err != nil {
			return spec, nil, err
		}
		spec.Image = &m
		if up != nil {
			uploads = append(uploads, up)
		}
	}
	if images {
		var srcs []string
		for _, s := range strings.Split(cf.images, ",") {
			if s = strings.TrimSpace(s); s != "" {
				srcs = append(srcs, s)
			}
		}
		if cf.imagesDir != "" {
			files, err := imageFilesIn(cf.imagesDir)
			if err != nil {
				return spec, nil, err
			}
			srcs = append(srcs, files...)
		}
		if len(srcs) == 0 {
			return spec, nil, errors.New("no images given")
		}
		if len(srcs) > proto.MaxImages {
			return spec, nil, fmt.Errorf("%d images; at most %d fit in a slideshow", len(srcs), proto.MaxImages)
		}
		spec.Images = make([]proto.Media, len(srcs))
		for i, s := range srcs {
			m, up, err := resolveMedia(s)
			if err != nil {
				return spec, nil, err
			}
			spec.Images[i] = m
			if up != nil {
				uploads = append(uploads, up)
			}
		}
	}
	return spec, uploads, nil
}

// resolveMedia turns a --image value into Media: a URL is used as is, a
// local file is checked and hashed and must be uploaded.
func resolveMedia(s string) (proto.Media, *pendingUpload, error) {
	if strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://") {
		if len(s) > 2048 {
			return proto.Media{}, nil, errors.New("image URL longer than 2048 characters")
		}
		return proto.Media{URL: s}, nil, nil
	}
	sha, size, err := checkImageFile(s)
	if err != nil {
		return proto.Media{}, nil, err
	}
	return proto.Media{Blob: sha}, &pendingUpload{file: s, sha: sha, size: size}, nil
}

// checkImageFile verifies that name is an image the nodes can show and
// returns its SHA-256 and size.
func checkImageFile(name string) (string, int64, error) {
	f, err := os.Open(name)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", 0, err
	}
	lim := imaging.DefaultLimits
	if !st.Mode().IsRegular() {
		return "", 0, fmt.Errorf("%s is not a regular file", name)
	}
	if st.Size() > lim.MaxBytes {
		return "", 0, fmt.Errorf("%s is larger than %d MiB", name, lim.MaxBytes>>20)
	}
	cfg, format, err := image.DecodeConfig(io.LimitReader(f, lim.MaxBytes))
	if err != nil {
		return "", 0, fmt.Errorf("%s: not a supported image (png, jpeg, gif, bmp, webp): %w", name, err)
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width > lim.MaxSide || cfg.Height > lim.MaxSide ||
		int64(cfg.Width)*int64(cfg.Height) > int64(lim.MaxPixels) {
		return "", 0, fmt.Errorf("%s: %s image is %dx%d; the limit is %d pixels per side and %d megapixels",
			name, format, cfg.Width, cfg.Height, lim.MaxSide, lim.MaxPixels>>20)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return "", 0, err
	}
	sha, size, err := hashReader(context.Background(), f)
	if err != nil {
		return "", 0, fmt.Errorf("%s: %w", name, err)
	}
	return sha, size, nil
}

var imageExts = []string{".png", ".jpg", ".jpeg", ".gif", ".bmp", ".webp"}

// imageFilesIn lists the image files in dir, sorted by name.
func imageFilesIn(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if !slices.Contains(imageExts, strings.ToLower(filepath.Ext(e.Name()))) {
			continue
		}
		p := filepath.Join(dir, e.Name())
		if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no image files (%s) in %s", strings.Join(imageExts, " "), dir)
	}
	return out, nil
}

// uploadMedia uploads pending local images (each distinct file once).
func (a *app) uploadMedia(ctx context.Context, c *Client, ups []*pendingUpload) error {
	done := map[string]bool{}
	for _, up := range ups {
		if done[up.sha] {
			continue
		}
		f, err := os.Open(up.file)
		if err != nil {
			return err
		}
		_, err = c.PutBlob(ctx, up.sha, up.size, f)
		f.Close()
		if err != nil {
			return fmt.Errorf("upload %s: %w", up.file, err)
		}
		done[up.sha] = true
		fmt.Fprintf(a.stderr, "uploaded %s (%s)\n", up.file, fmtBytes(up.size))
	}
	return nil
}

func cmdDisplay(a *app, ctx context.Context, args []string) int {
	f := a.flagSet("display", "<ref> <mode> [flags]", `Set what a node's screen shows. Modes: status, off, color, text, clock,
image, slideshow, dashboard, test. Local image files are uploaded to the hive.
Examples:
  savior ctl display lobby-1 text --text "Welcome" --bg 003366
  savior ctl display lobby-2 slideshow --images-dir ./photos --interval 15
  savior ctl display lobby-3 clock --timezone Europe/Berlin`)
	var cf contentFlags
	cf.register(f, false)
	pos, code, ok := a.parse(f, args, 2, 2)
	if !ok {
		return code
	}
	ref, mode := pos[0], pos[1]
	if mode == proto.DisplayWall {
		return a.usageError("walls are set up with 'savior ctl wall create'")
	}
	if !slices.Contains(proto.DisplayModes, mode) {
		return a.usageError("unknown mode %q (status, off, color, text, clock, image, slideshow, dashboard, test)", sanitizeCell(mode))
	}
	if err := checkRef("node", ref); err != nil {
		return a.usageError("%v", err)
	}
	spec, ups, err := a.buildContent(mode, &cf)
	if err != nil {
		return a.usageError("%v", err)
	}
	if err := proto.ValidateDisplaySpec(&spec); err != nil {
		return a.usageError("%v", err)
	}
	c, err := a.connect(ctx)
	if err != nil {
		return a.fail(err)
	}
	if err := a.uploadMedia(ctx, c, ups); err != nil {
		return a.fail(err)
	}
	return a.patchNode(ctx, ref, proto.NodePatch{Display: &spec}, func(n proto.NodeView) string {
		rev := ""
		if n.Display != nil {
			rev = fmt.Sprintf(" (rev %d)", n.Display.Rev)
		}
		return fmt.Sprintf("node %s now shows %s%s", describeNode(n), mode, rev)
	})
}

func cmdWall(a *app, ctx context.Context, args []string) int {
	usage := func(w io.Writer) {
		fmt.Fprint(w, `usage: savior ctl wall <command> [arguments]

Video walls spread one picture, slideshow, text or color over several
screens. Nodes are listed row by row; '-' leaves a cell empty.

commands:
  create --name N --rows R --cols C --nodes a,b,-,d [--gap-x MM --gap-y MM]
         [--image FILE|--images F1,F2|--images-dir DIR|--text T|--color #hex|--test]
  ls                        list walls
  show <id>                 show a wall's layout
  rm <id>                   delete a wall (its screens go back to status)
  test <id>                 show the calibration pattern
  content <id> [content flags]  change what a wall shows
`)
	}
	if len(args) == 0 {
		usage(a.stderr)
		return 2
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "create":
		return wallCreate(a, ctx, rest)
	case "ls", "list":
		return wallList(a, ctx, rest)
	case "show":
		return wallShow(a, ctx, rest)
	case "rm", "delete":
		return wallRemove(a, ctx, rest)
	case "test":
		return wallSetContent(a, ctx, rest, true)
	case "content":
		return wallSetContent(a, ctx, rest, false)
	case "-h", "--help", "help":
		usage(a.stdout)
		return 0
	}
	fmt.Fprintf(a.stderr, "savior ctl wall: unknown command %q\n\n", sanitizeCell(sub))
	usage(a.stderr)
	return 2
}

// wallContent builds wall content from exactly one content choice
// (default: test pattern).
func (a *app) wallContent(cf *contentFlags) (proto.DisplaySpec, []*pendingUpload, error) {
	var modes []string
	if cf.image != "" {
		modes = append(modes, proto.DisplayImage)
	}
	if cf.images != "" || cf.imagesDir != "" {
		modes = append(modes, proto.DisplaySlideshow)
	}
	if cf.text != "" || cf.title != "" {
		modes = append(modes, proto.DisplayText)
	}
	if cf.color != "" {
		modes = append(modes, proto.DisplayColor)
	}
	if cf.test {
		modes = append(modes, proto.DisplayTest)
	}
	if len(modes) > 1 {
		return proto.DisplaySpec{}, nil, errors.New("choose one of --image, --images/--images-dir, --text, --color or --test")
	}
	mode := proto.DisplayTest
	if len(modes) == 1 {
		mode = modes[0]
	}
	if cf.color != "" {
		if cf.bg != "" {
			return proto.DisplaySpec{}, nil, errors.New("use --color or --bg, not both")
		}
		cf.bg = cf.color
	}
	spec, ups, err := a.buildContent(mode, cf)
	if err != nil {
		return spec, nil, err
	}
	return spec, ups, nil
}

// wallCells lays out refs row by row; "-" is an empty cell.
func wallCells(rows, cols int, refs []string) ([]proto.WallCell, error) {
	if len(refs) != rows*cols {
		return nil, fmt.Errorf("--nodes lists %d cells but the wall has %d (%d rows x %d columns); use - for empty cells",
			len(refs), rows*cols, rows, cols)
	}
	var cells []proto.WallCell
	for i, ref := range refs {
		ref = strings.TrimSpace(ref)
		if ref == "-" {
			continue
		}
		if err := checkRef("node", ref); err != nil {
			return nil, fmt.Errorf("cell %d: %w (use - for an empty cell)", i+1, err)
		}
		cells = append(cells, proto.WallCell{Node: ref, Row: i / cols, Col: i % cols})
	}
	if len(cells) == 0 {
		return nil, errors.New("the wall has no nodes")
	}
	return cells, nil
}

func wallCreate(a *app, ctx context.Context, args []string) int {
	f := a.flagSet("wall create", "--name N --rows R --cols C --nodes a,b,-,d [flags]",
		`Create a video wall. --nodes lists node names, IDs or short codes row by
row (left to right, top to bottom); '-' leaves a cell empty. Screen sizes
come from the monitors' EDID; --gap-x/--gap-y are the bezel gaps in
millimeters.`)
	name := f.String("name", "", "wall name (required)")
	rows := f.Int("rows", 0, "rows of screens (1-16)")
	cols := f.Int("cols", 0, "columns of screens (1-16)")
	nodes := f.String("nodes", "", "comma-separated node refs, row-major; - for empty")
	gapX := f.Int("gap-x", 0, "horizontal gap between screens in mm")
	gapY := f.Int("gap-y", 0, "vertical gap between screens in mm")
	var cf contentFlags
	cf.register(f, true)
	if _, code, ok := a.parse(f, args, 0, 0); !ok {
		return code
	}
	if *name == "" || *rows < 1 || *cols < 1 || *nodes == "" {
		return a.usageError("--name, --rows, --cols and --nodes are required")
	}
	if *rows > proto.MaxWallSize || *cols > proto.MaxWallSize {
		return a.usageError("rows and cols must be 1..%d", proto.MaxWallSize)
	}
	cells, err := wallCells(*rows, *cols, strings.Split(*nodes, ","))
	if err != nil {
		return a.usageError("%v", err)
	}
	content, ups, err := a.wallContent(&cf)
	if err != nil {
		return a.usageError("%v", err)
	}
	w := proto.WallSpec{Name: *name, Rows: *rows, Cols: *cols, GapXMM: *gapX, GapYMM: *gapY, Cells: cells, Content: content}
	if err := proto.ValidateWallSpec(&w); err != nil {
		return a.usageError("%v", err)
	}
	c, err := a.connect(ctx)
	if err != nil {
		return a.fail(err)
	}
	if err := a.uploadMedia(ctx, c, ups); err != nil {
		return a.fail(err)
	}
	out, err := c.CreateWall(ctx, w)
	if err != nil {
		return a.fail(err)
	}
	if a.g.json {
		return a.printJSON(out)
	}
	fmt.Fprintf(a.stdout, "Created wall %s (%s, %dx%d, %d screens, showing %s).\n",
		sanitizeCell(out.ID), sanitizeCell(out.Name), out.Rows, out.Cols, len(out.Cells), sanitizeCell(out.Content.Mode))
	return 0
}

func wallList(a *app, ctx context.Context, args []string) int {
	f := a.flagSet("wall ls", "", "List video walls.")
	if _, code, ok := a.parse(f, args, 0, 0); !ok {
		return code
	}
	c, err := a.connect(ctx)
	if err != nil {
		return a.fail(err)
	}
	walls, err := c.Walls(ctx)
	if err != nil {
		return a.fail(err)
	}
	if a.g.json {
		return a.printJSON(walls)
	}
	t := newTable(a.stdout, "ID", "NAME", "SIZE", "SCREENS", "CONTENT")
	for _, w := range walls {
		t.row(w.ID, w.Name, fmt.Sprintf("%dx%d", w.Rows, w.Cols), strconv.Itoa(len(w.Cells)), w.Content.Mode)
	}
	t.flush()
	return 0
}

func wallShow(a *app, ctx context.Context, args []string) int {
	f := a.flagSet("wall show", "<id>", "Show a wall's layout.")
	pos, code, ok := a.parse(f, args, 1, 1)
	if !ok {
		return code
	}
	c, err := a.connect(ctx)
	if err != nil {
		return a.fail(err)
	}
	w, err := c.Wall(ctx, pos[0])
	if err != nil {
		return a.fail(err)
	}
	if a.g.json {
		return a.printJSON(w)
	}
	names := map[string]string{}
	if nodes, err := c.Nodes(ctx); err == nil {
		for _, n := range nodes {
			names[n.ID] = n.Name
		}
	}
	fmt.Fprintf(a.stdout, "Wall %s %q: %d rows x %d columns, gaps %dx%d mm, showing %s\n\n",
		sanitizeCell(w.ID), sanitizeCell(w.Name), w.Rows, w.Cols, w.GapXMM, w.GapYMM, sanitizeCell(describeContent(w.Content)))
	if w.Rows < 1 || w.Cols < 1 || w.Rows > proto.MaxWallSize || w.Cols > proto.MaxWallSize {
		return 0
	}
	grid := make([][]string, w.Rows)
	for r := range grid {
		grid[r] = make([]string, w.Cols)
	}
	for _, cell := range w.Cells {
		if cell.Row >= 0 && cell.Row < w.Rows && cell.Col >= 0 && cell.Col < w.Cols {
			label := cell.Node
			if n := names[cell.Node]; n != "" {
				label = n
			}
			grid[cell.Row][cell.Col] = label
		}
	}
	header := []string{""}
	for col := 0; col < w.Cols; col++ {
		header = append(header, "col "+strconv.Itoa(col+1))
	}
	t := newTable(a.stdout, header...)
	for r, row := range grid {
		t.row(append([]string{"row " + strconv.Itoa(r+1)}, row...)...)
	}
	t.flush()
	return 0
}

func describeContent(s proto.DisplaySpec) string {
	switch s.Mode {
	case proto.DisplaySlideshow:
		iv := s.IntervalS
		if iv == 0 {
			iv = proto.DefaultInterval
		}
		return fmt.Sprintf("slideshow (%d images, %d s)", len(s.Images), iv)
	case proto.DisplayText:
		return "text " + strconv.Quote(truncate(s.Text, 40))
	case proto.DisplayColor:
		return "color " + s.BG
	}
	return s.Mode
}

func wallRemove(a *app, ctx context.Context, args []string) int {
	f := a.flagSet("wall rm", "<id>", "Delete a wall; its screens go back to the status screen.")
	pos, code, ok := a.parse(f, args, 1, 1)
	if !ok {
		return code
	}
	c, err := a.connect(ctx)
	if err != nil {
		return a.fail(err)
	}
	if err := c.DeleteWall(ctx, pos[0]); err != nil {
		return a.fail(err)
	}
	if !a.g.json {
		fmt.Fprintf(a.stdout, "Deleted wall %s.\n", sanitizeCell(pos[0]))
	}
	return 0
}

// wallSetContent implements `wall test` and `wall content`.
func wallSetContent(a *app, ctx context.Context, args []string, test bool) int {
	name, synopsis, help := "wall content", "<id> [--image FILE|--images ...|--text T|--color #hex|--test]", "Change what a wall shows."
	if test {
		name, synopsis, help = "wall test", "<id>", "Show the calibration test pattern on a wall (grid, circles and each screen's position)."
	}
	f := a.flagSet(name, synopsis, help)
	var cf contentFlags
	if !test {
		cf.register(f, true)
	}
	pos, code, ok := a.parse(f, args, 1, 1)
	if !ok {
		return code
	}
	if test {
		cf.test = true
	}
	content, ups, err := a.wallContent(&cf)
	if err != nil {
		return a.usageError("%v", err)
	}
	c, err := a.connect(ctx)
	if err != nil {
		return a.fail(err)
	}
	w, err := c.Wall(ctx, pos[0])
	if err != nil {
		return a.fail(err)
	}
	w.Content = content
	if err := proto.ValidateWallSpec(&w); err != nil {
		return a.usageError("%v", err)
	}
	if err := a.uploadMedia(ctx, c, ups); err != nil {
		return a.fail(err)
	}
	out, err := c.UpdateWall(ctx, pos[0], w)
	if err != nil {
		return a.fail(err)
	}
	if a.g.json {
		return a.printJSON(out)
	}
	fmt.Fprintf(a.stdout, "Wall %s now shows %s.\n", sanitizeCell(out.ID), sanitizeCell(describeContent(out.Content)))
	return 0
}
