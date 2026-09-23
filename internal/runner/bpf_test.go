package runner

import (
	"encoding/binary"
	"fmt"
)

// seccompData mirrors struct seccomp_data for the test interpreter.
type seccompData struct {
	Nr   int32
	Arch uint32
	IP   uint64
	Args [6]uint64
}

func (d seccompData) bytes() []byte {
	b := make([]byte, 64)
	binary.LittleEndian.PutUint32(b[0:], uint32(d.Nr))
	binary.LittleEndian.PutUint32(b[4:], d.Arch)
	binary.LittleEndian.PutUint64(b[8:], d.IP)
	for i, a := range d.Args {
		binary.LittleEndian.PutUint64(b[16+8*i:], a)
	}
	return b
}

// runBPF interprets the subset of classic BPF the filter uses, with the
// same restrictions the kernel's seccomp checker applies: 32-bit absolute
// loads, aligned and inside seccomp_data; forward jumps only; the program
// must end in a return.
func runBPF(prog []bpfInsn, d seccompData) (uint32, error) {
	data := d.bytes()
	var acc uint32
	for pc := 0; pc < len(prog); pc++ {
		in := prog[pc]
		switch in.Code {
		case bpfLD | bpfW | bpfABS:
			if in.K%4 != 0 || in.K+4 > uint32(len(data)) {
				return 0, fmt.Errorf("pc %d: bad load offset %d", pc, in.K)
			}
			acc = binary.LittleEndian.Uint32(data[in.K:])
		case bpfJMP | bpfJEQ | bpfK, bpfJMP | bpfJSET | bpfK:
			cond := acc == in.K
			if in.Code == bpfJMP|bpfJSET|bpfK {
				cond = acc&in.K != 0
			}
			off := int(in.Jf)
			if cond {
				off = int(in.Jt)
			}
			pc += off
			if pc+1 >= len(prog) {
				return 0, fmt.Errorf("pc %d: jump past end", pc)
			}
		case bpfJMP | bpfJA:
			pc += int(in.K)
		case bpfRET | bpfK:
			return in.K, nil
		default:
			return 0, fmt.Errorf("pc %d: unsupported opcode %#x", pc, in.Code)
		}
	}
	return 0, fmt.Errorf("program fell off the end")
}
