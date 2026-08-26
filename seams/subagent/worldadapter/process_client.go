package worldadapter

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"

	"github.com/Eastsidegunn/JANUS/core/world"
	"github.com/Eastsidegunn/JANUS/core/world/processwire"
)

type processClient struct {
	control       net.Conn
	output        net.Conn
	enc           *processwire.Encoder
	outputDecoder *processwire.Decoder

	mu           sync.Mutex
	pending      map[uint64]chan error
	exit         processwire.ExitObserved
	exitSet      bool
	outputClosed bool
	err          error
	done         chan struct{}
	once         sync.Once
}

func connectProcess(ctx context.Context, endpoint world.ProcessEndpoint, spanID string) (*processClient, error) {
	if endpoint.Network() != "unix" || endpoint.Address() == "" || endpoint.LeaseID() == "" ||
		endpoint.ControlCapability() == "" || endpoint.OutputCapability() == "" || spanID == "" {
		return nil, fmt.Errorf("worldadapter: process endpoint 계약 위반")
	}
	output, _, outputDecoder, err := connectProcessRole(ctx, endpoint, spanID, processwire.RoleOutput, endpoint.OutputCapability())
	if err != nil {
		return nil, err
	}
	control, controlEncoder, controlDecoder, err := connectProcessRole(ctx, endpoint, spanID, processwire.RoleControl, endpoint.ControlCapability())
	if err != nil {
		output.Close()
		return nil, err
	}
	c := &processClient{
		control: control, output: output, enc: controlEncoder, outputDecoder: outputDecoder,
		pending: map[uint64]chan error{}, done: make(chan struct{}),
	}
	go c.readControl(controlDecoder)
	return c, nil
}

func connectProcessRole(ctx context.Context, endpoint world.ProcessEndpoint, spanID string, role processwire.Role, capability string) (net.Conn, *processwire.Encoder, *processwire.Decoder, error) {
	dialer := net.Dialer{}
	conn, err := dialer.DialContext(ctx, endpoint.Network(), endpoint.Address())
	if err != nil {
		return nil, nil, nil, fmt.Errorf("worldadapter: process %s connect: %w", role, err)
	}
	enc, dec := processwire.NewEncoder(conn), processwire.NewDecoder(conn)
	payload, err := processwire.Marshal(processwire.Hello{
		Version: processwire.Version, Role: role, LeaseID: endpoint.LeaseID(),
		SpanID: spanID, Capability: capability,
	})
	if err == nil {
		_, err = enc.Write(processwire.KindHello, processwire.StreamControl, 0, payload)
	}
	if err != nil {
		conn.Close()
		return nil, nil, nil, err
	}
	frame, err := dec.Read()
	if err != nil || frame.Kind != processwire.KindHelloAck {
		conn.Close()
		return nil, nil, nil, fmt.Errorf("worldadapter: process %s hello ACK: kind=%d err=%w", role, frame.Kind, err)
	}
	return conn, enc, dec, nil
}

func (c *processClient) request(ctx context.Context, kind processwire.Kind, payload []byte) error {
	response := make(chan error, 1)
	c.mu.Lock()
	if c.err != nil {
		err := c.err
		c.mu.Unlock()
		return err
	}
	seq, err := c.enc.Write(kind, processwire.StreamControl, 0, payload)
	if err == nil {
		c.pending[seq] = response
	}
	c.mu.Unlock()
	if err != nil {
		return err
	}
	select {
	case err := <-response:
		return err
	case <-c.done:
		return c.failure()
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *processClient) readControl(dec *processwire.Decoder) {
	for {
		frame, err := dec.Read()
		if err != nil {
			c.fail(err)
			return
		}
		switch frame.Kind {
		case processwire.KindAck:
			var ack processwire.Ack
			if err := processwire.Unmarshal(frame.Payload, &ack); err != nil {
				c.fail(err)
				return
			}
			c.mu.Lock()
			waiter := c.pending[ack.RequestSeq]
			delete(c.pending, ack.RequestSeq)
			c.mu.Unlock()
			if waiter == nil {
				c.fail(fmt.Errorf("worldadapter: 상관되지 않은 process ACK %d", ack.RequestSeq))
				return
			}
			waiter <- nil
		case processwire.KindError:
			var wireErr processwire.WireError
			if err := processwire.Unmarshal(frame.Payload, &wireErr); err != nil {
				c.fail(err)
				return
			}
			c.fail(fmt.Errorf("worldadapter: process broker error: %s", wireErr.Reason))
			return
		case processwire.KindExitObserved:
			var observed processwire.ExitObserved
			if err := processwire.Unmarshal(frame.Payload, &observed); err != nil {
				c.fail(err)
				return
			}
			c.mu.Lock()
			if c.exitSet {
				c.mu.Unlock()
				c.fail(fmt.Errorf("worldadapter: exit_observed 중복"))
				return
			}
			c.exit, c.exitSet = observed, true
			c.mu.Unlock()
			c.once.Do(func() { close(c.done) })
		default:
			c.fail(fmt.Errorf("worldadapter: 미지 control frame kind=%d", frame.Kind))
			return
		}
	}
}

func (c *processClient) Start(ctx context.Context, taskLine []byte) error {
	if err := c.request(ctx, processwire.KindStart, nil); err != nil {
		return err
	}
	line := append(append([]byte(nil), taskLine...), '\n')
	if err := c.request(ctx, processwire.KindStdinData, line); err != nil {
		return err
	}
	return c.request(ctx, processwire.KindWait, nil)
}

func (c *processClient) SendLine(ctx context.Context, line []byte) error {
	data := append(append([]byte(nil), line...), '\n')
	return c.request(ctx, processwire.KindStdinData, data)
}

func (c *processClient) Stop(ctx context.Context, reason string) error {
	payload, err := processwire.Marshal(processwire.Stop{Reason: reason})
	if err != nil {
		return err
	}
	if err := c.request(ctx, processwire.KindStop, payload); err != nil {
		return err
	}
	return nil
}

func (c *processClient) Wait(ctx context.Context) (processwire.ExitObserved, error) {
	select {
	case <-c.done:
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.err != nil {
			return processwire.ExitObserved{}, c.err
		}
		if !c.exitSet {
			return processwire.ExitObserved{}, errors.New("worldadapter: exit 결과 없음")
		}
		return c.exit, nil
	case <-ctx.Done():
		return processwire.ExitObserved{}, ctx.Err()
	}
}

func (c *processClient) DrainOutput(handler func(processwire.Frame) error) error {
	for {
		frame, err := c.outputDecoder.Read()
		if err != nil {
			return err
		}
		if err := handler(frame); err != nil {
			return err
		}
		if frame.Kind == processwire.KindStreamEnd {
			return nil
		}
	}
}

func (c *processClient) fail(err error) {
	if err == nil {
		return
	}
	c.mu.Lock()
	if c.err == nil {
		c.err = err
	}
	for seq, waiter := range c.pending {
		waiter <- c.err
		delete(c.pending, seq)
	}
	c.mu.Unlock()
	c.once.Do(func() { close(c.done) })
}

func (c *processClient) failure() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return c.err
	}
	return io.EOF
}

func (c *processClient) Close() {
	_ = c.control.Close()
	c.CloseOutput()
}

func (c *processClient) CloseOutput() {
	c.mu.Lock()
	if c.outputClosed {
		c.mu.Unlock()
		return
	}
	c.outputClosed = true
	output := c.output
	c.mu.Unlock()
	if output != nil {
		_ = output.Close()
	}
}

func (c *processClient) OutputClosed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.outputClosed
}
