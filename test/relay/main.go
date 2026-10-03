// relay mimics Console's /relay/webrelay.ashx KVM relay (controller/ws/v1 +
// usecase/devices/interceptor.go) closely enough to test fake-amt without
// building Console: same websocket handshake, same 64 KiB device reads, same
// interceptor functions (copied verbatim), same 30s inactivity cut-off.
package main

import (
	"flag"
	"log"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

var (
	listen = flag.String("listen", "127.0.0.1:8181", "listen address")
	device = flag.String("device", "127.0.0.1", "fake-amt host")
	user   = flag.String("user", "admin", "AMT user")
	pass   = flag.String("pass", "P@ssw0rd", "AMT password")
)

func main() {
	flag.Parse()
	http.HandleFunc("/relay/webrelay.ashx", handle)
	log.Printf("relay listening on %s -> %s:16994", *listen, *device)
	log.Fatal(http.ListenAndServe(*listen, nil))
}

func handle(w http.ResponseWriter, r *http.Request) {
	token := r.Header.Get("Sec-Websocket-Protocol")
	up := websocket.Upgrader{Subprotocols: []string{token}, CheckOrigin: func(*http.Request) bool { return true }}
	ws, err := up.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("upgrade: %v", err)
		return
	}
	defer ws.Close()
	log.Printf("websocket open host=%s mode=%s", r.URL.Query().Get("host"), r.URL.Query().Get("mode"))

	dev, err := net.Dial("tcp", net.JoinHostPort(*device, "16994"))
	if err != nil {
		log.Printf("device connect: %v", err)
		return
	}
	defer dev.Close()

	var mu sync.Mutex
	direct := false
	lastDataRecv := time.Now()
	challenge := &AuthChallenge{Username: *user, Password: *pass}
	done := make(chan struct{})
	var once sync.Once
	stop := func(why string) {
		once.Do(func() { log.Printf("session closing: %s", why); close(done); dev.Close(); ws.Close() })
	}

	go func() { // ListenToDevice
		buf := make([]byte, 64*1024)
		for {
			n, err := dev.Read(buf)
			if err != nil {
				stop("device read: " + err.Error())
				return
			}
			data := append([]byte(nil), buf[:n]...)
			mu.Lock()
			lastDataRecv = time.Now()
			d := direct
			mu.Unlock()
			toSend := data
			if !d {
				var nd bool
				toSend, nd = processDeviceData(toSend, challenge)
				mu.Lock()
				direct = nd
				mu.Unlock()
			}
			if err := ws.WriteMessage(websocket.BinaryMessage, toSend); err != nil {
				stop("browser write: " + err.Error())
				return
			}
		}
	}()

	go func() { // ListenToBrowser
		for {
			_, msg, err := ws.ReadMessage()
			if err != nil {
				stop("browser read: " + err.Error())
				return
			}
			mu.Lock()
			d := direct
			mu.Unlock()
			toSend := msg
			if !d {
				toSend = processBrowserData(msg, challenge)
			}
			if len(toSend) == 0 {
				continue
			}
			if _, err := dev.Write(toSend); err != nil {
				stop("device write: " + err.Error())
				return
			}
		}
	}()

	t := time.NewTicker(30 * time.Second) // MonitorConnectionHealth
	defer t.Stop()
	for {
		select {
		case <-done:
			return
		case <-t.C:
			mu.Lock()
			idle := time.Since(lastDataRecv)
			mu.Unlock()
			if idle > 30*time.Second {
				stop("INACTIVITY TIMEOUT (no device data for 30s) - this is what Console does")
				return
			}
		}
	}
}
