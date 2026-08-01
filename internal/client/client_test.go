package client

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"testing"

	"github.com/stayradiated/mosscap/internal/protocol"
)

func TestHelloThenCall(t *testing.T) {
	server, conn := net.Pipe()
	defer server.Close()
	defer conn.Close()
	c := &Client{conn: conn, codec: protocol.NewCodec(conn, conn, 1024)}
	go func() {
		codec := protocol.NewCodec(server, server, 1024)
		for i := 0; i < 2; i++ {
			payload, err := codec.ReadFrame()
			if err != nil {
				return
			}
			req, err := protocol.DecodeRequest(payload)
			if err != nil {
				return
			}
			var result []byte
			resultType := "gateway_status"
			if i == 0 {
				result, _ = json.Marshal(protocol.HelloResult{SelectedVersion: protocol.Version, Capabilities: []string{protocol.CapabilityStatus}})
				resultType = "hello"
			} else {
				result = []byte(`{}`)
			}
			payload, _ = protocol.EncodeResponse(protocol.Response{ProtocolVersion: protocol.Version, RequestID: req.RequestID, ServerVersion: "test", ResultType: resultType, Result: result})
			if codec.WriteFrame(payload) != nil {
				return
			}
		}
	}()
	if _, err := c.Call(context.Background(), request("status.get", "req_beforehello")); err == nil {
		t.Fatal("call before hello succeeded")
	}
	if _, err := c.Hello(context.Background(), "req_hello", "cli", "test", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Call(context.Background(), request("status.get", "req_status")); err != nil {
		t.Fatal(err)
	}
}
func TestHelloReturnsTypedUnsupportedVersion(t *testing.T) {
	server, conn := net.Pipe()
	defer server.Close()
	defer conn.Close()
	c := &Client{conn: conn, codec: protocol.NewCodec(conn, conn, 1024)}
	go func() {
		codec := protocol.NewCodec(server, server, 1024)
		payload, err := codec.ReadFrame()
		if err != nil {
			return
		}
		req, err := protocol.DecodeRequest(payload)
		if err != nil {
			return
		}
		out, _ := protocol.EncodeResponse(protocol.NewErrorResponse(req.RequestID, "test", "unsupported_protocol_version", "unsupported", "protocol version is unsupported", false))
		_ = codec.WriteFrame(out)
	}()
	_, err := c.Hello(context.Background(), "req_hello", "cli", "test", nil)
	var remote *RemoteError
	if !errors.As(err, &remote) || remote.ProtocolError.Category != "unsupported" {
		t.Fatalf("Hello error = %#v", err)
	}
}

func request(command, id string) protocol.Request {
	return protocol.Request{ProtocolVersion: protocol.Version, RequestID: id, ClientKind: "cli", ClientVersion: "test", Command: command, Params: []byte(`{}`)}
}
