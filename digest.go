package main

import (
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
)

func md5hex(s string) string {
	sum := md5.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}

func randomHex(nBytes int) string {
	b := make([]byte, nBytes)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// digestResponse computes an RFC 2617 digest response with qop=auth
// (or the legacy form when qop is empty), which is what Intel AMT uses for
// both WS-Management over HTTP and the redirection protocol.
func digestResponse(user, realm, pass, method, uri, nonce, nc, cnonce, qop string) string {
	ha1 := md5hex(user + ":" + realm + ":" + pass)
	ha2 := md5hex(method + ":" + uri)
	if qop == "" {
		return md5hex(ha1 + ":" + nonce + ":" + ha2)
	}
	return md5hex(ha1 + ":" + nonce + ":" + nc + ":" + cnonce + ":" + qop + ":" + ha2)
}

// parseDigestHeader parses the parameters of an HTTP
// `Authorization: Digest k=v, k="v"` header.
func parseDigestHeader(h string) (map[string]string, error) {
	const prefix = "digest "
	if len(h) < len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return nil, fmt.Errorf("not a Digest authorization header")
	}
	s := h[len(prefix):]
	out := map[string]string{}
	for len(s) > 0 {
		s = strings.TrimLeft(s, " ,\t")
		if s == "" {
			break
		}
		eq := strings.IndexByte(s, '=')
		if eq < 0 {
			return nil, fmt.Errorf("malformed digest parameter %q", s)
		}
		key := strings.ToLower(strings.TrimSpace(s[:eq]))
		s = s[eq+1:]
		var val string
		if strings.HasPrefix(s, `"`) {
			end := strings.IndexByte(s[1:], '"')
			if end < 0 {
				return nil, fmt.Errorf("unterminated quoted value for %s", key)
			}
			val = s[1 : end+1]
			s = s[end+2:]
		} else {
			end := strings.IndexByte(s, ',')
			if end < 0 {
				end = len(s)
			}
			val = strings.TrimSpace(s[:end])
			s = s[end:]
		}
		out[key] = val
	}
	return out, nil
}
