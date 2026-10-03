package main

import (
	"bufio"
	"bytes"
	"crypto/des"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"
)

// pixelFormat is an RFB PIXEL_FORMAT.
type pixelFormat struct {
	bpp, depth                      uint8
	bigEndian, trueColour           bool
	redMax, greenMax, blueMax       uint16
	redShift, greenShift, blueShift uint8
}

func (p pixelFormat) bytes() []byte {
	b := make([]byte, 16)
	b[0], b[1] = p.bpp, p.depth
	if p.bigEndian {
		b[2] = 1
	}
	if p.trueColour {
		b[3] = 1
	}
	binary.BigEndian.PutUint16(b[4:], p.redMax)
	binary.BigEndian.PutUint16(b[6:], p.greenMax)
	binary.BigEndian.PutUint16(b[8:], p.blueMax)
	b[10], b[11], b[12] = p.redShift, p.greenShift, p.blueShift
	return b
}

func parsePixelFormat(b []byte) pixelFormat {
	return pixelFormat{
		bpp: b[0], depth: b[1], bigEndian: b[2] != 0, trueColour: b[3] != 0,
		redMax: binary.BigEndian.Uint16(b[4:]), greenMax: binary.BigEndian.Uint16(b[6:]), blueMax: binary.BigEndian.Uint16(b[8:]),
		redShift: b[10], greenShift: b[11], blueShift: b[12],
	}
}

func (p pixelFormat) String() string {
	return fmt.Sprintf("%dbpp R%d<<%d G%d<<%d B%d<<%d", p.bpp, p.redMax, p.redShift, p.greenMax, p.greenShift, p.blueMax, p.blueShift)
}

var (
	// What the bridge asks the VNC server for: 32-bit little-endian xRGB.
	serverPF = pixelFormat{bpp: 32, depth: 24, trueColour: true, redMax: 255, greenMax: 255, blueMax: 255, redShift: 16, greenShift: 8}
	// What ui-toolkit assumes Intel AMT uses unless it sends SetPixelFormat (RGB565).
	amtDefaultPF = pixelFormat{bpp: 16, depth: 16, trueColour: true, redMax: 31, greenMax: 63, blueMax: 31, redShift: 11, greenShift: 5}
)

const (
	encRaw         = 0
	encDesktopSize = -223
	tileSize       = 64 // ui-toolkit rejects RAW/ZRLE rectangles larger than 64x64, as AMT always tiles at 64.
	kvmDataChannel = "\x00KvmDataChannel\x00"
)

var encDesktopSizeU32 = func() uint32 { v := int32(encDesktopSize); return uint32(v) }()

const (
	_ = 0
)

type kvmBridge struct {
	s     *redirSession
	vnc   net.Conn
	vncR  *bufio.Reader
	brR   *bufio.Reader // browser side (via Console)
	brMu  sync.Mutex    // serialises writes to the browser side
	brW   *bufio.Writer
	lastW time.Time

	mu       sync.Mutex
	clientPF pixelFormat
	width    uint16
	height   uint16
	name     string
	pointerN int
	lastBtns uint8
}

