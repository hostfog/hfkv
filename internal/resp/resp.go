// Package resp reads and writes the Redis serialization protocol (RESP2),
// so any Redis client library can talk to hfkv.
package resp

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
)

// Limits that stop a bad client from making the server allocate too much.
const (
	maxBulk  = 512 << 20
	maxArray = 1 << 20
)

var ErrProtocol = errors.New("protocol error")

type Reader struct {
	r *bufio.Reader
}

func NewReader(r io.Reader) *Reader {
	return &Reader{r: bufio.NewReaderSize(r, 16<<10)}
}

// Buffered reports whether more input is waiting, so replies to pipelined
// commands can be flushed together.
func (r *Reader) Buffered() int { return r.r.Buffered() }

// ReadCommand reads one command: a RESP array of bulk strings, or an inline
// command such as "PING" typed into telnet.
func (r *Reader) ReadCommand() ([][]byte, error) {
	b, err := r.r.ReadByte()
	if err != nil {
		return nil, err
	}
	if b != '*' {
		r.r.UnreadByte()
		return r.readInline()
	}
	n, err := r.readInt()
	if err != nil {
		return nil, err
	}
	if n < 0 || n > maxArray {
		return nil, fmt.Errorf("%w: invalid multibulk length", ErrProtocol)
	}
	args := make([][]byte, 0, n)
	for i := 0; i < n; i++ {
		b, err := r.r.ReadByte()
		if err != nil {
			return nil, err
		}
		if b != '$' {
			return nil, fmt.Errorf("%w: expected '$', got '%c'", ErrProtocol, b)
		}
		size, err := r.readInt()
		if err != nil {
			return nil, err
		}
		if size < 0 || size > maxBulk {
			return nil, fmt.Errorf("%w: invalid bulk length", ErrProtocol)
		}
		buf := make([]byte, size+2)
		if _, err := io.ReadFull(r.r, buf); err != nil {
			return nil, err
		}
		if buf[size] != '\r' || buf[size+1] != '\n' {
			return nil, fmt.Errorf("%w: bulk not terminated", ErrProtocol)
		}
		args = append(args, buf[:size])
	}
	return args, nil
}

func (r *Reader) readLine() ([]byte, error) {
	line, err := r.r.ReadSlice('\n')
	if err == bufio.ErrBufferFull {
		return nil, fmt.Errorf("%w: line too long", ErrProtocol)
	}
	if err != nil {
		return nil, err
	}
	if len(line) >= 2 && line[len(line)-2] == '\r' {
		return line[:len(line)-2], nil
	}
	return line[:len(line)-1], nil
}

func (r *Reader) readInt() (int, error) {
	line, err := r.readLine()
	if err != nil {
		return 0, err
	}
	n, err := strconv.Atoi(string(line))
	if err != nil {
		return 0, fmt.Errorf("%w: invalid length", ErrProtocol)
	}
	return n, nil
}

func (r *Reader) readInline() ([][]byte, error) {
	line, err := r.readLine()
	if err != nil {
		return nil, err
	}
	var args [][]byte
	for i := 0; i < len(line); {
		for i < len(line) && (line[i] == ' ' || line[i] == '\t') {
			i++
		}
		if i == len(line) {
			break
		}
		if line[i] == '"' || line[i] == '\'' {
			q := line[i]
			i++
			start := i
			for i < len(line) && line[i] != q {
				i++
			}
			if i == len(line) {
				return nil, fmt.Errorf("%w: unbalanced quotes", ErrProtocol)
			}
			args = append(args, append([]byte(nil), line[start:i]...))
			i++
			continue
		}
		start := i
		for i < len(line) && line[i] != ' ' && line[i] != '\t' {
			i++
		}
		args = append(args, append([]byte(nil), line[start:i]...))
	}
	return args, nil
}

// Writer buffers replies; call Flush to send them. Proto selects RESP2
// (the default) or RESP3, which has real nulls, maps, sets and doubles.
type Writer struct {
	w     *bufio.Writer
	Proto int
}

