// Copyright 2019 Andy Pan. All rights reserved.
// Copyright 2018 Joshua J Baker. All rights reserved.
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

// +build darwin netbsd freebsd openbsd dragonfly linux

package gnet

import (
	"fmt"
	"net"
	"time"

	"github.com/panjf2000/gnet/internal"
	"github.com/panjf2000/gnet/ringbuffer"
	"golang.org/x/sys/unix"
)

type loop struct {
	idx     int            // loop index in the server loops list
	poll    *internal.Poll // epoll or kqueue
	packet  []byte         // read packet buffer
	fdconns map[int]*conn  // loop connections fd -> conn
}

func loopCloseConn(s *server, l *loop, c *conn, err error) error {
	delete(l.fdconns, c.fd)
	fmt.Printf("closing fd: %d, err: %v\n", c.fd, unix.Close(c.fd))
	if s.events.OnClosed != nil {
		switch s.events.OnClosed(c, err) {
		case None:
		case Shutdown:
			return errClosing
		}
	}
	return nil
}

func loopDetachConn(s *server, l *loop, c *conn, err error) error {
	if s.events.OnDetached == nil {
		return loopCloseConn(s, l, c, err)
	}
	l.poll.ModDetach(c.fd)

	delete(l.fdconns, c.fd)
	if err := unix.SetNonblock(c.fd, false); err != nil {
		return err
	}
	switch s.events.OnDetached(c, &detachedConn{fd: c.fd}) {
	case None:
	case Shutdown:
		return errClosing
	}
	return nil
}

func loopNote(s *server, l *loop, note interface{}) error {
	var err error
	switch v := note.(type) {
	case time.Duration:
		delay, action := s.events.Tick()
		switch action {
		case None:
		case Shutdown:
			err = errClosing
		}
		s.tch <- delay
	case error: // shutdown
		err = v
	case *conn:
		// Wake called for connection
		if l.fdconns[v.fd] != v {
			return nil // ignore stale wakes
		}
		return loopWake(s, l, v)
	case *signal:
		l.fdconns[v.fd] = v.c
		l.poll.AddReadWrite(v.fd)
		fmt.Printf("trigger loop: %d with c: %d\n", l.idx, v.fd)
	}
	return err
}

func loopRun(s *server, l *loop) {
	defer func() {
		s.signalShutdown()
		s.wg.Done()
	}()

	if l.idx == 0 && s.events.Tick != nil {
		go loopTicker(s, l)
	}

	_ = l.poll.Polling(func(fd int, note interface{}) error {
		if fd == 0 {
			return loopNote(s, l, note)
		}
		c := l.fdconns[fd]
		switch {
		case c == nil:
			return loopAccept(s, l, fd)
		case !c.opened:
			return loopOpened(s, l, c)
		case c.outBuf.Length() > 0:
			return loopWrite(s, l, c)
		case c.action != None:
			return loopAction(s, l, c)
		default:
			return loopRead(s, l, c)
		}
	})
}

func loopTicker(s *server, l *loop) {
	for {
		if err := l.poll.Trigger(time.Duration(0)); err != nil {
			break
		}
		time.Sleep(<-s.tch)
	}
}

func loopAccept(s *server, l *loop, fd int) error {
	for i, ln := range s.lns {
		if ln.fd == fd {
			if ln.pconn != nil {
				return loopUDPRead(s, l, i, fd)
			}
			nfd, sa, err := unix.Accept(fd)
			if err != nil {
				if err == unix.EAGAIN {
					return nil
				}
				return err
			}
			if err := unix.SetNonblock(nfd, true); err != nil {
				return err
			}
			c := &conn{fd: nfd,
				sa:     sa,
				lnidx:  i,
				inBuf:  ringbuffer.New(RingBufferSize),
				outBuf: ringbuffer.New(RingBufferSize),
				loop:   l,
			}
			l.fdconns[c.fd] = c
			l.poll.AddReadWrite(c.fd)
			return nil
		}
	}
	return nil
}

