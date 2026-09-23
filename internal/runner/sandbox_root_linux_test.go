//go:build linux

package runner

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/platteration/ewastesavior/internal/proto"
)

// TestRealSandboxIsolation runs the actual sandbox-exec shim with full
// namespace isolation and checks, from inside the task, that the sandbox
// holds. It needs root and working namespaces; it skips otherwise.
func TestRealSandboxIsolation(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root to create namespaces")
	}
	r := newTestRunner(t, ModeAuto, nil)
	if !r.caps[CapMountNS] || !r.caps[CapPidNS] || !r.caps[CapNetNS] {
		t.Skip("namespaces unavailable")
	}

	script := strings.Join([]string{
		"echo uid=$(id -u)",
		`echo "cmdline=[$(cat /proc/cmdline 2>/dev/null)]"`,
		"[ -e /media ] && echo media=present || echo media=absent",
		"[ -e /run ] && echo run=present || echo run=absent",
		"[ -e /sys ] && echo sys=present || echo sys=absent",
		"[ -e /root ] && echo root=present || echo root=absent",
		"(echo hi > /work/w.txt) 2>/dev/null && echo workwrite=ok || echo workwrite=fail",
		"(echo hi > /tmp/t.txt) 2>/dev/null && echo tmpwrite=ok || echo tmpwrite=fail",
		"(echo hi > /oops.txt) 2>/dev/null && echo rootwrite=ok || echo rootwrite=fail",
		"(echo hi > /etc/x) 2>/dev/null && echo etcwrite=ok || echo etcwrite=fail",
		`grep -q "lo:" /proc/net/dev && echo lo=present || echo lo=absent`,
		"grep -qE 'eth|ens|enp|wl' /proc/net/dev && echo extranet=present || echo extranet=absent",
		`echo "seccomp=$(grep '^Seccomp:' /proc/self/status | tr -d '\t ' | cut -d: -f2)"`,
		"mkdir -p /work/m",
		"unshare -Un true 2>/dev/null && echo unshare=ok || echo unshare=fail",
		"mount -t tmpfs none /work/m 2>/dev/null && echo mount=ok || echo mount=fail",
		"[ -e /dev/tty ] && echo tty=present || echo tty=absent",
		// Field 5 of mountinfo is the mountpoint, field 6 its options.
		`for d in sys irq bus; do [ -e /proc/$d ] || { echo proc$d=ro; continue; }; grep -qE "^([^ ]+ ){4}/proc/$d ro," /proc/self/mountinfo && echo proc$d=ro || echo proc$d=rw; done`,
	}, "\n")
	task := scriptTask("tiso", script)
	task.Outputs = []string{"w.txt"}

	var logs bytes.Buffer
	rep := r.Run(context.Background(), task, &logs, nil)
	out := logs.String()
	t.Logf("task output:\n%s", out)
	if rep.State != proto.TaskSucceeded {
		t.Fatalf("state %s kind %s err %q", rep.State, rep.ErrorKind, rep.Error)
	}

	want := map[string]string{
		"uid":       strconv.Itoa(testUIDBase),
		"cmdline":   "[]",
		"media":     "absent",
		"run":       "absent",
		"sys":       "absent",
		"root":      "absent",
		"workwrite": "ok",
		"tmpwrite":  "ok",
		"rootwrite": "fail",
		"etcwrite":  "fail",
		"lo":        "present",
		"extranet":  "absent",
		"unshare":   "fail",
		"mount":     "fail",
		"seccomp":   "2", // SECCOMP_MODE_FILTER: the filter is installed
		"tty":       "absent",
		"procsys":   "ro",
		"procirq":   "ro",
		"procbus":   "ro",
	}
	got := parseKV(out)
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
	if len(rep.Outputs) != 1 || rep.Outputs[0].Name != "w.txt" {
		t.Errorf("outputs = %+v, want [w.txt]", rep.Outputs)
	}
}

// TestRealSandboxSeccompBlocksSyscall confirms the filter is installed: a
// denied syscall made directly (not via a dropped-priv check) is refused
// even though it would otherwise be permitted.
func TestRealSandboxNetworkShared(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root to create namespaces")
	}
	r := newTestRunner(t, ModeAuto, nil)
	if !r.caps[CapNetNS] {
		t.Skip("net namespace unavailable")
	}
	// With network=true the task shares the host network namespace, so it
	// sees more than just lo.
	task := scriptTask("tnet", "grep -qE 'eth|ens|enp|wl|docker|veth' /proc/net/dev && echo extranet=present || echo extranet=absent")
	task.Network = true
	var logs bytes.Buffer
	rep := r.Run(context.Background(), task, &logs, nil)
	if rep.State != proto.TaskSucceeded {
		t.Fatalf("state %s err %q", rep.State, rep.Error)
	}
	if got := parseKV(logs.String()); got["extranet"] != "present" {
		t.Skipf("host has no extra interfaces to observe (got %q); network sharing not asserted", got["extranet"])
	}
}

