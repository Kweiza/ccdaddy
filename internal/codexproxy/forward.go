package codexproxy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

const (
	// turnStateHeader is the per-turn continuation token. It is meaningful only
	// to the account that issued it, so a replay onto another account strips it.
	turnStateHeader = "x-codex-turn-state"
	// accountIDHeader names the workspace the bearer belongs to. codex does not
	// send it on this route; ccdad adds it, from the store, per attempt.
	accountIDHeader = "chatgpt-account-id"

	// streamChunk is the copy buffer. It is small because the point is to hand
	// each flush to the client as it arrives rather than to move bytes fast.
	streamChunk = 8 << 10
)

// strippedRequestHeaders never go upstream.
//
// The first four are credentials the caller must not be able to smuggle past
// ccdad's own. host and the forwarding family would describe a hop that is not
// the one being made. The rest are hop-by-hop, which belong to the connection
// this proxy terminates and not to the one it opens. content-length is here
// because the proxy rebuilds it from the body it actually forwards.
var strippedRequestHeaders = map[string]bool{
	"authorization":       true,
	"x-api-key":           true,
	"cookie":              true,
	"proxy-authorization": true,
	"host":                true,
	"forwarded":           true,
	"via":                 true,
	"te":                  true,
	"trailer":             true,
	"transfer-encoding":   true,
	"connection":          true,
	"upgrade":             true,
	"expect":              true,
	"content-length":      true,
}

// strippedRequestPrefixes are the two families stripped whole.
var strippedRequestPrefixes = []string{"proxy-", "x-forwarded-"}

// droppedResponseHeaders never reach the client. The cookie is the one that
// matters: it would be set for the proxy's own loopback origin and belongs to
// nobody. The rest are hop-by-hop or rebuilt by this process.
var droppedResponseHeaders = map[string]bool{
	"set-cookie":        true,
	"connection":        true,
	"transfer-encoding": true,
	"keep-alive":        true,
	"trailer":           true,
	"upgrade":           true,
	"content-length":    true,
}

// droppedResponsePrefixes are the edge's own headers.
var droppedResponsePrefixes = []string{"cf-"}

// errNoCredential marks a send that never left this process because the
// account has no usable Codex credential.
//
// The attempt loop has to tell it apart from a transport failure, and the two
// end in opposite answers: the first is an account nobody can serve from until
// somebody logs in again, and the second is a network that is down. Told apart
// by a sentinel rather than by inspecting the message, because the message is
// whatever the store's reader said.
var errNoCredential = errors.New("no usable codex credential")

// attempt is one upstream request that produced an answer.
type attempt struct {
	// uuid is the account that paid for it.
	uuid string
	// token is the access token the attempt used, which is what the refresher
	// has to be told it saw.
	token  string
	status int
	header http.Header
	// body is read for every non-2xx, because the caller may have to answer
	// with it after a replay fails.
	body []byte
	// stream is set only for a 2xx, and the caller owns closing it.
	stream io.ReadCloser
}

// responses is the whole request path, and the ORDER of its first three steps
// is the security property:
//
//  1. the bearer is read out of the header;
//  2. it is validated against the launch records;
//  3. only then is the body read.
//
// A proxy that buffered first would let anything on this machine hand the
// daemon 32 MiB per connection without being anybody at all.
func (s *Server) responses(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.notFound(w, r)
		return
	}
	rec, ok := s.authorize(r)
	if !ok {
		writeUnknownLaunch(w)
		return
	}
	body, ok := readBody(r)
	if !ok {
		writeUnavailable(w)
		return
	}

	threadID := threadIDOf(r)
	order, pinned := s.chooseOrder(rec, threadID)
	if len(order) == 0 {
		s.logf("the codex proxy has no account it can serve a request from")
		writeUnavailable(w)
		return
	}
	// Read BEFORE the first attempt: a thread that already has responses is
	// carrying one account's encrypted reasoning, and whether it may be moved is
	// a different question from whether a brand-new thread may be.
	_, midThread := s.threadAccount(threadID)

	// first is the rate limit that started the search. A replacement that fails
	// for some unrelated reason must not turn a "you are out of quota" into a
	// "your request was malformed". dead collects the accounts nothing on this
	// machine can serve from until somebody logs in again, which is a different
	// answer from an account that is merely out of quota for an hour.
	var (
		first *attempt
		dead  []string
	)

	for i, uuid := range order {
		// Every attempt after the first strips the turn state: it is a
		// continuation token only the account that issued it can read.
		a, err := s.send(r.Context(), uuid, r, body, i > 0)
		if err != nil {
			if errors.Is(err, errNoCredential) {
				// The request never left this process. That is a fact about the
				// ACCOUNT, and the answer it earns says so.
				s.logf("codex account %s cannot serve: %v", short(uuid), err)
				dead = append(dead, uuid)
				continue
			}
			s.logf("the codex proxy could not reach the upstream for %s: %v", short(uuid), err)
			continue
		}
		if a.stream != nil {
			s.rememberThread(threadID, uuid)
			// Past this call a byte may have reached the client, so nothing
			// after it may be replayed.
			s.streamBack(w, a)
			return
		}
		if a.status == http.StatusTooManyRequests {
			s.recordLimit(uuid, a)
			if first == nil {
				first = a
			}
			switch {
			case pinned:
				// A pin never bills another account.
				writeBack(w, a)
				return
			case midThread && !s.cfg.CrossAccountReplay:
				// The user starts a new thread and lands on the new account.
				writeBack(w, a)
				return
			}
			continue
		}
		// A 401 is the exception, and it is passed through rather than covered
		// by the original rate limit: the endpoint is saying something about
		// THIS account that the user has to see. Every other 4xx from a
		// replacement is answered with the rate limit that started the search.
		if first != nil && a.status != http.StatusUnauthorized {
			s.logf("a replacement codex account answered %d; answering the original rate limit instead", a.status)
			writeBack(w, first)
			return
		}
		writeBack(w, a)
		return
	}
	if first != nil {
		writeBack(w, first)
		return
	}
	writeUnavailable(w)
}

