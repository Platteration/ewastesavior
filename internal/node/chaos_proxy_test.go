//go:build linux

package node

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/platteration/ewastesavior/internal/auth"
	"github.com/platteration/ewastesavior/internal/proto"
)

// faultProxy sits between the node agents and the hive. It terminates TLS
// with the hive's own certificate (from its data dir), so the nodes' pin
// and swarm-key proofs still verify, and forwards every request to the
// hive. On the way it drops requests, drops responses after the hive has
// acted on them, delays both, duplicates reports and claims, and cuts off
// partitioned nodes. It also records the node<->hive traffic the lease
// invariants need: every lease the hive issued (claim responses) and every
// task report with the hive's answer.
type faultProxy struct {
	c        *chaosCluster
	ln       net.Listener
	srv      *http.Server
	upstream *http.Client

	mu        sync.Mutex
	rng       *rand.Rand
	rates     faultRates
	partition map[string]time.Time // node ID -> until
	// dropIf, when set, drops the requests it returns true for (repros).
	dropIf func(kind, node string, r *http.Request) bool
	tokens map[string]string // node token -> node ID
	stats  map[string]int

	leases    map[string]*leaseRec   // lease -> issue record
	byTask    map[string][]*leaseRec // task -> leases in issue order
	reports   []*reportRec
	repByTask map[string][]*reportRec
	minSent   map[string]time.Time // task|lease|state -> when its first copy was sent to the hive
	replayQ   []replayItem
	adopted   map[string]int // node -> tasks adopted on its last registration
	lastSent  map[string]time.Time
}

// faultRates are per-request probabilities.
type faultRates struct {
	dropReq, dropResp, delay float64
	maxDelay                 time.Duration
	dupReport, dupClaim      float64
}

var noFaults = faultRates{}

type leaseRec struct {
	task, lease, node, job string
	index, attempt         int
	sent, issued           time.Time // when the claim went to the hive; when the proxy read the answer
	// lost: issued after the state a crashed hive came back from, so that
	// hive never knew it (certain), or maybe never (lostMaybe).
	lost, lostMaybe bool
}

type reportRec struct {
	src               string // node | dup | replay
	node, task, lease string
	state             proto.TaskState
	kind, err         string
	exit              int
	outputs           []proto.Output
	status            int
	sent, answered    time.Time
	attempt           int // from the lease record, 0 if unknown
	hiveEpoch         int
	// A crashed hive certainly (lost) or perhaps (maybeLost) forgot that
	// it acted on this report.
	lost, maybeLost bool
}

type replayItem struct {
	token, task string
	body        []byte
}

func newFaultProxy(c *chaosCluster) *faultProxy {
	cert, err := tls.LoadX509KeyPair(filepath.Join(c.hiveDir, "tls/cert.pem"), filepath.Join(c.hiveDir, "tls/key.pem"))
	if err != nil {
		c.t.Fatal(err)
	}
	if fp := auth.Fingerprint(cert.Certificate[0]); fp != c.fp {
		c.t.Fatalf("proxy certificate %s is not the hive's %s", fp, c.fp)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		c.t.Fatal(err)
	}
	p := &faultProxy{
		c:         c,
		ln:        ln,
		rng:       rand.New(rand.NewSource(c.cfg.seed ^ 0x5eed)),
		partition: map[string]time.Time{},
		tokens:    map[string]string{},
		stats:     map[string]int{},
		leases:    map[string]*leaseRec{},
		byTask:    map[string][]*leaseRec{},
		minSent:   map[string]time.Time{},
		repByTask: map[string][]*reportRec{},
		adopted:   map[string]int{},
		lastSent:  map[string]time.Time{},
	}
	p.upstream = &http.Client{Transport: &http.Transport{
		TLSClientConfig:     auth.ClientTLSConfig(c.fp, nil),
		MaxIdleConnsPerHost: 64,
		IdleConnTimeout:     30 * time.Second,
	}}
	p.srv = &http.Server{
		Handler:           p,
		ReadHeaderTimeout: 10 * time.Second,
		ErrorLog:          log.New(io.Discard, "", 0),
		TLSConfig:         &tls.Config{Certificates: []tls.Certificate{cert}, NextProtos: []string{"http/1.1"}},
	}
	go p.srv.ServeTLS(ln, "", "")
	return p
}

