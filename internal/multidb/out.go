package multidb

import (
	"bytes"
	"io"
	"sync"
)

// lineWriter puts the name of a database in front of every line written through it, so the output
// of databases that run at the same time can be told apart. The writers of one fleet share a mutex,
// so two of them never mix their text inside a line. Empty lines are dropped.
type lineWriter struct {
	mu     *sync.Mutex
	w      io.Writer
	prefix []byte
	buf    []byte
}

func newLineWriter(mu *sync.Mutex, w io.Writer, name string) *lineWriter {
	return &lineWriter{mu: mu, w: w, prefix: []byte("[" + name + "] ")}
}

// Write buffers text up to the next line break and writes every complete line with the prefix.
func (l *lineWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.buf = append(l.buf, p...)
	for {
		i := bytes.IndexByte(l.buf, '\n')
		if i < 0 {
			return len(p), nil
		}
		line := l.buf[:i]
		l.buf = l.buf[i+1:]
		if err := l.writeLine(line); err != nil {
			return len(p), err
		}
	}
}

// Flush writes the last line when it did not end with a line break.
func (l *lineWriter) Flush() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.buf) > 0 {
		_ = l.writeLine(l.buf)
		l.buf = nil
	}
}

func (l *lineWriter) writeLine(line []byte) error {
	if len(bytes.TrimSpace(line)) == 0 {
		return nil
	}
	out := make([]byte, 0, len(l.prefix)+len(line)+1)
	out = append(out, l.prefix...)
	out = append(out, line...)
	out = append(out, '\n')
	_, err := l.w.Write(out)
	return err
}
