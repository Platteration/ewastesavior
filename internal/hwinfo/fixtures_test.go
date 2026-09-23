package hwinfo

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// machines are the fixture trees under testdata (see testdata/README).
var machines = []string{"thinkpad-t60", "p4-desktop", "atom-netbook", "amd-desktop", "qemu"}

// fixture returns the absolute path of a fixture tree. The trees use
// symlinks like real sysfs; checkouts without symlink support skip.
func fixture(t *testing.T, name string) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Lstat(filepath.Join(root, "sys/class/net/eth0/device"))
	if err != nil {
		t.Fatalf("fixture %s: %v", name, err)
	}
	if fi.Mode()&fs.ModeSymlink == 0 {
		t.Skip("fixture symlinks were not checked out as symlinks")
	}
	return root
}

// copyFixture copies a fixture tree (symlinks included) into a temp dir so
// a test can modify it.
func copyFixture(t *testing.T, name string) string {
	t.Helper()
	src := fixture(t, name)
	dst := filepath.Join(t.TempDir(), name)
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		switch {
		case d.Type()&fs.ModeSymlink != 0:
			link, err := os.Readlink(p)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		case d.IsDir():
			return os.MkdirAll(target, 0o755)
		default:
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			return os.WriteFile(target, b, 0o644)
		}
	})
	if err != nil {
		t.Fatalf("copy fixture: %v", err)
	}
	return dst
}

// writeFile creates root/rel (and its parents) with content.
func writeFile(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// symlink creates root/rel pointing at target (relative targets as in sysfs).
func symlink(t *testing.T, root, rel, target string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.FromSlash(target), p); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
}

// removeAll deletes root/rel.
func removeAll(t *testing.T, root, rel string) {
	t.Helper()
	if err := os.RemoveAll(filepath.Join(root, filepath.FromSlash(rel))); err != nil {
		t.Fatal(err)
	}
}