func (p *faultProxy) url() string { return "https://" + p.ln.Addr().String() }

func (p *faultProxy) close() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	p.srv.Shutdown(ctx)
	p.srv.Close()
	p.upstream.CloseIdleConnections()
}

func (p *faultProxy) setRates(r faultRates) {
	p.mu.Lock()
	p.rates = r
	p.mu.Unlock()
}

func (p *faultProxy) setDropIf(f func(kind, node string, r *http.Request) bool) {
	p.mu.Lock()
	p.dropIf = f
	p.mu.Unlock()
}

func (p *faultProxy) setPartition(node string, until time.Time) {
	p.mu.Lock()
	if until.IsZero() {
		delete(p.partition, node)
	} else {
		p.partition[node] = until
	}
	p.mu.Unlock()
}

func (p *faultProxy) partitioned(node string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	until, ok := p.partition[node]
	return ok && time.Now().Before(until)
}

func (p *faultProxy) count(k string) {
	p.mu.Lock()
	p.stats[k]++
	p.mu.Unlock()
}

// action is what the proxy does to one request.
type action struct {
	dropReq, dropResp, dup bool
	delayReq, delayResp    time.Duration
}

func (p *faultProxy) decide(kind, node string) action {
	p.mu.Lock()
	defer p.mu.Unlock()
	var a action
	if until, ok := p.partition[node]; ok && node != "" {
		if time.Now().Before(until) {
			a.dropReq = true
			p.stats["partition_drop"]++
			return a
		}
		delete(p.partition, node)
	}
	r := p.rates
	if kind == "hello" {
		return a // no node identity; the register that follows is faulted
	}
	f := p.rng.Float64
	if f() < r.dropReq {
		a.dropReq = true
		return a
	}
	if r.maxDelay > 0 && f() < r.delay {
		a.delayReq = time.Duration(p.rng.Int63n(int64(r.maxDelay)))
	}
	if r.maxDelay > 0 && f() < r.delay {
		a.delayResp = time.Duration(p.rng.Int63n(int64(r.maxDelay)))
	}
	if f() < r.dropResp {
		a.dropResp = true
	}
	switch kind {
	case "report":
		a.dup = f() < r.dupReport
	case "claim":
		a.dup = f() < r.dupClaim
	}
	return a
}

func classifyPath(r *http.Request) (kind, task string) {
	p := r.URL.Path
	switch {
	case p == "/api/v1/hello":
		return "hello", ""
	case p == "/api/v1/register":
		return "register", ""
	case p == "/api/v1/heartbeat":
		return "heartbeat", ""
	case p == "/api/v1/claim":
		return "claim", ""
	case strings.HasPrefix(p, "/api/v1/tasks/") && strings.HasSuffix(p, "/report"):
		return "report", strings.TrimSuffix(strings.TrimPrefix(p, "/api/v1/tasks/"), "/report")
	case strings.HasPrefix(p, "/api/v1/tasks/") && strings.HasSuffix(p, "/log"):
		return "log", strings.TrimSuffix(strings.TrimPrefix(p, "/api/v1/tasks/"), "/log")
	case strings.HasPrefix(p, "/api/v1/blobs/") && r.Method == http.MethodPut:
		return "blobput", ""
	case strings.HasPrefix(p, "/api/v1/blobs/"):
		return "blobget", ""
	}
	return "other", ""
}

func bearer(r *http.Request) string {
	return strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
}

