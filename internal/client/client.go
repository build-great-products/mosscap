// Package client provides the local protocol v1 client transport.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"sync"
	"time"

	"github.com/stayradiated/mosscap/internal/protocol"
)

const DefaultMaxFrameBytes = 1048576

type Client struct {
	conn         net.Conn
	codec        protocol.Codec
	mu           sync.Mutex
	hello        bool
	helloPending bool
}

// Dial connects to a private Unix-domain endpoint. The caller must Hello before Call.
func Dial(ctx context.Context, endpoint string, maxFrameBytes uint32) (*Client, error) {
	if maxFrameBytes == 0 {
		maxFrameBytes = DefaultMaxFrameBytes
	}
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", endpoint)
	if err != nil {
		return nil, err
	}
	return &Client{conn: conn, codec: protocol.NewCodec(conn, conn, maxFrameBytes)}, nil
}
func (c *Client) Close() error {
	if c == nil || c.conn == nil {
		return nil
	}
	return c.conn.Close()
}

// Hello negotiates protocol version and capabilities. It may only be called once.
func (c *Client) Hello(ctx context.Context, requestID, clientKind, clientVersion string, capabilities []string) (protocol.HelloResult, error) {
	var result protocol.HelloResult
	if c == nil {
		return result, errors.New("client: closed")
	}
	c.mu.Lock()
	if c.hello || c.helloPending {
		c.mu.Unlock()
		return result, errors.New("client: hello already completed")
	}
	c.helloPending = true
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.helloPending = false
		c.mu.Unlock()
	}()
	params, err := json.Marshal(protocol.HelloParams{SupportedVersions: []int{protocol.Version}, Capabilities: capabilities})
	if err != nil {
		return result, err
	}
	response, err := c.exchange(ctx, protocol.Request{ProtocolVersion: protocol.Version, RequestID: requestID, ClientKind: clientKind, ClientVersion: clientVersion, Command: "hello", Params: params}, false)
	if err != nil {
		return result, err
	}
	if response.Error != nil {
		return result, &RemoteError{ProtocolError: *response.Error}
	}
	if response.ResultType != "hello" {
		return result, errors.New("client: hello returned unexpected result type")
	}
	if err := decodeResult(response.Result, &result); err != nil {
		return result, err
	}
	if result.SelectedVersion != protocol.Version {
		return result, errors.New("client: server selected unsupported protocol version")
	}
	c.mu.Lock()
	c.hello = true
	c.mu.Unlock()
	return result, nil
}

// Call sends one ordered request after a successful Hello and returns its typed response.
func (c *Client) Call(ctx context.Context, request protocol.Request) (protocol.Response, error) {
	return c.exchange(ctx, request, true)
}
func (c *Client) exchange(ctx context.Context, request protocol.Request, needHello bool) (protocol.Response, error) {
	var zero protocol.Response
	if c == nil || c.conn == nil {
		return zero, errors.New("client: closed")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if needHello && !c.hello {
		return zero, errors.New("client: hello is required")
	}
	if !needHello && c.hello {
		return zero, errors.New("client: hello already completed")
	}
	payload, err := protocol.EncodeRequest(request)
	if err != nil {
		return zero, err
	}
	if err := c.setDeadline(ctx); err != nil {
		return zero, err
	}
	defer c.conn.SetDeadline(time.Time{})
	if err := c.codec.WriteFrame(payload); err != nil {
		return zero, err
	}
	payload, err = c.codec.ReadFrame()
	if err != nil {
		return zero, err
	}
	response, err := protocol.DecodeResponse(payload)
	if err != nil {
		return zero, err
	}
	if response.RequestID != request.RequestID {
		return zero, errors.New("client: response request_id mismatch")
	}
	return response, nil
}
func (c *Client) setDeadline(ctx context.Context) error {
	if deadline, ok := ctx.Deadline(); ok {
		return c.conn.SetDeadline(deadline)
	}
	return c.conn.SetDeadline(time.Now().Add(10 * time.Second))
}
func decodeResult(raw json.RawMessage, target any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	return dec.Decode(target)
}

type RemoteError struct{ ProtocolError protocol.Error }

func (e *RemoteError) Error() string { return e.ProtocolError.Code + ": " + e.ProtocolError.Message }
