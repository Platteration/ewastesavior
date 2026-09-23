package ctl

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/platteration/ewastesavior/internal/proto"
)

// Logout revokes the client's current session on the hive (without
// logging in first) and forgets it.
func (c *Client) Logout(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	session, _ := c.Session()
	if session == "" {
		return nil
	}
	resp, err := c.do(ctx, apiRequest{method: http.MethodPost, path: adminPath("logout"), noRelogin: true})
	c.dropSession(session)
	if err != nil {
		return err
	}
	drainClose(resp.Body)
	return nil
}

// RevokeAllSessions revokes every admin session, including this client's.
func (c *Client) RevokeAllSessions(ctx context.Context) error {
	err := c.callJSON(ctx, http.MethodPost, adminPath("sessions", "revoke"), nil, nil, nil)
	if err == nil {
		c.mu.Lock()
		c.session, c.expires = "", time.Time{}
		c.mu.Unlock()
	}
	return err
}

// Pair asks the hive for a single-use pairing code for the web dashboard.
func (c *Client) Pair(ctx context.Context) (proto.PairCode, error) {
	var pc proto.PairCode
	err := c.callJSON(ctx, http.MethodPost, adminPath("pair"), nil, nil, &pc)
	return pc, err
}

// Info returns the hive's description.
func (c *Client) Info(ctx context.Context) (proto.HiveInfo, error) {
	var hi proto.HiveInfo
	err := c.callJSON(ctx, http.MethodGet, adminPath("info"), nil, nil, &hi)
	return hi, err
}

// NodeConfig returns a savior.conf for nodes (swarm key, fingerprint pin
// and hive address). hiveAddr is the address nodes should use; "" lets the
// hive use the address this client connected to, and "auto" makes nodes
// find the hive on the LAN.
func (c *Client) NodeConfig(ctx context.Context, hiveAddr string) (string, error) {
	if err := c.ensureSession(ctx); err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	var q url.Values
	if hiveAddr != "" {
		q = url.Values{"hive": {hiveAddr}}
	}
	resp, err := c.do(ctx, apiRequest{method: http.MethodGet, path: adminPath("node-config"), query: q})
	if err != nil {
		return "", err
	}
	defer drainClose(resp.Body)
	b, err := io.ReadAll(&capReader{r: resp.Body, left: maxNodeConfig})
	if err != nil {
		return "", fmt.Errorf("node config: %w", err)
	}
	return string(b), nil
}

// Stats returns aggregate swarm numbers.
func (c *Client) Stats(ctx context.Context) (proto.SwarmStats, error) {
	var st proto.SwarmStats
	err := c.callJSON(ctx, http.MethodGet, "/api/v1/stats", nil, nil, &st)
	return st, err
}

// Nodes lists every node the hive knows.
func (c *Client) Nodes(ctx context.Context) ([]proto.NodeView, error) {
	var out []proto.NodeView
	err := c.callJSON(ctx, http.MethodGet, adminPath("nodes"), nil, nil, &out)
	return out, err
}

// Node returns one node by ID or name.
func (c *Client) Node(ctx context.Context, ref string) (proto.NodeView, error) {
	var nv proto.NodeView
	if err := checkRef("node", ref); err != nil {
		return nv, err
	}
	err := c.callJSON(ctx, http.MethodGet, adminPath("nodes", ref), nil, nil, &nv)
	return nv, err
}

// PatchNode changes a node's name, labels, drain, approval, quarantine,
// display or rotation.
func (c *Client) PatchNode(ctx context.Context, ref string, p proto.NodePatch) (proto.NodeView, error) {
	var nv proto.NodeView
	if err := checkRef("node", ref); err != nil {
		return nv, err
	}
	err := c.callJSON(ctx, http.MethodPatch, adminPath("nodes", ref), nil, p, &nv)
	return nv, err
}

// DeleteNode forgets an offline node.
func (c *Client) DeleteNode(ctx context.Context, ref string) error {
	if err := checkRef("node", ref); err != nil {
		return err
	}
	return c.callJSON(ctx, http.MethodDelete, adminPath("nodes", ref), nil, nil, nil)
}