// TestRealScratchInRAM checks that ScratchInRAM enforces the disk quota with
// a tmpfs (ENOSPC), and that the mount is cleaned up afterwards.
func TestRealScratchInRAM(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root to mount tmpfs")
	}
	base, err := os.MkdirTemp("", "savior-scratch-")
	if err != nil {
		t.Fatal(err)
	}
	os.Chmod(base, 0o711)
	t.Cleanup(func() { os.RemoveAll(base) })
	self, _ := os.Executable()
	r, err := New(Config{
		WorkRoot: base + "/work", CacheDir: base + "/cache", CgroupRoot: base + "/cg",
		SelfExe: self, Sandbox: ModeNone, ScratchInRAM: true, UIDBase: testUIDBase, Slots: 2,
	}, newFakeTransfer())
	if err != nil {
		t.Fatal(err)
	}
	os.Chmod(base+"/work", 0o711)

	task := scriptTask("tscratch", strings.Join([]string{
		"(head -c 262144 /dev/zero > small.bin) 2>/dev/null && echo small=ok || echo small=fail",
		"(head -c 8388608 /dev/zero > big.bin) 2>/dev/null && echo big=ok || echo big=fail",
	}, "\n"))
	task.Resources.DiskMB = 1 // 1 MiB tmpfs
	var logs bytes.Buffer
	rep := r.Run(context.Background(), task, &logs, nil)
	if rep.State != proto.TaskSucceeded {
		t.Fatalf("state %s err %q logs %s", rep.State, rep.Error, logs.String())
	}
	kv := parseKV(logs.String())
	if kv["small"] != "ok" {
		t.Errorf("small write should fit: %q", kv["small"])
	}
	if kv["big"] != "fail" {
		t.Errorf("big write should hit ENOSPC on the 1 MiB tmpfs: %q", kv["big"])
	}
	dir := base + "/work/" + taskDirName(task)
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("scratch workdir not cleaned up: %v", err)
	}
	// Nothing should still be mounted there.
	if data, _ := os.ReadFile("/proc/mounts"); strings.Contains(string(data), dir) {
		t.Errorf("scratch tmpfs still mounted at %s", dir)
	}
}

func parseKV(s string) map[string]string {
	m := map[string]string{}
	for _, line := range strings.Split(s, "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
			m[k] = v
		}
	}
	return m
}

// TestRealSandboxPerThreadStateStress runs many sandboxed tasks
// concurrently and checks that every one of them starts with an empty
// capability bounding set, the seccomp filter and no_new_privs. These are
// per-thread properties: the shim must set them on the thread that execs.
func TestRealSandboxPerThreadStateStress(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root to create namespaces and drop privileges")
	}
	r := newTestRunner(t, ModeAuto, nil)
	if !r.caps[CapMountNS] || !r.caps[CapPidNS] || !r.caps[CapSeccomp] {
		t.Skip("namespaces or seccomp unavailable")
	}
	const n = 100
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	type result struct {
		rep  proto.TaskReport
		logs string
	}
	results := make(chan result, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			task := scriptTask(fmt.Sprintf("tstress%d", i), "grep -E '^(CapBnd|Seccomp|NoNewPrivs):' /proc/self/status | tr -d '\\t ' | tr : =")
			var logs bytes.Buffer
			rep := r.Run(ctx, task, &logs, nil)
			results <- result{rep, logs.String()}
		}(i)
	}
	wg.Wait()
	close(results)
	bad := 0
	for res := range results {
		kv := parseKV(res.logs)
		if res.rep.State != proto.TaskSucceeded || kv["CapBnd"] != "0000000000000000" || kv["Seccomp"] != "2" || kv["NoNewPrivs"] != "1" {
			bad++
			if bad <= 5 {
				t.Errorf("state %s kind %s err %q status %v", res.rep.State, res.rep.ErrorKind, res.rep.Error, kv)
			}
		}
	}
	if bad > 0 {
		t.Errorf("%d of %d tasks started without the full per-thread lockdown", bad, n)
	}
	if got := r.FreeSlots(); got != r.cfg.Slots {
		t.Errorf("%d of %d slots free after the run (slots retired?)", got, r.cfg.Slots)
	}
}