func loopUDPRead(s *server, l *loop, lnidx, fd int) error {
	n, sa, err := unix.Recvfrom(fd, l.packet, 0)
	if err != nil || n == 0 {
		return nil
	}
	if s.events.React != nil {
		var sa6 unix.SockaddrInet6
		switch sa := sa.(type) {
		case *unix.SockaddrInet4:
			sa6.ZoneId = 0
			sa6.Port = sa.Port
			for i := 0; i < 12; i++ {
				sa6.Addr[i] = 0
			}
			sa6.Addr[12] = sa.Addr[0]
			sa6.Addr[13] = sa.Addr[1]
			sa6.Addr[14] = sa.Addr[2]
			sa6.Addr[15] = sa.Addr[3]
		case *unix.SockaddrInet6:
			sa6 = *sa
		}
		c := &conn{
			addrIndex:  lnidx,
			localAddr:  s.lns[lnidx].lnaddr,
			remoteAddr: internal.SockaddrToAddr(&sa6),
			inBuf:      ringbuffer.New(RingBufferSize),
		}
		_, _ = c.inBuf.Write(l.packet[:n])
		out, action := s.events.React(c, c.inBuf)
		if len(out) > 0 {
			if s.events.PreWrite != nil {
				s.events.PreWrite()
			}
			sniffError(unix.Sendto(fd, out, 0, sa))
		}
		switch action {
		case Shutdown:
			return errClosing
		}
	}
	return nil
}

func loopOpened(s *server, l *loop, c *conn) error {
	c.opened = true
	c.addrIndex = c.lnidx
	c.localAddr = s.lns[c.lnidx].lnaddr
	c.remoteAddr = internal.SockaddrToAddr(c.sa)
	if s.events.OnOpened != nil {
		out, opts, action := s.events.OnOpened(c)
		c.action = action
		if opts.TCPKeepAlive > 0 {
			if _, ok := s.lns[c.lnidx].ln.(*net.TCPListener); ok {
				sniffError(internal.SetKeepAlive(c.fd, int(opts.TCPKeepAlive/time.Second)))
			}
		}

		if len(out) > 0 {
			fmt.Printf("c: %d, out length: %d in opened\n", c.fd, len(out))
			//_, _ = c.outBuf.Write(out)
			c.sendOut(out)
		}
	}
	fmt.Printf("c: %d, outBuf length: %d in opened\n", c.fd, c.outBuf.Length())
	if c.outBuf.Length() == 0 && c.action == None {
		l.poll.ModRead(c.fd)
	}
	return nil
}

func loopWrite(s *server, l *loop, c *conn) error {
	if s.events.PreWrite != nil {
		s.events.PreWrite()
	}
	out := c.outBuf.Bytes()
	n, err := unix.Write(c.fd, out)
	if err != nil {
		if err == unix.EAGAIN {
			return nil
		}
		fmt.Println("closing in write")
		return loopCloseConn(s, l, c, err)
	}
	c.outBuf.Move(n)
	ringbuffer.Recycle(out)
	//fmt.Printf("c: %d, writing data length: %d", c.fd, n)
	if c.outBuf.Length() == 0 && c.action == None {
		l.poll.ModRead(c.fd)
	}
	return nil
}

func loopAction(s *server, l *loop, c *conn) error {
	switch c.action {
	default:
		c.action = None
	case Close:
		return loopCloseConn(s, l, c, nil)
	case Shutdown:
		return errClosing
	case Detach:
		return loopDetachConn(s, l, c, nil)
	}
	if c.outBuf.Length() == 0 && c.action == None {
		l.poll.ModRead(c.fd)
	}
	return nil
}

func loopWake(s *server, l *loop, c *conn) error {
	if s.events.React == nil {
		return nil
	}
	out, action := s.events.React(c, c.inBuf)
	c.action = action
	if len(out) > 0 {
		//_, _ = c.outBuf.Write(out)
		c.sendOut(out)
	}
	if c.outBuf.Length() != 0 || c.action != None {
		l.poll.ModReadWrite(c.fd)
	}
	return nil
}

func loopRead(s *server, l *loop, c *conn) error {
	n, err := unix.Read(c.fd, l.packet)
	if n == 0 || err != nil {
		if err == unix.EAGAIN {
			return nil
		}
		fmt.Printf("closing in read, conn: %d, err: %v\n", c.fd, err)
		return loopCloseConn(s, l, c, err)
	}

	_, _ = c.inBuf.Write(l.packet[:n])
	if s.events.React != nil {
		out, action := s.events.React(c, c.inBuf)
		c.action = action
		if len(out) > 0 {
			//_, _ = c.outBuf.Write(out)
			c.sendOut(out)
		}
	}
	//fmt.Printf("c: %d, outBuf length %d in read", c.fd, c.outBuf.Length())
	if c.outBuf.Length() != 0 || c.action != None {
		l.poll.ModReadWrite(c.fd)
	}
	return nil
}