// send makes one upstream attempt as one account.
//
// The Authorization and the workspace header are built HERE, from the store,
// on every attempt -- never carried from a previous one. A replay after a
// refresh has to reach the endpoint with the token the refresh produced, and
// reusing a header built before it is exactly how that stops being true.
func (s *Server) send(ctx context.Context, uuid string, in *http.Request, body []byte, stripTurnState bool) (*attempt, error) {
	cred, err := s.credential(uuid)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errNoCredential, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.cfg.Upstream, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header = outboundHeader(in.Header)
	if stripTurnState {
		req.Header.Del(turnStateHeader)
	}
	req.Header.Set("Authorization", "Bearer "+cred.AccessToken)
	if cred.AccountID != "" {
		req.Header.Set(accountIDHeader, cred.AccountID)
	}
	req.ContentLength = int64(len(body))

	res, err := s.cfg.Client.Do(req)
	if err != nil {
		return nil, err
	}
	a := &attempt{uuid: uuid, token: cred.AccessToken, status: res.StatusCode, header: res.Header}
	if res.StatusCode >= 200 && res.StatusCode < 300 {
		a.stream = res.Body
		return a, nil
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, maxErrorBody))
	if err != nil {
		return nil, err
	}
	a.body = data
	return a, nil
}

// streamBack copies the upstream answer to the client as it arrives.
//
// A stream that ends because the upstream broke must NOT be terminated
// cleanly: a clean end tells codex the turn finished, and it would render a
// truncated answer as a complete one. http.ErrAbortHandler is net/http's own
// way to drop the connection without the terminating chunk, which is what
// makes the break visible on the other side.
func (s *Server) streamBack(w http.ResponseWriter, a *attempt) {
	defer a.stream.Close()
	copyResponseHeader(w.Header(), a.header)
	w.WriteHeader(a.status)
	flusher, _ := w.(http.Flusher)
	if flusher != nil {
		flusher.Flush()
	}
	buf := make([]byte, streamChunk)
	for {
		n, rerr := a.stream.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				// The client hung up. Nothing to report to it.
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if rerr == io.EOF {
			return
		}
		if rerr != nil {
			s.logf("the codex upstream stream for %s ended early: %v", short(a.uuid), rerr)
			panic(http.ErrAbortHandler)
		}
	}
}

// writeBack answers with an attempt the proxy already read whole.
func writeBack(w http.ResponseWriter, a *attempt) {
	copyResponseHeader(w.Header(), a.header)
	w.Header().Set("Content-Length", strconv.Itoa(len(a.body)))
	w.WriteHeader(a.status)
	_, _ = w.Write(a.body)
}

// outboundHeader is the request header as it goes upstream: everything the
// caller sent, minus the strip list, and rewritten nowhere. The turn metadata
// in particular travels verbatim -- it carries codex's own installation id, and
// a proxy that edited it would be lying to the endpoint about which client is
// speaking.
func outboundHeader(in http.Header) http.Header {
	out := make(http.Header, len(in))
	for name, values := range in {
		lower := strings.ToLower(name)
		if strippedRequestHeaders[lower] || hasAnyPrefix(lower, strippedRequestPrefixes) {
			continue
		}
		out[http.CanonicalHeaderKey(name)] = append([]string(nil), values...)
	}
	return out
}

// copyResponseHeader is the same rule in the other direction.
func copyResponseHeader(dst, src http.Header) {
	for name, values := range src {
		lower := strings.ToLower(name)
		if droppedResponseHeaders[lower] || hasAnyPrefix(lower, droppedResponsePrefixes) {
			continue
		}
		dst[http.CanonicalHeaderKey(name)] = append([]string(nil), values...)
	}
}

func hasAnyPrefix(s string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}
