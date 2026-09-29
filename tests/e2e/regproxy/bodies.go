package regproxy

import (
	"errors"
	"fmt"
	"io"
	"sync/atomic"
)

// errTruncated ends a truncated body early. ReverseProxy answers a body
// read error by aborting the client connection with http.ErrAbortHandler,
// so the client sees fewer bytes than the announced Content-Length.
var errTruncated = errors.New("regproxy: body truncated by fault")

// countingBody is read by the transport's writer goroutine while the handler
// may already be recording, hence the atomic counter.
type countingBody struct {
	rc io.ReadCloser
	n  atomic.Int64
}

func (b *countingBody) Read(p []byte) (int, error) {
	n, err := b.rc.Read(p)
	b.n.Add(int64(n))

	return n, passRead(err)
}

func (b *countingBody) Close() error {
	return closeBody(b.rc)
}

type corruptingBody struct {
	rc  io.ReadCloser
	at  int64
	off int64
}

func (b *corruptingBody) Read(p []byte) (int, error) {
	n, err := b.rc.Read(p)
	if i := b.at - b.off; i >= 0 && i < int64(n) {
		p[i] ^= 0xff
	}

	b.off += int64(n)

	return n, passRead(err)
}

func (b *corruptingBody) Close() error {
	return closeBody(b.rc)
}

type truncatingBody struct {
	rc        io.ReadCloser
	remaining int64
}

func (b *truncatingBody) Read(p []byte) (int, error) {
	if b.remaining <= 0 {
		return 0, errTruncated
	}

	if int64(len(p)) > b.remaining {
		p = p[:b.remaining]
	}

	n, err := b.rc.Read(p)
	b.remaining -= int64(n)

	return n, passRead(err)
}

func (b *truncatingBody) Close() error {
	return closeBody(b.rc)
}

// passRead returns io.EOF unwrapped because io.Copy and ReverseProxy compare
// it by identity.
func passRead(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, io.EOF):
		return io.EOF
	default:
		return fmt.Errorf("regproxy: read body: %w", err)
	}
}

func closeBody(rc io.Closer) error {
	if err := rc.Close(); err != nil {
		return fmt.Errorf("regproxy: close body: %w", err)
	}

	return nil
}
