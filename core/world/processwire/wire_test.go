package processwire_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/Eastsidegunn/JANUS/core/world/processwire"
)

func TestFrameRoundTripWithPartialIO(t *testing.T) {
	var raw bytes.Buffer
	enc := processwire.NewEncoder(shortWriter{w: &raw, n: 3})
	payload := bytes.Repeat([]byte("x"), processwire.MaxPayload)
	seq, err := enc.Write(processwire.KindStdinData, processwire.StreamControl, 7, payload)
	if err != nil || seq != 1 {
		t.Fatalf("Write=(%d,%v)", seq, err)
	}
	frame, err := processwire.NewDecoder(shortReader{r: bytes.NewReader(raw.Bytes()), n: 2}).Read()
	if err != nil {
		t.Fatal(err)
	}
	if frame.Seq != 1 || frame.Flags != 7 || frame.Kind != processwire.KindStdinData || !bytes.Equal(frame.Payload, payload) {
		t.Fatalf("round trip mismatch: %+v", frame)
	}
}

func TestFrameFailuresAreFatalAndCategorized(t *testing.T) {
	tests := map[string]struct {
		data []byte
		want error
	}{
		"zero length":     {frameBytes(0, processwire.Version, processwire.KindStart, processwire.StreamControl, 1, nil), processwire.ErrProtocol},
		"oversize":        {frameBytes(processwire.MaxFrameBytes+1, processwire.Version, processwire.KindStart, processwire.StreamControl, 1, nil), processwire.ErrResourceExhausted},
		"partial payload": {frameBytes(processwire.HeaderBytes+4, processwire.Version, processwire.KindStart, processwire.StreamControl, 1, []byte("x")), processwire.ErrProtocol},
		"sequence gap":    {frameBytes(processwire.HeaderBytes, processwire.Version, processwire.KindStart, processwire.StreamControl, 2, nil), processwire.ErrProtocol},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := processwire.NewDecoder(bytes.NewReader(tc.data)).Read()
			if !errors.Is(err, processwire.ErrFatal) || !errors.Is(err, tc.want) {
				t.Fatalf("err=%v", err)
			}
		})
	}
	_, err := processwire.NewEncoder(io.Discard).Write(processwire.KindStdinData, processwire.StreamControl, 0, make([]byte, processwire.MaxPayload+1))
	if !errors.Is(err, processwire.ErrFatal) || !errors.Is(err, processwire.ErrResourceExhausted) {
		t.Fatalf("oversize write err=%v", err)
	}
}

func TestJSONIsClosed(t *testing.T) {
	payload := []byte(`{"version":1,"role":"control","lease_id":"l","span_id":"s","capability":"c","extra":true}`)
	var hello processwire.Hello
	if err := processwire.Unmarshal(payload, &hello); !errors.Is(err, processwire.ErrProtocol) {
		t.Fatalf("unknown field err=%v", err)
	}
}

func TestSocketWriteBackpressureIsNotDropped(t *testing.T) {
	left, right := net.Pipe()
	defer left.Close()
	defer right.Close()
	done := make(chan error, 1)
	go func() {
		_, err := processwire.NewEncoder(left).Write(processwire.KindStdoutData, processwire.StreamStdout, 0, bytes.Repeat([]byte("z"), processwire.MaxPayload))
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("peer read 전 write가 완료됨: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	frame, err := processwire.NewDecoder(right).Read()
	if err != nil || len(frame.Payload) != processwire.MaxPayload {
		t.Fatalf("Read=(%d,%v)", len(frame.Payload), err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("backpressured write release timeout")
	}
}

func frameBytes(length, version int, kind processwire.Kind, stream processwire.Stream, seq uint64, payload []byte) []byte {
	buf := make([]byte, 16+len(payload))
	binary.BigEndian.PutUint32(buf[:4], uint32(length))
	buf[4] = byte(version)
	buf[5] = byte(kind)
	buf[6] = byte(stream)
	binary.BigEndian.PutUint64(buf[8:16], seq)
	copy(buf[16:], payload)
	if length > processwire.MaxFrameBytes {
		return buf[:4]
	}
	return buf
}

type shortWriter struct {
	w io.Writer
	n int
}

func (w shortWriter) Write(p []byte) (int, error) {
	if len(p) > w.n {
		p = p[:w.n]
	}
	return w.w.Write(p)
}

type shortReader struct {
	r io.Reader
	n int
}

func (r shortReader) Read(p []byte) (int, error) {
	if len(p) > r.n {
		p = p[:r.n]
	}
	return r.r.Read(p)
}