func (p *faultProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 256<<20))
	if err != nil {
		panic(http.ErrAbortHandler)
	}
	kind, task := classifyPath(r)
	node := ""
	if kind == "register" {
		var req proto.RegisterRequest
		if json.Unmarshal(body, &req) == nil {
			node = req.NodeID
		}
	} else if tok := bearer(r); tok != "" {
		p.mu.Lock()
		node = p.tokens[tok]
		p.mu.Unlock()
	}
	act := p.decide(kind, node)
	p.mu.Lock()
	dropIf := p.dropIf
	p.mu.Unlock()
	if dropIf != nil && dropIf(kind, node, r) {
		act.dropReq = true
	}
	if act.dropReq {
		p.count("drop_req/" + kind)
		panic(http.ErrAbortHandler)
	}
	if act.delayReq > 0 {
		p.count("delay_req/" + kind)
		if !sleepCtx(r.Context(), act.delayReq) {
			panic(http.ErrAbortHandler)
		}
	}
	sent := time.Now()
	p.noteSent(kind, task, body, sent)
	status, hdr, rbody, err := p.forward(r.Context(), r.Method, r.URL.RequestURI(), r.Header, body)
	if err != nil {
		p.count("upstream_err/" + kind)
		panic(http.ErrAbortHandler)
	}
	p.observe(kind, node, task, bearer(r), body, status, rbody, sent, "node")
	if act.dup && status == http.StatusOK && (kind != "claim" || claimHasTasks(rbody)) {
		// The node's request again, as a retry after a lost response
		// would send it. A claim repeats its ClaimID.
		p.count("dup/" + kind)
		sent2 := time.Now()
		p.noteSent(kind, task, body, sent2)
		s2, h2, b2, err := p.forward(context.Background(), r.Method, r.URL.RequestURI(), r.Header, body)
		if err == nil {
			p.observe(kind, node, task, bearer(r), body, s2, b2, sent2, "dup")
			status, hdr, rbody = s2, h2, b2
		}
	}
	if act.dropResp {
		p.count("drop_resp/" + kind)
		panic(http.ErrAbortHandler)
	}
	if act.delayResp > 0 {
		p.count("delay_resp/" + kind)
		if !sleepCtx(r.Context(), act.delayResp) {
			panic(http.ErrAbortHandler)
		}
	}
	for k, vs := range hdr {
		if k == "Content-Length" || k == "Connection" {
			continue
		}
		w.Header()[k] = vs
	}
	w.WriteHeader(status)
	w.Write(rbody)
}

func claimHasTasks(b []byte) bool {
	var cr proto.ClaimResponse
	return json.Unmarshal(b, &cr) == nil && len(cr.Tasks) > 0
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func (p *faultProxy) forward(ctx context.Context, method, uri string, h http.Header, body []byte) (int, http.Header, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, "https://"+p.c.addr+uri, bytes.NewReader(body))
	if err != nil {
		return 0, nil, nil, err
	}
	for _, k := range []string{"Authorization", "Content-Type", "Range", "X-Savior-Api-Version"} {
		if v := h.Get(k); v != "" {
			req.Header.Set(k, v)
		}
	}
	resp, err := p.upstream.Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 256<<20))
	if err != nil {
		return 0, nil, nil, err
	}
	return resp.StatusCode, resp.Header, b, nil
}

// noteSent records when a copy of a report was first sent to the hive,
// whether or not its answer ever comes back: the hive may act on it.
func (p *faultProxy) noteSent(kind, task string, body []byte, sent time.Time) {
	if kind != "report" {
		return
	}
	var rep proto.TaskReport
	if json.Unmarshal(body, &rep) != nil {
		return
	}
	key := task + "|" + rep.Lease + "|" + string(rep.State)
	p.mu.Lock()
	if t, ok := p.minSent[key]; !ok || sent.Before(t) {
		p.minSent[key] = sent
	}
	p.mu.Unlock()
}

