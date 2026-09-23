package ctl

import (
	"context"
	"errors"
	"io"
	"strings"
	"unicode/utf8"
)

// dangerousRune reports whether r must not reach a terminal: C0 controls
// (the caller decides about \n and \t), DEL, C1 controls (0x9b is a CSI on
// some terminals) and bidirectional overrides that can disguise text.
func dangerousRune(r rune) bool {
	switch {
	case r < 0x20, r >= 0x7f && r < 0xa0:
		return true
	case r == 0x061c, r == 0x200e, r == 0x200f, r >= 0x202a && r <= 0x202e, r >= 0x2066 && r <= 0x2069:
		return true
	}
	return false
}

// sanitize strips terminal control characters from s, keeping \n and \t.
// Invalid UTF-8 becomes U+FFFD.
func sanitize(s string) string {
	clean := true
	for _, r := range s {
		if r == utf8.RuneError || (dangerousRune(r) && r != '\n' && r != '\t') {
			clean = false
			break
		}
	}
	if clean {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range strings.ToValidUTF8(s, "\ufffd") {
		if dangerousRune(r) && r != '\n' && r != '\t' {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// sanitizeCell is sanitize for single-line output such as table cells:
// newlines and tabs become spaces.
func sanitizeCell(s string) string {
	s = sanitize(s)
	if strings.ContainsAny(s, "\n\t") {
		s = strings.NewReplacer("\n", " ", "\t", " ").Replace(s)
	}
	return s
}

// truncate shortens s to at most n runes, marking the cut with "...".
func truncate(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	if n <= 3 {
		return string([]rune(s)[:n])
	}
	return string([]rune(s)[:n-3]) + "..."
}

// sanitizeWriter sanitizes a byte stream (task logs) for a terminal. A
// UTF-8 sequence split across writes is held back until it is complete.
type sanitizeWriter struct {
	w       io.Writer
	pending []byte
}

func (s *sanitizeWriter) Write(p []byte) (int, error) {
	buf := make([]byte, 0, len(s.pending)+len(p))
	buf = append(append(buf, s.pending...), p...)
	cut := len(buf)
	for i := 1; i <= utf8.UTFMax-1 && i <= len(buf); i++ {
		if utf8.RuneStart(buf[len(buf)-i]) {
			if !utf8.FullRune(buf[len(buf)-i:]) {
				cut = len(buf) - i
			}
			break
		}
	}
	s.pending = append(s.pending[:0], buf[cut:]...)
	if _, err := io.WriteString(s.w, sanitize(string(buf[:cut]))); err != nil {
		return 0, err
	}
	return len(p), nil
}

// Flush writes out a trailing incomplete sequence.
func (s *sanitizeWriter) Flush() error {
	if len(s.pending) == 0 {
		return nil
	}
	_, err := io.WriteString(s.w, sanitize(string(s.pending)))
	s.pending = s.pending[:0]
	return err
}

const maxSecretLine = 4096

// readLine reads one line byte by byte (so nothing after it is consumed)
// and strips the line ending.
func readLine(r io.Reader) (string, error) {
	var b []byte
	var one [1]byte
	for {
		n, err := r.Read(one[:])
		if n == 1 {
			if one[0] == '\n' {
				break
			}
			if len(b) >= maxSecretLine {
				return "", errors.New("input line too long")
			}
			b = append(b, one[0])
			continue
		}
		if err == io.EOF {
			if len(b) == 0 {
				return "", io.ErrUnexpectedEOF
			}
			break
		}
		if err != nil {
			return "", err
		}
	}
	return strings.TrimRight(string(b), "\r"), nil
}

// readLineCtx is readLine that gives up when ctx ends, calling onCancel
// first (to restore the terminal). The blocked read is abandoned.
func readLineCtx(ctx context.Context, r io.Reader, onCancel func()) (string, error) {
	type result struct {
		s   string
		err error
	}
	ch := make(chan result, 1)
	go func() {
		s, err := readLine(r)
		ch <- result{s, err}
	}()
	select {
	case res := <-ch:
		return res.s, res.err
	case <-ctx.Done():
		onCancel()
		return "", ctx.Err()
	}
}
