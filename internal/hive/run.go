package hive

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/platteration/ewastesavior/internal/discovery"
	"github.com/platteration/ewastesavior/internal/proto"
)

// newHTTPServer applies DESIGN 6.3 "Server" settings.
func newHTTPServer(h http.Handler, log *slog.Logger) *http.Server {
	return &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    16 << 10,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelDebug),
	}
}

// Run serves HTTPS on Config.Listen (plus the netboot file server), runs
// the beacon and waits for ctx. On return the state has been saved.
func (s *Server) Run(ctx context.Context) error {
	ln := s.cfg.Listener
	if ln == nil {
		var err error
		ln, err = net.Listen("tcp", s.cfg.Listen)
		if err != nil {
			s.Close()
			return fmt.Errorf("hive listen %s: %w", s.cfg.Listen, err)
		}
	}
	s.listenAddr.Store(ln.Addr())
	srv := newHTTPServer(s.handler, s.log)
	srv.TLSConfig = s.TLSConfig()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	errc := make(chan error, 2)

	wg.Add(1)
	go func() {
		defer wg.Done()
		// ServeTLS clones the config and enables HTTP/2 with the certificate
		// already in it (no files).
		if err := srv.ServeTLS(ln, "", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- fmt.Errorf("hive HTTPS server: %w", err)
		}
	}()
	s.log.Info("hive listening", "addr", ln.Addr().String(), "fingerprint", s.fp, "hive_id", s.hiveID)

	var nb *http.Server
	if s.cfg.NetbootDir != "" {
		nln, err := net.Listen("tcp", s.cfg.NetbootListen)
		if err != nil {
			s.log.Warn("netboot HTTP server disabled", "listen", s.cfg.NetbootListen, "err", err)
		} else {
			nb = newHTTPServer(netbootHandler(s.cfg.NetbootDir), s.log)
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := nb.Serve(nln); err != nil && !errors.Is(err, http.ErrServerClosed) {
					s.log.Warn("netboot HTTP server stopped", "err", err)
				}
			}()
			s.log.Info("serving netboot files", "dir", s.cfg.NetbootDir, "listen", nln.Addr().String())
		}
	}

	if s.cfg.Beacon {
		b := proto.Beacon{HiveID: s.hiveID, Port: s.listenPort(), Fingerprint: s.fp, SwarmHint: s.hint, Version: versionString()}
		wg.Add(2)
		go func() {
			defer wg.Done()
			err := discovery.Announce(ctx, b, discovery.AnnounceOptions{Port: s.cfg.BeaconPort, Targets: s.cfg.BeaconTargets, Log: s.log})
			if err != nil && ctx.Err() == nil {
				s.log.Warn("beacon stopped", "err", err)
			}
		}()
		go func() {
			defer wg.Done()
			s.watchOtherHives(ctx)
		}()
	}

	var runErr error
	select {
	case <-ctx.Done():
	case runErr = <-errc:
	}
	cancel()
	// Stop dispatching and end the claim and log long-polls first: they
	// would hold the server's shutdown for its whole timeout and could
	// still assign tasks after the final save.
	s.beginShutdown()
	sctx, scancel := context.WithTimeout(context.Background(), 5*time.Second)
	_ = srv.Shutdown(sctx)
	if nb != nil {
		_ = nb.Shutdown(sctx)
	}
	scancel()
	wg.Wait()
	if err := s.Close(); err != nil && runErr == nil {
		runErr = fmt.Errorf("save hive state: %w", err)
	}
	return runErr
}

// watchOtherHives probes for beacons from other hives that use our swarm
// key (DESIGN 7.3) and reports them in HiveInfo.Warnings. It probes from an
// ephemeral port so it never competes with the beacon's probe listener.
func (s *Server) watchOtherHives(ctx context.Context) {
	for {
		s.scanOtherHives(ctx, nil)
		select {
		case <-ctx.Done():
			return
		case <-time.After(otherHiveInterval):
		}
	}
}

// scanOtherHives runs one probe round; targets nil = broadcast.
func (s *Server) scanOtherHives(ctx context.Context, targets []string) {
	cands, err := discovery.Collect(ctx, 2*time.Second, discovery.DiscoverOptions{
		Port: s.cfg.BeaconPort, ListenAddr: ":0", ProbeInterval: time.Second, Targets: targets,
	})
	if err != nil {
		s.log.Debug("probe for other hives failed", "err", err)
		return
	}
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range cands {
		if c.Beacon.SwarmHint != s.hint || c.Beacon.HiveID == s.hiveID {
			continue
		}
		if _, known := s.otherHives[c.URL]; !known {
			s.log.Warn("another hive with this swarm key is on the network", "url", c.URL, "hive_id", c.Beacon.HiveID)
		}
		s.otherHives[c.URL] = otherHive{url: c.URL, seen: now}
	}
	for u, h := range s.otherHives {
		if now.Sub(h.seen) > 10*time.Minute {
			delete(s.otherHives, u)
		}
	}
}

// netbootHandler serves PXE payloads read-only over plain HTTP (DESIGN
// 13.1). There are no directory listings, no dotfiles, and names that
// could hold secrets are refused: netboot never serves the swarm key.
func netbootHandler(dir string) http.Handler {
	fs := http.FileServer(netbootFS{http.Dir(dir)})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		setDeadlines(w, 30*time.Minute)
		fs.ServeHTTP(w, r)
	})
}

type netbootFS struct{ fs http.FileSystem }

var netbootBlocked = []string{"savior.conf", "baked.conf", "swarm_key", "admin_token", "state.json", "key.pem"}

// Open implements http.FileSystem, refusing hidden and secret-looking
// names and directories.
func (n netbootFS) Open(name string) (http.File, error) {
	clean := path.Clean("/" + name)
	for _, seg := range strings.Split(clean, "/") {
		if strings.HasPrefix(seg, ".") {
			return nil, os.ErrNotExist
		}
		for _, b := range netbootBlocked {
			if strings.EqualFold(seg, b) {
				return nil, os.ErrNotExist
			}
		}
	}
	f, err := n.fs.Open(clean)
	if err != nil {
		return nil, err
	}
	if st, err := f.Stat(); err != nil || st.IsDir() {
		f.Close()
		return nil, os.ErrNotExist
	}
	return f, nil
}
