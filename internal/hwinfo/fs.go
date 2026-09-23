package hwinfo

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// ErrUnsupported is returned when the live system (root "" or "/") is read
// on an operating system other than Linux.
var ErrUnsupported = errors.New("hwinfo: reading live hardware information is only supported on Linux")

// maxAttrSize caps reads of sysfs and procfs attributes. Real attributes are
// at most a page; the cap only protects against a bogus root.
const maxAttrSize = 64 << 10

// maxListSize caps how many entries of one directory are looked at.
const maxListSize = 4096

// isLive reports whether root refers to the running system.
func isLive(root string) bool {
	return root == "" || filepath.Clean(root) == string(filepath.Separator)
}

// rootPath joins a slash-separated path below root.
func rootPath(root, rel string) string {
	if root == "" {
		root = string(filepath.Separator)
	}
	return filepath.Join(root, filepath.FromSlash(rel))
}

// readFileLimit reads at most max bytes of the file at p.
func readFileLimit(p string, max int64) ([]byte, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, max))
}

// readAttr returns the trimmed content of a small attribute file.
func readAttr(p string) (string, bool) {
	b, err := readFileLimit(p, maxAttrSize)
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(b)), true
}

// attr is readAttr without the ok flag.
func attr(p string) string {
	s, _ := readAttr(p)
	return s
}

// readInt parses an integer attribute.
func readInt(p string) (int64, bool) {
	s, ok := readAttr(p)
	if !ok {
		return 0, false
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

// linkBase returns the last element of the symlink target at p, or "".
func linkBase(p string) string {
	t, err := os.Readlink(p)
	if err != nil || t == "" {
		return ""
	}
	return filepath.Base(filepath.FromSlash(t))
}

// exists reports whether p exists (a dangling symlink counts).
func exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

// isDir reports whether p is a directory, following symlinks.
func isDir(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

// listDir returns up to maxListSize entry names of dir in natural order.
// Symlinks are listed like any other entry. Errors yield nil.
func listDir(dir string) []string {
	f, err := os.Open(dir)
	if err != nil {
		return nil
	}
	defer f.Close()
	names, _ := f.Readdirnames(maxListSize)
	sort.Slice(names, func(i, j int) bool { return naturalLess(names[i], names[j]) })
	return names
}

// numberedName splits names like "card12" into ("card", 12). ok is false when
// name isn't prefix followed by only digits.
func numberedName(name, prefix string) (int, bool) {
	digits, ok := strings.CutPrefix(name, prefix)
	if !ok || digits == "" || len(digits) > 6 {
		return 0, false
	}
	n := 0
	for i := 0; i < len(digits); i++ {
		if !isDigit(digits[i]) {
			return 0, false
		}
		n = n*10 + int(digits[i]-'0')
	}
	return n, true
}

// naturalLess orders strings so that embedded numbers compare numerically
// ("fb2" < "fb10", "hwmon9" < "hwmon10").
func naturalLess(a, b string) bool {
	for a != "" && b != "" {
		ca, cb := a[0], b[0]
		if isDigit(ca) && isDigit(cb) {
			na, ra := leadingDigits(a)
			nb, rb := leadingDigits(b)
			// Compare numerically by length after stripping zeros, then lexically.
			ta, tb := strings.TrimLeft(na, "0"), strings.TrimLeft(nb, "0")
			if len(ta) != len(tb) {
				return len(ta) < len(tb)
			}
			if ta != tb {
				return ta < tb
			}
			if na != nb {
				return na < nb
			}
			a, b = ra, rb
			continue
		}
		if ca != cb {
			return ca < cb
		}
		a, b = a[1:], b[1:]
	}
	return len(a) < len(b)
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func leadingDigits(s string) (digits, rest string) {
	i := 0
	for i < len(s) && isDigit(s[i]) {
		i++
	}
	return s[:i], s[i:]
}

// collapseSpace trims s and replaces runs of whitespace with one space
// ("Genuine Intel(R) CPU           T2400" -> "Genuine Intel(R) CPU T2400").
func collapseSpace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// devicesRel resolves p (a sysfs class or block entry, or a device link) and
// returns its path relative to root's sys/devices, slash-separated. ok is
// false when p doesn't resolve to somewhere below sys/devices.
func devicesRel(root, p string) (string, bool) {
	real, err := filepath.EvalSymlinks(p)
	if err != nil {
		return "", false
	}
	base, err := filepath.EvalSymlinks(rootPath(root, "sys/devices"))
	if err != nil {
		return "", false
	}
	rel, err := filepath.Rel(base, real)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return filepath.ToSlash(rel), true
}
