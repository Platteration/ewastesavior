// Package hive implements the SaviorOS coordinator: the node and admin
// HTTPS API, the scheduler, the state store, blobs, video walls, admin
// sessions and the embedded web dashboard. See docs/DESIGN.md sections 6-9.
//
// All mutable state lives behind one mutex (Server.mu). File I/O (blobs,
// state snapshots, log tails) happens outside it.
package hive

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/platteration/ewastesavior/internal/auth"
	"github.com/platteration/ewastesavior/internal/config"
	"github.com/platteration/ewastesavior/internal/proto"
	"github.com/platteration/ewastesavior/internal/version"
)

// Default timings (DESIGN 7.4).
const (
	DefaultHeartbeatInterval = 5 * time.Second
	DefaultOfflineAfter      = 20 * time.Second
	DefaultLostAfter         = 60 * time.Second
	DefaultMissingAfter      = 20 * time.Second
	DefaultRecoveryWindow    = 80 * time.Second
	DefaultReserveAfter      = 120 * time.Second
	DefaultNetbootListen     = ":7702"
	DefaultStatusFile        = "/run/savior/hive-status.json"
)

// APIVersionHeader may carry the client's API version on /register. A
// mismatch is answered with 426 (proto.RegisterRequest has no version field;
// a JSON "api_version" field in the request body is honored too).
const APIVersionHeader = "X-Savior-API-Version"

// ClientTimeHeader carries an admin client's clock (RFC 3339 or Unix
// seconds). See DESIGN 9 "Clock".
const ClientTimeHeader = "X-Savior-Client-Time"

// Limits and fixed timings from DESIGN 6-9.
const (
	maxJSONBody       = 1 << 20
	maxLogChunk       = 256 << 10
	logRingSize       = 1 << 20
	logTailSize       = 64 << 10
	maxHistory        = 10
	maxFinishedJobs   = 500
	maxTaskRecords    = 200000
	maxActiveJobs     = 10000
	leaseUploadCap    = 1 << 30
	maxClaimsPerNode  = 4
	maxClaimTasks     = 16
	maxClaimWait      = 30 * time.Second
	maxLogWait        = 25 * time.Second
	maxNodes          = 10000
	maxPendingNodes   = 256
	maxRunningListed  = 1024
	maxHWIDs          = 32
	maxSessions       = 1000
	maxPairCodes      = 32
	maxUsedNonces     = 100000
	vfatMaxBlob       = 4<<30 - 1
	renderCacheBytes  = 256 << 20
	jsonDeadline      = 30 * time.Second
	blobRateFloor     = 128 << 10 // bytes/s assumed when sizing blob deadlines
	quarantineErrors  = 3
	quarantineFast    = 5
	fastFailRunS      = 10.0
	identifyDefault   = 30
	identifyMax       = 600
	otherHiveInterval = 60 * time.Second
)

// Config configures a hive. Zero durations select the DESIGN 7.4 defaults.
type Config struct {
	Listen     string // HTTPS listen address (default ":7700")
	DataDir    string // state directory; "" or "auto" = DESIGN 13.4 resolution
	SwarmKey   string // "" = load or generate <data>/swarm_key
	AdminToken string // "" = load or generate <data>/admin_token
	JoinPolicy string // open (default) or approve

	Beacon        bool     // announce on UDP (DESIGN 7.3)
	BeaconPort    int      // default proto.DiscoveryPort
	BeaconTargets []string // override broadcast destinations ("ip:port")

	// NetbootDir, when set, is served read-only over plain HTTP on
	// NetbootListen (default :7702) for PXE payloads (DESIGN 13.1).
	NetbootDir    string
	NetbootListen string

	HeartbeatInterval time.Duration
	OfflineAfter      time.Duration
	LostAfter         time.Duration // offline duration after which a node's tasks are requeued
	MissingAfter      time.Duration
	RecoveryWindow    time.Duration
	ReserveAfter      time.Duration

	// Clock returns the hive's wall clock (display times, Hello.Time,
	// clock sync). Durations always use the monotonic clock. Nil = time.Now.
	Clock func() time.Time
	Log   *slog.Logger

	// Listener, when set, is used by Run instead of listening on Listen.
	Listener net.Listener
	// StatusFile receives the hive status panel (URLs, fingerprint, pairing
	// code) for the local status screen. "" = DefaultStatusFile when its
	// directory exists; "-" disables it.
	StatusFile string

	tune tuning
}

