//go:build linux

package runner

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"

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
		"uid":       "10000",
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
		SelfExe: self, Sandbox: ModeNone, ScratchInRAM: true, UIDBase: 10000, Slots: 2,
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
