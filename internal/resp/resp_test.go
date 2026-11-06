package resp

import (
	"bytes"
	"math"
	"strings"
	"testing"
)

func TestReadCommand(t *testing.T) {
	tests := []struct {
		in   string
		want []string
	}{
		{"*2\r\n$3\r\nGET\r\n$1\r\nk\r\n", []string{"GET", "k"}},
		{"*1\r\n$0\r\n\r\n", []string{""}},
		{"PING\r\n", []string{"PING"}},
		{"set  key \"hello world\"\r\n", []string{"set", "key", "hello world"}},
		{"echo 'a b'\n", []string{"echo", "a b"}},
	}
	for _, tt := range tests {
		got, err := NewReader(strings.NewReader(tt.in)).ReadCommand()
		if err != nil {
			t.Fatalf("%q: %v", tt.in, err)
		}
		if len(got) != len(tt.want) {
			t.Fatalf("%q: got %q, want %q", tt.in, got, tt.want)
		}
		for i := range got {
			if string(got[i]) != tt.want[i] {
				t.Errorf("%q: arg %d = %q, want %q", tt.in, i, got[i], tt.want[i])
			}
		}
	}
}

func TestReadCommandErrors(t *testing.T) {
	for _, in := range []string{
		"*1\r\n:5\r\n",
		"*1\r\n$3\r\nabcd\r\n",
		"*x\r\n",
		"*1\r\n$-5\r\n",
		"echo \"open\r\n",
	} {
		if _, err := NewReader(strings.NewReader(in)).ReadCommand(); err == nil {
			t.Errorf("%q: expected an error", in)
		}
	}
}

func TestWriter(t *testing.T) {
	var b bytes.Buffer
	w := NewWriter(&b)
	w.OK()
	w.Error("boom")
	w.Error("WRONGTYPE bad")
	w.Int(-3)
	w.BulkString("hi")
	w.Null()
	w.Map(1)
	w.Double(1.5)
	w.Flush()
	want := "+OK\r\n-ERR boom\r\n-WRONGTYPE bad\r\n:-3\r\n$2\r\nhi\r\n$-1\r\n*2\r\n$3\r\n1.5\r\n"
	if b.String() != want {
		t.Errorf("RESP2:\n got %q\nwant %q", b.String(), want)
	}

	b.Reset()
	w.Proto = 3
	w.Null()
	w.Map(1)
	w.Set(2)
	w.Double(math.Inf(-1))
	w.Push(3)
	w.Flush()
	want = "_\r\n%1\r\n~2\r\n,-inf\r\n>3\r\n"
	if b.String() != want {
		t.Errorf("RESP3:\n got %q\nwant %q", b.String(), want)
	}
}
