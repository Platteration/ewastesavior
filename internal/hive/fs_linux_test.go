package hive

import "testing"

func TestMountInfo(t *testing.T) {
	t.Parallel()
	if !isMountPoint("/") || isMountPoint(t.TempDir()) {
		t.Fatal("mount point detection")
	}
	if unescapeMount(`/media/my\040stick`) != "/media/my stick" || unescapeMount(`/a\\b`) != `/a\\b` {
		t.Fatal("mountinfo unescape")
	}
	fi, err := statFS(t.TempDir())
	if err != nil || fi.total <= 0 {
		t.Fatalf("statfs: %+v %v", fi, err)
	}
}
