// Package processwire owns the bounded, host-only process broker framing
// protocol (FR-SBX-05, FR-LOG-09). It shares neither messages nor capabilities
// with approvalwire and is never mounted into an agent container.
package processwire

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
)

const (
	Version       = 1
	HeaderBytes   = 12
	MaxPayload    = 64 * 1024
	MaxFrameBytes = HeaderBytes + MaxPayload
)

var (
	ErrProtocol          = errors.New("processwire: protocol violation")
	ErrResourceExhausted = errors.New("processwire: resource exhausted")
	ErrFatal             = errors.New("processwire: broker fatal")
)

type Kind uint8

const (
	KindHello Kind = iota + 1
	KindHelloAck
	KindStart
	KindStdinData
	KindStdinClose
	KindStop
	KindWait
	KindAck
	KindError
	KindExitObserved
	KindStdoutData
	KindStderrData
	KindStreamEnd
)

type Stream uint8

const (
	StreamControl Stream = iota + 1
	StreamStdout
	StreamStderr
)

type Role string

const (
	RoleControl Role = "control"
	RoleOutput  Role = "output"
)

type Frame struct {
	Kind    Kind
	Stream  Stream
	Flags   uint8
	Seq     uint64
	Payload []byte
}

type Hello struct {
	Version    int    `json:"version"`
	Role       Role   `json:"role"`
	LeaseID    string `json:"lease_id"`
	SpanID     string `json:"span_id"`
	Capability string `json:"capability"`
}

type Ack struct {
	RequestSeq uint64 `json:"request_seq"`
}

type WireError struct {
	RequestSeq uint64 `json:"request_seq"`
	Reason     string `json:"reason"`
	Fatal      bool   `json:"fatal"`
}

type Stop struct {
	Reason string `json:"reason"`
}

type ExitObserved struct {
	Code   int    `json:"code"`
	Reason string `json:"reason"`
}

type StreamEnd struct {
	AttachError string `json:"attach_error,omitempty"`
}

// FatalError distinguishes protocol/resource failures from ordinary agent
// exit status. Unwrap preserves the underlying category for errors.Is.
type FatalError struct{ Err error }

func (e *FatalError) Error() string   { return fmt.Sprintf("%v: %v", ErrFatal, e.Err) }
func (e *FatalError) Unwrap() []error { return []error{ErrFatal, e.Err} }

func fatal(err error) error {
	if err == nil {
		return nil
	}
	return &FatalError{Err: err}
}

// Encoder assigns monotonically increasing sequence numbers and serializes
// writes. A frame is visible to the peer only after writeFull completes.
type Encoder struct {
	w    io.Writer
	mu   sync.Mutex
	next uint64
}

func NewEncoder(w io.Writer) *Encoder { return &Encoder{w: w, next: 1} }

func (e *Encoder) Write(kind Kind, stream Stream, flags uint8, payload []byte) (uint64, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !validKind(kind) || !validStream(stream) {
		return 0, fatal(fmt.Errorf("%w: kind=%d stream=%d", ErrProtocol, kind, stream))
	}
	if len(payload) > MaxPayload {
		return 0, fatal(fmt.Errorf("%w: payload=%d max=%d", ErrResourceExhausted, len(payload), MaxPayload))
	}
	seq := e.next
	frameLength := HeaderBytes + len(payload)
	buf := make([]byte, 4+frameLength)
	binary.BigEndian.PutUint32(buf[:4], uint32(frameLength))
	buf[4] = Version
	buf[5] = byte(kind)
	buf[6] = byte(stream)
	buf[7] = flags
	binary.BigEndian.PutUint64(buf[8:16], seq)
	copy(buf[16:], payload)
	if err := writeFull(e.w, buf); err != nil {
		return 0, err
	}
	e.next++
	return seq, nil
}

type Decoder struct {
	r      io.Reader
	expect uint64
}

func NewDecoder(r io.Reader) *Decoder { return &Decoder{r: r, expect: 1} }

func (d *Decoder) Read() (Frame, error) {
	var prefix [4]byte
	if _, err := io.ReadFull(d.r, prefix[:]); err != nil {
		return Frame{}, err
	}
	length := int(binary.BigEndian.Uint32(prefix[:]))
	if length < HeaderBytes {
		return Frame{}, fatal(fmt.Errorf("%w: frame length=%d", ErrProtocol, length))
	}
	if length > MaxFrameBytes {
		return Frame{}, fatal(fmt.Errorf("%w: frame length=%d max=%d", ErrResourceExhausted, length, MaxFrameBytes))
	}
	buf := make([]byte, length)
	if _, err := io.ReadFull(d.r, buf); err != nil {
		return Frame{}, fatal(fmt.Errorf("%w: partial frame: %v", ErrProtocol, err))
	}
	if int(buf[0]) != Version {
		return Frame{}, fatal(fmt.Errorf("%w: version=%d", ErrProtocol, buf[0]))
	}
	kind, stream := Kind(buf[1]), Stream(buf[2])
	if !validKind(kind) || !validStream(stream) {
		return Frame{}, fatal(fmt.Errorf("%w: kind=%d stream=%d", ErrProtocol, kind, stream))
	}
	seq := binary.BigEndian.Uint64(buf[4:12])
	if seq != d.expect {
		return Frame{}, fatal(fmt.Errorf("%w: sequence=%d want=%d", ErrProtocol, seq, d.expect))
	}
	d.expect++
	return Frame{Kind: kind, Stream: stream, Flags: buf[3], Seq: seq, Payload: append([]byte(nil), buf[12:]...)}, nil
}

func Marshal(value any) ([]byte, error) {
	payload, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	if len(payload) > MaxPayload {
		return nil, fatal(fmt.Errorf("%w: JSON payload=%d", ErrResourceExhausted, len(payload)))
	}
	return payload, nil
}

func Unmarshal(payload []byte, dst any) error {
	if len(payload) > MaxPayload {
		return fatal(fmt.Errorf("%w: JSON payload=%d", ErrResourceExhausted, len(payload)))
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return fatal(fmt.Errorf("%w: JSON: %v", ErrProtocol, err))
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return fatal(fmt.Errorf("%w: trailing JSON", ErrProtocol))
	}
	return nil
}

func writeFull(w io.Writer, data []byte) error {
	for len(data) > 0 {
		n, err := w.Write(data)
		if err != nil {
			return err
		}
		if n <= 0 {
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	return nil
}

func validKind(kind Kind) bool       { return kind >= KindHello && kind <= KindStreamEnd }
func validStream(stream Stream) bool { return stream >= StreamControl && stream <= StreamStderr }