// Action sends a one-shot action (identify, reboot, poweroff) to a node.
func (c *Client) Action(ctx context.Context, ref string, a proto.NodeAction) error {
	if err := checkRef("node", ref); err != nil {
		return err
	}
	return c.callJSON(ctx, http.MethodPost, adminPath("nodes", ref, "action"), nil, a, nil)
}

// Identify makes nodes show their identify screen (empty Nodes = all online).
func (c *Client) Identify(ctx context.Context, r proto.IdentifyRequest) error {
	for _, ref := range r.Nodes {
		if err := checkRef("node", ref); err != nil {
			return err
		}
	}
	return c.callJSON(ctx, http.MethodPost, adminPath("identify"), nil, r, nil)
}

// SubmitJob submits a job. The hive applies defaults and validates it.
func (c *Client) SubmitJob(ctx context.Context, spec proto.JobSpec) (proto.JobDetail, error) {
	var jd proto.JobDetail
	err := c.callJSON(ctx, http.MethodPost, adminPath("jobs"), nil, spec, &jd)
	return jd, err
}

// JobsQuery filters Jobs. Zero fields are not sent.
type JobsQuery struct {
	State  proto.JobState
	Limit  int
	Before uint64 // only jobs with a smaller Seq (paging)
}

// Jobs lists jobs, newest first.
func (c *Client) Jobs(ctx context.Context, q JobsQuery) ([]proto.JobView, error) {
	v := url.Values{}
	if q.State != "" {
		v.Set("state", string(q.State))
	}
	if q.Limit > 0 {
		v.Set("limit", strconv.Itoa(q.Limit))
	}
	if q.Before > 0 {
		v.Set("before", strconv.FormatUint(q.Before, 10))
	}
	var out []proto.JobView
	err := c.callJSON(ctx, http.MethodGet, adminPath("jobs"), v, nil, &out)
	return out, err
}

// Job returns a job with its normalized spec.
func (c *Client) Job(ctx context.Context, id string) (proto.JobDetail, error) {
	var jd proto.JobDetail
	if err := checkRef("job", id); err != nil {
		return jd, err
	}
	err := c.callJSON(ctx, http.MethodGet, adminPath("jobs", id), nil, nil, &jd)
	return jd, err
}

// TasksQuery filters Tasks. Zero fields are not sent.
type TasksQuery struct {
	State  proto.TaskState
	Offset int
	Limit  int // at most 1000
}

// Tasks returns one page of a job's tasks.
func (c *Client) Tasks(ctx context.Context, jobID string, q TasksQuery) (proto.TaskPage, error) {
	var tp proto.TaskPage
	if err := checkRef("job", jobID); err != nil {
		return tp, err
	}
	v := url.Values{}
	if q.State != "" {
		v.Set("state", string(q.State))
	}
	if q.Offset > 0 {
		v.Set("offset", strconv.Itoa(q.Offset))
	}
	if q.Limit > 0 {
		v.Set("limit", strconv.Itoa(q.Limit))
	}
	err := c.callJSON(ctx, http.MethodGet, adminPath("jobs", jobID, "tasks"), v, nil, &tp)
	return tp, err
}

// Task returns one task.
func (c *Client) Task(ctx context.Context, id string) (proto.TaskView, error) {
	var tv proto.TaskView
	if err := checkRef("task", id); err != nil {
		return tv, err
	}
	err := c.callJSON(ctx, http.MethodGet, adminPath("tasks", id), nil, nil, &tv)
	return tv, err
}

// Cancel cancels a job's unfinished tasks.
func (c *Client) Cancel(ctx context.Context, jobID string) (proto.JobView, error) {
	var jv proto.JobView
	if err := checkRef("job", jobID); err != nil {
		return jv, err
	}
	err := c.callJSON(ctx, http.MethodPost, adminPath("jobs", jobID, "cancel"), nil, nil, &jv)
	return jv, err
}

