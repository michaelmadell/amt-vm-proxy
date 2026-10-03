package main

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"net"
	"strings"
	"sync/atomic"
	"time"
)

// Intel AMT redirection protocol command bytes (as used by Console's
// interceptor and ui-toolkit's AMTRedirector).
const (
	cmdStartRedirectionSession      = 0x10
	cmdStartRedirectionSessionReply = 0x11
	cmdEndRedirectionSession        = 0x12
	cmdAuthenticateSession          = 0x13
	cmdAuthenticateSessionReply     = 0x14
	cmdKVMStart                     = 0x40
	cmdKVMStartReply                = 0x41

	authQuery  = 0
	authDigest = 4

	authStatusSuccess = 0
	authStatusFail    = 1

	redirAuthURI = "/RedirectionService"
)

var sessionCounter atomic.Int64

type redirSession struct {
	id    int64
	cfg   *config
	conn  net.Conn
	r     *bufio.Reader
	realm string
	nonce string
	proto string
}

func (s *redirSession) logf(format string, args ...any) {
	log.Printf("redir#%d: "+format, append([]any{s.id}, args...)...)
}

func serveRedirection(cfg *config, conn net.Conn, realm string) {
	s := &redirSession{
		id:    sessionCounter.Add(1),
		cfg:   cfg,
		conn:  conn,
		r:     bufio.NewReaderSize(conn, 64<<10),
		realm: realm,
	}
	defer conn.Close()
	s.logf("connection from %s", conn.RemoteAddr())
	if err := s.run(); err != nil && err != io.EOF {
		s.logf("closed: %v", err)
	} else {
		s.logf("closed")
	}
}

func (s *redirSession) readFull(n int) ([]byte, error) {
	b := make([]byte, n)
	_, err := io.ReadFull(s.r, b)
	return b, err
}

// write sends one protocol message in a single Write. Console's relay parses
// each TCP read as exactly one message until authentication completes, so
// replies must not be split or coalesced.
func (s *redirSession) write(b []byte) error {
	_, err := s.conn.Write(b)
	return err
}

func (s *redirSession) run() error {
	authenticated := false
	_ = s.conn.SetReadDeadline(time.Now().Add(2 * time.Minute))

	for {
		cmd, err := s.r.ReadByte()
		if err != nil {
			return err
		}
		switch cmd {
		case cmdStartRedirectionSession:
			rest, err := s.readFull(7)
			if err != nil {
				return err
			}
			s.proto = string(rest[3:7])
			switch s.proto {
			case "KVMR", "IDER":
				s.logf("StartRedirectionSession %s", map[string]string{"KVMR": "KVM", "IDER": "IDER"}[s.proto])
				// 0x11, status, 2 reserved, 8 bytes protocol/version info, OEM length.
				reply := []byte{cmdStartRedirectionSessionReply, 0, 0, 0, 1, 0, 0, 0, 0, 0, 0, 0, 0}
				if err := s.write(reply); err != nil {
					return err
				}
			default:
				s.logf("StartRedirectionSession %q requested: not implemented yet (only KVM and IDER are emulated), closing", strings.TrimSpace(s.proto))
				return nil
			}

		case cmdAuthenticateSession:
			hdr, err := s.readFull(8)
			if err != nil {
				return err
			}
			authType := hdr[3]
			n := binary.LittleEndian.Uint32(hdr[4:8])
			if n > 4096 {
				return fmt.Errorf("authenticate message too long (%d bytes)", n)
			}
			data, err := s.readFull(int(n))
			if err != nil {
				return err
			}
			ok, err := s.handleAuth(authType, data)
			if err != nil {
				return err
			}
			if ok {
				authenticated = true
			}

		case cmdKVMStart: // also IDER's OPEN_SESSION, which shares the command byte
			if !authenticated {
				return fmt.Errorf("session start before authentication")
			}
			_ = s.conn.SetReadDeadline(time.Time{})
			if s.proto == "IDER" {
				open, err := s.readFull(17)
				if err != nil {
					return err
				}
				return runIDER(s, open[7:])
			}
			if _, err := s.readFull(7); err != nil {
				return err
			}
			return runKVM(s)

		case cmdEndRedirectionSession:
			_, _ = s.readFull(3)
			s.logf("EndRedirectionSession")
			return nil

		default:
			return fmt.Errorf("unexpected redirection command 0x%02x", cmd)
		}
	}
}

// handleAuth implements AMT's digest handshake: a query for supported auth
// types, an "empty" digest request that gets a realm/nonce challenge back,
// and the real digest response.
func (s *redirSession) handleAuth(authType byte, data []byte) (bool, error) {
	switch authType {
	case authQuery:
		// Supported: user/pass (1), bad digest (3), digest (4) - same list AMT returns.
		return false, s.write(authReply(authStatusSuccess, authQuery, []byte{1, 3, 4}))

	case authDigest:
		f := parseLenPrefixed(data)
		user, realm, nonce, uri, cnonce, nc, response, qop := f.get(0), f.get(1), f.get(2), f.get(3), f.get(4), f.get(5), f.get(6), f.get(7)
		if realm == "" {
			// Initial request: issue a challenge.
			s.nonce = randomHex(16)
			var b []byte
			b = appendLenPrefixed(b, s.realm)
			b = appendLenPrefixed(b, s.nonce)
			b = appendLenPrefixed(b, "auth")
			s.logf("digest challenge sent (user %q)", user)
			return false, s.write(authReply(authStatusFail, authDigest, b))
		}
		want := digestResponse(s.cfg.user, realm, s.cfg.pass, "POST", uri, nonce, nc, cnonce, qop)
		switch {
		case user != s.cfg.user:
			s.logf("auth FAILED: unknown user %q", user)
		case nonce != s.nonce || realm != s.realm:
			s.logf("auth FAILED: stale nonce/realm")
		case !strings.EqualFold(want, response):
			s.logf("auth FAILED: wrong password (digest mismatch)")
		default:
			s.logf("authenticated as %q", user)
			return true, s.write(authReply(authStatusSuccess, authDigest, nil))
		}
		_ = s.write(authReply(authStatusFail, authDigest, nil))
		return false, fmt.Errorf("authentication failed")

	default:
		_ = s.write(authReply(2 /* not supported */, authType, nil))
		return false, fmt.Errorf("auth type %d not supported", authType)
	}
}

func authReply(status, authType byte, data []byte) []byte {
	b := []byte{cmdAuthenticateSessionReply, status, 0, 0, authType, 0, 0, 0, 0}
	binary.LittleEndian.PutUint32(b[5:9], uint32(len(data)))
	return append(b, data...)
}

type lenPrefixed []string

func (l lenPrefixed) get(i int) string {
	if i < len(l) {
		return l[i]
	}
	return ""
}

func parseLenPrefixed(b []byte) lenPrefixed {
	var out lenPrefixed
	for len(b) > 0 {
		n := int(b[0])
		b = b[1:]
		if n > len(b) {
			n = len(b)
		}
		out = append(out, string(b[:n]))
		b = b[n:]
	}
	return out
}

func appendLenPrefixed(b []byte, s string) []byte {
	return append(append(b, byte(len(s))), s...)
}
