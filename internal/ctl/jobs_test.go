package ctl

import (
	"bytes"
	"debug/elf"
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/platteration/ewastesavior/internal/proto"
)

func writeFile(t *testing.T, name string, data []byte, mode os.FileMode) string {
	t.Helper()
	if err := os.WriteFile(name, data, mode); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(name, mode); err != nil {
			t.Fatal(err)
		}
	}
	return name
}

func TestRunBuildsJobSpecFromFlags(t *testing.T) {
	h := newFakeHive(t)
	e := loggedIn(t, h)
	dir := t.TempDir()
	data := writeFile(t, filepath.Join(dir, "data.txt"), []byte("some input data\n"), 0o644)
	prog := writeFile(t, filepath.Join(dir, "prog.sh"), []byte("#!/bin/sh\necho hi\n"), 0o755)
	urlSHA := strings.Repeat("ab", 32)
	e.env["FROM_HERE"] = "copied value"

	code, stdout, stderr := e.run("run",
		"--name", "demo", "--count", "3", "--cores", "0.5", "--mem", "256", "--disk", "100",
		"--timeout", "2h", "--retries", "0", "--arch", "amd64, 386", "--cpu-flags", "sse2",
		"--min-mem", "512", "--label", "zone=a", "--label", "gpu=none", "--node", "lobby-1,n0123456789ab",
		"--network", "--isolation", "any", "--priority", "5",
		"--input", data+":in/data.txt", "--input", prog,
		"--input-url", "https://example.com:8443/x.bin:"+urlSHA+":1234:x.bin",
		"--output", "out/*.txt", "--output", "result.json",
		"--env", "FOO=bar=baz", "--env", "FROM_HERE",
		"--", "./prog.sh", "{{index}}", "--flag-for-the-program")
	if code != 0 {
		t.Fatalf("run: code %d, stderr %s", code, stderr)
	}
	if len(h.submitted) != 1 {
		t.Fatalf("submitted %d jobs", len(h.submitted))
	}
	got := h.submitted[0]
	zero := 0
	want := proto.JobSpec{
		Name:    "demo",
		Kind:    proto.KindExec,
		Command: []string{"./prog.sh", "{{index}}", "--flag-for-the-program"},
		Env:     map[string]string{"FOO": "bar=baz", "FROM_HERE": "copied value"},
		Inputs: []proto.Input{
			{Name: "in/data.txt", Blob: sha256Hex([]byte("some input data\n"))},
			{Name: "prog.sh", Blob: sha256Hex([]byte("#!/bin/sh\necho hi\n")), Executable: true},
			{Name: "x.bin", URL: "https://example.com:8443/x.bin", SHA256: urlSHA, Size: 1234},
		},
		Outputs:   []string{"out/*.txt", "result.json"},
		Resources: proto.Resources{Cores: 0.5, MemMB: 256, DiskMB: 100},
		Requirements: proto.Requirements{
			Arch: []string{"amd64", "386"}, MinMemMB: 512, CPUFlags: []string{"sse2"},
			Labels: map[string]string{"zone": "a", "gpu": "none"},
			Nodes:  []string{"lobby-1", "n0123456789ab"}, Isolation: proto.IsolationAny,
		},
		TimeoutS: 7200,
		Retries:  &zero,
		Network:  true,
		Count:    3,
		Priority: 5,
	}
	if !reflect.DeepEqual(got, want) {
		gj, _ := json.MarshalIndent(got, "", " ")
		wj, _ := json.MarshalIndent(want, "", " ")
		t.Fatalf("job spec mismatch\n got: %s\nwant: %s", gj, wj)
	}
	for _, in := range want.Inputs[:2] {
		if _, ok := h.blobs[in.Blob]; !ok {
			t.Errorf("input %s was not uploaded", in.Name)
		}
	}
	if !strings.Contains(stdout, "Submitted job j") {
		t.Errorf("stdout: %s", stdout)
	}

	// Running again re-uses the stored blobs: the hive skips the transfer.
	if code, _, stderr := e.run("run", "--input", data, "--", "cat", "data.txt"); code != 0 {
		t.Fatalf("second run: %s", stderr)
	}
	if n := h.blobPuts[sha256Hex([]byte("some input data\n"))]; n != 1 {
		t.Errorf("data.txt body transferred %d times, want 1", n)
	}
	assertTokenNeverSent(t, h)
}

