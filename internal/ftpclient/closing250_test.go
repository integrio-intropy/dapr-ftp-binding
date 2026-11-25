package ftpclient

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/textproto"
	"strings"
	"testing"
	"time"
)

// closing250Server is a scripted FTP server that ends every data transfer with
// "250" instead of "226", as IBM i does ("125 List started." then
// "250 List completed."). RFC 959 allows either; ftpserverlib always sends 226,
// so this case needs its own server.
type closing250Server struct {
	ln      net.Listener
	listing string
	file    []byte
	stored  chan []byte
}

func startClosing250Server(t *testing.T, listing string, file []byte) *closing250Server {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &closing250Server{ln: ln, listing: listing, file: file, stored: make(chan []byte, 1)}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go s.serve(c)
		}
	}()
	return s
}

func (s *closing250Server) serve(c net.Conn) {
	defer func() { _ = c.Close() }()
	r := bufio.NewReader(c)
	reply := func(format string, a ...any) { _, _ = fmt.Fprintf(c, format+"\r\n", a...) }
	var data net.Listener
	defer func() {
		if data != nil {
			_ = data.Close()
		}
	}()
	accept := func() net.Conn {
		if data == nil {
			return nil
		}
		dc, err := data.Accept()
		_ = data.Close()
		data = nil
		if err != nil {
			return nil
		}
		return dc
	}

	reply("220 test server ready")
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		cmd, _, _ := strings.Cut(strings.TrimRight(line, "\r\n"), " ")
		switch strings.ToUpper(cmd) {
		case "USER":
			reply("331 password required")
		case "PASS":
			reply("230 logged in")
		case "FEAT":
			// No MLST, so the client falls back to LIST.
			reply("211-Features:\r\n SIZE\r\n211 End")
		case "TYPE":
			reply("200 type set")
		case "EPSV", "PASV":
			if data, err = net.Listen("tcp", "127.0.0.1:0"); err != nil {
				reply("425 cannot open data connection")
				continue
			}
			port := data.Addr().(*net.TCPAddr).Port
			if strings.EqualFold(cmd, "EPSV") {
				reply("229 Entering Extended Passive Mode (|||%d|)", port)
			} else {
				reply("227 Entering Passive Mode (127,0,0,1,%d,%d)", port/256, port%256)
			}
		case "LIST":
			reply("125 List started.")
			if dc := accept(); dc != nil {
				_, _ = io.WriteString(dc, s.listing)
				_ = dc.Close()
			}
			reply("250 List completed.")
		case "RETR":
			reply("125 Data connection already open; transfer starting.")
			if dc := accept(); dc != nil {
				_, _ = dc.Write(s.file)
				_ = dc.Close()
			}
			reply("250 Transfer completed.")
		case "STOR":
			reply("125 Data connection already open; transfer starting.")
			if dc := accept(); dc != nil {
				b, _ := io.ReadAll(dc)
				_ = dc.Close()
				s.stored <- b
			}
			reply("250 Transfer completed.")
		case "QUIT":
			reply("221 bye")
			return
		default:
			reply("502 not implemented")
		}
	}
}

func TestDataTransfersAcceptA250ClosingReply(t *testing.T) {
	const listing = "-rw-r--r--   1 owner    group         12 Nov 24 10:00 order.xml\r\n" +
		"drwxr-xr-x   2 owner    group          0 Nov 24 10:00 archive\r\n"
	s := startClosing250Server(t, listing, binaryPayload)

	c, err := NewDialer(DialerConfig{
		Address:    s.ln.Addr().String(),
		Username:   testUser,
		Password:   testPass,
		Timeout:    10 * time.Second,
		TargetName: "test://250",
	}).Dial(context.Background())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	ctx := context.Background()

	entries, err := c.List(ctx, "/export/folder")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(entries) != 2 || !hasEntry(entries, "order.xml") || !hasEntry(entries, "archive") {
		t.Fatalf("list: unexpected entries %+v", entries)
	}

	var buf bytes.Buffer
	n, err := c.Retrieve(ctx, "/export/folder/order.xml", &buf)
	if err != nil {
		t.Fatalf("retrieve: %v", err)
	}
	if n != int64(len(binaryPayload)) || !bytes.Equal(buf.Bytes(), binaryPayload) {
		t.Fatalf("retrieve: got %q", buf.Bytes())
	}

	if err := c.Store(ctx, "/export/folder/out.xml", bytes.NewReader(binaryPayload)); err != nil {
		t.Fatalf("store: %v", err)
	}
	if got := <-s.stored; !bytes.Equal(got, binaryPayload) {
		t.Fatalf("store: server received %q", got)
	}

	if c.(*jlConn).Broken() {
		t.Fatal("a 250 closing reply marked the session broken")
	}
}

func TestAcceptClosingReply(t *testing.T) {
	r250 := &textproto.Error{Code: 250, Msg: "List completed."}
	r451 := &textproto.Error{Code: 451, Msg: "Local error"}
	other := errors.New("read: connection reset")

	if err := acceptClosingReply(nil); err != nil {
		t.Errorf("nil: got %v", err)
	}
	if err := acceptClosingReply(errors.Join(r250)); err != nil {
		t.Errorf("joined 250: got %v", err)
	}
	if err := acceptClosingReply(errors.Join(r451)); !errors.Is(err, r451) {
		t.Errorf("joined 451: got %v", err)
	}
	if err := acceptClosingReply(errors.Join(other, r250)); !errors.Is(err, other) || errors.Is(err, r250) {
		t.Errorf("other + 250: got %v, want only the other error", err)
	}
	// A bare 250 is not the library's closing reply, so it stays an error.
	if err := acceptClosingReply(r250); !errors.Is(err, r250) {
		t.Errorf("bare 250: got %v", err)
	}
}
