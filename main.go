// fake-amt pretends to be an Intel AMT (vPro) device for Device Management
// Toolkit Console, bridges Console's KVM sessions to an ordinary VNC
// server (e.g. a QEMU/libvirt VM), and emulates IDER (virtual media), which
// can optionally insert the mounted image into a Hyper-V VM. It lets you
// exercise Console's relay and ui-toolkit's browser KVM/IDER clients against
// a real, live remote desktop without vPro hardware.
package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"flag"
	"fmt"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type config struct {
	bind         string
	user, pass   string
	vncAddr      string
	vncPassword  string
	tls          bool
	noPlain      bool
	fixturesDir  string
	tracePointer string
	traceKeys    bool
	traceWSMAN   bool
	keepalive    time.Duration
	hypervVM     string
	iderDir      string
}

func main() {
	cfg := &config{}
	flag.StringVar(&cfg.bind, "bind", "0.0.0.0", "address to listen on (AMT ports 16992/16994, and 16993/16995 with -tls)")
	flag.StringVar(&cfg.user, "user", "admin", "AMT username Console will use (Console only handles \"admin\" for redirection)")
	flag.StringVar(&cfg.pass, "pass", "P@ssw0rd", "AMT password Console will use")
	flag.StringVar(&cfg.vncAddr, "vnc", "127.0.0.1:5900", "VNC server to show over KVM (host:port), e.g. QEMU's -vnc display")
	flag.StringVar(&cfg.vncPassword, "vnc-password", "", "password for the VNC server, if it requires one")
	flag.BoolVar(&cfg.tls, "tls", false, "also listen on the TLS ports 16993/16995 with a self-signed cert (set the device to TLS + allow self-signed in Console)")
	flag.BoolVar(&cfg.noPlain, "tls-only", false, "with -tls, don't listen on the non-TLS ports")
	flag.StringVar(&cfg.fixturesDir, "fixtures", "", "directory of extra/override WS-Management response XML files")
	flag.StringVar(&cfg.tracePointer, "trace-pointer", "clicks", "log pointer events from the browser: none, clicks, all")
	flag.BoolVar(&cfg.traceKeys, "trace-keys", false, "log key events from the browser")
	flag.BoolVar(&cfg.traceWSMAN, "trace-wsman", false, "log every WS-Management request and the fixture that answered it")
	flag.DurationVar(&cfg.keepalive, "keepalive", 10*time.Second, "send an RFB bell to the browser after this much idle time so Console's 30s inactivity timeout doesn't drop a static desktop (0 = off)")
	flag.StringVar(&cfg.hypervVM, "hyperv-vm", "", "Hyper-V VM to put IDER-mounted images into (its DVD drive); needs an elevated fake-amt. Empty = IDER is emulated but the image goes nowhere")
	flag.StringVar(&cfg.iderDir, "ider-dir", filepath.Join(os.TempDir(), "fake-amt-ider"), "where IDER images are saved while inserted into the Hyper-V VM")
	flag.Parse()

	switch cfg.tracePointer {
	case "none", "clicks", "all":
	default:
		log.Fatalf("-trace-pointer must be none, clicks or all")
	}
	if cfg.noPlain && !cfg.tls {
		log.Fatalf("-tls-only needs -tls")
	}

	log.SetFlags(log.Ltime | log.Lmicroseconds)

	fixtures, err := loadFixtures(cfg.fixturesDir)
	if err != nil {
		log.Fatalf("loading fixtures: %v", err)
	}
	realm := "Digest:" + strings.ToUpper(randomHex(16))
	ws := &wsmanServer{cfg: cfg, fixtures: fixtures, realm: realm}

	type listener struct {
		port string
		tls  bool
		kind string
	}
	var ls []listener
	if !cfg.noPlain {
		ls = append(ls, listener{"16992", false, "wsman"}, listener{"16994", false, "redir"})
	}
	var tlsCfg *tls.Config
	if cfg.tls {
		cert, err := selfSignedCert()
		if err != nil {
			log.Fatalf("generating TLS certificate: %v", err)
		}
		tlsCfg = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
		ls = append(ls, listener{"16993", true, "wsman"}, listener{"16995", true, "redir"})
	}

	for _, l := range ls {
		addr := net.JoinHostPort(cfg.bind, l.port)
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			log.Fatalf("listen %s: %v (is something else - e.g. Intel LMS - already using the AMT ports?)", addr, err)
		}
		if l.tls {
			ln = tls.NewListener(ln, tlsCfg)
		}
		switch l.kind {
		case "wsman":
			srv := &http.Server{Handler: ws, ReadHeaderTimeout: 30 * time.Second}
			go func() { log.Fatal(srv.Serve(ln)) }()
		case "redir":
			go func(ln net.Listener) {
				for {
					c, err := ln.Accept()
					if err != nil {
						log.Fatal(err)
					}
					if tc, ok := c.(*net.TCPConn); ok {
						_ = tc.SetNoDelay(true)
					}
					go serveRedirection(cfg, c, realm)
				}
			}(ln)
		}
		log.Printf("listening on %s (%s%s)", addr, l.kind, map[bool]string{true: ", TLS", false: ""}[l.tls])
	}

	log.Printf("fake-amt ready: %d recorded WS-Management responses, KVM -> VNC %s, user %q", fixtures.len(), cfg.vncAddr, cfg.user)
	if c, err := net.DialTimeout("tcp", cfg.vncAddr, 2*time.Second); err != nil {
		log.Printf("WARNING: VNC server %s is not reachable yet (%v); KVM sessions will fail until it is", cfg.vncAddr, err)
	} else {
		c.Close()
	}
	if cfg.hypervVM != "" {
		if err := hypervCheck(cfg.hypervVM); err != nil {
			log.Printf("WARNING: Hyper-V VM %q not usable for IDER yet: %v", cfg.hypervVM, err)
		} else {
			log.Printf("IDER images will be inserted into Hyper-V VM %q (copied via %s)", cfg.hypervVM, cfg.iderDir)
		}
	}
	fmt.Fprintln(os.Stderr, "In Console, add a device with this machine's address as hostname, TLS off (or on with -tls + allow self-signed), and the user/password above.")
	select {}
}

func selfSignedCert() (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	host, _ := os.Hostname()
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: "fake-amt", Organization: []string{"fake-amt (not Intel AMT)"}},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(1, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost", host},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, nil
}
