package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// IDE-Redirection (IDER). Real AMT exposes the browser's disk image to the
// host as an IDE/ATAPI CD drive and forwards the host's SCSI packets to the
// browser, which answers from the local file. fake-amt plays the AMT *and*
// host side: it opens the session, enables the registers, then issues the
// SCSI commands a booting host would (TEST UNIT READY, READ CAPACITY,
// READ(10)) so ui-toolkit's IDER client runs exactly as it would against
// real hardware.
//
// With -hyperv-vm, fake-amt also reads the whole image through IDER, saves it
// as an .iso and inserts it into that Hyper-V VM's DVD drive; ending the IDER
// session ejects it again.
//
// Message layout (both directions): cmd, 2 reserved, attributes, uint32 LE
// sequence number, then a command-specific payload. The browser checks that
// our sequence numbers start at 0 and increase by one.

// Browser -> AMT.
const (
	iderOpenSession        = 0x40
	iderKeepAlivePong      = 0x45
	iderResetOccurredResp  = 0x47
	iderDisableEnable      = 0x48
	iderHeartbeat          = 0x4b
	iderCommandEndResponse = 0x51
	iderGetDataFromHost    = 0x52
	iderDataToHost         = 0x54
)

// AMT -> browser.
const (
	iderOpenSessionReply = 0x41
	iderStatusData       = 0x49
	iderCommandWritten   = 0x50
)

const (
	iderDevCD        = 0xb0 // drive/head register value for the ATAPI CD on the secondary channel
	cdSectorSize     = 2048
	iderReadBuffer   = 8192 // largest buffer ui-toolkit accepts; it splits reads into chunks this size
	iderPullSectors  = 512  // READ(10) size while copying the image for Hyper-V (1 MiB)
	iderCmdTimeout   = 60 * time.Second
	statusRegsToggle = 3
	statusRegsStatus = 2
	attrCompleted    = 0x02
	ataStatusErr     = 0x01
)

type iderMsg struct {
	cmd     byte
	attr    byte
	payload []byte // everything after the 8-byte header
}

type iderSession struct {
	s       *redirSession
	wmu     sync.Mutex
	outSeq  uint32
	lastW   time.Time
	msgs    chan iderMsg
	readErr chan error
	quit    chan struct{} // closed when the session ends, so readLoop can't block forever
}

func runIDER(s *redirSession, open []byte) error {
	d := &iderSession{s: s, msgs: make(chan iderMsg, 64), readErr: make(chan error, 1), quit: make(chan struct{})}
	defer close(d.quit)
	// open = OPEN_SESSION payload: rx timeout, tx timeout, heartbeat (uint16 LE each), version (uint32 LE).
	s.logf("IDER OPEN_SESSION (heartbeat %dms, version %d)",
		binary.LittleEndian.Uint16(open[4:6]), binary.LittleEndian.Uint32(open[6:10]))

	reply := make([]byte, 22)
	reply[0], reply[1] = 1, 0 // protocol version 1.0
	reply[2], reply[3] = 11, 8
	binary.LittleEndian.PutUint16(reply[8:10], iderReadBuffer)
	binary.LittleEndian.PutUint16(reply[10:12], iderReadBuffer)
	reply[13] = 0                                    // protocol 0 (the only one ui-toolkit accepts)
	binary.LittleEndian.PutUint32(reply[17:21], 343) // Intel's IANA number
	if err := d.send(iderOpenSessionReply, 0, reply); err != nil {
		return err
	}

	go d.readLoop()
	done := make(chan struct{})
	defer close(done)
	if s.cfg.keepalive > 0 {
		go d.heartbeat(done)
	}

	err := d.drive()
	if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) || (err != nil && isConnReset(err)) {
		err = nil
	}
	s.logf("IDER session ended")
	return err
}

