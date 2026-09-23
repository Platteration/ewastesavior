//go:build !unix

package discovery

import "net"

// listenBroadcast opens a UDP4 socket. On Windows, sending to broadcast
// addresses works without SO_BROADCAST for most stacks.
func listenBroadcast(addr string) (net.PacketConn, error) {
	return net.ListenPacket("udp4", addr)
}