func TestRunValidatesBeforeUploading(t *testing.T) {
	h := newFakeHive(t)
	e := loggedIn(t, h)
	dir := t.TempDir()
	data := writeFile(t, filepath.Join(dir, "data.txt"), []byte("x"), 0o644)
	big := writeFile(t, filepath.Join(dir, "big.bin"), bytes.Repeat([]byte{1}, 2<<20), 0o644)
	before := h.requestCount()
	cases := [][]string{
		{"run", "--input", data + ":../escape", "--", "true"},
		{"run", "--output", "../*", "--", "true"},
		{"run", "--env", "SAVIOR_TASK_INDEX=1", "--", "true"},
		{"run", "--isolation", "maybe", "--", "true"},
		{"run", "--input", data, "--input", data, "--", "true"},
		{"run", "--input", big, "--disk", "1", "--", "true"},
		{"run", "--input-url", "https://x/y:nothex:10:y", "--", "true"},
		{"run", "--label", "Bad Key=1", "--", "true"},
		{"run", "--count", "0", "--cores", "-1", "--", "true"},
		{"run", "--wait"},
		{"run", "--env", "UNSET_HERE", "--", "true"},
	}
	for _, args := range cases {
		code, _, stderr := e.run(args...)
		if code != 2 {
			t.Errorf("%v: code %d (stderr %s), want 2", args, code, stderr)
		}
	}
	if h.requestCount() != before {
		t.Fatal("invalid jobs reached the hive")
	}
}

func TestRunWaitSuccessFetchesOutputs(t *testing.T) {
	h := newFakeHive(t)
	e := loggedIn(t, h)
	content := []byte("result 42\n")
	sha := sha256Hex(content)
	h.blobs[sha] = content
	out := filepath.Join(t.TempDir(), "results")

	h.mu.Lock()
	h.pollsToEnd = 3
	h.anyOutputs = []proto.OutputEntry{{TaskID: "t1", Index: 0, Name: "out/r.txt", Blob: sha, Size: int64(len(content))}}
	h.mu.Unlock()
	code, stdout, stderr := e.run("run", "--wait", "--fetch", out, "--", "echo", "hi")
	if code != 0 {
		t.Fatalf("code %d, stderr %s", code, stderr)
	}
	if !strings.Contains(stdout, "succeeded") {
		t.Errorf("stdout: %s", stdout)
	}
	if !strings.Contains(stderr, "running") {
		t.Errorf("no progress line in stderr: %s", stderr)
	}
	got, err := os.ReadFile(filepath.Join(out, "task-0", "out", "r.txt"))
	if err != nil || !bytes.Equal(got, content) {
		t.Fatalf("fetched output = %q, %v", got, err)
	}
}

func TestRunWaitFailureExplains(t *testing.T) {
	h := newFakeHive(t)
	e := loggedIn(t, h)
	exit := 3
	var log strings.Builder
	for i := 1; i <= 15; i++ {
		log.WriteString("line " + string(rune('a'+i-1)) + "\n")
	}
	h.mu.Lock()
	h.jobFinal = proto.JobFailed
	h.failedTask = proto.TaskView{ID: "tfail", Index: 2, State: proto.TaskFailed, Node: "n0123456789ab", Attempt: 2,
		ExitCode: &exit, ErrorKind: proto.ErrExit, Error: "process exited"}
	h.logs["tfail"] = []byte(log.String() + "\x1b[2Jboom\n")
	h.mu.Unlock()

	code, _, stderr := e.run("run", "--wait", "--count", "4", "--", "false")
	if code != 1 {
		t.Fatalf("code %d, want 1; stderr %s", code, stderr)
	}
	for _, want := range []string{"failed: 1 of 4 tasks failed", "tfail", "exit code 3", "process exited", "line o", "[2Jboom"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr lacks %q:\n%s", want, stderr)
		}
	}
	if strings.Contains(stderr, "line e\n") {
		t.Errorf("more than the last 10 lines shown:\n%s", stderr)
	}
	if strings.Contains(stderr, "\x1b") {
		t.Error("escape sequence from the task log reached the terminal")
	}
}