func runKVM(s *redirSession) error {
	cfg := s.cfg
	vnc, err := net.DialTimeout("tcp", cfg.vncAddr, 5*time.Second)
	if err != nil {
		return fmt.Errorf("cannot reach VNC server at %s: %w", cfg.vncAddr, err)
	}
	defer vnc.Close()
	if tc, ok := vnc.(*net.TCPConn); ok {
		_ = tc.SetNoDelay(true)
	}

	b := &kvmBridge{
		s:        s,
		vnc:      vnc,
		vncR:     bufio.NewReaderSize(vnc, 256<<10),
		brR:      s.r,
		brW:      bufio.NewWriterSize(s.conn, 256<<10),
		clientPF: amtDefaultPF,
	}
	if err := b.vncHandshake(); err != nil {
		return fmt.Errorf("VNC handshake with %s: %w", cfg.vncAddr, err)
	}
	s.logf("VNC connected: %q %dx%d", b.name, b.width, b.height)

	// Tell the browser the KVM channel is open, then play the RFB server role
	// the way Intel AMT does (RFB 3.8, security "None" because the AMT
	// redirection digest auth already happened).
	if err := b.sendBrowser(append([]byte{cmdKVMStartReply, 0, 0, 0, 0, 0, 0, 0}, "RFB 003.008\n"...)); err != nil {
		return err
	}
	if err := b.browserHandshake(); err != nil {
		return fmt.Errorf("RFB handshake with browser: %w", err)
	}
	s.logf("KVM session running (browser assumes %s until it says otherwise)", b.clientPF)

	errc := make(chan error, 3)
	done := make(chan struct{})
	go func() { errc <- b.pumpBrowserToVNC() }()
	go func() { errc <- b.pumpVNCToBrowser() }()
	if cfg.keepalive > 0 {
		go func() { errc <- b.keepalive(done) }()
	}
	err = <-errc
	close(done)
	_ = vnc.Close()
	_ = s.conn.Close()
	if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
		err = nil
	}
	s.logf("KVM session ended after %d pointer events", b.pointerN)
	return err
}

// ---- VNC server side (bridge acts as an RFB client) ------------------------

// vncEOFHints explains, per handshake step, the usual reasons a VNC server
// hangs up at that point.
var vncEOFHints = map[string]string{
	"waiting for the server's RFB version": "the server accepted the TCP connection then closed it without a word. Usual causes: " +
		"no VNC password set on the server (UltraVNC/TightVNC refuse all clients until one is), its access control / IP filter rejecting this address, " +
		"loopback connections disabled (when connecting via 127.0.0.1), or another viewer already connected with 'disconnect new clients' set",
	"waiting for the security types":     "the server closed after the version exchange. Usual cause: an encryption plugin (UltraVNC DSM / SecureVNC) is enabled - turn it off",
	"waiting for the VNC auth challenge": "the server closed before sending the password challenge. Usual cause: UltraVNC MS-Logon / Windows-account auth enabled - turn it off",
	"waiting for the auth result":        "the server closed after the password response. Usual cause: wrong -vnc-password, or the server is temporarily blocking this address after failed attempts",
	"waiting for ServerInit":             "the server closed after authentication. Usual cause: it only allows one viewer and is set to refuse new ones while one is connected",
}

