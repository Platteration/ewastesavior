package storage

import (
	"bufio"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/platteration/ewastesavior/internal/auth"
)

// cpuHasSSE2 reports whether every "flags" line of a /proc/cpuinfo lists
// sse2 (and at least one flags line exists).
func cpuHasSSE2(r io.Reader) bool {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	seen := false
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), ":")
		if !ok || strings.TrimSpace(k) != "flags" {
			continue
		}
		seen = true
		has := false
		for _, f := range strings.Fields(v) {
			if f == "sse2" {
				has = true
				break
			}
		}
		if !has {
			return false
		}
	}
	return seen
}

// pickBinary keeps the 386 build that matches the CPU (DESIGN 3): with both
// <dir>/savior-sse2 and <dir>/savior-softfloat present, the SSE2 build when
// the CPU has SSE2, else the softfloat build, is renamed to <dir>/savior and
// the other is deleted. With neither present it does nothing.
func pickBinary(cpuinfo, dir string, stdout, stderr io.Writer) int {
	sse2 := filepath.Join(dir, "savior-sse2")
	soft := filepath.Join(dir, "savior-softfloat")
	target := filepath.Join(dir, "savior")
	haveSSE2, haveSoft := isFile(sse2), isFile(soft)
	if !haveSSE2 && !haveSoft {
		if !isFile(target) {
			fmt.Fprintf(stderr, "savior storage: pick-binary: no savior binary in %s\n", dir)
			return 1
		}
		return 0
	}
	cpuSSE2 := false
	if f, err := os.Open(cpuinfo); err == nil {
		cpuSSE2 = cpuHasSSE2(f)
		f.Close()
	} else {
		fmt.Fprintf(stderr, "savior storage: pick-binary: %v (assuming no SSE2)\n", err)
	}
	keep, drop, variant := soft, sse2, "softfloat"
	switch {
	case haveSSE2 && (cpuSSE2 || !haveSoft):
		keep, drop, variant = sse2, soft, "sse2"
		if !cpuSSE2 {
			fmt.Fprintf(stderr, "savior storage: pick-binary: warning: CPU lacks SSE2 but only the SSE2 build is present\n")
		}
	}
	if err := os.Rename(keep, target); err != nil {
		fmt.Fprintf(stderr, "savior storage: pick-binary: %v\n", err)
		return 1
	}
	if err := os.Remove(drop); err != nil && !errors.Is(err, os.ErrNotExist) {
		fmt.Fprintf(stderr, "savior storage: pick-binary: %v\n", err)
	}
	fmt.Fprintf(stdout, "%s build (cpu sse2: %v)\n", variant, cpuSSE2)
	return 0
}

func isFile(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular()
}

// certFingerprint returns "sha256:<hex>" of the first certificate in a PEM
// file (or of a DER file).
func certFingerprint(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	rest := b
	for {
		var blk *pem.Block
		blk, rest = pem.Decode(rest)
		if blk == nil {
			break
		}
		if blk.Type == "CERTIFICATE" {
			if _, err := x509.ParseCertificate(blk.Bytes); err != nil {
				return "", fmt.Errorf("%s: %w", path, err)
			}
			return auth.Fingerprint(blk.Bytes), nil
		}
	}
	if _, err := x509.ParseCertificate(b); err == nil {
		return auth.Fingerprint(b), nil
	}
	return "", fmt.Errorf("%s: no certificate found", path)
}
