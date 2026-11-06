// Command hfkv-cli is a command line client for hfkv (and any server that
// speaks the Redis protocol).
//
//	hfkv-cli                    interactive prompt
//	hfkv-cli SET greeting hello run one command and print the reply
package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"

	"github.com/hostfog/hfkv/internal/resp"
)

func main() {
	host := flag.String("h", "127.0.0.1", "server host")
	port := flag.Int("p", 6379, "server port")
	password := flag.String("a", os.Getenv("HFKV_PASSWORD"), "password")
	db := flag.Int("n", 0, "database number")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: hfkv-cli [-h host] [-p port] [-a password] [-n db] [command [arg ...]]\n")
		flag.PrintDefaults()
	}
	flag.Parse()

	addr := net.JoinHostPort(*host, strconv.Itoa(*port))
	c, err := connect(addr, *password, *db)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Could not connect to %s: %v\n", addr, err)
		os.Exit(1)
	}
	defer c.conn.Close()

	if flag.NArg() > 0 {
		reply, err := c.do(flag.Args()...)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Print(format(reply, ""))
		if _, isErr := reply.(replyError); isErr {
			os.Exit(1)
		}
		return
	}
	repl(c, addr)
}

type client struct {
	conn net.Conn
	r    *bufio.Reader
	w    *resp.Writer
}

func connect(addr, password string, db int) (*client, error) {
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		return nil, err
	}
	c := &client{conn: conn, r: bufio.NewReader(conn), w: resp.NewWriter(conn)}
	if password != "" {
		if r, err := c.do("AUTH", password); err != nil {
			return nil, err
		} else if e, ok := r.(replyError); ok {
			return nil, errors.New(string(e))
		}
	}
	if db != 0 {
		if r, err := c.do("SELECT", strconv.Itoa(db)); err != nil {
			return nil, err
		} else if e, ok := r.(replyError); ok {
			return nil, errors.New(string(e))
		}
	}
	return c, nil
}

func (c *client) do(args ...string) (any, error) {
	c.w.Command(args...)
	if err := c.w.Flush(); err != nil {
		return nil, err
	}
	return readReply(c.r)
}

func repl(c *client, addr string) {
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 1<<20), 64<<20)
	subscribed := false
	for {
		fmt.Printf("%s> ", addr)
		if !in.Scan() {
			fmt.Println()
			return
		}
		args, err := splitArgs(in.Text())
		if err != nil {
			fmt.Println("(error) " + err.Error())
			continue
		}
		if len(args) == 0 {
			continue
		}
		switch strings.ToLower(args[0]) {
		case "exit", "quit":
			return
		case "subscribe", "psubscribe":
			subscribed = true
		}
		reply, err := c.do(args...)
		if err != nil {
			fmt.Println("Connection lost:", err)
			return
		}
		fmt.Print(format(reply, ""))
		if subscribed {
			fmt.Println("Reading messages... (press Ctrl+C to quit)")
			for {
				reply, err := readReply(c.r)
				if err != nil {
					return
				}
				fmt.Print(format(reply, ""))
			}
		}
	}
}

type (
	replyError string
	status     string // a simple string such as OK or PONG
)

// readReply decodes one reply: status, string, int64, float64, nil,
// replyError or []any.
func readReply(r *bufio.Reader) (any, error) {
	line, err := r.ReadString('\n')
	if err != nil {
		return nil, err
	}
	line = strings.TrimSuffix(line, "\r\n")
	if line == "" {
		return nil, errors.New("empty reply")
	}
	body := line[1:]
	switch line[0] {
	case '+':
		return status(body), nil
	case '-':
		return replyError(body), nil
	case ':':
		return strconv.ParseInt(body, 10, 64)
	case ',':
		return strconv.ParseFloat(body, 64)
	case '_':
		return nil, nil
	case '$', '=':
		n, err := strconv.Atoi(body)
		if err != nil || n < 0 {
			return nil, err
		}
		buf := make([]byte, n+2)
		if _, err := io.ReadFull(r, buf); err != nil {
			return nil, err
		}
		return string(buf[:n]), nil
	case '*', '~', '>', '%':
		n, err := strconv.Atoi(body)
		if err != nil || n < 0 {
			return nil, err
		}
		if line[0] == '%' {
			n *= 2
		}
		items := make([]any, n)
		for i := range items {
			if items[i], err = readReply(r); err != nil {
				return nil, err
			}
		}
		return items, nil
	}
	return nil, fmt.Errorf("unexpected reply %q", line)
}

// format prints a reply the way redis-cli does.
func format(v any, indent string) string {
	switch v := v.(type) {
	case nil:
		return "(nil)\n"
	case status:
		return string(v) + "\n"
	case string:
		return strconv.Quote(v) + "\n"
	case int64:
		return fmt.Sprintf("(integer) %d\n", v)
	case float64:
		return fmt.Sprintf("(double) %s\n", strconv.FormatFloat(v, 'g', -1, 64))
	case replyError:
		return "(error) " + string(v) + "\n"
	case []any:
		if len(v) == 0 {
			return "(empty array)\n"
		}
		var b strings.Builder
		width := len(strconv.Itoa(len(v)))
		for i, item := range v {
			prefix := fmt.Sprintf("%*d) ", width, i+1)
			if i > 0 {
				b.WriteString(indent)
			}
			b.WriteString(prefix)
			b.WriteString(format(item, indent+strings.Repeat(" ", len(prefix))))
		}
		return b.String()
	}
	return fmt.Sprintf("%v\n", v)
}

// splitArgs splits a typed line into arguments, honouring quotes.
func splitArgs(line string) ([]string, error) {
	var (
		args []string
		cur  strings.Builder
		in   bool
	)
	for i := 0; i < len(line); i++ {
		ch := line[i]
		switch {
		case ch == ' ' || ch == '\t':
			if in {
				args = append(args, cur.String())
				cur.Reset()
				in = false
			}
		case ch == '"' || ch == '\'':
			end := strings.IndexByte(line[i+1:], ch)
			if end < 0 {
				return nil, errors.New("unbalanced quotes")
			}
			s := line[i+1 : i+1+end]
			if ch == '"' {
				if u, err := strconv.Unquote(`"` + s + `"`); err == nil {
					s = u
				}
			}
			cur.WriteString(s)
			in = true
			i += end + 1
		default:
			cur.WriteByte(ch)
			in = true
		}
	}
	if in {
		args = append(args, cur.String())
	}
	return args, nil
}
