package main

import (
	"embed"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// Recorded responses from real Intel AMT devices, taken from
// github.com/device-management-toolkit/go-wsman-messages
// (pkg/wsman/wsmantesting/responses, Apache-2.0 — see fixtures/LICENSE).
//
//go:embed fixtures/responses
var embeddedFixtures embed.FS

var (
	reAction      = regexp.MustCompile(`<(?:\w+:)?Action\b[^>]*>\s*([^<\s]+)\s*</`)
	reResourceURI = regexp.MustCompile(`<(?:\w+:)?ResourceURI\b[^>]*>\s*([^<\s]+)\s*</`)
	reMessageID   = regexp.MustCompile(`<(?:\w+:)?MessageID\b[^>]*>\s*([^<\s]+)\s*</`)
	reRelatesTo   = regexp.MustCompile(`(<(?:\w+:)?RelatesTo\b[^>]*>)[^<]*(</)`)
)

type fixture struct {
	source      string // where it came from, for logging
	body        []byte
	action      string // response action, e.g. .../transfer/GetResponse
	resourceURI string
	preferred   bool // file name matches the request method (get.xml for Get)
}

type fixtureStore struct {
	mu    sync.RWMutex
	byKey map[string]*fixture
}

func fixtureKey(resourceURI, responseAction string) string {
	return resourceURI + "|" + responseAction
}

func lastSegment(uri string) string {
	if i := strings.LastIndexByte(uri, '/'); i >= 0 {
		return uri[i+1:]
	}
	return uri
}

func parseFixture(source string, body []byte) (*fixture, bool) {
	a := reAction.FindSubmatch(body)
	r := reResourceURI.FindSubmatch(body)
	if a == nil || r == nil {
		return nil, false
	}
	action := string(a[1])
	if strings.Contains(strings.ToLower(action), "fault") {
		return nil, false
	}
	method := strings.TrimSuffix(lastSegment(action), "Response")
	base := strings.TrimSuffix(path.Base(filepath.ToSlash(source)), ".xml")
	return &fixture{
		source:      source,
		body:        body,
		action:      action,
		resourceURI: string(r[1]),
		preferred:   strings.EqualFold(base, method),
	}, true
}

// add indexes a fixture. Overrides always win; otherwise a file whose name
// matches the method (get.xml for a GetResponse) beats one that doesn't, and
// ties keep the first one seen (files are walked in sorted order).
func (s *fixtureStore) add(f *fixture, override bool) {
	k := fixtureKey(f.resourceURI, f.action)
	cur, exists := s.byKey[k]
	if !exists || override || (!cur.preferred && f.preferred) {
		s.byKey[k] = f
	}
}

func loadFixtures(overrideDir string) (*fixtureStore, error) {
	s := &fixtureStore{byKey: map[string]*fixture{}}

	var paths []string
	err := fs.WalkDir(embeddedFixtures, "fixtures/responses", func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(p, ".xml") {
			paths = append(paths, p)
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)
	for _, p := range paths {
		body, err := embeddedFixtures.ReadFile(p)
		if err != nil {
			return nil, err
		}
		if f, ok := parseFixture(strings.TrimPrefix(p, "fixtures/responses/"), body); ok {
			s.add(f, false)
		}
	}

	if overrideDir != "" {
		var opaths []string
		err := filepath.WalkDir(overrideDir, func(p string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() && strings.HasSuffix(strings.ToLower(p), ".xml") {
				opaths = append(opaths, p)
			}
			return err
		})
		if err != nil {
			return nil, fmt.Errorf("reading -fixtures dir: %w", err)
		}
		sort.Strings(opaths)
		for _, p := range opaths {
			body, err := os.ReadFile(p)
			if err != nil {
				return nil, err
			}
			f, ok := parseFixture("override:"+p, body)
			if !ok {
				log.Printf("wsman: ignoring %s (no Action/ResourceURI header found)", p)
				continue
			}
			s.add(f, true)
			log.Printf("wsman: override %s -> %s %s", p, lastSegment(f.resourceURI), lastSegment(f.action))
		}
	}
	return s, nil
}

func (s *fixtureStore) lookup(resourceURI, requestAction string) (*fixture, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	f, ok := s.byKey[fixtureKey(resourceURI, requestAction+"Response")]
	return f, ok
}

func (s *fixtureStore) len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.byKey)
}

