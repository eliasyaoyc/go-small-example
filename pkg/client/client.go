package client

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
	"toy-rpc/pkg"
	"toy-rpc/pkg/codec"
)

type Call struct {
	Sequence      uint64
	ServiceMethod string
	Args          interface{}
	Reply         interface{}
	Error         error
	Done          chan *Call
}

type Client struct {
	cc       codec.Codec
	opt      *pkg.Option
	sending  sync.Mutex
	header   codec.Header
	mu       sync.Mutex
	sequence uint64
	pending  map[uint64]*Call
	closing  bool
	shutdown bool
}

type clientResult struct {
	client *Client
	err    error
}

var _ io.Closer = (*Client)(nil)

var ErrShutdown = errors.New("Connection is shut down")

type newClientFunc func(conn net.Conn, opt *pkg.Option) (client *Client, err error)

func (call *Call) done() {
	call.Done <- call
}

func NewClient(conn net.Conn, opt *pkg.Option) (*Client, error) {
	f := codec.NewCodecFuncMap[opt.CodecType]
	if f == nil {
		err := fmt.Errorf("invalid codec type %s", opt.CodecType)
		log.Println("rpc client: codec error:", err)
		return nil, err
	}
	if err := json.NewEncoder(conn).Encode(opt); err != nil {
		log.Println("rpc client: options error: ", err)
		_ = conn.Close()
		return nil, err
	}
	return newClientCodec(f(conn), opt), nil
}

func newClientCodec(codec codec.Codec, opt *pkg.Option) *Client {
	client := &Client{
		cc:       codec,
		opt:      opt,
		pending:  make(map[uint64]*Call),
		sequence: 1,
	}
	go client.receive()
	return client
}

func parseOptions(opts ...*pkg.Option) (*pkg.Option, error) {
	if len(opts) == 0 || opts[0] == nil {
		return pkg.DefaultOption, nil
	}

	if len(opts) != 1 {
		return nil, errors.New("number of options is more than 1")
	}
	opt := opts[0]
	opt.MagicNumber = pkg.DefaultOption.MagicNumber
	if opt.CodecType == "" {
		opt.CodecType = pkg.DefaultOption.CodecType
	}
	return opt, nil
}

func Dial(network, address string, opts ...*pkg.Option) (client *Client, err error) {
	//opt, err := parseOptions(opts...)
	//if err != nil {
	//	return nil, err
	//}
	//conn, err := net.Dial(network, address)
	//if err != nil {
	//	return nil, err
	//}
	//defer func() {
	//	if client == nil {
	//		_ = conn.Close()
	//	}
	//}()
	//return NewClient(conn, opt)
	return dialTimeout(NewClient, network, address, opts...)
}

func dialTimeout(f newClientFunc, network, address string, opts ...*pkg.Option) (client *Client, err error) {
	opt, err := parseOptions(opts...)
	if err != nil {
		return nil, err
	}
	conn, err := net.DialTimeout(network, address, opt.ConnectTimeout)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			_ = conn.Close()
		}
	}()

	ch := make(chan clientResult)
	go func() {
		client, err := f(conn, opt)
		ch <- clientResult{
			client: client,
			err:    err,
		}
	}()
	if opt.ConnectTimeout == 0 {
		result := <-ch
		return result.client, result.err
	}
	select {
	case <-time.After(opt.ConnectTimeout):
		return nil, fmt.Errorf("rpc client: connect timeout: expect within %s", opt.ConnectTimeout)
	case result := <-ch:
		return result.client, result.err
	}
}

func (client *Client) send(call *Call) {
	client.sending.Lock()
	defer client.sending.Unlock()

	seq, err := client.registerCall(call)
	if err != nil {
		call.Error = err
		call.done()
		return
	}

	client.header.ServiceMethod = call.ServiceMethod
	client.header.Sequence = call.Sequence
	client.header.Error = ""

	if err := client.cc.Write(&client.header, call.Args); err != nil {
		call := client.removeCall(seq)
		if call != nil {
			call.Error = err
			call.done()
		}
	}
}

