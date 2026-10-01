package node

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/platteration/ewastesavior/internal/proto"
)

// These tests check that a blob transfer survives what a flaky link or a
// restarting hive does to it (chaos soak: one lost request failed the
// task as an input or output node error, and three such quarantined a
// healthy node).

func fastTransferRetries(t *testing.T) {
	b, m, f := transferBackoff, transferBackoffMax, transferRetryFor
	transferBackoff, transferBackoffMax, transferRetryFor = 5*time.Millisecond, 20*time.Millisecond, 5*time.Second
	t.Cleanup(func() { transferBackoff, transferBackoffMax, transferRetryFor = b, m, f })
}

func hexSum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// sessionAgent is an agent whose current hive session uses token.
func sessionAgent(base, token string) *Agent {
	a := fakeAgent(nil, 0, nil)
	hc := newHiveClient(base, "")
	hc.setToken(token)
	a.hc = hc
	return a
}

func newSession(a *Agent, token string) {
	a.mu.Lock()
	base := a.hc.base
	a.mu.Unlock()
	hc := newHiveClient(base, "")
	hc.setToken(token)
	a.mu.Lock()
	a.hc = hc
	a.mu.Unlock()
}

func TestTransferUploadRetries(t *testing.T) {
	fastTransferRetries(t)
	data := bytes.Repeat([]byte("output line\n"), 40000)
	sum := hexSum(data)
	var mu sync.Mutex
	var tokens []string
	var a *Agent
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		tokens = append(tokens, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		n := len(tokens)
		mu.Unlock()
		switch n {
		case 1: // the connection drops after the upload
			io.Copy(io.Discard, r.Body)
			panic(http.ErrAbortHandler)
		case 2: // an answer before the body is read
			w.WriteHeader(http.StatusServiceUnavailable)
		case 3: // the hive restarted; the node registers again a bit later
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprintln(w, `{"error":"unknown node token; register again"}`)
			go func() {
				time.Sleep(50 * time.Millisecond)
				newSession(a, "new")
			}()
		default:
			body, err := io.ReadAll(r.Body)
			if err != nil || r.ContentLength != int64(len(data)) || !bytes.Equal(body, data) {
				w.WriteHeader(http.StatusBadRequest)
				fmt.Fprintf(w, `{"error":"got %d bytes (content-length %d): %v"}`, len(body), r.ContentLength, err)
				return
			}
			fmt.Fprintln(w, "{}")
		}
	}))
	t.Cleanup(srv.Close)
	a = sessionAgent(srv.URL, "old")
	if err := (transfer{a}).UploadBlob(context.Background(), sum, int64(len(data)), bytes.NewReader(data)); err != nil {
		t.Fatalf("upload: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if want := []string{"old", "old", "old", "new"}; strings.Join(tokens, ",") != strings.Join(want, ",") {
		t.Fatalf("tokens per attempt %v, want %v (no stale token after a 401)", tokens, want)
	}
}

func TestTransferRefusalIsNotRetried(t *testing.T) {
	fastTransferRetries(t)
	for _, code := range []int{http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound, http.StatusRequestEntityTooLarge} {
		calls := 0
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls++
			w.WriteHeader(code)
			fmt.Fprintln(w, `{"error":"no"}`)
		}))
		a := sessionAgent(srv.URL, "tok")
		data := []byte("x")
		err := (transfer{a}).UploadBlob(context.Background(), hexSum(data), 1, bytes.NewReader(data))
		_, ferr := (transfer{a}).FetchBlob(context.Background(), hexSum(data), io.Discard)
		srv.Close()
		if statusOf(err) != code || statusOf(ferr) != code || calls != 2 {
			t.Errorf("HTTP %d: upload %v, download %v after %d requests", code, err, ferr, calls)
		}
	}
}