func TestScriptAndSubmit(t *testing.T) {
	h := newFakeHive(t)
	e := loggedIn(t, h)
	dir := t.TempDir()
	script := writeFile(t, filepath.Join(dir, "job.sh"), []byte("echo $SAVIOR_TASK_INDEX\n"), 0o644)
	if code, _, stderr := e.run("script", script, "--count", "2", "--timeout", "60"); code != 0 {
		t.Fatalf("script: %s", stderr)
	}
	got := h.submitted[len(h.submitted)-1]
	if got.Kind != proto.KindScript || got.Script != "echo $SAVIOR_TASK_INDEX\n" || got.Count != 2 || got.TimeoutS != 60 || len(got.Command) != 0 {
		t.Errorf("script job = %+v", got)
	}

	spec := writeFile(t, filepath.Join(dir, "job.json"), []byte(`{"command":["uname","-a"],"count":2}`), 0o644)
	code, stdout, stderr := e.run("--json", "submit", spec)
	if code != 0 {
		t.Fatalf("submit: %s", stderr)
	}
	var jd proto.JobDetail
	if err := json.Unmarshal([]byte(stdout), &jd); err != nil || jd.ID == "" || jd.Count != 2 {
		t.Fatalf("submit --json output %q: %v", stdout, err)
	}
	bad := writeFile(t, filepath.Join(dir, "bad.json"), []byte(`{"comand":["typo"]}`), 0o644)
	if code, _, stderr := e.run("submit", bad); code != 2 || !strings.Contains(stderr, "comand") {
		t.Errorf("unknown field: code %d, %s", code, stderr)
	}
}

func TestSecondsFlag(t *testing.T) {
	for in, want := range map[string]int{"90": 90, "2h": 7200, "1m30s": 90} {
		var s secondsFlag
		if err := s.Set(in); err != nil || int(s) != want {
			t.Errorf("%q -> %d, %v", in, s, err)
		}
	}
	var s secondsFlag
	if s.Set("1.5s") == nil || s.Set("soon") == nil {
		t.Error("accepted a bad duration")
	}
}

func TestSplitInputAndURL(t *testing.T) {
	dir := t.TempDir()
	f := writeFile(t, filepath.Join(dir, "a.txt"), []byte("x"), 0o644)
	if file, name, err := splitInput(f); err != nil || file != f || name != "a.txt" {
		t.Errorf("splitInput(%q) = %q %q %v", f, file, name, err)
	}
	if file, name, err := splitInput(f + ":sub/b.txt"); err != nil || file != f || name != "sub/b.txt" {
		t.Errorf("with name: %q %q %v", file, name, err)
	}
	for _, bad := range []string{f + ":../x", f + ":/abs", f + `:a\b`, f + ":.savior-x"} {
		if _, _, err := splitInput(bad); err == nil {
			t.Errorf("splitInput(%q) accepted", bad)
		}
	}
	sha := strings.Repeat("0f", 32)
	in, err := parseInputURL("http://h:81/p?q=1:" + strings.ToUpper(sha) + ":10:n")
	if err != nil || in.URL != "http://h:81/p?q=1" || in.SHA256 != sha || in.Size != 10 || in.Name != "n" {
		t.Errorf("parseInputURL = %+v, %v", in, err)
	}
	for _, bad := range []string{"http://x", "http://x:" + sha + ":0:n", "http://x:" + sha + ":10:", "x:y:z:w"} {
		if _, err := parseInputURL(bad); err == nil {
			t.Errorf("parseInputURL(%q) accepted", bad)
		}
	}
}

