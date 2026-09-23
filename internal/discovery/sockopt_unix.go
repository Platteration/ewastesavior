//go:build unix

package discovery

import (
	"context"
	"net"
	"syscall"
)

// listenBroadcast opens a UDP4 socket with SO_BROADCAST and SO_REUSEADDR so
// it can send to broadcast addresses and share the discovery port.
func listenBroadcast(addr string) (net.PacketConn, error) {
	lc := net.ListenConfig{Control: func(network, address string, c syscall.RawConn) error {
		var serr error
		err := c.Control(func(fd uintptr) {
			if serr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_BROADCAST, 1); serr != nil {
				return
			}
			serr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1)
		})
		if err != nil {
			return err
		}
		return serr
	}}
	return lc.ListenPacket(context.Background(), "udp4", addr)
}
