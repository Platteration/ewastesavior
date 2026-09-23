package ctl

import (
	"fmt"
	"runtime"
	"strconv"
	"strings"
	"time"
)

func isWindows() bool { return runtime.GOOS == "windows" }

// fmtTime formats t in local time; the zero time is "".
func fmtTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Local().Format("2006-01-02 15:04:05")
}

// fmtAge formats how long ago t was, relative to now.
func fmtAge(t, now time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := now.Sub(t)
	if d < 0 {
		return "just now"
	}
	return fmtDuration(d) + " ago"
}

// fmtDuration formats d compactly: 850ms, 12s, 5m, 3h20m, 4d.
func fmtDuration(d time.Duration) string {
	switch {
	case d < time.Second:
		return d.Round(time.Millisecond).String()
	case d < time.Minute:
		return strconv.FormatFloat(d.Seconds(), 'f', 1, 64) + "s"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		h := int(d.Hours())
		m := int(d.Minutes()) % 60
		if m == 0 {
			return fmt.Sprintf("%dh", h)
		}
		return fmt.Sprintf("%dh%dm", h, m)
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

// fmtBytes formats a byte count with binary units.
func fmtBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit && exp < 5; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// fmtMB formats a size given in megabytes.
func fmtMB(mb int) string {
	if mb >= 10240 {
		return fmt.Sprintf("%.0f GB", float64(mb)/1024)
	}
	if mb >= 1024 {
		return fmt.Sprintf("%.1f GB", float64(mb)/1024)
	}
	return fmt.Sprintf("%d MB", mb)
}

// fmtCores formats a (possibly fractional) core count.
func fmtCores(c float64) string {
	s := strconv.FormatFloat(c, 'f', 2, 64)
	s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	if s == "" || s == "-" {
		return "0"
	}
	return s
}

// shortHash abbreviates a sha256 for tables.
func shortHash(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}