func TestJobsTasksAndCancel(t *testing.T) {
	h := newFakeHive(t)
	e := loggedIn(t, h)
	if code, _, stderr := e.run("run", "--", "true"); code != 0 {
		t.Fatal(stderr)
	}
	var id string
	for k := range h.jobs {
		id = k
	}
	code, stdout, _ := e.run("jobs")
	if code != 0 || !strings.Contains(stdout, id) {
		t.Errorf("jobs: %d %s", code, stdout)
	}
	if code, stdout, _ := e.run("job", id); code != 0 || !strings.Contains(stdout, "true") {
		t.Errorf("job: %d %s", code, stdout)
	}
	if code, stdout, _ := e.run("cancel", id); code != 0 || !strings.Contains(stdout, "canceled") {
		t.Errorf("cancel: %d %s", code, stdout)
	}
	if code, _, stderr := e.run("jobs", "--state", "bogus"); code != 2 {
		t.Errorf("bad state accepted: %s", stderr)
	}
	if code, _, stderr := e.run("job", "../../nodes"); code != 1 || !strings.Contains(stderr, "invalid job") {
		t.Errorf("path-like job id: %d %s", code, stderr)
	}
}

// elfHeader returns a minimal ELF header for the given class and machine.
func elfHeader(class elf.Class, machine elf.Machine) []byte {
	var b bytes.Buffer
	b.Write([]byte{0x7f, 'E', 'L', 'F', byte(class), byte(elf.ELFDATA2LSB), byte(elf.EV_CURRENT)})
	b.Write(make([]byte, 9))
	le := binary.LittleEndian
	b.Write(le.AppendUint16(nil, uint16(elf.ET_EXEC)))
	b.Write(le.AppendUint16(nil, uint16(machine)))
	b.Write(le.AppendUint32(nil, uint32(elf.EV_CURRENT)))
	if class == elf.ELFCLASS64 {
		b.Write(make([]byte, 8*3+4))                    // entry, phoff, shoff, flags
		b.Write(le.AppendUint16(nil, 64))               // ehsize
		b.Write([]byte{56, 0, 0, 0, 64, 0, 0, 0, 0, 0}) // phentsize, phnum, shentsize, shnum, shstrndx
	} else {
		b.Write(make([]byte, 4*3+4))
		b.Write(le.AppendUint16(nil, 52))
		b.Write([]byte{32, 0, 0, 0, 40, 0, 0, 0, 0, 0})
	}
	return b.Bytes()
}

func TestRunArchFromELFInputs(t *testing.T) {
	h := newFakeHive(t)
	e := loggedIn(t, h)
	dir := t.TempDir()
	amd64 := writeFile(t, filepath.Join(dir, "render"), elfHeader(elf.ELFCLASS64, elf.EM_X86_64), 0o755)
	i386 := writeFile(t, filepath.Join(dir, "render32"), elfHeader(elf.ELFCLASS32, elf.EM_386), 0o755)
	data := writeFile(t, filepath.Join(dir, "scene.dat"), []byte("\x7fELF but not really"), 0o644)
	for _, c := range []struct {
		args []string
		want []string
		note bool
	}{
		{[]string{"--input", amd64, "--input", data, "--", "./render"}, []string{"amd64"}, true},
		{[]string{"--input", i386, "--input", amd64, "--", "sh", "-c", "true"}, []string{"386", "amd64"}, true},
		{[]string{"--input", data, "--", "cat", "scene.dat"}, nil, false},
		{[]string{"--arch", "386", "--input", amd64, "--", "./render"}, []string{"386"}, false},
		{[]string{"--arch", "any", "--input", amd64, "--", "./render"}, nil, false},
	} {
		n := len(h.submitted)
		code, _, stderr := e.run(append([]string{"run"}, c.args...)...)
		if code != 0 || len(h.submitted) != n+1 {
			t.Fatalf("%v: code %d stderr %s", c.args, code, stderr)
		}
		if got := h.submitted[n].Requirements.Arch; !reflect.DeepEqual(got, c.want) {
			t.Errorf("%v: arch %v, want %v", c.args, got, c.want)
		}
		if strings.Contains(stderr, "note: the inputs contain") != c.note {
			t.Errorf("%v: note shown = %v, want %v (stderr %q)", c.args, !c.note, c.note, stderr)
		}
	}
}