// drive is the "host": enable IDER, probe the CD, optionally copy it into
// Hyper-V, then idle until the browser ends the session (= unmount).
func (d *iderSession) drive() error {
	s := d.s
	m, err := d.await(iderDisableEnable)
	if err != nil {
		return err
	}
	if len(m.payload) >= 5 && m.payload[0] == statusRegsToggle {
		v := binary.LittleEndian.Uint32(m.payload[1:5])
		mode := map[uint32]string{1: "on next reboot", 2: "graceful", 3: "now"}[(v>>3)&3]
		s.logf("IDER enable requested (start %s); fake-amt exposes the drive immediately", mode)
	}
	if err := d.sendStatus(statusRegsToggle, 1); err != nil {
		return err
	}
	if err := d.sendStatus(statusRegsStatus, 0x03); err != nil {
		return err
	}

	sectors, err := d.probe()
	if err != nil {
		return err
	}

	var iso string
	if sectors > 0 && s.cfg.hypervVM != "" {
		iso, err = d.pull(sectors)
		if err != nil {
			return err
		}
		defer os.Remove(iso)
		if err := hypervSetDVD(s.cfg.hypervVM, iso); err != nil {
			s.logf("Hyper-V: inserting the image into %q FAILED: %v", s.cfg.hypervVM, err)
			iso = ""
		} else {
			s.logf("Hyper-V: image inserted into %q's DVD drive; end the IDER session (unmount) to eject", s.cfg.hypervVM)
			defer func() {
				if err := hypervSetDVD(s.cfg.hypervVM, ""); err != nil {
					s.logf("Hyper-V: ejecting from %q FAILED: %v", s.cfg.hypervVM, err)
				} else {
					s.logf("Hyper-V: ejected from %q", s.cfg.hypervVM)
				}
			}()
		}
	}

	s.logf("IDER idle: drive stays mounted until the browser ends the session")
	for {
		select {
		case m := <-d.msgs:
			d.logUnsolicited(m)
		case err := <-d.readErr:
			return err
		}
	}
}

// probe does what a host does when a CD appears: wait for the unit to be
// ready, read its capacity and the ISO 9660 volume descriptor.
func (d *iderSession) probe() (sectors uint32, err error) {
	s := d.s
	for i := 0; i < 3; i++ {
		st, err := d.command(cdb(0x00), nil) // TEST UNIT READY
		if err != nil {
			return 0, err
		}
		if st.ok() {
			break
		}
		s.logf("IDER TEST UNIT READY: %s", st)
	}

	var capacity []byte
	st, err := d.command(cdb(0x25), &capacity) // READ CAPACITY
	if err != nil {
		return 0, err
	}
	if !st.ok() || len(capacity) < 8 {
		s.logf("IDER no medium (READ CAPACITY: %s, %d bytes)", st, len(capacity))
		return 0, nil
	}
	sectors = binary.BigEndian.Uint32(capacity[0:4]) + 1
	blockLen := binary.BigEndian.Uint32(capacity[4:8])
	s.logf("IDER medium present: %d sectors x %d bytes = %.1f MiB", sectors, blockLen, float64(sectors)*cdSectorSize/(1<<20))

	if sectors > 16 {
		var pvd []byte
		st, err := d.command(read10(16, 1), &pvd)
		if err != nil {
			return 0, err
		}
		switch {
		case !st.ok():
			s.logf("IDER READ(10) of sector 16 failed: %s", st)
		case len(pvd) >= 72 && string(pvd[1:6]) == "CD001":
			s.logf("IDER ISO 9660 volume %q (system %q)", strings.TrimSpace(string(pvd[40:72])), strings.TrimSpace(string(pvd[8:40])))
		default:
			s.logf("IDER sector 16 is not an ISO 9660 volume descriptor (UDF-only or not a CD image?)")
		}
	}
	return sectors, nil
}

