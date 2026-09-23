package runner

import (
	"bufio"
	"bytes"
	"os"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// procStatus holds the fields of /proc/<pid>/status that uid sweeps use.
type procStatus struct {
	state byte   // first letter of State:, e.g. 'S', 'Z'
	uids  [3]int // real, effective, saved
}

// parseProcStatus parses the State: and Uid: lines of a status file.
func parseProcStatus(b []byte) (procStatus, bool) {
	var st procStatus
	haveUID := false
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		line := sc.Text()
		if v, ok := strings.CutPrefix(line, "State:"); ok {
			if v = strings.TrimSpace(v); v != "" {
				st.state = v[0]
			}
		} else if v, ok := strings.CutPrefix(line, "Uid:"); ok {
			f := strings.Fields(v)
			if len(f) < 3 {
				return st, false
			}
			for i := range st.uids {
				n, err := strconv.Atoi(f[i])
				if err != nil {
					return st, false
				}
				st.uids[i] = n
			}
			haveUID = true
		}
	}
	return st, haveUID
}

// liveInRange reports whether the process is alive (not a zombie or dead)
// and its real, effective or saved uid is in [lo, hi].
func (st procStatus) liveInRange(lo, hi int) bool {
	if st.state == 'Z' || st.state == 'X' || st.state == 'x' {
		return false
	}
	for _, u := range st.uids {
		if u >= lo && u <= hi {
			return true
		}
	}
	return false
}

// readProcStatus reads and parses /proc/<pid>/status.
func readProcStatus(pid int) (procStatus, bool) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/status")
	if err != nil {
		return procStatus{}, false
	}
	return parseProcStatus(b)
}

// uidProcs lists the live processes owned by a uid in [lo, hi].
func uidProcs(lo, hi int) []int {
	ents, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	var out []int
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 0 {
			continue
		}
		if st, ok := readProcStatus(pid); ok && st.liveInRange(lo, hi) {
			out = append(out, pid)
		}
	}
	return out
}

// killUIDProcs SIGKILLs every process owned by a uid in [lo, hi] (the task
// slot uids, which nothing else uses) until none is left or timeout
// passes. It returns how many processes it signalled and whether none is
// left. Each process is signalled through a pidfd after re-checking its
// owner, so a recycled pid is never hit.
func killUIDProcs(lo, hi int, timeout time.Duration) (killed int, ok bool) {
	deadline := time.Now().Add(timeout)
	seen := map[int]bool{}
	for {
		pids := uidProcs(lo, hi)
		if len(pids) == 0 {
			return len(seen), true
		}
		if time.Now().After(deadline) {
			return len(seen), false
		}
		for _, pid := range pids {
			if killUIDProc(pid, lo, hi) {
				seen[pid] = true
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// killUIDProc SIGKILLs pid if it is still owned by a uid in [lo, hi].
func killUIDProc(pid, lo, hi int) bool {
	fd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		if err != unix.ENOSYS {
			return false // gone already
		}
		// Kernels before 5.3: plain kill after the check.
		if st, ok := readProcStatus(pid); ok && st.liveInRange(lo, hi) {
			return unix.Kill(pid, unix.SIGKILL) == nil
		}
		return false
	}
	defer unix.Close(fd)
	if st, ok := readProcStatus(pid); !ok || !st.liveInRange(lo, hi) {
		return false
	}
	return unix.PidfdSendSignal(fd, unix.SIGKILL, nil, 0) == nil
}