// observe records what the lease invariants need.
func (p *faultProxy) observe(kind, node, task, tok string, body []byte, status int, rbody []byte, sent time.Time, src string) {
	now := time.Now()
	switch kind {
	case "register":
		var req proto.RegisterRequest
		var resp proto.RegisterResponse
		if json.Unmarshal(body, &req) != nil || status != http.StatusOK || json.Unmarshal(rbody, &resp) != nil {
			return
		}
		p.mu.Lock()
		p.tokens[resp.Token] = req.NodeID
		p.adopted[req.NodeID] = len(resp.AdoptedTasks)
		p.mu.Unlock()
		p.c.ev.add("register %s: listed %d, adopted %d, cancel %d", req.NodeID, len(req.RunningTasks), len(resp.AdoptedTasks), len(resp.Directives.CancelTasks))
	case "claim":
		var resp proto.ClaimResponse
		if status != http.StatusOK || json.Unmarshal(rbody, &resp) != nil {
			return
		}
		p.mu.Lock()
		for _, t := range resp.Tasks {
			if l := p.leases[t.Lease]; l != nil {
				if l.task != t.ID || l.node != node || l.attempt != t.Attempt {
					p.mu.Unlock()
					p.c.viol.add("lease-reissued", "lease %s issued again for task %s attempt %d to %s (first: task %s attempt %d to %s)",
						short(t.Lease), t.ID, t.Attempt, node, l.task, l.attempt, l.node)
					p.mu.Lock()
				}
				continue // a ClaimID replay returns the same assignment
			}
			l := &leaseRec{task: t.ID, lease: t.Lease, node: node, job: t.JobID, index: t.Index, attempt: t.Attempt, sent: sent, issued: now}
			for _, prev := range p.byTask[t.ID] {
				if prev.attempt >= t.Attempt {
					p.mu.Unlock()
					p.c.viol.add("attempt-not-increasing", "task %s dispatched as attempt %d to %s after attempt %d (to %s)",
						t.ID, t.Attempt, node, prev.attempt, prev.node)
					p.mu.Lock()
					break
				}
			}
			p.leases[t.Lease] = l
			p.byTask[t.ID] = append(p.byTask[t.ID], l)
		}
		p.mu.Unlock()
	case "report":
		var rep proto.TaskReport
		if json.Unmarshal(body, &rep) != nil {
			return
		}
		rec := &reportRec{src: src, node: node, task: task, lease: rep.Lease, state: rep.State, kind: rep.ErrorKind, err: rep.Error,
			exit: rep.ExitCode, outputs: rep.Outputs, status: status, sent: sent, answered: now, hiveEpoch: p.c.epoch()}
		p.mu.Lock()
		if src == "node" && tok != "" {
			p.replayQ = append(p.replayQ, replayItem{token: tok, task: task, body: body})
			if len(p.replayQ) > 400 {
				p.replayQ = p.replayQ[100:]
			}
		}
		p.mu.Unlock()
		p.checkReport(rec)
	}
}

// checkReport applies the lease invariants (DESIGN 8.4) to one answered
// report:
//
//   - the hive accepts (200) a terminal report only for a lease it issued,
//     from the node it issued it to;
//   - once the hive issued a newer lease for the task, a report for an
//     older lease is accepted only as a replay of a report already accepted
//     before the newer lease was issued (idempotent replay);
//   - at most one lease of a task is ever accepted as succeeded.
func (p *faultProxy) checkReport(rec *reportRec) {
	for _, b := range p.checkReportLocked(rec) {
		id, msg, _ := strings.Cut(b, "|")
		p.c.viol.add(id, "%s", msg)
	}
}

