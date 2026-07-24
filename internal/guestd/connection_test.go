package guestd

import (
	"bytes"
	"net"
	"time"
)

type scriptedNetConn struct {
	reader  *bytes.Reader
	written bytes.Buffer
}

func (conn *scriptedNetConn) Read(value []byte) (int, error) {
	return conn.reader.Read(value)
}

func (conn *scriptedNetConn) Write(value []byte) (int, error) {
	return conn.written.Write(value)
}

func (*scriptedNetConn) Close() error                     { return nil }
func (*scriptedNetConn) LocalAddr() net.Addr              { return nil }
func (*scriptedNetConn) RemoteAddr() net.Addr             { return nil }
func (*scriptedNetConn) SetDeadline(time.Time) error      { return nil }
func (*scriptedNetConn) SetReadDeadline(time.Time) error  { return nil }
func (*scriptedNetConn) SetWriteDeadline(time.Time) error { return nil }