// pull copies the whole medium to a local .iso via READ(10).
func (d *iderSession) pull(sectors uint32) (string, error) {
	s := d.s
	if err := os.MkdirAll(s.cfg.iderDir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(s.cfg.iderDir, fmt.Sprintf("ider-%d-%d.iso", os.Getpid(), s.id))
	f, err := os.Create(path)
	if err != nil {
		return "", err
	}
	ok := false
	defer func() {
		f.Close()
		if !ok {
			os.Remove(path)
		}
	}()

	total := float64(sectors) * cdSectorSize / (1 << 20)
	s.logf("IDER copying the image to %s for Hyper-V (%.1f MiB)", path, total)
	sum := sha256.New()
	w := io.MultiWriter(f, sum)
	start, lastLog := time.Now(), time.Now()
	for lba := uint32(0); lba < sectors; {
		n := min(uint32(iderPullSectors), sectors-lba)
		var data []byte
		st, err := d.command(read10(lba, uint16(n)), &data)
		if err != nil {
			return "", err
		}
		if !st.ok() || len(data) != int(n)*cdSectorSize {
			return "", fmt.Errorf("IDER READ(10) lba %d x %d: %s, got %d bytes", lba, n, st, len(data))
		}
		if _, err := w.Write(data); err != nil {
			return "", err
		}
		lba += n
		if time.Since(lastLog) >= 5*time.Second {
			lastLog = time.Now()
			done := float64(lba) * cdSectorSize / (1 << 20)
			s.logf("IDER copy %.0f/%.0f MiB (%.1f MiB/s)", done, total, done/time.Since(start).Seconds())
		}
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	ok = true
	s.logf("IDER copy finished in %s, sha256 %x", time.Since(start).Round(time.Second), sum.Sum(nil))
	return path, nil
}

// ---- one SCSI command round trip -------------------------------------------

type scsiStatus struct {
	ataStatus      byte
	sense, asc, aq byte
	gotEnd         bool
}

func (st scsiStatus) ok() bool { return !st.gotEnd || st.ataStatus&ataStatusErr == 0 }

func (st scsiStatus) String() string {
	if st.ok() {
		return "GOOD"
	}
	return fmt.Sprintf("CHECK CONDITION sense %#x asc %#x ascq %#x", st.sense, st.asc, st.aq)
}

// command sends one ATAPI packet and collects the browser's answer: either a
// COMMAND_END_RESPONSE, or DATA_TO_HOST chunks until one is marked completed.
// Data is appended to *data when data != nil.
func (d *iderSession) command(packet []byte, data *[]byte) (scsiStatus, error) {
	p := make([]byte, 20)
	p[4], p[5] = 0xfe, 0xff // byte count limit
	p[6] = iderDevCD
	p[7] = 0xa0 // ATA PACKET
	copy(p[8:], packet)
	if err := d.send(iderCommandWritten, 0, p); err != nil {
		return scsiStatus{}, err
	}
	timeout := time.NewTimer(iderCmdTimeout)
	defer timeout.Stop()
	for {
		select {
		case err := <-d.readErr:
			return scsiStatus{}, err
		case <-timeout.C:
			return scsiStatus{}, fmt.Errorf("IDER: no answer from the browser to SCSI command %#02x within %s", packet[0], iderCmdTimeout)
		case m := <-d.msgs:
			switch m.cmd {
			case iderCommandEndResponse:
				return scsiStatus{gotEnd: true, ataStatus: m.payload[19], sense: m.payload[20], asc: m.payload[21], aq: m.payload[22]}, nil
			case iderDataToHost:
				if data != nil {
					*data = append(*data, m.payload[26:]...)
				}
				if m.attr&attrCompleted != 0 {
					return scsiStatus{}, nil
				}
			default:
				d.logUnsolicited(m)
			}
		}
	}
}

func cdb(op byte) []byte { return []byte{op} }

func read10(lba uint32, n uint16) []byte {
	c := make([]byte, 12)
	c[0] = 0x28
	binary.BigEndian.PutUint32(c[2:6], lba)
	binary.BigEndian.PutUint16(c[7:9], n)
	return c
}

// ---- transport ---------------------------------------------------------------

func (d *iderSession) send(cmd, attr byte, payload []byte) error {
	d.wmu.Lock()
	defer d.wmu.Unlock()
	b := make([]byte, 8, 8+len(payload))
	b[0], b[3] = cmd, attr
	binary.LittleEndian.PutUint32(b[4:8], d.outSeq)
	d.outSeq++
	d.lastW = time.Now()
	return d.s.write(append(b, payload...))
}

func (d *iderSession) sendStatus(typ byte, value uint32) error {
	p := make([]byte, 5)
	p[0] = typ
	binary.LittleEndian.PutUint32(p[1:5], value)
	return d.send(iderStatusData, 0, p)
}

// await returns the next message of the given type, logging anything else.
func (d *iderSession) await(cmd byte) (iderMsg, error) {
	timeout := time.NewTimer(iderCmdTimeout)
	defer timeout.Stop()
	for {
		select {
		case m := <-d.msgs:
			if m.cmd == cmd {
				return m, nil
			}
			d.logUnsolicited(m)
		case err := <-d.readErr:
			return iderMsg{}, err
		case <-timeout.C:
			return iderMsg{}, fmt.Errorf("IDER: browser sent no %#02x within %s", cmd, iderCmdTimeout)
		}
	}
}

func (d *iderSession) logUnsolicited(m iderMsg) {
	switch m.cmd {
	case iderKeepAlivePong, iderHeartbeat, iderResetOccurredResp:
	case iderGetDataFromHost:
		d.s.logf("IDER browser asked to receive written data; writes are not emulated")
	default:
		d.s.logf("IDER unexpected message %#02x (%d bytes) ignored", m.cmd, len(m.payload)+8)
	}
}

// readLoop splits the browser's byte stream into messages.
func (d *iderSession) readLoop() {
	r := d.s.r
	for {
		hdr, err := readN(r, 8)
		if err != nil {
			d.readErr <- err
			return
		}
		var n int
		switch hdr[0] {
		case iderKeepAlivePong, iderResetOccurredResp, iderHeartbeat:
			n = 0
		case iderDisableEnable:
			n = 5
		case iderCommandEndResponse, iderGetDataFromHost:
			n = 23
		case iderDataToHost:
			fixed, err := readN(r, 26)
			if err != nil {
				d.readErr <- err
				return
			}
			data, err := readN(r, int(binary.LittleEndian.Uint16(fixed[1:3])))
			if err != nil {
				d.readErr <- err
				return
			}
			if !d.deliver(iderMsg{hdr[0], hdr[3], append(fixed, data...)}) {
				return
			}
			continue
		default:
			d.readErr <- fmt.Errorf("IDER: unknown message %#02x from the browser", hdr[0])
			return
		}
		p, err := readN(r, n)
		if err != nil {
			d.readErr <- err
			return
		}
		if !d.deliver(iderMsg{hdr[0], hdr[3], p}) {
			return
		}
	}
}

// deliver hands a message to the driver; false once the session has ended.
func (d *iderSession) deliver(m iderMsg) bool {
	select {
	case d.msgs <- m:
		return true
	case <-d.quit:
		return false
	}
}

// heartbeat keeps Console's 30s device-inactivity timeout from closing an
// idle IDER session (ui-toolkit just logs HEARTBEAT).
func (d *iderSession) heartbeat(done <-chan struct{}) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-done:
			return
		case <-t.C:
			d.wmu.Lock()
			idle := time.Since(d.lastW)
			d.wmu.Unlock()
			if idle >= d.s.cfg.keepalive {
				if err := d.send(iderHeartbeat, 0, nil); err != nil {
					return
				}
			}
		}
	}
}

