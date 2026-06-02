package sandbox

import (
	"bytes"
	"io"
)

// LimitedWriter is an io.Writer that discards bytes once the write limit is
// reached.  It is exported so packages like runtime can use it for capturing
// database container output.
type LimitedWriter struct {
	buf   bytes.Buffer
	limit int64
	n     int64
}

// SetLimit configures the maximum number of bytes that will be buffered.
// Must be called before any Write calls.
func (b *LimitedWriter) SetLimit(l int64) {
	b.limit = l
}

func (b *LimitedWriter) Write(p []byte) (int, error) {
	lim := b.limit
	if lim <= 0 {
		lim = MaxOutputSize
	}
	remaining := lim - b.n
	if remaining <= 0 {
		return len(p), nil // silently discard
	}
	if int64(len(p)) > remaining {
		p = p[:remaining]
	}
	n, err := b.buf.Write(p)
	b.n += int64(n)
	return len(p), err
}

func (b *LimitedWriter) String() string { return b.buf.String() }

// Bytes returns the buffered bytes.
func (b *LimitedWriter) Bytes() []byte { return b.buf.Bytes() }

// Reset clears the buffer and byte count.
func (b *LimitedWriter) Reset() {
	b.buf.Reset()
	b.n = 0
}

// StdCopyStreams demultiplexes the Docker attach stream (8-byte framed) into
// separate dst (stdout) and dstErr (stderr) writers.  It is exported so
// callers outside this package (e.g., runtime/db_runtime.go) can reuse it
// without duplicating the frame-parsing logic.
func StdCopyStreams(dst, dstErr io.Writer, src io.Reader) (int64, error) {
	return stdCopy(dst, dstErr, src)
}
