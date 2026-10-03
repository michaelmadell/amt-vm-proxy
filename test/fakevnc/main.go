// fakevnc is a tiny RFB 3.8 server for testing fake-amt. It serves a
// deterministic pixel pattern as large RAW rectangles (to exercise the
// bridge's 64x64 re-tiling), can resize the desktop mid-session, and prints
// every input event it receives as a JSON line on stdout.
package main

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"sync"
	"time"
)

var (
	addr        = flag.String("listen", "127.0.0.1:5901", "listen address")
	width       = flag.Int("w", 1280, "initial width")
	height      = flag.Int("h", 800, "initial height")
	resizeAfter = flag.Int("resize-after", 3, "after this many update requests, resize desktop to -rw x -rh (0 = never)")
	rw          = flag.Int("rw", 1024, "resize width")
	rh          = flag.Int("rh", 768, "resize height")
	static      = flag.Bool("static", false, "after the resize, ignore incremental update requests (an idle desktop)")
)

// Pattern returns the RGB the test expects at (x,y) for desktop generation gen.
func Pattern(x, y, gen int) (r, g, b byte) {
	return byte(x*3 + gen*40), byte(y * 5), byte(x ^ y)
}

var outMu sync.Mutex

func emit(v map[string]any) {
	outMu.Lock()
	defer outMu.Unlock()
	b, _ := json.Marshal(v)
	fmt.Println(string(b))
}

func main() {
	flag.Parse()
	log.SetOutput(os.Stderr)
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("fakevnc listening on %s (%dx%d)", *addr, *width, *height)
	for {
		c, err := ln.Accept()
		if err != nil {
			log.Fatal(err)
		}
		go serve(c)
	}
}

type pf struct {
	bpp                    int
	big                    bool
	rmax, gmax, bmax       uint32
	rshift, gshift, bshift uint
}