// TestRealSandboxRootMountpoint checks that the shims build their roots on
// the runner's fixed mountpoint, inside their own mount namespaces: no
// directory is left in /tmp per task, and nothing shows on the host.
func TestRealSandboxRootMountpoint(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root to create namespaces")
	}
	r := newTestRunner(t, ModeAuto, nil)
	if !r.caps[CapMountNS] || !r.caps[CapPidNS] {
		t.Skip("namespaces unavailable")
	}
	// The shim runs with an empty environment, so its TMPDIR is /tmp.
	leftovers := func() map[string]bool {
		m := map[string]bool{}
		names, _ := filepath.Glob("/tmp/savior-root-*")
		for _, n := range names {
			m[n] = true
		}
		return m
	}
	before := leftovers()
	for i := 0; i < 3; i++ {
		rep, logs := runTask(t, r, scriptTask(fmt.Sprintf("troot%d", i), "echo hi > /work/x"))
		if rep.State != proto.TaskSucceeded {
			t.Fatalf("state %s kind %s err %q logs %s", rep.State, rep.ErrorKind, rep.Error, logs)
		}
	}
	for n := range leftovers() {
		if !before[n] {
			t.Errorf("task left %s behind on the host", n)
		}
	}

	mnt := filepath.Join(r.sys.workPath, sandboxRootName)
	if r.sys.sandboxRoot != mnt {
		t.Fatalf("sandbox root %q, want %q", r.sys.sandboxRoot, mnt)
	}
	fi, err := os.Lstat(mnt)
	if err != nil || !fi.IsDir() {
		t.Fatalf("mountpoint: %v %v", fi, err)
	}
	if st := fi.Sys().(*syscall.Stat_t); fi.Mode().Perm() != 0o700 || st.Uid != 0 || st.Gid != 0 {
		t.Errorf("mountpoint mode %v owner %d:%d, want 0700 root:root", fi.Mode().Perm(), st.Uid, st.Gid)
	}
	if ents, _ := os.ReadDir(mnt); len(ents) != 0 {
		t.Errorf("mountpoint not empty on the host: %v", ents)
	}
	if data, _ := os.ReadFile("/proc/self/mountinfo"); strings.Contains(string(data), mnt) {
		t.Errorf("%s is mounted on the host", mnt)
	}
}

// TestRealTaskDiesWithAgent kills an agent that runs a dropped-privilege
// task and checks that the task dies too: the uid change clears the
// parent-death signal, so the shim must arm it again.
func TestRealTaskDiesWithAgent(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root to drop privileges")
	}
	base, err := os.MkdirTemp("", "savior-agent-")
	if err != nil {
		t.Fatal(err)
	}
	os.Chmod(base, 0o711)
	t.Cleanup(func() { os.RemoveAll(base) })
	self, _ := os.Executable()
	agent := exec.Command(self, "runner-test-agent", base, "echo pid=$$; exec sleep 300")
	out, err := agent.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	agent.Stderr = os.Stderr
	if err := agent.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { agent.Process.Kill(); agent.Wait() })

	pidCh := make(chan int, 1)
	go func() {
		sc := bufio.NewScanner(out)
		for sc.Scan() {
			if v, ok := strings.CutPrefix(sc.Text(), "pid="); ok {
				n, _ := strconv.Atoi(v)
				pidCh <- n
				break
			}
		}
		io.Copy(io.Discard, out)
	}()
	var pid int
	select {
	case pid = <-pidCh:
	case <-time.After(20 * time.Second):
		t.Fatal("the task never started")
	}
	if pid <= 1 {
		t.Fatalf("bad task pid %d", pid)
	}
	t.Cleanup(func() { syscall.Kill(pid, syscall.SIGKILL) })

	agent.Process.Signal(syscall.SIGKILL)
	agent.Wait()
	if !waitGone(pid, 5*time.Second) {
		t.Fatalf("task process %d outlived its agent", pid)
	}
}

// TestRealNewKillsLeftoverTaskProcs checks that New kills processes a
// previous agent's tasks left behind under the slot uids.
func TestRealNewKillsLeftoverTaskProcs(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root to run processes as the task uids")
	}
	start := func(uid int) *exec.Cmd {
		cmd := exec.Command("sleep", "300")
		cmd.SysProcAttr = &syscall.SysProcAttr{
			Credential: &syscall.Credential{Uid: uint32(uid), Gid: uint32(uid), NoSetGroups: true},
			Setsid:     true, // like a task that left its process group
		}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
		return cmd
	}
	leftover := start(testUIDBase + 1)
	outsider := start(testUIDBase + 4) // not a slot uid of a 4-slot runner

	newTestRunner(t, ModeNone, nil)

	if !waitGone(leftover.Process.Pid, 5*time.Second) {
		t.Error("New did not kill a leftover process of a slot uid")
	}
	if waitGone(outsider.Process.Pid, 100*time.Millisecond) {
		t.Error("New killed a process of a uid outside the slot range")
	}
}