// tuning holds internal timings and hooks; tests in this package shrink them.
type tuning struct {
	loopInterval    time.Duration
	xferStall       time.Duration
	actionExpiry    time.Duration
	blobTouch       time.Duration
	uploadGC        time.Duration
	reserveMax      time.Duration
	nonceTTL        time.Duration
	sessionTTL      time.Duration
	pairTTL         time.Duration
	quarantineWin   time.Duration
	livenessPersist time.Duration
	minPersist      time.Duration
	blobGCEvery     time.Duration
	maxBlob         int64
	setClock        func(time.Time) error
	canSetClock     func() bool
	diskFree        func(path string) (free, total int64, ok bool)
	clockRaised     bool // Main raised the clock to the build time
}

// ConfigFromFile maps savior.conf keys (DESIGN 5.3) onto a hive Config.
func ConfigFromFile(c config.Config) Config {
	cfg := Config{
		Listen:     c.HiveListen,
		DataDir:    c.HiveData,
		SwarmKey:   c.SwarmKey,
		AdminToken: c.AdminToken,
		JoinPolicy: c.JoinPolicy,
		Beacon:     c.Beacon,
	}
	if c.Netboot {
		// S65netboot builds the TFTP tree (boot files + generated grub.cfg,
		// never the swarm key); the same tree is offered over HTTP.
		cfg.NetbootDir = "/run/savior/tftp"
	}
	return cfg
}

func (c *Config) setDefaults() {
	def := func(d *time.Duration, v time.Duration) {
		if *d <= 0 {
			*d = v
		}
	}
	if c.Listen == "" {
		c.Listen = fmt.Sprintf(":%d", proto.DefaultPort)
	}
	if c.JoinPolicy == "" {
		c.JoinPolicy = "open"
	}
	if c.BeaconPort == 0 {
		c.BeaconPort = proto.DiscoveryPort
	}
	if c.NetbootDir != "" && c.NetbootListen == "" {
		c.NetbootListen = DefaultNetbootListen
	}
	def(&c.HeartbeatInterval, DefaultHeartbeatInterval)
	def(&c.OfflineAfter, DefaultOfflineAfter)
	def(&c.LostAfter, DefaultLostAfter)
	def(&c.MissingAfter, DefaultMissingAfter)
	def(&c.RecoveryWindow, DefaultRecoveryWindow)
	def(&c.ReserveAfter, DefaultReserveAfter)
	if c.Log == nil {
		c.Log = slog.Default()
	}
	t := &c.tune
	def(&t.loopInterval, time.Second)
	def(&t.xferStall, 5*time.Minute)
	def(&t.actionExpiry, 5*time.Minute)
	def(&t.blobTouch, time.Hour)
	def(&t.uploadGC, time.Hour)
	def(&t.reserveMax, 30*time.Minute)
	def(&t.nonceTTL, 60*time.Second)
	def(&t.sessionTTL, 12*time.Hour)
	def(&t.pairTTL, 10*time.Minute)
	def(&t.quarantineWin, 10*time.Minute)
	def(&t.livenessPersist, 60*time.Second)
	def(&t.blobGCEvery, 5*time.Minute)
	if t.maxBlob <= 0 {
		t.maxBlob = proto.MaxBlobBytes
	}
	if t.setClock == nil {
		t.setClock = setSystemClock
	}
	if t.canSetClock == nil {
		t.canSetClock = canSetSystemClock
	}
	if t.diskFree == nil {
		t.diskFree = func(p string) (int64, int64, bool) {
			fi, err := statFS(p)
			if err != nil || fi.total <= 0 {
				return 0, 0, false
			}
			return fi.free, fi.total, true
		}
	}
}