func (b *kvmBridge) vncHandshake() (err error) {
	_ = b.vnc.SetDeadline(time.Now().Add(15 * time.Second))
	defer b.vnc.SetDeadline(time.Time{})

	step := "waiting for the server's RFB version"
	defer func() {
		if err == nil {
			return
		}
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || isConnReset(err) {
			err = fmt.Errorf("server closed the connection while %s: %s", step, vncEOFHints[step])
		} else {
			err = fmt.Errorf("%s: %w", step, err)
		}
	}()

	ver := make([]byte, 12)
	if _, err := io.ReadFull(b.vncR, ver); err != nil {
		return err
	}
	var major, minor int
	if _, err := fmt.Sscanf(string(ver), "RFB %03d.%03d\n", &major, &minor); err != nil || major != 3 {
		return fmt.Errorf("not an RFB server (got %q)", ver)
	}
	if minor >= 8 {
		minor = 8
	} else if minor >= 7 {
		minor = 7
	} else {
		minor = 3
	}
	b.s.logf("VNC server speaks %q, using RFB 3.%d", strings.TrimSpace(string(ver)), minor)
	if _, err := fmt.Fprintf(b.vnc, "RFB 003.%03d\n", minor); err != nil {
		return err
	}
	step = "waiting for the security types"

	var secType uint32
	if minor == 3 {
		if err := binary.Read(b.vncR, binary.BigEndian, &secType); err != nil {
			return err
		}
		if secType == 0 {
			return b.vncReason()
		}
	} else {
		n, err := b.vncR.ReadByte()
		if err != nil {
			return err
		}
		if n == 0 {
			return b.vncReason()
		}
		types := make([]byte, n)
		if _, err := io.ReadFull(b.vncR, types); err != nil {
			return err
		}
		b.s.logf("VNC server offers security types %v (1=None, 2=VNC password)", types)
		switch {
		case bytes.IndexByte(types, 1) >= 0:
			secType = 1
		case bytes.IndexByte(types, 2) >= 0:
			secType = 2
		default:
			return fmt.Errorf("server offers security types %v; only None (1) and VNC password (2) are supported", types)
		}
		if _, err := b.vnc.Write([]byte{byte(secType)}); err != nil {
			return err
		}
	}

	switch secType {
	case 1:
		step = "waiting for the auth result"
		if minor == 8 {
			if err := b.vncSecurityResult(minor); err != nil {
				return err
			}
		}
	case 2:
		if b.s.cfg.vncPassword == "" {
			return fmt.Errorf("the VNC server requires a password: pass -vnc-password")
		}
		step = "waiting for the VNC auth challenge"
		challenge := make([]byte, 16)
		if _, err := io.ReadFull(b.vncR, challenge); err != nil {
			return err
		}
		resp, err := vncAuthResponse(b.s.cfg.vncPassword, challenge)
		if err != nil {
			return err
		}
		if _, err := b.vnc.Write(resp); err != nil {
			return err
		}
		step = "waiting for the auth result"
		if err := b.vncSecurityResult(minor); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unsupported security type %d", secType)
	}

	// ClientInit: shared session, so a normal VNC viewer can watch alongside.
	step = "waiting for ServerInit"
	if _, err := b.vnc.Write([]byte{1}); err != nil {
		return err
	}
	si := make([]byte, 24)
	if _, err := io.ReadFull(b.vncR, si); err != nil {
		return err
	}
	b.width = binary.BigEndian.Uint16(si[0:])
	b.height = binary.BigEndian.Uint16(si[2:])
	nameLen := binary.BigEndian.Uint32(si[20:])
	if nameLen > 1<<16 {
		return fmt.Errorf("desktop name too long")
	}
	name := make([]byte, nameLen)
	if _, err := io.ReadFull(b.vncR, name); err != nil {
		return err
	}
	b.name = string(name)

	// Fix the server's pixel format and encodings; the bridge converts and
	// re-tiles everything for the browser.
	msg := append([]byte{0, 0, 0, 0}, serverPF.bytes()...)
	enc := []byte{2, 0, 0, 2}
	enc = binary.BigEndian.AppendUint32(enc, uint32(encRaw))
	enc = binary.BigEndian.AppendUint32(enc, encDesktopSizeU32)
	_, err = b.vnc.Write(append(msg, enc...))
	return err
}

func (b *kvmBridge) vncReason() error {
	var n uint32
	if err := binary.Read(b.vncR, binary.BigEndian, &n); err != nil {
		return err
	}
	reason := make([]byte, min(n, 4096))
	_, _ = io.ReadFull(b.vncR, reason)
	return fmt.Errorf("server refused connection: %s", reason)
}

func (b *kvmBridge) vncSecurityResult(minor int) error {
	var res uint32
	if err := binary.Read(b.vncR, binary.BigEndian, &res); err != nil {
		return err
	}
	if res != 0 {
		if minor == 8 {
			return b.vncReason()
		}
		return fmt.Errorf("VNC authentication failed (check -vnc-password)")
	}
	return nil
}

// vncAuthResponse implements classic VNC authentication: DES-encrypt the
// challenge with the password (max 8 chars) as key, bits of each key byte
// reversed.
func vncAuthResponse(password string, challenge []byte) ([]byte, error) {
	key := make([]byte, 8)
	copy(key, password)
	for i, c := range key {
		var r byte
		for j := 0; j < 8; j++ {
			if c&(1<<j) != 0 {
				r |= 1 << (7 - j)
			}
		}
		key[i] = r
	}
	block, err := des.NewCipher(key)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 16)
	block.Encrypt(out[:8], challenge[:8])
	block.Encrypt(out[8:], challenge[8:])
	return out, nil
}

// ---- browser side (bridge acts as an AMT RFB server) -----------------------

