package display

import (
	"slices"
	"strings"
	"testing"
)

// The text scene and wall text wrap between words: a smaller font is
// chosen rather than breaking a word that fits a line at some size
// (SPEC-RUNTIME-01, DESIGN 11.3 "word-wrapped").
func TestLayoutTextKeepsWordsWhole(t *testing.T) {
	msgs := []string{
		"Maintenance tonight",
		"Welcome to the lab",
		"HELLO SWARM",
		"Presentation starts at 3pm",
		"Welcome visitors",
		"Old computers get a second life here.",
		"Lab closed\nuntil Monday",
	}
	areas := []struct {
		name string
		w, h int
	}{
		{"1024x768", 1024, 768}, {"1280x1024", 1280, 1024}, {"480x640", 480, 640},
		{"768x1024", 768, 1024}, {"320x240", 320, 240},
		{"wall 2x1 canvas (mm)", 690, 270}, {"wall 1x3 canvas (mm)", 270, 1040},
	}
	for _, a := range areas {
		for _, title := range []string{"", "Notice"} {
			for _, msg := range msgs {
				var words []string
				for _, l := range layoutText(title, msg, a.w, a.h) {
					if l.st == styleBold {
						continue // the title line
					}
					words = append(words, strings.Fields(l.s)...)
				}
				if want := strings.Fields(msg); !slices.Equal(words, want) {
					t.Errorf("%s title %q: %q laid out as words %q", a.name, title, msg, words)
				}
			}
		}
	}
}

// A single token too wide for the area at any size is still split rather
// than dropped.
func TestLayoutTextSplitsUnfittableToken(t *testing.T) {
	token := strings.Repeat("W", 40)
	var got string
	for _, l := range layoutText("", token, 6, 400) {
		got += l.s
	}
	if got != token {
		t.Fatalf("unfittable token laid out as %q", got)
	}
}