// Server is a running hive.
type Server struct {
	cfg  Config
	log  *slog.Logger
	data dataDir

	cert           tls.Certificate
	fp             string
	hiveID         string
	swarmKey       string
	swarm          auth.Secret
	hint           string
	adminToken     string
	adminTokenHash string
	adminTokenFile string
	admin          auth.Secret
	nonces         *auth.NonceIssuer
	startedAt      time.Time // wall
	startMono      time.Time
	maxBlob        int64
	statusFile     string

	limits  *limiter
	blobs   *blobStore
	renders *renderCache
	io      *ioQueue
	handler http.Handler

	mu            sync.Mutex
	nodes         map[string]*node
	tokens        map[string]string // SHA-256(node token) -> node ID
	jobs          map[string]*job
	queue         []*job // unfinished jobs in queue order
	tasks         map[string]*task
	walls         map[string]*proto.WallSpec
	blobMeta      map[string]*blob
	nextSeq       uint64
	nextDoneSeq   uint64 // next jobRecord.DoneSeq
	taskRecords   int
	sessions      map[string]*session // SHA-256(session) -> session
	pairCodes     []*pairCode
	screenCode    *pairCode
	usedNonces    map[string]time.Time // nonce -> expiry (monotonic)
	wake          chan struct{}
	dirty         bool
	liveDirty     bool
	recovering    bool
	recoveryUntil time.Time
	reservation   *reservation
	headKey       string
	headSince     time.Time
	warnings      map[string]string
	storageLow    bool // blob storage below minFreeDisk: jobs with outputs wait
	timeSynced    bool
	timeSource    string
	completed     int64
	cpuTotal      float64
	otherHives    map[string]otherHive
	lastBlobGC    time.Time
	lastNTPCheck  time.Time
	lastStatus    string
	closing       bool // shutting down: nothing is dispatched any more

	persistMu    sync.Mutex
	lastWrite    time.Time // monotonic
	lastWriteDur time.Duration
	persistErr   error

	listenAddr   atomic.Value // net.Addr
	stop         context.CancelFunc
	bgDone       chan struct{}
	shutdown     chan struct{} // closed by beginShutdown; ends long-polls
	shutdownOnce sync.Once
	closeOnce    sync.Once
	closeErr     error
}