func NewWriter(w io.Writer) *Writer {
	return &Writer{w: bufio.NewWriterSize(w, 16<<10), Proto: 2}
}

func (w *Writer) Flush() error { return w.w.Flush() }

func (w *Writer) Simple(s string) {
	w.w.WriteByte('+')
	w.w.WriteString(s)
	w.w.WriteString("\r\n")
}

func (w *Writer) OK() { w.Simple("OK") }

// Error writes an error reply. Messages without a code get the ERR prefix.
func (w *Writer) Error(msg string) {
	w.w.WriteByte('-')
	if !hasCode(msg) {
		w.w.WriteString("ERR ")
	}
	w.w.WriteString(msg)
	w.w.WriteString("\r\n")
}

func hasCode(msg string) bool {
	for i := 0; i < len(msg); i++ {
		c := msg[i]
		if c == ' ' {
			return i > 0
		}
		if c < 'A' || c > 'Z' {
			return false
		}
	}
	return false
}

func (w *Writer) Int(n int64) {
	w.w.WriteByte(':')
	w.w.WriteString(strconv.FormatInt(n, 10))
	w.w.WriteString("\r\n")
}

func (w *Writer) Bulk(b []byte) {
	w.w.WriteByte('$')
	w.w.WriteString(strconv.Itoa(len(b)))
	w.w.WriteString("\r\n")
	w.w.Write(b)
	w.w.WriteString("\r\n")
}

func (w *Writer) BulkString(s string) {
	w.w.WriteByte('$')
	w.w.WriteString(strconv.Itoa(len(s)))
	w.w.WriteString("\r\n")
	w.w.WriteString(s)
	w.w.WriteString("\r\n")
}

func (w *Writer) Null() {
	if w.Proto == 3 {
		w.w.WriteString("_\r\n")
		return
	}
	w.w.WriteString("$-1\r\n")
}

func (w *Writer) NullArray() {
	if w.Proto == 3 {
		w.w.WriteString("_\r\n")
		return
	}
	w.w.WriteString("*-1\r\n")
}

func (w *Writer) header(c byte, n int) {
	w.w.WriteByte(c)
	w.w.WriteString(strconv.Itoa(n))
	w.w.WriteString("\r\n")
}

func (w *Writer) Array(n int) { w.header('*', n) }

// Map starts a map of n key/value pairs (a flat array in RESP2).
func (w *Writer) Map(n int) {
	if w.Proto == 3 {
		w.header('%', n)
		return
	}
	w.header('*', n*2)
}

// Set starts a set of n members (an array in RESP2).
func (w *Writer) Set(n int) {
	if w.Proto == 3 {
		w.header('~', n)
		return
	}
	w.header('*', n)
}

// Push starts an out-of-band message such as a pub/sub delivery.
func (w *Writer) Push(n int) {
	if w.Proto == 3 {
		w.header('>', n)
		return
	}
	w.header('*', n)
}

// Double writes a float (a bulk string in RESP2).
func (w *Writer) Double(f float64) {
	s := formatFloat(f)
	if w.Proto == 3 {
		w.w.WriteByte(',')
		w.w.WriteString(s)
		w.w.WriteString("\r\n")
		return
	}
	w.BulkString(s)
}

// FormatFloat formats a score or float result the way Redis does.
func FormatFloat(f float64) string { return formatFloat(f) }

func formatFloat(f float64) string {
	switch {
	case math.IsInf(f, 1):
		return "inf"
	case math.IsInf(f, -1):
		return "-inf"
	}
	return strconv.FormatFloat(f, 'g', -1, 64)
}

func (w *Writer) Strings(items []string) {
	w.Array(len(items))
	for _, s := range items {
		w.BulkString(s)
	}
}

// Command encodes args as a RESP array, the form clients send.
func (w *Writer) Command(args ...string) { w.Strings(args) }

// Peek waits until input is available or the connection fails. The server
// uses it to notice a client hanging up while it waits in BLPOP.
func (r *Reader) Peek() error {
	_, err := r.r.Peek(1)
	return err
}
