package remote

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"
)

// Always drain the child's pipe after reaching the disk cap.
type capture struct {
	mu         sync.Mutex
	file       *os.File
	cap, total int64
	err        error
}

func (c *capture) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := len(p)
	remaining := c.cap - c.total
	if remaining > 0 && c.err == nil {
		write := p
		if int64(len(write)) > remaining {
			write = write[:remaining]
		}
		_, c.err = c.file.Write(write)
	}
	c.total += int64(n)
	return n, nil
}
func (c *capture) info() (int64, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.total, c.total > c.cap, c.err
}
func (c *capture) close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.file.Sync(); c.err == nil {
		c.err = err
	}
	if err := c.file.Close(); c.err == nil {
		c.err = err
	}
}

func parseCursor(s string) (int64, int64, error) {
	if s == "" {
		return 0, 0, nil
	}
	p := strings.Split(s, ":")
	if len(p) != 2 {
		return 0, 0, errors.New("invalid output cursor")
	}
	a, e1 := strconv.ParseInt(p[0], 10, 64)
	b, e2 := strconv.ParseInt(p[1], 10, 64)
	if e1 != nil || e2 != nil || a < 0 || b < 0 {
		return 0, 0, errors.New("invalid output cursor")
	}
	return a, b, nil
}
func readOutput(path string, offset int64, limit int, terminal bool) (string, int64, bool, error) {
	f, err := os.Open(path)
	if os.IsNotExist(err) && offset == 0 {
		return "", 0, false, nil
	}
	if err != nil {
		return "", offset, false, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", offset, false, err
	}
	if offset > st.Size() {
		return "", offset, false, fmt.Errorf("cursor exceeds captured output size")
	}
	buf := make([]byte, limit)
	n, err := f.ReadAt(buf, offset)
	if err != nil && err != io.EOF {
		return "", offset, false, err
	}
	buf = buf[:n]
	// Do not split a UTF-8 character at a page boundary or at a live pipe tail.
	end := 0
	for end < len(buf) {
		if !utf8.FullRune(buf[end:]) && (!terminal || offset+int64(n) < st.Size()) {
			break
		}
		_, size := utf8.DecodeRune(buf[end:])
		end += size
	}
	next := offset + int64(end)
	return strings.ToValidUTF8(string(buf[:end]), "�"), next, next < st.Size(), nil
}