func (b *kvmBridge) sendBrowser(p []byte) error {
	b.brMu.Lock()
	defer b.brMu.Unlock()
	if _, err := b.brW.Write(p); err != nil {
		return err
	}
	b.lastW = time.Now()
	return b.brW.Flush()
}

func (b *kvmBridge) browserHandshake() error {
	ver := make([]byte, 12)
	if _, err := io.ReadFull(b.brR, ver); err != nil {
		return err
	}
	if !strings.HasPrefix(string(ver), "RFB 003.") {
		return fmt.Errorf("unexpected client version %q", ver)
	}
	if err := b.sendBrowser([]byte{1, 1}); err != nil { // one security type: None
		return err
	}
	if _, err := b.brR.ReadByte(); err != nil { // chosen type
		return err
	}
	if err := b.sendBrowser([]byte{0, 0, 0, 0}); err != nil { // SecurityResult OK
		return err
	}
	if _, err := b.brR.ReadByte(); err != nil { // ClientInit shared flag
		return err
	}
	si := make([]byte, 4, 64)
	binary.BigEndian.PutUint16(si[0:], b.width)
	binary.BigEndian.PutUint16(si[2:], b.height)
	si = append(si, amtDefaultPF.bytes()...)
	name := "fake-amt: " + b.name
	si = binary.BigEndian.AppendUint32(si, uint32(len(name)))
	si = append(si, name...)
	return b.sendBrowser(si)
}

func (b *kvmBridge) pumpBrowserToVNC() error {
	r := b.brR
	for {
		t, err := r.ReadByte()
		if err != nil {
			return err
		}
		switch t {
		case 0: // SetPixelFormat
			m, err := readN(r, 19)
			if err != nil {
				return err
			}
			pf := parsePixelFormat(m[3:19])
			if !pf.trueColour || (pf.bpp != 8 && pf.bpp != 16 && pf.bpp != 32) {
				return fmt.Errorf("browser asked for unsupported pixel format %s (truecolour=%v)", pf, pf.trueColour)
			}
			b.mu.Lock()
			b.clientPF = pf
			b.mu.Unlock()
			b.s.logf("browser SetPixelFormat %s", pf)

		case 2: // SetEncodings - the bridge controls server encodings itself
			m, err := readN(r, 3)
			if err != nil {
				return err
			}
			n := int(binary.BigEndian.Uint16(m[1:]))
			encs, err := readN(r, 4*n)
			if err != nil {
				return err
			}
			var list []string
			for i := 0; i < n; i++ {
				list = append(list, fmt.Sprint(int32(binary.BigEndian.Uint32(encs[4*i:]))))
			}
			b.s.logf("browser SetEncodings [%s] (bridge always sends RAW 64x64 tiles)", strings.Join(list, ","))

		case 3: // FramebufferUpdateRequest
			m, err := readN(r, 9)
			if err != nil {
				return err
			}
			if _, err := b.vnc.Write(append([]byte{3}, m...)); err != nil {
				return err
			}

		case 4: // KeyEvent
			m, err := readN(r, 7)
			if err != nil {
				return err
			}
			if b.s.cfg.traceKeys {
				b.s.logf("key %s keysym=0x%04x", map[bool]string{true: "down", false: "up  "}[m[0] != 0], binary.BigEndian.Uint32(m[3:]))
			}
			if _, err := b.vnc.Write(append([]byte{4}, m...)); err != nil {
				return err
			}

		case 5: // PointerEvent
			m, err := readN(r, 5)
			if err != nil {
				return err
			}
			b.tracePointer(m[0], binary.BigEndian.Uint16(m[1:]), binary.BigEndian.Uint16(m[3:]))
			if _, err := b.vnc.Write(append([]byte{5}, m...)); err != nil {
				return err
			}

		case 6: // ClientCutText
			m, err := readN(r, 7)
			if err != nil {
				return err
			}
			n := binary.BigEndian.Uint32(m[3:])
			if n > 16<<20 {
				return fmt.Errorf("client cut text too large")
			}
			text, err := readN(r, int(n))
			if err != nil {
				return err
			}
			if bytes.HasPrefix(text, []byte(kvmDataChannel)) {
				// Intel KVM data channel keep-alive/data: AMT acks it with an empty one.
				cut := []byte{3, 0, 0, 0}
				cut = binary.BigEndian.AppendUint32(cut, uint32(len(kvmDataChannel)))
				if err := b.sendBrowser(append(cut, kvmDataChannel...)); err != nil {
					return err
				}
				continue
			}
			if _, err := b.vnc.Write(append(append([]byte{6}, m...), text...)); err != nil {
				return err
			}

		default:
			return fmt.Errorf("unknown RFB client message type %d from browser", t)
		}
	}
}

