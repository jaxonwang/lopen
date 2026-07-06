package protocol

import (
	"bufio"
	"bytes"
	"io"
	"strings"
	"testing"
)

func validReq() *Request {
	return &Request{
		V:      Version,
		Op:     OpOpen,
		Origin: Origin{Host: "h.example.com", Label: "h"},
		Path:   "/home/u/file.pdf",
		Mode:   ModeInline,
		Size:   100,
	}
}

func TestValidateAccepts(t *testing.T) {
	if err := validReq().Validate(DefaultMaxPayload); err != nil {
		t.Fatal(err)
	}
}

func TestValidateRejects(t *testing.T) {
	cases := []func(*Request){
		func(r *Request) { r.V = 99 },
		func(r *Request) { r.Op = "exec" },
		func(r *Request) { r.Mode = "shell" },
		func(r *Request) { r.Origin.Label = "../etc" },
		func(r *Request) { r.Origin.Label = "-flag" },
		func(r *Request) { r.Origin.Label = ".hidden" },
		func(r *Request) { r.Origin.Label = "" },
		func(r *Request) { r.Path = "relative/path" },
		func(r *Request) { r.Path = "" },
		func(r *Request) { r.Path = "/a\nb" },
		func(r *Request) { r.Path = "/a\x00b" },
		func(r *Request) { r.Path = "/" + strings.Repeat("x", 5000) },
		func(r *Request) { r.App = "-a; rm -rf /" },
		func(r *Request) { r.App = "/Applications/Foo.app" },
		func(r *Request) { r.Size = -1 },
		func(r *Request) { r.Size = DefaultMaxPayload + 1 },
	}
	for i, mut := range cases {
		r := validReq()
		mut(r)
		if err := r.Validate(DefaultMaxPayload); err == nil {
			t.Errorf("case %d: expected rejection, got nil", i)
		}
	}
}

func TestRequestRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	want := validReq()
	if err := WriteRequest(&buf, want); err != nil {
		t.Fatal(err)
	}
	got, err := ReadRequest(bufio.NewReaderSize(&buf, MaxHeaderBytes))
	if err != nil {
		t.Fatal(err)
	}
	if *got != *want {
		t.Fatalf("round trip mismatch: %+v != %+v", got, want)
	}
}

func TestReadRequestRejectsUnknownFields(t *testing.T) {
	br := bufio.NewReaderSize(strings.NewReader(`{"v":1,"op":"open","evil":true}`+"\n"), MaxHeaderBytes)
	if _, err := ReadRequest(br); err == nil {
		t.Fatal("expected unknown-field rejection")
	}
}

func TestReadRequestHeaderTooLong(t *testing.T) {
	line := `{"v":1,"path":"` + strings.Repeat("a", MaxHeaderBytes) + `"}` + "\n"
	br := bufio.NewReaderSize(strings.NewReader(line), MaxHeaderBytes)
	if _, err := ReadRequest(br); err == nil {
		t.Fatal("expected header-too-long rejection")
	}
}

func TestChunkRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	cw := NewChunkWriter(&buf)
	payload := bytes.Repeat([]byte("abc123"), 500000) // ~3 MB, spans chunks
	if _, err := cw.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := cw.Close(); err != nil {
		t.Fatal(err)
	}
	cr := NewChunkReader(&buf, int64(len(payload)))
	got, err := io.ReadAll(cr)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatal("chunk payload mismatch")
	}
	if cr.Total() != int64(len(payload)) {
		t.Fatalf("total = %d, want %d", cr.Total(), len(payload))
	}
}

func TestChunkReaderEnforcesCap(t *testing.T) {
	var buf bytes.Buffer
	cw := NewChunkWriter(&buf)
	if _, err := cw.Write(make([]byte, 1000)); err != nil {
		t.Fatal(err)
	}
	if err := cw.Close(); err != nil {
		t.Fatal(err)
	}
	cr := NewChunkReader(&buf, 999)
	if _, err := io.ReadAll(cr); err != ErrPayloadTooLarge {
		t.Fatalf("want ErrPayloadTooLarge, got %v", err)
	}
}

func TestChunkReaderRejectsOversizeChunkHeader(t *testing.T) {
	// Hand-craft a header claiming a chunk larger than MaxChunkBytes.
	buf := []byte{0xFF, 0xFF, 0xFF, 0xFF}
	cr := NewChunkReader(bytes.NewReader(buf), 1<<30)
	if _, err := io.ReadAll(cr); err == nil {
		t.Fatal("expected oversize chunk rejection")
	}
}

func TestChunkReaderTruncatedStream(t *testing.T) {
	var buf bytes.Buffer
	cw := NewChunkWriter(&buf)
	if _, err := cw.Write(make([]byte, 100)); err != nil {
		t.Fatal(err)
	}
	// No Close(): stream ends without the zero-length terminator.
	cr := NewChunkReader(&buf, 1000)
	if _, err := io.ReadAll(cr); err == nil {
		t.Fatal("expected truncated-stream error")
	}
}