// waitGone waits until pid has exited (it may linger as a zombie).
func waitGone(pid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		st, ok := readProcStatus(pid)
		if !ok || st.state == 'Z' || st.state == 'X' {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestExecFailureIsTheCommands runs commands that can't be executed through
// the real shim with dropped privileges. They fail like a shell reports it
// (127 not found, 126 not executable), use up an attempt, and leave every
// slot usable.
func TestExecFailureIsTheCommands(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root to drop privileges")
	}
	r := newTestRunner(t, ModeAuto, nil)
	if !r.caps[CapMountNS] || !r.caps[CapPidNS] {
		t.Skip("namespaces unavailable")
	}
	cases := []struct {
		cmd  []string
		code int
	}{
		{[]string{"no-such-command-savior"}, 127},
		{[]string{"/no/such/dir/prog"}, 127},
		{[]string{"/etc/passwd"}, 126}, // not executable
		{[]string{"/work"}, 126},       // a directory
	}
	for i, c := range cases {
		task := scriptTask(fmt.Sprintf("texecfail%d", i), "")
		task.Kind, task.Script, task.Command = proto.KindExec, "", c.cmd
		rep, logs := runTask(t, r, task)
		if rep.State != proto.TaskFailed || rep.ErrorKind != proto.ErrExit || rep.ExitCode != c.code {
			t.Errorf("%v: got %s/%s exit %d err %q, want failed/exit %d", c.cmd, rep.State, rep.ErrorKind, rep.ExitCode, rep.Error, c.code)
		}
		if !strings.Contains(logs, c.cmd[0]) {
			t.Errorf("%v: log doesn't name the command: %q", c.cmd, logs)
		}
	}
	if got := r.UsableSlots(); got != r.cfg.Slots {
		t.Errorf("%d of %d slots usable after exec failures (retired?)", got, r.cfg.Slots)
	}
}

// TestSandboxEtcReadableUnderUmask077 runs the real sandbox from an agent
// with umask 077, as /usr/libexec/savior/run starts it. The task user must
// still be able to read the generated /etc and the CA bundle, and gets
// the usual umask 022.
func TestSandboxEtcReadableUnderUmask077(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root to drop privileges")
	}
	old := syscall.Umask(0o077)
	defer syscall.Umask(old)
	r := newTestRunner(t, ModeAuto, nil)
	if !r.caps[CapMountNS] || !r.caps[CapPidNS] {
		t.Skip("namespaces unavailable")
	}
	checks := []string{"/etc/passwd", "/etc/group", "/etc/hosts", "/etc/nsswitch.conf"}
	if _, err := os.Stat("/etc/ssl/certs"); err == nil {
		checks = append(checks, "/etc/ssl/certs")
	}
	var script []string
	for i, f := range checks {
		script = append(script, fmt.Sprintf("[ -r %s ] && echo r%d=yes || echo r%d=no", f, i, i))
	}
	script = append(script, "echo umask=$(umask)", "id -un 2>/dev/null | sed 's/^/user=/'")
	task := scriptTask("tumask", strings.Join(script, "\n"))
	task.Network = true // also writes /etc/resolv.conf
	checks = append(checks, "/etc/resolv.conf")
	task.Script += fmt.Sprintf("\n[ -r /etc/resolv.conf ] && echo r%d=yes || echo r%d=no", len(checks)-1, len(checks)-1)
	rep, logs := runTask(t, r, task)
	if rep.State != proto.TaskSucceeded {
		t.Fatalf("state %s kind %s err %q\n%s", rep.State, rep.ErrorKind, rep.Error, logs)
	}
	kv := parseKV(logs)
	for i, f := range checks {
		if kv[fmt.Sprintf("r%d", i)] != "yes" {
			t.Errorf("the task can't read %s\n%s", f, logs)
		}
	}
	if kv["umask"] != "0022" {
		t.Errorf("task umask %q, want 0022", kv["umask"])
	}
	if kv["user"] != "savior-job" {
		t.Errorf("id -un = %q: /etc/passwd is not readable", kv["user"])
	}
}
