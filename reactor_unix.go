// Copyright 2019 Andy Pan. All rights reserved.
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package gnet

import (
	"fmt"
	"time"

	"github.com/panjf2000/gnet/ringbuffer"
	"github.com/smartystreets-prototypes/go-disruptor"
	"golang.org/x/sys/unix"
)

const (
	RingBufferSize   = 1024
	RingBufferMask   = RingBufferSize - 1
	DisruptorCleanup = time.Millisecond * 10
)

var connRingBuffer = [RingBufferSize]*conn{}

type signal struct {
	fd int
	c  *conn
}

type eventConsumer struct {
	s *server
	l *loop
}

func (ec *eventConsumer) Consume(lower, upper int64) {
	fmt.Printf("consumer with loop: %d, consuming message, lower: %d, upper: %d\n", ec.l.idx, lower, upper)
	for ; lower <= upper; lower++ {
		c := connRingBuffer[lower&RingBufferMask]
		c.inBuf = ringbuffer.New(RingBufferSize)
		c.outBuf = ringbuffer.New(RingBufferSize)
		fmt.Printf("lower: %d, consuming fd: %d in loop: %d\n", lower, c.fd, ec.l.idx)
		c.loop = ec.l

		// Connections load balance under round-robin algorithm.
		if ec.s.numLoops > 1 {
			idx := int(lower) % ec.s.numLoops
			if idx != ec.l.idx {
				fmt.Printf("lower: %d, ignoring fd: %d in loop: %d\n", lower, c.fd, ec.l.idx)
				// Don't match the round-robin rule, ignore this connection.
				continue
			}
		}
		fmt.Printf("lower: %d, send fd: %d to loop: %d\n", lower, c.fd, ec.l.idx)
		_ = ec.l.poll.Trigger(&signal{fd: c.fd, c: c})
	}
}

func activateMainReactor(s *server) {
	defer func() {
		time.Sleep(DisruptorCleanup)
		s.signalShutdown()
		s.wg.Done()
	}()

	eventConsumers := make([]disruptor.Consumer, 0, s.numLoops)
	for _, l := range s.loops {
		ec := &eventConsumer{s, l}
		eventConsumers = append(eventConsumers, ec)
	}
	fmt.Printf("length of loops: %d and consumers: %d\n", s.numLoops, len(eventConsumers))

	// Initialize go-disruptor with ring-buffer for dispatching events to loops.
	controller := disruptor.Configure(RingBufferSize).WithConsumerGroup(eventConsumers...).Build()

	controller.Start()
	defer controller.Stop()

	writer := controller.Writer()
	sequence := disruptor.InitialSequenceValue

	fmt.Println("main reactor polling...")
	_ = s.mainLoop.poll.Polling(func(fd int, note interface{}) error {
		if fd == 0 {
			return loopNote(s, s.mainLoop, note)
		}

		for i, ln := range s.lns {
			if ln.fd == fd {
				if ln.pconn != nil {
					return loopUDPRead(s, s.mainLoop, i, fd)
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
				c := &conn{fd: nfd, sa: sa, lnidx: i}
				fmt.Printf("accepted fd: %d in main reactor\n", nfd)
				sequence = writer.Reserve(1)
				connRingBuffer[sequence&RingBufferMask] = c
				writer.Commit(sequence, sequence)
				return nil
			}
		}
		return nil
	})
}

func activateSubReactor(s *server, l *loop) {
	defer func() {
		s.signalShutdown()
		s.wg.Done()
	}()

	if l.idx == 0 && s.events.Tick != nil {
		fmt.Println("start ticker...")
		go loopTicker(s, l)
	}

	fmt.Printf("sub reactor polling, loop: %d\n", l.idx)
	_ = l.poll.Polling(func(fd int, note interface{}) error {
		if fd == 0 {
			return loopNote(s, l, note)
		}

		c := l.fdconns[fd]
		if c == nil {
			fmt.Printf("c: %d not in loop: %d\n", fd, l.idx)
		}
		switch {
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
