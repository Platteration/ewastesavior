package hive

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
)

func securityBlobServer(t *testing.T) *Server {
	t.Helper()
	blobs, err := newBlobStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{}
	s.blobs = blobs
	s.cfg.tune.diskFree = func(string) (int64, int64, bool) {
		return 300, 1000, true // reserve 100; 200 bytes may be admitted
	}
	s.log = slog.New(slog.NewTextHandler(io.Discard, nil))
	s.blobMeta = map[string]*blob{}
	s.warnings = map[string]string{}
	s.wake = make(chan struct{})
	return s
}

func TestConcurrentBlobReservationsProtectHeadroom(t *testing.T) {
	s := securityBlobServer(t)
	start := make(chan struct{})
	releases := make(chan func(), 20)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			release, err := s.reserveBlobSpace(60)
			if err == nil {
				releases <- release
			} else if !errors.Is(err, errDiskFull) {
				t.Errorf("unexpected reservation error: %v", err)
			}
		}()
	}
	close(start)
	wg.Wait()
	close(releases)
	if len(releases) != 3 || s.blobReserved != 180 {
		t.Fatalf("over-admitted uploads: accepted=%d reserved=%d", len(releases), s.blobReserved)
	}
	for release := range releases {
		release()
	}
	if s.blobReserved != 0 {
		t.Fatalf("reservation leaked: %d", s.blobReserved)
	}
	release, err := s.reserveBlobSpace(200)
	if err != nil {
		t.Fatalf("capacity not returned: %v", err)
	}
	release()
}

func TestBlobLengthsAndFailureCleanup(t *testing.T) {
	for _, tc := range []struct {
		name   string
		length int64
		body   string
		status int
	}{
		{"chunked", -1, "abc", http.StatusLengthRequired},
		{"under-declared", 2, "abc", http.StatusRequestEntityTooLarge},
		{"over-declared", 4, "abc", http.StatusBadRequest},
		{"above-admission-limit", 201, "", http.StatusInsufficientStorage},
		{"valid", 3, "abc", http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := securityBlobServer(t)
			r := httptest.NewRequest(http.MethodPost, "/api/blobs", strings.NewReader(tc.body))
			r.ContentLength = tc.length
			info, status, err := s.receiveBlob(httptest.NewRecorder(), r, "", 1000, false)
			if status != tc.status || (status == http.StatusOK) != (err == nil) {
				t.Fatalf("status=%d error=%v, want %d", status, err, tc.status)
			}
			if status == http.StatusOK && info.Size != 3 {
				t.Fatalf("valid blob size=%d", info.Size)
			}
			entries, err := os.ReadDir(s.blobs.tmp)
			if err != nil || len(entries) != 0 || s.blobReserved != 0 {
				t.Fatalf("upload cleanup: temps=%d reservation=%d error=%v", len(entries), s.blobReserved, err)
			}
			if status != http.StatusOK && len(s.blobMeta) != 0 {
				t.Fatal("rejected upload committed a blob")
			}
		})
	}
}

func TestBlobHashFailureRemovesTemporaryFile(t *testing.T) {
	s := securityBlobServer(t)
	_, _, _, err := s.blobs.receive(strings.NewReader("abc"), strings.Repeat("0", 64), 3)
	if !errors.Is(err, errBlobMismatch) {
		t.Fatalf("want hash mismatch, got %v", err)
	}
	entries, err := os.ReadDir(s.blobs.tmp)
	if err != nil || len(entries) != 0 {
		t.Fatalf("hash failure leaked temporary file: %d, %v", len(entries), err)
	}
}

func TestBlobSpaceProbeFailureRefusesWrite(t *testing.T) {
	s := securityBlobServer(t)
	s.cfg.tune.diskFree = func(string) (int64, int64, bool) { return 0, 0, false }
	r := httptest.NewRequest(http.MethodPost, "/api/blobs", strings.NewReader("abc"))
	_, status, err := s.receiveBlob(httptest.NewRecorder(), r, "", 1000, false)
	if status != http.StatusServiceUnavailable || err == nil {
		t.Fatalf("failed disk probe admitted upload: %d, %v", status, err)
	}
}