// New creates a hive: it resolves the data directory, loads the TLS
// identity, secrets and saved state, and starts the background loop.
func New(cfg Config) (*Server, error) {
	cfg.setDefaults()
	switch cfg.JoinPolicy {
	case "open", "approve":
	default:
		return nil, fmt.Errorf("join_policy must be open or approve, not %q", cfg.JoinPolicy)
	}
	if cfg.SwarmKey != "" && len(cfg.SwarmKey) < config.MinSwarmKeyLen {
		return nil, fmt.Errorf("swarm key too short (min %d characters)", config.MinSwarmKeyLen)
	}
	if cfg.AdminToken != "" && len(cfg.AdminToken) < config.MinAdminTokenLen {
		return nil, fmt.Errorf("admin token too short (min %d characters)", config.MinAdminTokenLen)
	}
	dd, err := resolveDataDir(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	s := &Server{
		cfg:         cfg,
		log:         cfg.Log,
		data:        dd,
		startMono:   time.Now(),
		nodes:       map[string]*node{},
		tokens:      map[string]string{},
		jobs:        map[string]*job{},
		tasks:       map[string]*task{},
		walls:       map[string]*proto.WallSpec{},
		blobMeta:    map[string]*blob{},
		nextSeq:     1,
		nextDoneSeq: 1,
		sessions:    map[string]*session{},
		usedNonces:  map[string]time.Time{},
		wake:        make(chan struct{}),
		warnings:    map[string]string{},
		otherHives:  map[string]otherHive{},
		limits:      newLimiter(),
		io:          newIOQueue(),
		nonces:      auth.NewNonceIssuer(),
		shutdown:    make(chan struct{}),
	}
	s.startedAt = s.now()
	if dd.warning != "" {
		s.warnings["data"] = dd.warning
		s.log.Warn(dd.warning)
	}
	s.maxBlob = cfg.tune.maxBlob
	if dd.vfat && s.maxBlob > vfatMaxBlob {
		s.maxBlob = vfatMaxBlob
	}
	for _, sub := range []string{"blobs", "cache", "logs"} {
		if err := os.MkdirAll(filepath.Join(dd.path, sub), 0o700); err != nil {
			return nil, fmt.Errorf("create hive data dir: %w", err)
		}
	}

	s.cert, s.fp, err = auth.LoadOrCreateCert(filepath.Join(dd.path, "tls"))
	if err != nil {
		return nil, fmt.Errorf("hive TLS identity: %w", err)
	}
	s.hiveID, _, err = loadOrCreateSecret(filepath.Join(dd.path, "hive_id"), func() string { return "h" + auth.NewID(8) }, 9)
	if err != nil {
		return nil, fmt.Errorf("hive id: %w", err)
	}
	s.swarmKey = cfg.SwarmKey
	if s.swarmKey == "" {
		var created bool
		s.swarmKey, created, err = loadOrCreateSecret(filepath.Join(dd.path, "swarm_key"), GenKey, config.MinSwarmKeyLen)
		if err != nil {
			return nil, fmt.Errorf("swarm key: %w", err)
		}
		msg := "no swarm_key configured; using the key stored in " + filepath.Join(dd.path, "swarm_key")
		if created {
			msg = "no swarm_key configured; generated one and stored it in " + filepath.Join(dd.path, "swarm_key")
		}
		s.warnings["swarm_key"] = msg + " (get a node savior.conf with: savior ctl node-config)"
		s.log.Warn(msg)
	} else if len(s.swarmKey) < 24 {
		s.warnings["swarm_key"] = "the swarm key is short; use at least 24 characters (savior ctl genkey)"
	}
	s.adminToken = cfg.AdminToken
	if s.adminToken == "" {
		s.adminTokenFile = filepath.Join(dd.path, "admin_token")
		s.adminToken, _, err = loadOrCreateSecret(s.adminTokenFile, auth.NewToken, config.MinAdminTokenLen)
		if err != nil {
			return nil, fmt.Errorf("admin token: %w", err)
		}
	}
	s.adminTokenHash = auth.HashToken(s.adminToken)
	s.swarm = auth.NewSwarmSecret(s.swarmKey)
	s.hint = s.swarm.SwarmHint()
	s.admin = auth.NewAdminSecret(s.adminToken)

	s.blobs, err = newBlobStore(filepath.Join(dd.path, "blobs"))
	if err != nil {
		return nil, err
	}
	s.renders, err = newRenderCache(filepath.Join(dd.path, "cache"), renderCacheBytes)
	if err != nil {
		return nil, err
	}
	if err := s.loadState(); err != nil {
		return nil, err
	}
	if err := s.reconcileBlobs(); err != nil {
		return nil, err
	}
	s.initClock()
	s.statusFile = cfg.StatusFile
	if s.statusFile == "" {
		if st, err := os.Stat(filepath.Dir(DefaultStatusFile)); err == nil && st.IsDir() {
			s.statusFile = DefaultStatusFile
		}
	} else if s.statusFile == "-" {
		s.statusFile = ""
	}
	s.handler = s.routes()

	// Start the recovery window (DESIGN 8.6) only now: stretching the keys
	// takes seconds on old CPUs and must not eat into it.
	s.mu.Lock()
	s.recoveryUntil = time.Now().Add(cfg.RecoveryWindow)
	s.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	s.stop = cancel
	s.bgDone = make(chan struct{})
	go s.io.run()
	go s.loop(ctx)
	return s, nil
}

// Handler returns the full HTTP handler (node API, admin API, dashboard).
func (s *Server) Handler() http.Handler { return s.handler }

// TLSConfig returns the server TLS configuration with the hive certificate.
func (s *Server) TLSConfig() *tls.Config { return auth.ServerTLSConfig(s.cert) }

// Fingerprint returns the hive certificate fingerprint ("sha256:<hex>").
func (s *Server) Fingerprint() string { return s.fp }

// AdminToken returns the admin token.
func (s *Server) AdminToken() string { return s.adminToken }

// AdminTokenFile returns where the generated admin token is stored, or ""
// when it came from the configuration.
func (s *Server) AdminTokenFile() string { return s.adminTokenFile }

// SwarmKey returns the swarm key.
func (s *Server) SwarmKey() string { return s.swarmKey }

// HiveID returns the random, persistent hive ID.
func (s *Server) HiveID() string { return s.hiveID }

// DataDir returns the resolved data directory.
func (s *Server) DataDir() string { return s.data.path }

// Addr returns the address Run listens on, or nil before it listens.
func (s *Server) Addr() net.Addr {
	a, _ := s.listenAddr.Load().(net.Addr)
	return a
}

// Close stops dispatching and background work, waits for pending file
// writes and saves the state. It is safe to call more than once.
func (s *Server) Close() error {
	s.closeOnce.Do(func() {
		s.beginShutdown()
		s.stop()
		<-s.bgDone
		s.io.close()
		s.mu.Lock()
		s.dirty = true
		s.mu.Unlock()
		s.closeErr = s.persist(true)
	})
	return s.closeErr
}

// beginShutdown stops dispatching and ends claim and log long-polls, so
// that an HTTP server shutdown doesn't wait for them and no task is
// assigned after the final save. It is safe to call more than once.
func (s *Server) beginShutdown() {
	s.shutdownOnce.Do(func() {
		s.mu.Lock()
		s.closing = true
		s.mu.Unlock()
		close(s.shutdown)
	})
}

// now returns the hive's wall clock.
func (s *Server) now() time.Time {
	if s.cfg.Clock != nil {
		return s.cfg.Clock()
	}
	return time.Now()
}

// notifyLocked wakes every long-polling claim.
func (s *Server) notifyLocked() {
	close(s.wake)
	s.wake = make(chan struct{})
}

const crockfordUpper = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// randomCode returns n Crockford base32 characters from crypto/rand.
func randomCode(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("hive: crypto/rand failed: " + err.Error())
	}
	for i := range b {
		b[i] = crockfordUpper[b[i]&31] // 256 is a multiple of 32: uniform
	}
	return string(b)
}

// GenKey returns a new swarm key: 32 lowercase Crockford base32 characters
// (160 bits) from crypto/rand.
func GenKey() string { return strings.ToLower(randomCode(32)) }

// loadOrCreateSecret reads a one-line secret file, or creates it (0600)
// with gen() when missing. created reports whether it was generated.
func loadOrCreateSecret(path string, gen func() string, minLen int) (val string, created bool, err error) {
	b, err := os.ReadFile(path)
	if err == nil {
		v := strings.TrimSpace(string(b))
		if len(v) < minLen || strings.ContainsAny(v, "\r\n\x00") {
			return "", false, fmt.Errorf("%s is invalid (want at least %d characters on one line)", path, minLen)
		}
		return v, false, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", false, err
	}
	v := gen()
	if err := writeFileAtomic(path, []byte(v+"\n"), 0o600); err != nil {
		return "", false, err
	}
	return v, true, nil
}

// writeFileAtomic writes data to path via a synced temp file and rename.
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	ok := false
	defer func() {
		if !ok {
			f.Close()
			os.Remove(tmp)
		}
	}()
	if err := f.Chmod(mode); err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	ok = true
	return nil
}

// versionString is the hive version for Hello/HiveInfo/beacons.
func versionString() string { return version.Version }