func (b *kvmBridge) tracePointer(buttons uint8, x, y uint16) {
	b.mu.Lock()
	b.pointerN++
	w, h := b.width, b.height
	changed := buttons != b.lastBtns
	b.lastBtns = buttons
	b.mu.Unlock()

	oob := ""
	if x >= w || y >= h {
		oob = "  <-- OUTSIDE framebuffer"
	}
	switch b.s.cfg.tracePointer {
	case "all":
		b.s.logf("pointer x=%4d y=%4d buttons=%03b  fb=%dx%d%s", x, y, buttons, w, h, oob)
	case "clicks":
		if changed || oob != "" {
			b.s.logf("pointer x=%4d y=%4d buttons=%03b  fb=%dx%d%s", x, y, buttons, w, h, oob)
		}
	}
}

func (b *kvmBridge) pumpVNCToBrowser() error {
	r := b.vncR
	for {
		t, err := r.ReadByte()
		if err != nil {
			return err
		}
		switch t {
		case 0:
			if err := b.relayFramebufferUpdate(); err != nil {
				return err
			}
		case 1: // SetColourMapEntries - shouldn't happen in true-colour; skip it.
			m, err := readN(r, 5)
			if err != nil {
				return err
			}
			if _, err := readN(r, 6*int(binary.BigEndian.Uint16(m[3:]))); err != nil {
				return err
			}
		case 2: // Bell
			if err := b.sendBrowser([]byte{2}); err != nil {
				return err
			}
		case 3: // ServerCutText
			m, err := readN(r, 7)
			if err != nil {
				return err
			}
			n := binary.BigEndian.Uint32(m[3:])
			if n > 16<<20 {
				return fmt.Errorf("server cut text too large")
			}
			text, err := readN(r, int(n))
			if err != nil {
				return err
			}
			if err := b.sendBrowser(append(append([]byte{3}, m...), text...)); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unexpected message type %d from VNC server", t)
		}
	}
}

