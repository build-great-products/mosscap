package protocol

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"testing"
)

func validHello() Request {
	return Request{ProtocolVersion: Version, RequestID: "req_abcdefghijklmnopqrstuv", ClientKind: "cli", ClientVersion: "test", Command: "hello", Params: []byte(`{"supported_versions":[1],"capabilities":["status"]}`)}
}
func TestRequestGolden(t *testing.T) {
	got, err := EncodeRequest(validHello())
	if err != nil {
		t.Fatal(err)
	}
	want := `{"protocol_version":1,"request_id":"req_abcdefghijklmnopqrstuv","client_kind":"cli","client_version":"test","command":"hello","params":{"supported_versions":[1],"capabilities":["status"]}}`
	if string(got) != want {
		t.Fatalf("\ngot: %s\nwant: %s", got, want)
	}
}
func TestResponseGolden(t *testing.T) {
	r := Response{ProtocolVersion: Version, RequestID: "req_abcdefghijklmnopqrstuv", ServerVersion: "test", ResultType: "hello", Result: []byte(`{"selected_version":1,"capabilities":["status"]}`)}
	got, err := EncodeResponse(r)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"protocol_version":1,"request_id":"req_abcdefghijklmnopqrstuv","server_version":"test","result_type":"hello","result":{"selected_version":1,"capabilities":["status"]}}`
	if string(got) != want {
		t.Fatalf("\ngot: %s\nwant: %s", got, want)
	}
}
func TestFrameFragmentedAndMultiple(t *testing.T) {
	var wire bytes.Buffer
	codec := NewCodec(&wire, &wire, 64)
	if err := codec.WriteFrame([]byte(`{"one":1}`)); err != nil {
		t.Fatal(err)
	}
	if err := codec.WriteFrame([]byte(`{"two":2}`)); err != nil {
		t.Fatal(err)
	}
	fragmented := &oneByteReader{r: bytes.NewReader(wire.Bytes())}
	read := NewCodec(fragmented, io.Discard, 64)
	for _, want := range []string{`{"one":1}`, `{"two":2}`} {
		got, err := read.ReadFrame()
		if err != nil || string(got) != want {
			t.Fatalf("got %q, %v", got, err)
		}
	}
}
func TestFrameRejectsBadLengthsAndUTF8(t *testing.T) {
	for _, tc := range []struct {
		name string
		data []byte
		want error
	}{
		{"zero", []byte{0, 0, 0, 0}, ErrEmptyFrame},
		{"large", []byte{0, 0, 0, 65}, ErrFrameTooLarge},
		{"utf8", append([]byte{0, 0, 0, 1}, 0xff), ErrInvalidUTF8},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewCodec(bytes.NewReader(tc.data), io.Discard, 64).ReadFrame()
			if !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
		})
	}
}
func TestStrictDecode(t *testing.T) {
	cases := [][]byte{
		[]byte(`{"protocol_version":1,"request_id":"req_abcdefghijklmnopqrstuv","client_kind":"cli","client_version":"x","command":"hello","params":{"supported_versions":[2],"capabilities":[]}}`),
		[]byte(`{"protocol_version":1,"request_id":"req_abcdefghijklmnopqrstuv","client_kind":"cli","client_version":"x","command":"hello","params":{"supported_versions":[1],"capabilities":[]},"unknown":1}`),
		[]byte(`{"protocol_version":1,"request_id":"req_abcdefghijklmnopqrstuv","client_kind":"cli","client_version":"x","command":"hello","params":{"supported_versions":[1],"capabilities":[]}} {}`),
		[]byte(`{"protocol_version":1,"request_id":"req_abcdefghijklmnopqrstuv","client_kind":"cli","client_version":"x","command":"hello","params":null}`),
	}
	for _, payload := range cases {
		if _, err := DecodeRequest(payload); err == nil {
			t.Fatalf("accepted %s", payload)
		}
	}
}
func FuzzDecodeRequest(f *testing.F) {
	f.Add([]byte(`{"protocol_version":1,"request_id":"req_abcdefghijklmnopqrstuv","client_kind":"cli","client_version":"x","command":"hello","params":{"supported_versions":[1],"capabilities":[]}}`))
	f.Fuzz(func(t *testing.T, payload []byte) { _, _ = DecodeRequest(payload) })
}
func FuzzFrameRead(f *testing.F) {
	seed := make([]byte, 4)
	binary.BigEndian.PutUint32(seed, 2)
	seed = append(seed, '{', '}')
	f.Add(seed)
	f.Fuzz(func(t *testing.T, wire []byte) { _, _ = NewCodec(bytes.NewReader(wire), io.Discard, 1024).ReadFrame() })
}

type oneByteReader struct{ r io.Reader }

func (r *oneByteReader) Read(p []byte) (int, error) {
	if len(p) > 1 {
		p = p[:1]
	}
	return r.r.Read(p)
}
