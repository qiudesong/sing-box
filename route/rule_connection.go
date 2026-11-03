package route

import (
	"net"

	N "github.com/sagernet/sing/common/network"
)

// These wrappers own a final rule's reference after routing succeeds. The same
// idempotent release is also attached to onClose for handlers that complete
// without closing the wrapper themselves.
type ruleConn struct {
	net.Conn
	release func()
}

func (c *ruleConn) Close() error {
	c.release()
	return c.Conn.Close()
}

func (c *ruleConn) ReaderReplaceable() bool { return true }
func (c *ruleConn) WriterReplaceable() bool { return true }
func (c *ruleConn) Upstream() any           { return c.Conn }

type rulePacketConn struct {
	N.PacketConn
	release func()
}

func (c *rulePacketConn) Close() error {
	c.release()
	return c.PacketConn.Close()
}

func (c *rulePacketConn) ReaderReplaceable() bool { return true }
func (c *rulePacketConn) WriterReplaceable() bool { return true }
func (c *rulePacketConn) Upstream() any           { return c.PacketConn }