func serve(c net.Conn) {
	defer c.Close()
	r := bufio.NewReader(c)
	w := bufio.NewWriter(c)
	var wmu sync.Mutex
	send := func(b []byte) error {
		wmu.Lock()
		defer wmu.Unlock()
		if _, err := w.Write(b); err != nil {
			return err
		}
		return w.Flush()
	}

	W, H, gen := *width, *height, 0
	format := pf{bpp: 32, rmax: 255, gmax: 255, bmax: 255, rshift: 16, gshift: 8}

	_ = send([]byte("RFB 003.008\n"))
	ver := make([]byte, 12)
	if _, err := io.ReadFull(r, ver); err != nil {
		return
	}
	_ = send([]byte{1, 1})
	if _, err := r.ReadByte(); err != nil {
		return
	}
	_ = send([]byte{0, 0, 0, 0})
	if _, err := r.ReadByte(); err != nil { // shared
		return
	}
	si := make([]byte, 4)
	binary.BigEndian.PutUint16(si[0:], uint16(W))
	binary.BigEndian.PutUint16(si[2:], uint16(H))
	si = append(si, 32, 24, 0, 1, 0, 255, 0, 255, 0, 255, 16, 8, 0, 0, 0, 0)
	name := "fakevnc test pattern"
	si = binary.BigEndian.AppendUint32(si, uint32(len(name)))
	si = append(si, name...)
	_ = send(si)
	emit(map[string]any{"ev": "connected"})

	updates := 0
	needFull := true
	resized := false
	pixel := func(x, y int) []byte {
		cr, cg, cb := Pattern(x, y, gen)
		v := (uint32(cr)*format.rmax/255)<<format.rshift | (uint32(cg)*format.gmax/255)<<format.gshift | (uint32(cb)*format.bmax/255)<<format.bshift
		out := make([]byte, format.bpp/8)
		for i := range out {
			if format.big {
				out[len(out)-1-i] = byte(v >> (8 * i))
			} else {
				out[i] = byte(v >> (8 * i))
			}
		}
		return out
	}
	rect := func(x, y, rw, rh int) []byte {
		b := make([]byte, 12, 12+rw*rh*format.bpp/8)
		binary.BigEndian.PutUint16(b[0:], uint16(x))
		binary.BigEndian.PutUint16(b[2:], uint16(y))
		binary.BigEndian.PutUint16(b[4:], uint16(rw))
		binary.BigEndian.PutUint16(b[6:], uint16(rh))
		for yy := y; yy < y+rh; yy++ {
			for xx := x; xx < x+rw; xx++ {
				b = append(b, pixel(xx, yy)...)
			}
		}
		return b
	}

	for {
		t, err := r.ReadByte()
		if err != nil {
			emit(map[string]any{"ev": "disconnected"})
			return
		}
		switch t {
		case 0:
			m := make([]byte, 19)
			io.ReadFull(r, m)
			p := m[3:]
			format = pf{bpp: int(p[0]), big: p[2] != 0,
				rmax: uint32(binary.BigEndian.Uint16(p[4:])), gmax: uint32(binary.BigEndian.Uint16(p[6:])), bmax: uint32(binary.BigEndian.Uint16(p[8:])),
				rshift: uint(p[10]), gshift: uint(p[11]), bshift: uint(p[12])}
			emit(map[string]any{"ev": "pixelformat", "bpp": format.bpp, "rshift": format.rshift, "gshift": format.gshift})
		case 2:
			m := make([]byte, 3)
			io.ReadFull(r, m)
			n := int(binary.BigEndian.Uint16(m[1:]))
			e := make([]byte, 4*n)
			io.ReadFull(r, e)
			var encs []int32
			for i := 0; i < n; i++ {
				encs = append(encs, int32(binary.BigEndian.Uint32(e[4*i:])))
			}
			emit(map[string]any{"ev": "encodings", "list": encs})
		case 3:
			m := make([]byte, 9)
			io.ReadFull(r, m)
			incremental := m[0] != 0
			updates++
			if *resizeAfter > 0 && updates == *resizeAfter+1 && (W != *rw || H != *rh) {
				{
					W, H, gen = *rw, *rh, gen+1
					hdr := []byte{0, 0, 0, 1, 0, 0, 0, 0}
					hdr = binary.BigEndian.AppendUint16(hdr, uint16(W))
					hdr = binary.BigEndian.AppendUint16(hdr, uint16(H))
					hdr = binary.BigEndian.AppendUint32(hdr, uint32(0xFFFFFF21))
					send(hdr)
					emit(map[string]any{"ev": "resized", "w": W, "h": H})
					needFull = true
					resized = true
					continue // client asks for a fresh frame itself
				}
			}
			if !incremental || needFull {
				needFull = false
				// Whole screen as ONE big RAW rectangle.
				msg := append([]byte{0, 0, 0, 1}, rect(0, 0, W, H)...)
				send(msg)
				emit(map[string]any{"ev": "full", "w": W, "h": H, "gen": gen})
			} else if *static && resized {
				// idle desktop: nothing changed, so no reply
			} else {
				// Small change, deliberately not tile-aligned.
				time.Sleep(100 * time.Millisecond)
				msg := append([]byte{0, 0, 0, 1}, rect(37, 29, 150, 90)...)
				send(msg)
			}
		case 4:
			m := make([]byte, 7)
			io.ReadFull(r, m)
			emit(map[string]any{"ev": "key", "down": m[0] != 0, "keysym": binary.BigEndian.Uint32(m[3:])})
		case 5:
			m := make([]byte, 5)
			io.ReadFull(r, m)
			emit(map[string]any{"ev": "pointer", "buttons": m[0], "x": binary.BigEndian.Uint16(m[1:]), "y": binary.BigEndian.Uint16(m[3:]), "fbw": W, "fbh": H})
		case 6:
			m := make([]byte, 7)
			io.ReadFull(r, m)
			n := binary.BigEndian.Uint32(m[3:])
			txt := make([]byte, n)
			io.ReadFull(r, txt)
			emit(map[string]any{"ev": "cuttext", "text": string(txt)})
		default:
			log.Printf("unknown client message %d", t)
			return
		}
	}
}
