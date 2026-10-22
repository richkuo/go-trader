package notifybroker

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"trading-scheduler/internal/egresspolicy"
)

var ErrBrokerUnavailable = errors.New("notification broker unavailable")

type EventHandler func(kind string, raw json.RawMessage)

type Client struct {
	socket    string
	brokerUID int
	events    []string
	onEvent   EventHandler

	mu      sync.Mutex
	conn    net.Conn
	nextID  uint64
	pending map[uint64]chan *Frame
	hello   HelloResponse
	closed  bool
	wmu     sync.Mutex
}

func NewClient(socket string, brokerUID int, events []string, onEvent EventHandler) *Client {
	return &Client{
		socket:    socket,
		brokerUID: brokerUID,
		events:    append([]string(nil), events...),
		onEvent:   onEvent,
		pending:   map[uint64]chan *Frame{},
	}
}

func (c *Client) Start(ctx context.Context) error {
	err := c.connect(ctx)
	go c.maintain()
	return err
}

func (c *Client) RefreshHello(ctx context.Context) (HelloResponse, error) {
	var hello HelloResponse
	if err := c.Call(ctx, OpHello, HelloRequest{}, &hello); err != nil {
		return c.Hello(), err
	}
	c.mu.Lock()
	c.hello = hello
	c.mu.Unlock()
	return hello, nil
}

func (c *Client) Connected() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conn != nil
}

func (c *Client) Hello() HelloResponse {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.hello
}

func (c *Client) Close() {
	c.mu.Lock()
	c.closed = true
	conn := c.conn
	c.mu.Unlock()
	if conn != nil {
		_ = conn.Close()
	}
}

func (c *Client) connect(ctx context.Context) error {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", c.socket)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrBrokerUnavailable, err)
	}
	cred, err := egresspolicy.PeerCredentials(conn)
	if err != nil || cred.UID != c.brokerUID {
		conn.Close()
		if err == nil {
			err = fmt.Errorf("the broker socket is served by uid %d, not the broker uid %d", cred.UID, c.brokerUID)
		}
		return fmt.Errorf("%w: %v", ErrBrokerUnavailable, err)
	}
	c.mu.Lock()
	c.conn = conn
	c.mu.Unlock()
	go c.readLoop(conn)
	var hello HelloResponse
	if err := c.Call(ctx, OpHello, HelloRequest{Events: c.events}, &hello); err != nil {
		conn.Close()
		return err
	}
	c.mu.Lock()
	c.hello = hello
	c.mu.Unlock()
	return nil
}

func (c *Client) maintain() {
	backoff := time.Second
	for {
		c.mu.Lock()
		conn := c.conn
		closed := c.closed
		c.mu.Unlock()
		if closed {
			return
		}
		if conn != nil {
			time.Sleep(time.Second)
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		err := c.connect(ctx)
		cancel()
		if err == nil {
			backoff = time.Second
			continue
		}
		time.Sleep(backoff)
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

func (c *Client) readLoop(conn net.Conn) {
	r := bufio.NewReaderSize(conn, 64<<10)
	for {
		f, err := readFrame(r)
		if err != nil {
			break
		}
		switch f.Type {
		case FrameResponse:
			c.mu.Lock()
			ch := c.pending[f.ID]
			delete(c.pending, f.ID)
			c.mu.Unlock()
			if ch != nil {
				ch <- f
			}
		case FrameEvent:
			if f.ID != 0 || c.onEvent == nil {
				continue
			}
			subscribed := false
			for _, k := range c.events {
				if k == f.Op {
					subscribed = true
				}
			}
			if subscribed {
				go c.onEvent(f.Op, f.Payload)
			}
		}
	}
	conn.Close()
	c.mu.Lock()
	if c.conn == conn {
		c.conn = nil
	}
	pending := c.pending
	c.pending = map[uint64]chan *Frame{}
	c.mu.Unlock()
	for _, ch := range pending {
		ch <- &Frame{Type: FrameResponse, Error: "the notification broker connection closed"}
	}
}

func (c *Client) Call(ctx context.Context, op string, req any, out any) error {
	payload, err := json.Marshal(req)
	if err != nil {
		return err
	}
	c.mu.Lock()
	conn := c.conn
	if conn == nil {
		c.mu.Unlock()
		return fmt.Errorf("%w: not connected", ErrBrokerUnavailable)
	}
	c.nextID++
	id := c.nextID
	ch := make(chan *Frame, 1)
	c.pending[id] = ch
	c.mu.Unlock()
	data, err := encodeFrame(&Frame{ID: id, Type: FrameRequest, Op: op, Payload: payload})
	if err != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return err
	}
	c.wmu.Lock()
	_ = conn.SetWriteDeadline(time.Now().Add(requestTimeout))
	_, err = conn.Write(data)
	c.wmu.Unlock()
	if err != nil {
		conn.Close()
		return fmt.Errorf("%w: %v", ErrBrokerUnavailable, err)
	}
	select {
	case f := <-ch:
		if f.Error != "" {
			return errors.New(f.Error)
		}
		if out != nil && len(f.Payload) > 0 {
			return json.Unmarshal(f.Payload, out)
		}
		return nil
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return ctx.Err()
	}
}
