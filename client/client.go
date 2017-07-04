package client

import (
	"context"
	"errors"
	"net"
	"os"
	"runtime"

	"github.com/golang/sync/syncmap"

	"github.com/4396/tun/fake"
	"github.com/4396/tun/msg"
	"github.com/4396/tun/mux"
	"github.com/4396/tun/proxy"
	"github.com/4396/tun/version"
)

var (
	ErrDialerClosed  = errors.New("Dialer closed")
	ErrUnexpectedMsg = errors.New("Unexpected response")
)

type Client struct {
	service   proxy.Service
	session   *mux.Session
	listeners syncmap.Map
	cmd       net.Conn
	errc      chan error
}

func Dial(addr string) (c *Client, err error) {
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		return
	}

	sess, err := mux.Client(conn)
	if err != nil {
		conn.Close()
		return
	}

	cmd, err := sess.OpenConn()
	if err != nil {
		sess.Close()
		conn.Close()
		return
	}

	c = &Client{
		session: sess,
		cmd:     cmd,
		errc:    make(chan error, 1),
	}
	return
}

func (c *Client) authProxy(name, token string) (err error) {
	ver := version.Version
	hostname, _ := os.Hostname()
	err = msg.Write(c.cmd, &msg.Proxy{
		Name:     name,
		Token:    token,
		Version:  ver,
		Hostname: hostname,
		Os:       runtime.GOOS,
		Arch:     runtime.GOARCH,
	})
	if err != nil {
		return
	}

	m, err := msg.Read(c.cmd)
	if err != nil {
		return
	}

	switch mm := m.(type) {
	case *msg.Version:
		err = version.CompatServer(mm.Version)
	case *msg.Error:
		err = errors.New(mm.Message)
	default:
		err = ErrUnexpectedMsg
	}
	return
}

func (c *Client) Proxy(name, token, addr string) (err error) {
	err = c.authProxy(name, token)
	if err != nil {
		return
	}

	l := fake.NewListener()
	p := proxy.Wrap(name, l)
	p.Bind(&dialer{Addr: addr})
	err = c.service.Proxy(p)
	if err != nil {
		return
	}

	c.listeners.Store(name, l)
	return
}

func (c *Client) Run(ctx context.Context) (err error) {
	connc := make(chan net.Conn, 16)
	ctx, cancel := context.WithCancel(ctx)
	defer func() {
		cancel()

		c.cmd.Close()
		c.session.Close()

		close(connc)
		for conn := range connc {
			conn.Close()
		}
	}()

	go c.listen(ctx, connc)
	go func() {
		err := c.service.Serve(ctx)
		if err != nil {
			c.errc <- err
		}
	}()

	for {
		select {
		case conn := <-connc:
			c.handleConn(conn)
		case err = <-c.errc:
			return
		case <-ctx.Done():
			err = ctx.Err()
			return
		}
	}
}

func (c *Client) listen(ctx context.Context, connc chan<- net.Conn) {
	for {
		conn, err := c.session.AcceptConn()
		if err != nil {
			c.errc <- ErrDialerClosed
			return
		}

		select {
		case <-ctx.Done():
			return
		default:
			connc <- conn
		}
	}
}

func (c *Client) handleConn(conn net.Conn) {
	var (
		err    error
		worker msg.Worker
	)

	defer func() {
		if err != nil {
			conn.Close()
		}
	}()

	err = msg.ReadInto(conn, &worker)
	if err != nil {
		return
	}

	val, ok := c.listeners.Load(worker.Name)
	if ok {
		err = val.(*fake.Listener).Put(conn)
	}
}
