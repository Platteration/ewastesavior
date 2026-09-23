// Package display drives a node's screen: the Linux fbdev driver (16/24/32
// bpp, rotation, blanking), virtual terminal handling, the scene renderer
// (status, text, clock, image, slideshow, dashboard, video wall tiles, test
// patterns), fonts, media fetching and the `savior display` command.
//
// See docs/DESIGN.md section 11. Everything draws into an *image.RGBA of the
// logical (rotated) screen size; a Device converts it into the hardware
// pixel format. Scaling is integer-only (internal/imaging), so the package is
// usable on 32-bit CPUs without SSE2.
package display