func (p *faultProxy) checkReportLocked(rec *reportRec) (bad []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.reports = append(p.reports, rec)
	p.repByTask[rec.task] = append(p.repByTask[rec.task], rec)
	l := p.leases[rec.lease]
	if l != nil {
		rec.attempt = l.attempt
	}
	if rec.status != http.StatusOK {
		return nil
	}
	if l == nil {
		bad = append(bad, fmt.Sprintf("report-unknown-lease|hive accepted a %s report for task %s with lease %s that it never issued (src %s, node %s)",
			rec.state, rec.task, short(rec.lease), rec.src, rec.node))
	} else {
		if l.task != rec.task {
			bad = append(bad, fmt.Sprintf("report-wrong-task|hive accepted a report for task %s with the lease of task %s", rec.task, l.task))
		}
		if l.node != rec.node {
			bad = append(bad, fmt.Sprintf("report-wrong-node|hive accepted a %s report for task %s lease %s from %s; the lease was issued to %s",
				rec.state, rec.task, short(rec.lease), rec.node, l.node))
		}
		// The hive may accept a report for a lease only while the lease is
		// live, or replay that acceptance. So if a newer lease had been
		// issued before any copy of this report was even sent, the hive
		// accepted it for a dead lease.
		key := rec.task + "|" + rec.lease + "|" + string(rec.state)
		first := p.minSent[key]
		for _, nl := range p.byTask[rec.task] {
			if nl == l || !nl.issued.After(l.issued) || l.lostMaybe {
				continue
			}
			if nl.issued.Before(first) {
				bad = append(bad, fmt.Sprintf("stale-lease-accepted|hive accepted a %s report (src %s, kind %q) for task %s attempt %d lease %s from %s; every copy of it was sent after the hive issued attempt %d (lease %s) to %s (first copy %s later)",
					rec.state, rec.src, rec.kind, rec.task, l.attempt, short(rec.lease), rec.node,
					nl.attempt, short(nl.lease), nl.node, first.Sub(nl.issued).Round(time.Millisecond)))
			}
			break
		}
	}
	if l != nil && l.lost && rec.sent.After(l.issued) {
		bad = append(bad, fmt.Sprintf("lost-lease-accepted|after a crash, the hive accepted a %s report for task %s lease %s (attempt %d), which it issued after the state it came back from",
			rec.state, rec.task, short(rec.lease), l.attempt))
	}
	if rec.state == proto.TaskSucceeded {
		// Successes the hive still stands by (a crash may have undone some).
		ls := p.successLeasesLocked(rec.task, false)
		ls[rec.lease] = rec.attempt
		if len(ls) > 1 {
			var parts []string
			for lease, a := range ls {
				parts = append(parts, fmt.Sprintf("%s(attempt %d)", short(lease), a))
			}
			sort.Strings(parts)
			bad = append(bad, fmt.Sprintf("two-leases-succeeded|hive accepted succeeded reports for task %s from %d leases: %s",
				rec.task, len(ls), strings.Join(parts, ", ")))
		}
	}
	return bad
}

// successLeasesLocked returns the leases of task with an accepted succeeded
// report and their attempts, leaving out acceptances a crash certainly
// undid, and with maybe=false also those it perhaps undid.
func (p *faultProxy) successLeasesLocked(task string, maybe bool) map[string]int {
	out := map[string]int{}
	for _, r := range p.repByTask[task] {
		if r.state == proto.TaskSucceeded && r.status == http.StatusOK && !r.lost && (maybe || !r.maybeLost) {
			out[r.lease] = r.attempt
		}
	}
	return out
}

// forgetAfter records what a crashed hive lost: the leases it issued, and
// the reports it acted on, after the state it came back from was saved.
// Certainly lost is what reached it after the file was written; perhaps
// lost is what it answered within a second before (the snapshot is taken
// a little before the file is written). Lost leases are dead, and a later
// dispatch may reuse their attempt numbers.
func (p *faultProxy) forgetAfter(cr chaosCrash) {
	edge := cr.persisted.Add(-time.Second)
	p.mu.Lock()
	defer p.mu.Unlock()
	for task, ls := range p.byTask {
		kept := ls[:0:0]
		for _, l := range ls {
			if l.issued.After(edge) && l.sent.Before(cr.at) {
				l.lostMaybe = true
				l.lost = l.sent.After(cr.persisted)
				continue
			}
			kept = append(kept, l)
		}
		p.byTask[task] = kept
	}
	for _, r := range p.reports {
		if r.answered.After(edge) && r.sent.Before(cr.at) {
			r.maybeLost = true
			r.lost = r.lost || r.sent.After(cr.persisted)
		}
	}
}

