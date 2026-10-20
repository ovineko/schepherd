package runner

import (
	"encoding/base64"
	"sync"
	"unicode/utf8"
)

// Capture limits bound retained consumer bytes, not the number of bytes drained.
const (
	MaxStreamBytes  = 256 << 10
	MaxCaptureBytes = 4 << 20
)

type captureBudget struct {
	mu        sync.Mutex
	remaining int
}

type taskCapture struct {
	stdout captureStream
	stderr captureStream
}

type captureStream struct {
	budget     *captureBudget
	data       []byte
	bytes      int64
	pending    [utf8.UTFMax]byte
	partial    int
	invalid    bool
	incomplete bool
}

func (s *captureStream) Write(p []byte) (int, error) {
	s.bytes += int64(len(p))
	s.checkUTF8(p)
	s.budget.mu.Lock()
	keep := min(len(p), MaxStreamBytes-len(s.data), s.budget.remaining)
	s.budget.remaining -= keep
	s.budget.mu.Unlock()
	if keep > 0 {
		s.data = append(s.data, p[:keep]...)
	}
	return len(p), nil
}

func (s *captureStream) checkUTF8(p []byte) {
	if s.invalid {
		return
	}
	for s.partial > 0 && len(p) > 0 {
		s.pending[s.partial] = p[0]
		s.partial++
		p = p[1:]
		if utf8.FullRune(s.pending[:s.partial]) {
			r, size := utf8.DecodeRune(s.pending[:s.partial])
			if r == utf8.RuneError && size == 1 {
				s.invalid = true
				return
			}
			s.partial = 0
		}
	}
	for len(p) > 0 {
		r, size := utf8.DecodeRune(p)
		if r == utf8.RuneError && size == 1 {
			if !utf8.FullRune(p) {
				s.partial = copy(s.pending[:], p)
			} else {
				s.invalid = true
			}
			return
		}
		p = p[size:]
	}
}

func (s *captureStream) report() StreamReport {
	r := StreamReport{Bytes: s.bytes, Truncated: s.incomplete || int64(len(s.data)) < s.bytes}
	if !s.invalid && s.partial == 0 && utf8.Valid(s.data) {
		text := string(s.data)
		r.Text = &text
	} else {
		encoded := base64.StdEncoding.EncodeToString(s.data)
		r.Base64 = &encoded
	}
	return r
}