func TestTransferFetchResumes(t *testing.T) {
	fastTransferRetries(t)
	data := bytes.Repeat([]byte("input data "), 30000)
	sum := hexSum(data)
	var mu sync.Mutex
	var ranges []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		ranges = append(ranges, r.Header.Get("Range"))
		n := len(ranges)
		mu.Unlock()
		switch n {
		case 1: // lost before any answer
			panic(http.ErrAbortHandler)
		case 2: // the connection breaks a third of the way in
			w.Header().Set("Content-Length", fmt.Sprint(len(data)))
			w.WriteHeader(http.StatusOK)
			w.Write(data[:len(data)/3])
			w.(http.Flusher).Flush()
			panic(http.ErrAbortHandler)
		default:
			http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(data))
		}
	}))
	t.Cleanup(srv.Close)
	a := sessionAgent(srv.URL, "tok")
	var got bytes.Buffer
	n, err := (transfer{a}).FetchBlob(context.Background(), sum, &got)
	if err != nil || n != int64(len(data)) || !bytes.Equal(got.Bytes(), data) {
		t.Fatalf("download: %d bytes, %v; content ok %v", n, err, bytes.Equal(got.Bytes(), data))
	}
	mu.Lock()
	defer mu.Unlock()
	if len(ranges) != 3 || ranges[0] != "" || ranges[1] != "" || !strings.HasPrefix(ranges[2], "bytes=") || ranges[2] == "bytes=0-" {
		t.Fatalf("Range headers %q: want the third request to resume", ranges)
	}
}

func TestTransferFetchVerifiesHash(t *testing.T) {
	fastTransferRetries(t)
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Write([]byte("not what was asked for"))
	}))
	t.Cleanup(srv.Close)
	a := sessionAgent(srv.URL, "tok")
	_, err := (transfer{a}).FetchBlob(context.Background(), hexSum([]byte("wanted")), io.Discard)
	if err == nil || !strings.Contains(err.Error(), "hash mismatch") || calls != 1 {
		t.Fatalf("wrong content: %v after %d requests", err, calls)
	}
}

func TestTransferWaitsForASession(t *testing.T) {
	fastTransferRetries(t)
	data := []byte("between sessions")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		fmt.Fprintln(w, "{}")
	}))
	t.Cleanup(srv.Close)
	a := sessionAgent(srv.URL, "tok")
	a.hc = nil // between sessions
	go func() {
		time.Sleep(50 * time.Millisecond)
		hc := newHiveClient(srv.URL, "")
		a.mu.Lock()
		a.hc = hc
		a.mu.Unlock()
	}()
	if err := (transfer{a}).UploadBlob(context.Background(), hexSum(data), int64(len(data)), bytes.NewReader(data)); err != nil {
		t.Fatalf("upload: %v", err)
	}
	// Retries end with the task's context.
	a.mu.Lock()
	a.hc = nil
	a.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := (transfer{a}).FetchBlob(ctx, hexSum(data), io.Discard); err == nil || time.Since(start) > 2*time.Second {
		t.Fatalf("download without a session: %v after %s", err, time.Since(start))
	}
}

// logRunner writes a line of output and succeeds.
type logRunner struct{ fakeRunner }

func (r *logRunner) Run(_ context.Context, t proto.Task, logs io.Writer, _ func(proto.RunningTask)) proto.TaskReport {
	fmt.Fprintln(logs, "task output")
	return proto.TaskReport{Lease: t.Lease, State: proto.TaskSucceeded}
}

func logShippers() int {
	buf := make([]byte, 1<<20)
	return strings.Count(string(buf[:runtime.Stack(buf, true)]), "node.(*logStream).run(")
}

// A task's log shipper ends with the task: once the final report is
// answered, a log tail the hive never got is given up (chaos soak: the
// shipper kept posting every 2 s, with stale credentials, after the agent
// had stopped).
func TestLogShipperEndsWithTheTask(t *testing.T) {
	var mu sync.Mutex
	logs, reports := 0, 0
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/tasks/{id}/log", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		logs++
		mu.Unlock()
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	mux.HandleFunc("POST /api/v1/tasks/{id}/report", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		reports++
		mu.Unlock()
		fmt.Fprintln(w, "{}")
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	before := logShippers()
	a := fakeAgent(&logRunner{fakeRunner{slots: 1}}, 1, nil)
	hc := newHiveClient(srv.URL, "")
	a.hc = hc
	a.startTask(context.Background(), hc, proto.Task{ID: "t1", Lease: "l1"})
	eventually(t, "the final report answered", func() bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		return len(a.tasks) == 0
	})
	eventually(t, "the log shipper to end", func() bool { return logShippers() <= before })
	mu.Lock()
	defer mu.Unlock()
	if reports != 1 || logs == 0 {
		t.Fatalf("%d reports, %d log posts", reports, logs)
	}
}
