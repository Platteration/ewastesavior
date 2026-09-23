package display

import (
	"context"
	"image"
	"testing"
	"time"

	"github.com/platteration/ewastesavior/internal/proto"
)

func benchStatus() StatusInfo {
	return StatusInfo{Name: "lab-thinkpad-t60", ShortCode: "7KQ", NodeID: "n1a2b3c4d5e6f", Version: "v0.2.0",
		Link: proto.LinkConnected, HiveAddr: "192.168.1.20:7700", Addrs: []string{"192.168.1.57/24"},
		Roles:     []proto.Role{"compute", "display"},
		Metrics:   proto.Metrics{CPUPercent: 37, MemAvailableMB: 612, CPUTempC: 61, CPUTempLimitC: 85, BatteryPercent: -1},
		Inventory: proto.Inventory{CPUModel: "Intel(R) Core(TM)2 CPU T7200 @ 2.00GHz", Cores: 2, MemTotalMB: 1024}}
}

func benchRender(b *testing.B, spec proto.DisplaySpec, steady bool) {
	dst := image.NewRGBA(image.Rect(0, 0, 1024, 768))
	st := benchStatus()
	env := Env{Now: func() time.Time { return testNow }, Status: func() StatusInfo { return st }}
	first := renderFrame(context.Background(), spec, dst, env, "") // warm glyph cache
	prev := ""
	if steady {
		prev = first.key
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if res := renderFrame(context.Background(), spec, dst, env, prev); res.err != nil {
			b.Fatal(res.err)
		}
	}
}

// Full redraws (worst case: something visible changed).
func BenchmarkRenderStatus1024(b *testing.B) {
	benchRender(b, proto.DisplaySpec{Mode: "status"}, false)
}
func BenchmarkRenderClock1024(b *testing.B) { benchRender(b, proto.DisplaySpec{Mode: "clock"}, false) }
func BenchmarkRenderText1024(b *testing.B) {
	benchRender(b, proto.DisplaySpec{Mode: "text", Title: "Welcome", Text: "Old computers get a second life here. Please do not switch them off."}, false)
}

// Steady state: the controller's once-a-second check with nothing changed.
func BenchmarkRenderStatusSteady(b *testing.B) {
	benchRender(b, proto.DisplaySpec{Mode: "status"}, true)
}
func BenchmarkRenderClockSteady(b *testing.B) { benchRender(b, proto.DisplaySpec{Mode: "clock"}, true) }

func benchConvert(b *testing.B, format string, rotate int) {
	d := NewMemDevice(1024, 768, 0, format, 0, rotate)
	info := d.Info()
	frame := testFrame(info.Width, info.Height, 1)
	b.SetBytes(int64(len(frame.Pix)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		d.Invalidate() // force conversion and copy of every row
		if err := d.Show(frame, nil); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkConvertXRGB8888(b *testing.B)      { benchConvert(b, "XRGB8888", 0) }
func BenchmarkConvertXRGB8888Rot90(b *testing.B) { benchConvert(b, "XRGB8888", 90) }
func BenchmarkConvertRGB565(b *testing.B)        { benchConvert(b, "RGB565", 0) }
func BenchmarkConvertRGB565Rot90(b *testing.B)   { benchConvert(b, "RGB565", 90) }
func BenchmarkConvertGeneric(b *testing.B)       { benchConvert(b, "BGR565", 0) }

// BenchmarkShowUnchanged is the cost of re-showing an identical frame:
// conversion plus row comparison, no device writes.
func BenchmarkShowUnchanged(b *testing.B) {
	d := NewMemDevice(1024, 768, 0, "XRGB8888", 0, 0)
	frame := testFrame(1024, 768, 1)
	d.Show(frame, nil)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		d.Show(frame, nil)
	}
}