// ---- Hyper-V -----------------------------------------------------------------

// hypervSetDVD puts iso into the VM's first DVD drive, or ejects it when iso is "".
func hypervSetDVD(vm, iso string) error {
	path := "$null"
	if iso != "" {
		path = psQuote(iso)
	}
	return powershell(fmt.Sprintf(
		"$d = Get-VMDvdDrive -VMName %s | Select-Object -First 1; "+
			"if (-not $d) { throw 'VM has no DVD drive: add one in Hyper-V Manager (SCSI controller > DVD Drive)' }; "+
			"$d | Set-VMDvdDrive -Path %s", psQuote(vm), path))
}

// hypervCheck verifies at startup that the VM and its DVD drive are reachable.
func hypervCheck(vm string) error {
	return powershell(fmt.Sprintf(
		"$d = Get-VMDvdDrive -VMName %s | Select-Object -First 1; "+
			"if (-not $d) { throw 'VM has no DVD drive: add one in Hyper-V Manager (SCSI controller > DVD Drive)' }", psQuote(vm)))
}

func powershell(script string) error {
	if runtime.GOOS != "windows" {
		return errors.New("Hyper-V attach only works on Windows")
	}
	out, err := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-Command",
		"$ErrorActionPreference = 'Stop'; "+script).CombinedOutput()
	if err != nil {
		// Keep the cmdlet's message (word-wrapped by PowerShell), drop its position/category noise.
		msg, _, _ := strings.Cut(string(bytes.TrimSpace(out)), "At line:")
		msg = strings.Join(strings.Fields(msg), " ")
		if strings.Contains(msg, "permission") || strings.Contains(msg, "authorization") {
			msg += " (run fake-amt elevated, or as a member of Hyper-V Administrators)"
		}
		return errors.New(msg)
	}
	return nil
}

func psQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
