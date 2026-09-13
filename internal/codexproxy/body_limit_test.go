package codexproxy

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func TestCodexRequestsAboveThirtyTwoMiBAreForwardedIntact(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, sse(`{"type":"response.completed"}`)) })
	f.add("uuid-a", "a@example.com", "access-a")
	s := f.server(t, f.config())
	body := `{"input":"` + strings.Repeat("x", 33<<20) + `"}`
	w := post(s, unpinnedSecret, nil, body)
	if w.Code != http.StatusOK {
		t.Fatalf("33 MiB request status = %d: %s", w.Code, w.Body.String())
	}
	requests := f.took()
	if len(requests) != 1 {
		t.Fatalf("upstream requests = %d", len(requests))
	}
	got := requests[0]
	if got.length != int64(len(body)) || sha256.Sum256(got.body) != sha256.Sum256([]byte(body)) {
		t.Fatal("large request was truncated or changed")
	}
}

func TestCodexDefaultBodyLimitRejectsOverTwoHundredFiftySixMiBWith413(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) { t.Error("oversized body reached upstream") })
	s := f.server(t, f.config())
	size := int64(256<<20) + 17
	reader := &countBody{remaining: size}
	r := httptest.NewRequest(http.MethodPost, ResponsesPath, reader)
	r.ContentLength = size
	r.Header.Set("Authorization", "Bearer "+unpinnedSecret)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	assertBodyLimitError(t, w, 256<<20, size, false)
	if reader.read != 0 {
		t.Fatalf("read %d bytes despite an oversized Content-Length", reader.read)
	}
	if len(f.took()) != 0 {
		t.Fatal("oversized request went upstream")
	}
}

func TestCodexConfiguredBodyLimitEnforcesBoundariesAndUnknownLengths(t *testing.T) {
	for _, known := range []bool{false, true} {
		for _, size := range []int{1023, 1024, 1025, 8192} {
			t.Run(strconv.FormatBool(known)+"/"+strconv.Itoa(size), func(t *testing.T) {
				f := newFixture(t, func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, sse(`{"type":"response.completed"}`)) })
				f.add("uuid-a", "a@example.com", "access-a")
				cfg := f.config()
				cfg.MaxBodyBytes = 1024
				s := f.server(t, cfg)
				r := httptest.NewRequest(http.MethodPost, ResponsesPath, strings.NewReader(strings.Repeat("x", size)))
				if !known {
					r.ContentLength = -1
				}
				r.Header.Set("Authorization", "Bearer "+unpinnedSecret)
				w := httptest.NewRecorder()
				s.Handler().ServeHTTP(w, r)
				if size <= 1024 {
					if w.Code != http.StatusOK || len(f.took()) != 1 {
						t.Fatalf("in-limit request status = %d", w.Code)
					}
				} else {
					actual := int64(size)
					if !known {
						actual = 1025
					}
					assertBodyLimitError(t, w, 1024, actual, !known)
					if len(f.took()) != 0 {
						t.Fatal("oversized request reached upstream")
					}
				}
			})
		}
	}
}

func assertBodyLimitError(t *testing.T, w *httptest.ResponseRecorder, limit, actual int64, atLeast bool) {
	t.Helper()
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413: %s", w.Code, w.Body.String())
	}
	var result struct {
		Error struct {
			Type, Message string
			Limit         int64 `json:"limit_bytes"`
			Actual        int64 `json:"actual_bytes"`
			AtLeast       bool  `json:"actual_bytes_at_least"`
		}
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	e := result.Error
	if e.Type != "ccdad_payload_too_large" || e.Limit != limit || e.Actual != actual || e.AtLeast != atLeast {
		t.Fatalf("unexpected error: %s", w.Body.String())
	}
	for _, part := range []string{"Payload Too Large", strconv.FormatInt(limit, 10), strconv.FormatInt(actual, 10)} {
		if !strings.Contains(e.Message, part) {
			t.Fatalf("message does not contain %q: %s", part, e.Message)
		}
	}
	if atLeast && !strings.Contains(e.Message, "at least") {
		t.Fatal("partial body count was presented as an exact size")
	}
}

func TestBodyLimitReadsOnlyOneOverflowByteEvenIfContentLengthUnderstatesSize(t *testing.T) {
	for _, length := range []int64{-1, 1} {
		reader := &countBody{remaining: 10000}
		r := httptest.NewRequest(http.MethodPost, ResponsesPath, reader)
		r.ContentLength = length
		_, err := readBody(r, 1024)
		var tooLarge *bodyTooLarge
		if !errors.As(err, &tooLarge) || tooLarge.ActualBytes != 1025 || reader.read != 1025 {
			t.Fatalf("read %d bytes, error %v", reader.read, err)
		}
	}
}

type countBody struct{ remaining, read int64 }

func (b *countBody) Read(p []byte) (int, error) {
	if b.remaining == 0 {
		return 0, io.EOF
	}
	n := len(p)
	if int64(n) > b.remaining {
		n = int(b.remaining)
	}
	clear(p[:n])
	b.remaining -= int64(n)
	b.read += int64(n)
	return n, nil
}

func TestOversizedUnauthenticatedBodiesAreNotRead(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) { t.Error("unauthenticated request reached upstream") })
	s := f.server(t, f.config())
	reader := &countBody{remaining: 300 << 20}
	r := httptest.NewRequest(http.MethodPost, ResponsesPath, reader)
	r.ContentLength = 300 << 20
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized || reader.read != 0 {
		t.Fatalf("unauthenticated response = %d, read %d bytes", w.Code, reader.read)
	}
}

func TestProxyRejectsInvalidBodyLimitConfiguration(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {})
	for _, limit := range []int64{-1, 9223372036854775807} {
		cfg := f.config()
		cfg.MaxBodyBytes = limit
		if _, err := newServer(cfg); err == nil {
			t.Fatalf("accepted invalid body limit %d", limit)
		}
	}
}
