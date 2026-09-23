package hwinfo

// edidHeader starts every EDID base block.
var edidHeader = [8]byte{0x00, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x00}

// maxPlausibleMM bounds physical sizes; anything larger is garbage.
const maxPlausibleMM = 4000

// EDIDSizeMM returns a display's physical size in millimetres from its EDID
// base block, or 0, 0 when unknown. The first detailed timing descriptor
// (bytes 54-71; sizes in bytes 66-68) is preferred because it has mm
// precision. The base block's cm fields (bytes 21-22) are the fallback, and
// they win when the descriptor is much smaller than them, because some
// panels store centimetres in the descriptor. When one cm field is zero the
// other one encodes an aspect ratio (EDID 1.4), so both must be non-zero.
// The checksum is not verified: laptop panels with bad checksums are common
// and the fields used are range-checked instead.
func EDIDSizeMM(b []byte) (w, h int) {
	if len(b) < 128 || [8]byte(b[:8]) != edidHeader {
		return 0, 0
	}
	cmW, cmH := int(b[21])*10, int(b[22])*10
	if cmW == 0 || cmH == 0 {
		cmW, cmH = 0, 0
	}
	var dtW, dtH int
	if b[54] != 0 || b[55] != 0 { // non-zero pixel clock = detailed timing
		dtW = int(b[66]) | int(b[68]>>4)<<8
		dtH = int(b[67]) | int(b[68]&0x0f)<<8
		if dtW == 0 || dtH == 0 {
			dtW, dtH = 0, 0
		}
	}
	switch {
	case dtW > 0 && cmW > 0 && dtW*5 < cmW && dtH*5 < cmH:
		w, h = cmW, cmH // descriptor holds cm
	case dtW > 0:
		w, h = dtW, dtH
	default:
		w, h = cmW, cmH
	}
	if w > maxPlausibleMM || h > maxPlausibleMM {
		return 0, 0
	}
	return w, h
}