// relayFramebufferUpdate reads one FramebufferUpdate from the VNC server and
// re-emits it the way Intel AMT would: 64x64 RAW tiles in the browser's pixel
// format. Empty updates are dropped (ui-toolkit cannot handle 0 rectangles).
func (b *kvmBridge) relayFramebufferUpdate() error {
	r := b.vncR
	h, err := readN(r, 3)
	if err != nil {
		return err
	}
	nRects := int(binary.BigEndian.Uint16(h[1:]))

	b.mu.Lock()
	pf := b.clientPF
	b.mu.Unlock()
	outBpp := int(pf.bpp) / 8

	var out bytes.Buffer
	count := 0
	var rowBuf []byte

	for i := 0; i < nRects; i++ {
		rh, err := readN(r, 12)
		if err != nil {
			return err
		}
		x := int(binary.BigEndian.Uint16(rh[0:]))
		y := int(binary.BigEndian.Uint16(rh[2:]))
		w := int(binary.BigEndian.Uint16(rh[4:]))
		hh := int(binary.BigEndian.Uint16(rh[6:]))
		enc := int32(binary.BigEndian.Uint32(rh[8:]))

		switch enc {
		case encDesktopSize:
			b.mu.Lock()
			b.width, b.height = uint16(w), uint16(hh)
			b.mu.Unlock()
			b.s.logf("desktop resized to %dx%d", w, hh)
			out.Write(rh)
			count++

		case encRaw:
			need := w * hh * 4
			if cap(rowBuf) < need {
				rowBuf = make([]byte, need)
			}
			px := rowBuf[:need]
			if _, err := io.ReadFull(r, px); err != nil {
				return err
			}
			for ty := 0; ty < hh; ty += tileSize {
				th := min(tileSize, hh-ty)
				for tx := 0; tx < w; tx += tileSize {
					tw := min(tileSize, w-tx)
					var th12 [12]byte
					binary.BigEndian.PutUint16(th12[0:], uint16(x+tx))
					binary.BigEndian.PutUint16(th12[2:], uint16(y+ty))
					binary.BigEndian.PutUint16(th12[4:], uint16(tw))
					binary.BigEndian.PutUint16(th12[6:], uint16(th))
					out.Write(th12[:]) // encoding 0 = RAW
					for row := ty; row < ty+th; row++ {
						src := px[(row*w+tx)*4 : (row*w+tx+tw)*4]
						for p := 0; p < len(src); p += 4 {
							writePixel(&out, pf, outBpp, src[p+2], src[p+1], src[p])
						}
					}
					count++
				}
			}

		default:
			return fmt.Errorf("VNC server sent encoding %d which the bridge did not ask for", enc)
		}
	}

	if count == 0 {
		return nil
	}
	data := out.Bytes()
	// FramebufferUpdate's rectangle count is 16-bit; split if needed (only at
	// very high resolutions). Each chunk is a self-contained update.
	for count > 0 {
		n := min(count, 65535)
		end := len(data)
		if n < count {
			end = offsetAfterRects(data, n, outBpp)
		}
		hdr := []byte{0, 0, byte(n >> 8), byte(n)}
		if err := b.sendBrowser(append(hdr, data[:end]...)); err != nil {
			return err
		}
		data = data[end:]
		count -= n
	}
	return nil
}

func offsetAfterRects(data []byte, n, bpp int) int {
	off := 0
	for i := 0; i < n; i++ {
		w := int(binary.BigEndian.Uint16(data[off+4:]))
		h := int(binary.BigEndian.Uint16(data[off+6:]))
		enc := int32(binary.BigEndian.Uint32(data[off+8:]))
		off += 12
		if enc == encRaw {
			off += w * h * bpp
		}
	}
	return off
}

func writePixel(out *bytes.Buffer, pf pixelFormat, nbytes int, r, g, bl byte) {
	v := (uint32(r)*uint32(pf.redMax)+127)/255<<pf.redShift |
		(uint32(g)*uint32(pf.greenMax)+127)/255<<pf.greenShift |
		(uint32(bl)*uint32(pf.blueMax)+127)/255<<pf.blueShift
	switch nbytes {
	case 1:
		out.WriteByte(byte(v))
	case 2:
		if pf.bigEndian {
			out.Write([]byte{byte(v >> 8), byte(v)})
		} else {
			out.Write([]byte{byte(v), byte(v >> 8)})
		}
	default:
		if pf.bigEndian {
			out.Write([]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)})
		} else {
			out.Write([]byte{byte(v), byte(v >> 8), byte(v >> 16), byte(v >> 24)})
		}
	}
}

// keepalive sends an RFB Bell when nothing has gone to the browser for a
// while. Console closes a relay after 30s without device data, which a
// static VM desktop would otherwise trigger; ui-toolkit ignores bells.
func (b *kvmBridge) keepalive(done <-chan struct{}) error {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-done:
			return nil
		case <-t.C:
			b.brMu.Lock()
			idle := time.Since(b.lastW)
			b.brMu.Unlock()
			if idle >= b.s.cfg.keepalive {
				if err := b.sendBrowser([]byte{2}); err != nil {
					return err
				}
			}
		}
	}
}

func readN(r io.Reader, n int) ([]byte, error) {
	b := make([]byte, n)
	_, err := io.ReadFull(r, b)
	return b, err
}

func isConnReset(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "connection reset") || strings.Contains(msg, "forcibly closed") || strings.Contains(msg, "connection was aborted")
}
