//go:build linux

package display

import (
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// <linux/vt.h> and <linux/kd.h>.
const (
	vtGetState   = 0x5603
	vtSetMode    = 0x5602
	vtRelDisp    = 0x5605
	vtActivate   = 0x5606
	vtAuto       = 0
	vtProcess    = 1
	vtAckAcq     = 2
	kdSetMode    = 0x4B3A
	kdText       = 0
	kdGraphics   = 1
	kdSetLED     = 0x4B32
	kiocSound    = 0x4B2F
	ledsRestore  = 0xFF // KDSETLED > 7 reverts to the keyboard state
	pitHz        = 1193180
	vtActivateTO = 5 * time.Second
)

// vtMode is struct vt_mode.
type vtMode struct {
	Mode   int8
	Waitv  int8
	Relsig int16
	Acqsig int16
	Frsig  int16
}

// vtStat is struct vt_stat.
type vtStat struct {
	Active uint16
	Signal uint16
	State  uint16
}

// vtConsole owns the display's virtual terminal (DESIGN 11.2): graphics
// mode so fbcon stays off the screen, and process-controlled switching so
// Alt+F1/F2 work and we stop drawing while another VT is shown.
type vtConsole struct {
	f         *os.File
	fd        int
	num       int
	sigs      chan os.Signal
	done      chan struct{}
	onRelease func()
	onAcquire func()

	mu     sync.Mutex // guards fd use against Close (fd numbers get reused)
	closed bool
}

// vtNumber parses the N of /dev/ttyN (1..63).
func vtNumber(path string) (int, error) {
	n, err := strconv.Atoi(strings.TrimPrefix(path, "/dev/tty"))
	if err != nil || !strings.HasPrefix(path, "/dev/tty") || n < 1 || n > 63 {
		return 0, fmt.Errorf("%q is not a virtual terminal (/dev/tty1../dev/tty63)", path)
	}
	return n, nil
}

// openVT switches to the VT at path and takes control of it. onRelease is
// called before another VT is shown (stop drawing); onAcquire after ours
// is shown again (repaint). Both run on the VT goroutine.
func openVT(path string, onRelease, onAcquire func()) (vtHandle, error) {
	num, err := vtNumber(path)
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	v := &vtConsole{f: f, fd: int(f.Fd()), num: num, onRelease: onRelease, onAcquire: onAcquire,
		sigs: make(chan os.Signal, 4), done: make(chan struct{})}
	if err := ioctlVal(v.fd, vtActivate, uintptr(num)); err != nil {
		f.Close()
		return nil, fmt.Errorf("VT_ACTIVATE %d: %w", num, err)
	}
	// VT_WAITACTIVE can block forever if another VT owner never releases,
	// so poll VT_GETSTATE with a deadline instead.
	for deadline := time.Now().Add(vtActivateTO); !v.Foreground() && time.Now().Before(deadline); {
		time.Sleep(50 * time.Millisecond)
	}
	if err := ioctlVal(v.fd, kdSetMode, kdGraphics); err != nil {
		f.Close()
		return nil, fmt.Errorf("KDSETMODE KD_GRAPHICS: %w", err)
	}
	// The handler must be installed before VT_PROCESS: the default action
	// of SIGUSR1 would kill the agent. It stays installed after Close for
	// the same reason (a signal may still be in flight).
	signal.Notify(v.sigs, syscall.SIGUSR1, syscall.SIGUSR2)
	mode := vtMode{Mode: vtProcess, Relsig: int16(syscall.SIGUSR1), Acqsig: int16(syscall.SIGUSR2)}
	if err := ioctlPtr(v.fd, vtSetMode, unsafe.Pointer(&mode)); err != nil {
		_ = ioctlVal(v.fd, kdSetMode, kdText)
		f.Close()
		return nil, fmt.Errorf("VT_SETMODE VT_PROCESS: %w", err)
	}
	go v.loop()
	return v, nil
}

func (v *vtConsole) loop() {
	for {
		select {
		case <-v.done:
			return
		case s := <-v.sigs:
			// The switch must always be answered, or the console is
			// stuck on our VT; callback panics are contained.
			switch s {
			case syscall.SIGUSR1:
				safeCall(v.onRelease)
				v.relDisp(1)
			case syscall.SIGUSR2:
				v.relDisp(vtAckAcq)
				safeCall(v.onAcquire)
			}
		}
	}
}

func safeCall(f func()) {
	defer func() { _ = recover() }()
	if f != nil {
		f()
	}
}

func (v *vtConsole) relDisp(arg uintptr) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if !v.closed {
		_ = ioctlVal(v.fd, vtRelDisp, arg)
	}
}

// Foreground reports whether our VT is the active one.
func (v *vtConsole) Foreground() bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.closed {
		return false
	}
	var st vtStat
	if err := ioctlPtr(v.fd, vtGetState, unsafe.Pointer(&st)); err != nil {
		return false
	}
	return int(st.Active) == v.num
}

// SetLEDs lights all keyboard LEDs (on) or turns them off; restore gives
// the LEDs back to the keyboard.
func (v *vtConsole) SetLEDs(on, restore bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.closed {
		return
	}
	arg := uintptr(0)
	switch {
	case restore:
		arg = ledsRestore
	case on:
		arg = 7
	}
	_ = ioctlVal(v.fd, kdSetLED, arg)
}

// Tone starts a PC speaker tone at hz, or stops it for hz <= 0.
func (v *vtConsole) Tone(hz int) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.closed {
		return
	}
	arg := uintptr(0)
	if hz > 0 {
		arg = uintptr(pitHz / hz)
	}
	_ = ioctlVal(v.fd, kiocSound, arg)
}

// Close returns the VT to automatic switching and text mode.
func (v *vtConsole) Close() error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.closed {
		return nil
	}
	v.closed = true
	close(v.done)
	_ = ioctlVal(v.fd, kiocSound, 0)
	_ = ioctlVal(v.fd, kdSetLED, ledsRestore)
	err := resetVTfd(v.fd)
	if cerr := v.f.Close(); err == nil {
		err = cerr
	}
	return err
}

func resetVTfd(fd int) error {
	mode := vtMode{Mode: vtAuto}
	err1 := ioctlPtr(fd, vtSetMode, unsafe.Pointer(&mode))
	err2 := ioctlVal(fd, kdSetMode, kdText)
	return errors.Join(err1, err2)
}

// resetVT is `savior display vt-reset`: KD_TEXT and VT_AUTO on path, LEDs
// and speaker back to normal.
func resetVT(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()
	fd := int(f.Fd())
	_ = ioctlVal(fd, kiocSound, 0)
	_ = ioctlVal(fd, kdSetLED, ledsRestore)
	if err := resetVTfd(fd); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

// unblankFB turns a framebuffer back on (vt-reset after a crash).
func unblankFB(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	return ioctlVal(int(f.Fd()), fbioBlank, fbBlankUnblank)
}
