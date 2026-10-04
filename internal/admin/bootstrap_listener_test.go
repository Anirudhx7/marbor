package admin

import "net"

// fakeListener is a net.Listener that reports a chosen bound address, so the
// gate can be exercised with addresses that cannot be bound in a test.
type fakeListener struct{ addr *net.TCPAddr }

func newFakeListener(ip string) *fakeListener {
	return &fakeListener{addr: &net.TCPAddr{IP: net.ParseIP(ip), Port: 8080}}
}

func (f *fakeListener) Accept() (net.Conn, error) { return nil, net.ErrClosed }
func (f *fakeListener) Close() error              { return nil }
func (f *fakeListener) Addr() net.Addr            { return f.addr }

func listenTCP(bind string) (net.Listener, error) { return net.Listen("tcp", bind) }
