package logd

import (
	"bytes"
	"io"
	"sync"
)

const maxRedactingWriterBuffer = 64 << 10

// RedactingWriter buffers complete lines so a secret split across writes is
// redacted with the same Redactor as the event writer. Flush emits a final
// unterminated line and is required when the producing process exits.
type RedactingWriter struct {
	dst      io.Writer
	redactor *Redactor
	mu       sync.Mutex
	buf      bytes.Buffer
}

func NewRedactingWriter(dst io.Writer, redactor *Redactor) *RedactingWriter {
	return &RedactingWriter{dst: dst, redactor: redactor}
}

func (w *RedactingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.dst == nil {
		return len(p), nil
	}
	if _, err := w.buf.Write(p); err != nil {
		return 0, err
	}
	for {
		data := w.buf.Bytes()
		idx := bytes.IndexByte(data, '\n')
		if idx < 0 {
			break
		}
		line := append([]byte(nil), data[:idx+1]...)
		w.buf.Next(idx + 1)
		if _, err := w.dst.Write(w.redactor.RedactBytes(line)); err != nil {
			return 0, err
		}
	}
	if err := w.flushOversized(); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (w *RedactingWriter) flushOversized() error {
	if w.buf.Len() <= maxRedactingWriterBuffer {
		return nil
	}
	// The bounded-line contract is maxRedactingWriterBuffer plus the longest
	// literal overlap. A configured literal longer than that cap necessarily
	// retains its overlap so an exact match cannot be emitted prematurely.
	data := w.buf.Bytes()
	consume := len(data) - w.redactor.literalOverlap()
	if consume <= 0 {
		return nil
	}
	// A literal that begins before the safe boundary and ends in the retained
	// suffix must stay buffered as one unit; otherwise an unterminated line can
	// leak the prefix of a credential when the 64KiB bound is crossed.
	if w.redactor != nil {
		for _, literal := range w.redactor.literalValues {
			for from := 0; from+len(literal) <= len(data); {
				at := bytes.Index(data[from:], literal)
				if at < 0 {
					break
				}
				at += from
				if at < consume && at+len(literal) > consume {
					consume = at
				}
				from = at + 1
			}
		}
	}
	if consume == 0 {
		return nil
	}
	prefix := append([]byte(nil), data[:consume]...)
	w.buf.Next(consume)
	_, err := w.dst.Write(w.redactor.RedactBytes(prefix))
	return err
}

func (w *RedactingWriter) Flush() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.dst == nil || w.buf.Len() == 0 {
		return nil
	}
	_, err := w.dst.Write(w.redactor.RedactBytes(w.buf.Bytes()))
	w.buf.Reset()
	return err
}