// DeleteJob removes a finished job and its task records.
func (c *Client) DeleteJob(ctx context.Context, jobID string) error {
	if err := checkRef("job", jobID); err != nil {
		return err
	}
	return c.callJSON(ctx, http.MethodDelete, adminPath("jobs", jobID), nil, nil, nil)
}

// Logs reads a task's combined output from offset. It returns the data and
// the offset to continue from. With wait > 0 the hive holds the request
// (up to 25 s) until new data arrives.
func (c *Client) Logs(ctx context.Context, taskID string, offset int64, wait time.Duration) ([]byte, int64, error) {
	if err := checkRef("task", taskID); err != nil {
		return nil, offset, err
	}
	if offset < 0 {
		offset = 0
	}
	waitS := int((wait + time.Second - 1) / time.Second)
	if waitS > 25 {
		waitS = 25
	}
	q := url.Values{"offset": {strconv.FormatInt(offset, 10)}}
	if waitS > 0 {
		q.Set("wait_s", strconv.Itoa(waitS))
	}
	if err := c.ensureSession(ctx); err != nil {
		return nil, offset, err
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout+time.Duration(waitS)*time.Second)
	defer cancel()
	resp, err := c.do(ctx, apiRequest{method: http.MethodGet, path: adminPath("tasks", taskID, "log"), query: q})
	if err != nil {
		return nil, offset, err
	}
	defer drainClose(resp.Body)
	data, err := io.ReadAll(&capReader{r: resp.Body, left: maxLogResponse})
	if err != nil {
		return nil, offset, fmt.Errorf("read log: %w", err)
	}
	next := offset + int64(len(data))
	if h := resp.Header.Get(LogOffsetHeader); h != "" {
		n, err := strconv.ParseInt(h, 10, 64)
		if err != nil || n < 0 {
			return nil, offset, fmt.Errorf("the hive sent an invalid %s header", LogOffsetHeader)
		}
		next = n
	}
	return data, next, nil
}

// Outputs lists a job's output files.
func (c *Client) Outputs(ctx context.Context, jobID string) ([]proto.OutputEntry, error) {
	var out []proto.OutputEntry
	if err := checkRef("job", jobID); err != nil {
		return nil, err
	}
	err := c.callJSON(ctx, http.MethodGet, adminPath("jobs", jobID, "outputs"), nil, nil, &out)
	return out, err
}

// OutputsZip streams a zip of all of a job's outputs (entries
// task-<index>/<name>) to w. The zip has no size limit (a job's outputs
// together can exceed the largest single blob); the transfer fails only
// when no data moves for the client timeout.
func (c *Client) OutputsZip(ctx context.Context, jobID string, w io.Writer) (int64, error) {
	if err := checkRef("job", jobID); err != nil {
		return 0, err
	}
	return c.download(ctx, adminPath("jobs", jobID, "outputs.zip"), w, noLimit, "")
}

// TaskOutput streams one output file of a task to w.
func (c *Client) TaskOutput(ctx context.Context, taskID, name string, w io.Writer) (int64, error) {
	if err := checkRef("task", taskID); err != nil {
		return 0, err
	}
	if !proto.ValidRelPath(name) {
		return 0, fmt.Errorf("invalid output name %q", truncate(sanitizeCell(name), 80))
	}
	p := adminPath("tasks", taskID, "outputs")
	for _, seg := range strings.Split(name, "/") {
		p += "/" + url.PathEscape(seg)
	}
	return c.download(ctx, p, w, proto.MaxBlobBytes, "")
}

// Walls lists the video walls.
func (c *Client) Walls(ctx context.Context) ([]proto.WallSpec, error) {
	var out []proto.WallSpec
	err := c.callJSON(ctx, http.MethodGet, adminPath("walls"), nil, nil, &out)
	return out, err
}

// Wall returns one wall.
func (c *Client) Wall(ctx context.Context, id string) (proto.WallSpec, error) {
	var w proto.WallSpec
	if err := checkRef("wall", id); err != nil {
		return w, err
	}
	err := c.callJSON(ctx, http.MethodGet, adminPath("walls", id), nil, nil, &w)
	return w, err
}