// wsmanServer answers WS-Management requests from Console using recorded
// responses, behind HTTP digest auth like a real AMT device.
type wsmanServer struct {
	cfg      *config
	fixtures *fixtureStore
	realm    string
}

func (w *wsmanServer) challenge(rw http.ResponseWriter) {
	rw.Header().Set("WWW-Authenticate",
		fmt.Sprintf(`Digest realm="%s", nonce="%s", stale="false", qop="auth"`, w.realm, randomHex(16)))
	rw.WriteHeader(http.StatusUnauthorized)
}

func (w *wsmanServer) authorized(r *http.Request) (bool, string) {
	h := r.Header.Get("Authorization")
	if h == "" {
		return false, ""
	}
	p, err := parseDigestHeader(h)
	if err != nil {
		return false, err.Error()
	}
	if p["username"] != w.cfg.user {
		return false, fmt.Sprintf("unknown user %q", p["username"])
	}
	want := digestResponse(w.cfg.user, p["realm"], w.cfg.pass, r.Method, p["uri"], p["nonce"], p["nc"], p["cnonce"], p["qop"])
	if !strings.EqualFold(want, p["response"]) {
		return false, "wrong password (digest mismatch)"
	}
	return true, ""
}

func (w *wsmanServer) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || !strings.EqualFold(strings.TrimSuffix(r.URL.Path, "/"), "/wsman") {
		http.NotFound(rw, r)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if err != nil {
		http.Error(rw, err.Error(), http.StatusBadRequest)
		return
	}

	if ok, why := w.authorized(r); !ok {
		if why != "" {
			log.Printf("wsman: auth rejected from %s: %s", r.RemoteAddr, why)
		}
		w.challenge(rw)
		return
	}

	a := reAction.FindSubmatch(body)
	u := reResourceURI.FindSubmatch(body)
	if a == nil || u == nil {
		log.Printf("wsman: request without Action/ResourceURI from %s", r.RemoteAddr)
		http.Error(rw, "missing Action or ResourceURI", http.StatusBadRequest)
		return
	}
	action, resourceURI := string(a[1]), string(u[1])
	messageID := ""
	if m := reMessageID.FindSubmatch(body); m != nil {
		messageID = string(m[1])
	}

	f, ok := w.fixtures.lookup(resourceURI, action)
	if !ok {
		log.Printf("wsman: NO FIXTURE  %-18s %s  (drop an XML file into -fixtures to answer it)", lastSegment(action), resourceURI)
		rw.Header().Set("Content-Type", "application/soap+xml;charset=UTF-8")
		rw.WriteHeader(http.StatusBadRequest)
		fmt.Fprintf(rw, soapFault, messageID, "fake-amt has no recorded response for "+lastSegment(action)+" on "+lastSegment(resourceURI))
		return
	}

	resp := f.body
	if messageID != "" {
		resp = reRelatesTo.ReplaceAll(resp, []byte("${1}"+strings.ReplaceAll(messageID, "$", "$$")+"${2}"))
	}
	if w.cfg.traceWSMAN {
		log.Printf("wsman: %-18s %-45s <- %s", lastSegment(action), lastSegment(resourceURI), f.source)
	}
	rw.Header().Set("Content-Type", "application/soap+xml;charset=UTF-8")
	_, _ = rw.Write(resp)
}

const soapFault = `<?xml version="1.0" encoding="UTF-8"?>
<a:Envelope xmlns:a="http://www.w3.org/2003/05/soap-envelope" xmlns:b="http://schemas.xmlsoap.org/ws/2004/08/addressing">
<a:Header><b:Action a:mustUnderstand="true">http://schemas.xmlsoap.org/ws/2004/08/addressing/fault</b:Action><b:RelatesTo>%s</b:RelatesTo></a:Header>
<a:Body><a:Fault><a:Code><a:Value>a:Sender</a:Value><a:Subcode><a:Value>b:DestinationUnreachable</a:Value></a:Subcode></a:Code><a:Reason><a:Text xml:lang="en-US">%s</a:Text></a:Reason></a:Fault></a:Body>
</a:Envelope>`
