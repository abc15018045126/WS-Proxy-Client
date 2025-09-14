package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"regexp"
	"time"

	"github.com/gorilla/websocket"
	utls "github.com/refraction-networking/utls"
)

var errLogger *log.Logger

var (
	port    = flag.Int("port", 8080, "HTTP代理端口,可选值:1-65535")
	passwd  = flag.String("pwd", "testPASSword", "密码")
	wssHost = flag.String("wss", "", "websocket地址,[域名]:[端口](非443)")
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

// 自定义 TLS 指纹
func utlsDialTLSContext(ctx context.Context, network, addr string) (net.Conn, error) {

	// 标准 Dialer 建立 TCP 连接
	var d net.Dialer
	tcpConn, err := d.DialContext(ctx, network, addr)
	if err != nil {
		Debug(err)
		return nil, err
	}

	// 尝试去除端口，获取 SNI
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		Debug(err)
		host = addr
	}

	// 创建 uTLS 客户端连接（模拟浏览器）
	uconn := utls.UClient(tcpConn, &utls.Config{ServerName: host}, utls.HelloRandomized)

	// 设置超时，避免一直等待
	if dl, ok := ctx.Deadline(); ok {
		_ = uconn.SetDeadline(dl)
	}

	// tls 握手
	if err := uconn.Handshake(); err != nil {
		Debug(err)
		tcpConn.Close()
		return nil, err
	}

	// 握手后清除超时设置
	_ = uconn.SetDeadline(time.Time{})

	return uconn, nil
}

// 建立隧道
func SetUpTunnel(client net.Conn, target string) {
	defer client.Close()

	header := make(http.Header)
	header.Set("X-Target", target)
	header.Set("X-Password", *passwd)

	// 替换 Dialer
	dialer := websocket.Dialer{
		NetDialTLSContext: utlsDialTLSContext,
		HandshakeTimeout:  30 * time.Second,
	}

	ws, resp, err := dialer.Dial("wss://"+*wssHost, header)
	if err != nil {
		Debug(err)
		if resp != nil {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			log.Fatalln("连接websocket出错:", string(body))
		}
		return
	}
	defer ws.Close()

	PipeConn(ws, client)
}

func main() {
	flag.Parse()

	regx := regexp.MustCompile(`^[a-zA-Z0-9.-]+(:\d+)?(/.*)?$`)
	if !regx.MatchString(*wssHost) {
		log.Fatalln("websocket地址,[域名]:[端口](非443)")
	}

	if *port < 1 || *port > 65535 {
		log.Fatalln("HTTP代理端口,可选值:1-65535")
	}

	if *ckSize < 1 || *ckSize > 1024 {
		log.Fatalln("websocket每一帧的数据大小(KB),可选值:1-1024")
	}

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			w.WriteHeader(http.StatusServiceUnavailable)
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

		log.Printf("访问: %s\n", r.Host)

		go SetUpTunnel(client, r.Host)
	})

	addr := fmt.Sprintf(":%d", *port)

	log.Printf("开启HTTP代理,端口:%d", *port)

	if err := http.ListenAndServe(addr, handler); err != nil {
		Debug(err)
		log.Fatal("开启HTTP代理失败:", err)
	}
}