// CreateWall creates a wall; the hive assigns its ID and resolves node
// names to IDs.
func (c *Client) CreateWall(ctx context.Context, w proto.WallSpec) (proto.WallSpec, error) {
	var out proto.WallSpec
	err := c.callJSON(ctx, http.MethodPost, adminPath("walls"), nil, w, &out)
	return out, err
}

// UpdateWall replaces a wall's definition.
func (c *Client) UpdateWall(ctx context.Context, id string, w proto.WallSpec) (proto.WallSpec, error) {
	var out proto.WallSpec
	if err := checkRef("wall", id); err != nil {
		return out, err
	}
	err := c.callJSON(ctx, http.MethodPut, adminPath("walls", id), nil, w, &out)
	return out, err
}

// DeleteWall removes a wall; its nodes go back to the status screen.
func (c *Client) DeleteWall(ctx context.Context, id string) error {
	if err := checkRef("wall", id); err != nil {
		return err
	}
	return c.callJSON(ctx, http.MethodDelete, adminPath("walls", id), nil, nil, nil)
}

// Blobs lists stored blobs.
func (c *Client) Blobs(ctx context.Context) ([]proto.BlobInfo, error) {
	var out []proto.BlobInfo
	err := c.callJSON(ctx, http.MethodGet, adminPath("blobs"), nil, nil, &out)
	return out, err
}

// DeleteBlob deletes an unreferenced blob.
func (c *Client) DeleteBlob(ctx context.Context, sha string) error {
	if !proto.ValidSHA256(sha) {
		return fmt.Errorf("invalid blob hash %q", truncate(sanitizeCell(sha), 80))
	}
	return c.callJSON(ctx, http.MethodDelete, adminPath("blobs", sha), nil, nil, nil)
}

// GCResult is the answer of a blob garbage collection.
type GCResult struct {
	Deleted    int   `json:"deleted"`
	FreedBytes int64 `json:"freed_bytes"`
}

// GC deletes unreferenced blobs that were not touched recently.
func (c *Client) GC(ctx context.Context) (GCResult, error) {
	var r GCResult
	err := c.callJSON(ctx, http.MethodPost, adminPath("blobs", "gc"), nil, nil, &r)
	return r, err
}

// WaitJob polls a job until it is terminal. every is the longest pause
// between polls (default 2 s); progress, if not nil, sees every poll.
// Transient network errors are retried for up to two minutes.
func (c *Client) WaitJob(ctx context.Context, id string, every time.Duration, progress func(proto.JobDetail)) (proto.JobDetail, error) {
	if every <= 0 {
		every = 2 * time.Second
	}
	pause := every / 4
	if pause <= 0 {
		pause = every
	}
	var failingSince time.Time
	for {
		jd, err := c.Job(ctx, id)
		switch {
		case err == nil:
			failingSince = time.Time{}
			if progress != nil {
				progress(jd)
			}
			if jd.State.Terminal() {
				return jd, nil
			}
		case transient(err) && ctx.Err() == nil:
			c.log.Debug("polling the job failed; retrying", "job", id, "err", err)
			if failingSince.IsZero() {
				failingSince = time.Now()
			} else if time.Since(failingSince) > 2*time.Minute {
				return jd, err
			}
		default:
			return jd, err
		}
		t := time.NewTimer(pause)
		select {
		case <-ctx.Done():
			t.Stop()
			return jd, ctx.Err()
		case <-t.C:
		}
		if pause *= 2; pause > every {
			pause = every
		}
	}
}

// transient reports whether err may go away by itself (network trouble,
// hive restarting, rate limiting).
func transient(err error) bool {
	var fe *FingerprintError
	if errors.As(err, &fe) || errors.Is(err, ErrSessionExpired) || errors.Is(err, ErrNotLoggedIn) ||
		errors.Is(err, ErrHiveProof) || errors.Is(err, ErrNoFingerprint) {
		return false
	}
	var ae *APIError
	if errors.As(err, &ae) {
		return ae.Status == http.StatusTooManyRequests || ae.Status >= 500
	}
	return true
}