// replay sends an old report again, straight to the hive, as a node whose
// earlier response was lost would (or a delayed duplicate on the network).
func (p *faultProxy) replay(rng *rand.Rand) {
	p.mu.Lock()
	if len(p.replayQ) == 0 {
		p.mu.Unlock()
		return
	}
	// Mostly reports whose task has been dispatched again since: the hive
	// must answer those 409, or 200 only as a replay of an acceptance.
	var stale []replayItem
	for _, it := range p.replayQ {
		var rep proto.TaskReport
		if json.Unmarshal(it.body, &rep) != nil {
			continue
		}
		if ls := p.byTask[it.task]; len(ls) > 0 && ls[len(ls)-1].lease != rep.Lease {
			stale = append(stale, it)
		}
	}
	it := p.replayQ[rng.Intn(len(p.replayQ))]
	if len(stale) > 0 && rng.Intn(10) < 7 {
		it = stale[rng.Intn(len(stale))]
	}
	node := p.tokens[it.token]
	p.mu.Unlock()
	h := http.Header{}
	h.Set("Authorization", "Bearer "+it.token)
	h.Set("Content-Type", "application/json")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	sent := time.Now()
	p.noteSent("report", it.task, it.body, sent)
	status, _, rbody, err := p.forward(ctx, http.MethodPost, "/api/v1/tasks/"+it.task+"/report", h, it.body)
	if err != nil {
		return
	}
	p.count(fmt.Sprintf("replay/%d", status))
	p.observe("report", node, it.task, it.token, it.body, status, rbody, sent, "replay")
}

func (p *faultProxy) statsSnapshot() map[string]int {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := map[string]int{}
	for k, v := range p.stats {
		out[k] = v
	}
	return out
}

// succeededLeases returns, per task, the leases the hive accepted a
// succeeded report for (and may still stand by after crashes), with their
// attempt numbers.
func (p *faultProxy) succeededLeases() map[string]map[string]int {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := map[string]map[string]int{}
	for _, r := range p.reports {
		if _, ok := out[r.task]; !ok && r.state == proto.TaskSucceeded {
			out[r.task] = p.successLeasesLocked(r.task, true)
		}
	}
	return out
}

// acceptedReports returns the first 200-answered terminal report of each
// (task, lease, state), oldest first, leaving out those a crash certainly
// undid, and with maybe=false also those it perhaps undid.
func (p *faultProxy) acceptedReports(maybe bool) []*reportRec {
	p.mu.Lock()
	defer p.mu.Unlock()
	seen := map[string]bool{}
	var out []*reportRec
	for _, r := range p.reports {
		key := r.task + "|" + r.lease + "|" + string(r.state)
		if r.status != http.StatusOK || seen[key] || r.lost || (!maybe && r.maybeLost) {
			continue
		}
		seen[key] = true
		out = append(out, r)
	}
	return out
}

// reportTrail describes every lease and report of a task (diagnostics).
func (p *faultProxy) reportTrail(task string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var parts []string
	for _, l := range p.leases {
		if l.task == task {
			parts = append(parts, fmt.Sprintf("lease %s attempt %d to %s sent %s issued %s lost=%v/%v",
				short(l.lease), l.attempt, l.node, l.sent.Format("15:04:05.000"), l.issued.Format("15:04:05.000"), l.lost, l.lostMaybe))
		}
	}
	for _, r := range p.reports {
		if r.task == task {
			parts = append(parts, fmt.Sprintf("report %s %s lease %s from %s (%s) sent %s answered %s -> %d lost=%v/%v",
				r.state, r.kind, short(r.lease), r.node, r.src, r.sent.Format("15:04:05.000"), r.answered.Format("15:04:05.000"), r.status, r.lost, r.maybeLost))
		}
	}
	return strings.Join(parts, "; ")
}

func (p *faultProxy) leaseFor(lease string) *leaseRec {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.leases[lease]
}

func short(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}
