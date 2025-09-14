package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	utls "github.com/refraction-networking/utls"
)

var errLogger *log.Logger

var (
	port    = flag.Int("port", 8080, "HTTP代理端口,可选值:1-65535")
	wssHost = flag.String("wss", "c-a82.pages.dev", "websocket地址,[域名]:[端口](非443)")
	userID  = flag.String("uuid", "2ea73714-138e-4cc7-8cab-d7caf476d51b", "VLESS用户ID")
	ckSize  = flag.Int("chunk", 64, "websocket每一帧的数据大小(KB),可选值:1-1024")
	debug   = flag.Bool("debug", false, "是否输出调试信息")
)

func Debug(err error) {
	if *debug {
		if errLogger == nil {
			errLogger = log.New(os.Stderr, "\033[31m[ERROR]\033[0m ", log.LstdFlags|log.Lshortfile)
		}
		errLogger.Println(err)
	}
}

// TCP <-> pipe <-> Websocket
// (This function is reused from main.go without changes)
func PipeConn(ws *websocket.Conn, conn net.Conn) {
	buf := make([]byte, *ckSize*1024)

	// Websocket to TCP
	go func() {
		defer conn.Close()
		for {
			mt, r, err := ws.NextReader()
			if err != nil {
				Debug(err)
				return
			}
			if mt != websocket.BinaryMessage {
				io.Copy(io.Discard, r)
				continue
			}
			if _, err := io.CopyBuffer(conn, r, buf); err != nil {
				Debug(err)
				return
			}
		}
	}()

	// TCP to Websocket
	for {
		n, err := conn.Read(buf)
			if err != nil {
				Debug(err)
				return
			}

			if n > 0 {
				w, err := ws.NextWriter(websocket.BinaryMessage)
				if err != nil {
					Debug(err)
					return
				}

				if _, err = w.Write(buf[:n]); err != nil {
					Debug(err)
					w.Close()
					return
				}
				w.Close()
			}
		}
}

// utlsDialTLSContext is reused from main.go without changes
func utlsDialTLSContext(ctx context.Context, network, addr string) (net.Conn, error) {
	var d net.Dialer
	tcpConn, err := d.DialContext(ctx, network, addr)
	if err != nil {
		Debug(err)
		return nil, err
	}

	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		Debug(err)
		host = addr
	}

	uconn := utls.UClient(tcpConn, &utls.Config{ServerName: host}, utls.HelloRandomized)

	if dl, ok := ctx.Deadline(); ok {
		_ = uconn.SetDeadline(dl)
	}

	if err := uconn.Handshake(); err != nil {
		Debug(err)
		tcpConn.Close()
		return nil, err
	}

	_ = uconn.SetDeadline(time.Time{})

	return uconn, nil
}

// createVlessHeader creates the VLESS protocol header.
func createVlessHeader(userID string, host string, port uint16) ([]byte, error) {
	uid, err := uuid.Parse(userID)
	if err != nil {
		return nil, fmt.Errorf("无效的UUID: %w", err)
	}

	var buf bytes.Buffer

	// 1. VLESS Version (1 byte)
	buf.WriteByte(0)

	// 2. UUID (16 bytes)
	buf.Write(uid[:])

	// 3. Addons (1 byte length = 0)
	buf.WriteByte(0)

	// 4. Command (1 byte = 1 for TCP)
	buf.WriteByte(1)

	// 5. Port (2 bytes, big-endian)
	portBytes := make([]byte, 2)
	binary.BigEndian.PutUint16(portBytes, port)
	buf.Write(portBytes)

	// 7. Address Type & Address
	ip := net.ParseIP(host)
	if ip != nil {
		ipv4 := ip.To4()
		if ipv4 != nil {
			// IPv4 (1 byte type = 1, 4 bytes address)
			buf.WriteByte(1)
			buf.Write(ipv4)
		} else {
			// IPv6 (1 byte type = 3, 16 bytes address)
			buf.WriteByte(3)
			buf.Write(ip.To16())
		}
	} else {
		// Domain name (1 byte type = 2, 1 byte length, variable length domain)
		if len(host) > 255 {
			return nil, fmt.Errorf("目标域名过长: %s", host)
		}
		buf.WriteByte(2)
		buf.WriteByte(byte(len(host)))
		buf.WriteString(host)
	}

	return buf.Bytes(), nil
}

// SetUpVlessTunnel establishes a tunnel using the VLESS protocol.
func SetUpVlessTunnel(client net.Conn, target string) {
	defer client.Close()

	// Use the same uTLS dialer
	dialer := websocket.Dialer{
		NetDialTLSContext: utlsDialTLSContext,
		HandshakeTimeout:  30 * time.Second,
	}

	// Connect to the WebSocket server without custom headers
	ws, resp, err := dialer.Dial("wss://"+*wssHost, nil)
	if err != nil {
		Debug(err)
		if resp != nil {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			log.Printf("连接websocket出错: %s", string(body))
		}
		return
	}
	defer ws.Close()

	// --- VLESS Protocol Logic ---
	host, portStr, err := net.SplitHostPort(target)
	if err != nil {
		Debug(err)
		return
	}
	port, err := strconv.ParseUint(portStr, 10, 16)
	if err != nil {
		Debug(err)
		return
	}

	// Create the VLESS header
	vlessHeader, err := createVlessHeader(*userID, host, uint16(port))
	if err != nil {
		Debug(err)
		return
	}

	// Send the VLESS header as the first message
	if err := ws.WriteMessage(websocket.BinaryMessage, vlessHeader); err != nil {
		Debug(err)
		return
	}

	// Wait for the server's response (VLESS handshake)
	// The server should respond with [version, 0]. We can read and discard it.
	ws.SetReadDeadline(time.Now().Add(10 * time.Second))
	_, _, err = ws.ReadMessage()
	if err != nil {
		Debug(err)
		return
	}
	ws.SetReadDeadline(time.Time{}) // Clear read deadline

	// --- End of VLESS Logic ---

	// The rest of the traffic is piped directly
	PipeConn(ws, client)
}

func main() {
	flag.Parse()

	if *wssHost == "" {
		log.Fatalln("必须提供websocket地址, 例如: -wss=example.com")
	}
	if _, err := uuid.Parse(*userID); err != nil {
		log.Fatalln("无效的VLESS用户ID (UUID)")
	}
	if *port < 1 || *port > 65535 {
		log.Fatalln("HTTP代理端口,可选值:1-65535")
	}
	if *ckSize < 1 || *ckSize > 1024 {
		log.Fatalln("websocket每一帧的数据大小(KB),可选值:1-1024")
	}

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		hijacker, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "不支持 Hijacking", http.StatusInternalServerError)
			return
		}

		client, _, err := hijacker.Hijack()
		if err != nil {
			Debug(err)
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}

		client.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))

		log.Printf("VLESS 访问: %s", r.Host)

		// Use the new VLESS tunnel function
		go SetUpVlessTunnel(client, r.Host)
	})

	addr := fmt.Sprintf(":%d", *port)
	log.Printf("开启HTTP代理, 端口:%d", *port)
	log.Printf("VLESS UUID: %s", *userID)
	log.Printf("远程WSS服务器: %s", *wssHost)

	if err := http.ListenAndServe(addr, handler); err != nil {
		Debug(err)
		log.Fatal("开启HTTP代理失败:", err)
	}
}
