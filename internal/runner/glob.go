package runner

import (
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/platteration/ewastesavior/internal/proto"
)

// entryType classifies a directory entry without following symlinks.
type entryType int

const (
	entryOther entryType = iota // symlink, FIFO, socket, device, unknown
	entryFile
	entryDir
)

// dirEntry is one name in a directory listing.
type dirEntry struct {
	name string
	typ  entryType
}

// treeLister reads a task workdir without following symlinks. Paths are
// slash-separated and relative to the workdir; "" is the workdir itself.
type treeLister interface {
	list(dir string) ([]dirEntry, error)
	lstat(p string) (entryType, bool)
}

// maxScanEntries bounds how many directory entries output matching reads,
// so a task that creates millions of files can't stall the agent.
const maxScanEntries = 200000

// hasMeta reports whether a pattern segment contains glob syntax.
func hasMeta(s string) bool { return strings.ContainsAny(s, `*?[\`) }

// matchOutputs expands the output patterns over the workdir. Only real
// directories are descended into. It returns the sorted, de-duplicated
// matches that are regular files, plus notes about skipped candidates.
func matchOutputs(l treeLister, patterns []string) (files []string, notes []string, err error) {
	found := map[string]bool{}
	scanned := 0
	noted := map[string]bool{}
	note := func(format string, args ...any) {
		n := fmt.Sprintf(format, args...)
		if !noted[n] {
			noted[n] = true
			notes = append(notes, n)
		}
	}
	for _, pat := range patterns {
		if !proto.ValidOutputPattern(pat) {
			note("output pattern %q is invalid; ignored", pat)
			continue
		}
		segs := strings.Split(pat, "/")
		cur := []string{""}
		for i, seg := range segs {
			last := i == len(segs)-1
			var next []string
			for _, dir := range cur {
				cands, err := expandSegment(l, dir, seg, &scanned)
				if err != nil {
					return nil, notes, err
				}
				for _, c := range cands {
					p := joinRel(dir, c.name)
					switch {
					case !last:
						if c.typ == entryDir {
							next = append(next, p)
						}
					case c.typ == entryFile:
						if !proto.ValidRelPath(p) {
							if !strings.HasPrefix(p, ".savior") {
								note("skipping output %q: name not allowed", p)
							}
							continue
						}
						found[p] = true
					case c.typ == entryDir:
						note("skipping output %q: it is a directory (use %q)", p, p+"/*")
					default:
						note("skipping output %q: not a regular file", p)
					}
				}
			}
			cur = next
			if len(cur) == 0 {
				break
			}
		}
	}
	files = make([]string, 0, len(found))
	for p := range found {
		files = append(files, p)
	}
	sort.Strings(files)
	return files, notes, nil
}

// expandSegment returns the entries of dir matching one pattern segment.
func expandSegment(l treeLister, dir, seg string, scanned *int) ([]dirEntry, error) {
	if !hasMeta(seg) {
		typ, ok := l.lstat(joinRel(dir, seg))
		if !ok {
			return nil, nil
		}
		return []dirEntry{{name: seg, typ: typ}}, nil
	}
	ents, err := l.list(dir)
	if err != nil {
		return nil, err
	}
	*scanned += len(ents)
	if *scanned > maxScanEntries {
		return nil, fmt.Errorf("more than %d directory entries in the workdir", maxScanEntries)
	}
	var out []dirEntry
	for _, e := range ents {
		if ok, _ := path.Match(seg, e.name); ok {
			out = append(out, e)
		}
	}
	return out, nil
}

func joinRel(dir, name string) string {
	if dir == "" {
		return name
	}
	return dir + "/" + name
}
