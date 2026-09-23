package display

// vtHandle is the controller's view of the display VT.
type vtHandle interface {
	Foreground() bool
	SetLEDs(on, restore bool)
	Tone(hz int)
	Close() error
}

// DefaultVT is the dedicated display VT (Alt+F7).
const DefaultVT = "/dev/tty7"