func (client *Client) Go(serviceMethod string, args, reply interface{}, done chan *Call) *Call {
	if done == nil {
		done = make(chan *Call, 10)
	} else if cap(done) == 0 {
		log.Panic("rpc client: done channel is unbuffered")
	}
	call := &Call{
		ServiceMethod: serviceMethod,
		Args:          args,
		Reply:         reply,
		Done:          done,
	}
	client.send(call)
	return call
}

func (client *Client) Call(ctx context.Context, serviceMethod string, args, reply interface{}) error {
	call := client.Go(serviceMethod, args, reply, make(chan *Call, 1))
	select {
	case <-ctx.Done():
		client.removeCall(call.Sequence)
		return errors.New("rpc client: call failed: " + ctx.Err().Error())
	case call := <-call.Done:
		return call.Error
	}
}

func (client Client) Close() error {
	client.mu.Lock()
	defer client.mu.Unlock()

	if client.closing {
		return ErrShutdown
	}
	client.closing = true
	return client.cc.Close()
}

func (client *Client) IsAvailable() bool {
	client.mu.Lock()
	defer client.mu.Unlock()
	return !client.shutdown && !client.closing
}

func (client *Client) registerCall(call *Call) (uint64, error) {
	client.mu.Lock()
	defer client.mu.Unlock()

	if client.closing || client.shutdown {
		return 0, ErrShutdown
	}

	call.Sequence = client.sequence
	client.pending[call.Sequence] = call
	client.sequence++
	return call.Sequence, nil
}

func (client *Client) removeCall(sequence uint64) *Call {
	client.mu.Lock()
	defer client.mu.Unlock()

	call := client.pending[sequence]
	delete(client.pending, sequence)
	return call
}

func (client *Client) terminateCalls(err error) {
	client.sending.Lock()
	defer client.sending.Unlock()

	client.mu.Lock()
	defer client.mu.Lock()
	client.shutdown = true
	for _, call := range client.pending {
		call.Error = err
		call.done()
	}
}

func (client *Client) receive() {
	var err error
	for err == nil {
		var h codec.Header
		if err = client.cc.ReadHeader(&h); err != nil {
			break
		}
		call := client.removeCall(h.Sequence)
		switch {
		case call == nil:
			err = client.cc.ReadBody(nil)
		case h.Error != "":
			call.Error = fmt.Errorf(h.Error)
			err = client.cc.ReadBody(nil)
			call.done()
		default:
			if err = client.cc.ReadBody(call.Reply); err != nil {
				call.Error = errors.New("reading body " + err.Error())
			}
			call.done()
		}
	}
	client.terminateCalls(err)
}

func NewHTTPClient(conn net.Conn, opt *pkg.Option) (*Client, error) {
	_, _ = io.WriteString(conn, fmt.Sprintf("CONNECT &s HTTP/1.0\n\n", pkg.DefaultRPCPath))

	response, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: "CONNECT"})
	if err != nil && response.Status == pkg.Connected {
		return NewClient(conn, opt)
	}
	if err != nil {
		err = errors.New("unexpected HTTP response: " + response.Status)
	}
	return nil, err
}

func DialHTTP(network, address string, opts ...*pkg.Option) (*Client, error) {
	return dialTimeout(NewHTTPClient, network, address, opts...)
}

func XDial(rpcAddr string, opts ...*pkg.Option) (*Client, error) {
	parts := strings.Split(rpcAddr, "@")
	if len(parts) != 2 {
		return nil, fmt.Errorf("rpc client err: wrong format '%s', expect protocol@addr", rpcAddr)
	}
	protocol, addr := parts[0], parts[1]
	switch protocol {
	case "http":
		return DialHTTP("tcp", addr, opts...)
	default:
		// tcp, unix or other transport protocol
		return Dial(protocol, addr, opts...)
	}
}
