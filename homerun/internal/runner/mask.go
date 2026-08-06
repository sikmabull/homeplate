package runner

import (
	"io"
	"strings"
	"sync"
)

// maskWriter redacts known secrets from any stream before it reaches disk or a
// terminal, mirroring what the official runner does with ::add-mask::.
//
// Homerun registers registration tokens here. It intentionally masks on a
// line-buffered basis, because a secret split across two Write calls would
// otherwise slip through byte-wise matching.
type maskWriter struct {
	mu      sync.Mutex
	dst     io.Writer
	secrets []string
	buf     []byte
}

// newMaskWriter wraps dst, redacting every non-empty secret.
func newMaskWriter(dst io.Writer, secrets ...string) *maskWriter {
	var kept []string
	for _, s := range secrets {
		// Very short strings would redact ordinary output; require real length.
		if len(strings.TrimSpace(s)) >= 8 {
			kept = append(kept, s)
		}
	}
	return &maskWriter{dst: dst, secrets: kept}
}

// AddSecret registers an additional value to redact.
func (m *maskWriter) AddSecret(s string) {
	if len(strings.TrimSpace(s)) < 8 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.secrets = append(m.secrets, s)
}

func (m *maskWriter) Write(p []byte) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := len(p)
	m.buf = append(m.buf, p...)

	// Flush only complete lines; hold a partial tail so a secret straddling a
	// chunk boundary is still caught.
	for {
		idx := indexByte(m.buf, '\n')
		if idx < 0 {
			break
		}
		line := string(m.buf[:idx+1])
		m.buf = m.buf[idx+1:]
		if _, err := io.WriteString(m.dst, m.redact(line)); err != nil {
			return n, err
		}
	}
	// Bound the held tail so a stream with no newlines cannot grow forever.
	if len(m.buf) > 1<<20 {
		chunk := string(m.buf)
		m.buf = nil
		if _, err := io.WriteString(m.dst, m.redact(chunk)); err != nil {
			return n, err
		}
	}
	return n, nil
}

// Flush emits any buffered partial line.
func (m *maskWriter) Flush() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.buf) == 0 {
		return nil
	}
	chunk := string(m.buf)
	m.buf = nil
	_, err := io.WriteString(m.dst, m.redact(chunk))
	return err
}

func (m *maskWriter) redact(s string) string {
	for _, secret := range m.secrets {
		if secret == "" {
			continue
		}
		s = strings.ReplaceAll(s, secret, "***")
	}
	return s
}

func indexByte(b []byte, c byte) int {
	for i := range b {
		if b[i] == c {
			return i
		}
	}
	return -1
}
