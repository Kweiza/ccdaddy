package codexproxy

import (
	"io"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/usage"
)

// sink collects the samples the proxy harvests.
type sink struct {
	mu      sync.Mutex
	uuids   []string
	samples []*usage.Snapshot
}

func (s *sink) take(uuid string, snap *usage.Snapshot) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.uuids = append(s.uuids, uuid)
	s.samples = append(s.samples, snap)
}

func (s *sink) all() ([]string, []*usage.Snapshot) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.uuids...), append([]*usage.Snapshot(nil), s.samples...)
}

func wantWindow(t *testing.T, name string, w usage.Window, pct float64, length time.Duration, reset int64) {
	t.Helper()
	if !w.Present {
		t.Fatalf("%s window is absent", name)
	}
	got, ok := w.Percent()
	if !ok || got != pct {
		t.Errorf("%s percent = (%v, %v), want %v", name, got, ok, pct)
	}
	gotLen, ok := w.Length()
	if !ok || gotLen != length {
		t.Errorf("%s length = (%v, %v), want %v", name, gotLen, ok, length)
	}
	gotReset, ok := w.Reset()
	if !ok || gotReset.Unix() != reset {
		t.Errorf("%s reset = (%v, %v), want epoch %d", name, gotReset, ok, reset)
	}
}

func TestTheRateLimitHeadersBecomeAUsageSample(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Codex-Primary-Used-Percent", "42.5")
		w.Header().Set("X-Codex-Primary-Window-Minutes", "300")
		w.Header().Set("X-Codex-Primary-Reset-At", "1790000000")
		w.Header().Set("X-Codex-Secondary-Used-Percent", "7")
		w.Header().Set("X-Codex-Secondary-Window-Minutes", "10080")
		w.Header().Set("X-Codex-Secondary-Reset-At", "1790600000")
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, sse(`{"type":"response.completed"}`))
	})
	f.add("uuid-a", "a@example.com", "access-a")
	var got sink
	cfg := f.config()
	cfg.Harvest = got.take
	s := f.server(t, cfg)

	if w := post(s, unpinnedSecret, nil, `{"input":[]}`); w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	uuids, samples := got.all()
	if len(samples) == 0 {
		t.Fatal("no usage sample was harvested from the response headers")
	}
	if uuids[0] != "uuid-a" {
		t.Fatalf("the sample was attributed to %q, want uuid-a", uuids[0])
	}
	wantWindow(t, "primary", samples[0].CodexPrimary, 42.5, 300*time.Minute, 1790000000)
	wantWindow(t, "secondary", samples[0].CodexSecondary, 7, 10080*time.Minute, 1790600000)
}

func TestTheHeadersOnARateLimitedAnswerAreHarvestedToo(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Codex-Primary-Used-Percent", "100")
		w.Header().Set("X-Codex-Primary-Window-Minutes", "300")
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, `{"error":{"type":"usage_limit_reached"}}`)
	})
	f.add("uuid-a", "a@example.com", "access-a")
	var got sink
	cfg := f.config()
	cfg.Harvest = got.take
	cfg.RankedEligible = func() []string { return []string{"uuid-a"} }
	s := f.server(t, cfg)

	post(s, unpinnedSecret, nil, `{"input":[]}`)
	_, samples := got.all()
	if len(samples) == 0 {
		t.Fatal("the reading on a 429 was thrown away; it is the most informative one there is")
	}
	if pct, ok := samples[0].CodexPrimary.Percent(); !ok || pct != 100 {
		t.Fatalf("primary percent = (%v, %v), want 100", pct, ok)
	}
}

func TestTheInStreamRateLimitEventBecomesAUsageSample(t *testing.T) {
	event := `{"type":"codex.rate_limits","rate_limits":{"primary":{"used_percent":63,"window_minutes":300,"reset_at":1790000000},"secondary":null}}`
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "event: codex.rate_limits\ndata: "+event+"\n\ndata: [DONE]\n\n")
	})
	f.add("uuid-a", "a@example.com", "access-a")
	var got sink
	cfg := f.config()
	cfg.Harvest = got.take
	s := f.server(t, cfg)

	if w := post(s, unpinnedSecret, nil, `{"input":[]}`); w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	uuids, samples := got.all()
	if len(samples) != 1 {
		t.Fatalf("harvested %d samples, want 1", len(samples))
	}
	if uuids[0] != "uuid-a" {
		t.Fatalf("the sample was attributed to %q, want uuid-a", uuids[0])
	}
	wantWindow(t, "primary", samples[0].CodexPrimary, 63, 300*time.Minute, 1790000000)
	if samples[0].CodexSecondary.Present {
		t.Error("a null secondary window was read as present")
	}
}

func TestAnEventSplitAcrossTwoWritesIsStillRead(t *testing.T) {
	line := `data: {"type":"codex.rate_limits","rate_limits":{"primary":{"used_percent":11,"window_minutes":300,"reset_at":1790000000}}}` + "\n"
	var got []*usage.Snapshot
	sc := &sseScanner{on: func(snap *usage.Snapshot) { got = append(got, snap) }}

	sc.write([]byte(line[:20]))
	if len(got) != 0 {
		t.Fatal("half a line produced a sample")
	}
	sc.write([]byte(line[20:]))
	if len(got) != 1 {
		t.Fatalf("harvested %d samples, want 1 once the line completed", len(got))
	}
	if pct, ok := got[0].CodexPrimary.Percent(); !ok || pct != 11 {
		t.Fatalf("primary percent = (%v, %v), want 11", pct, ok)
	}
}

func TestOnlyRateLimitEventsAreHarvested(t *testing.T) {
	var got []*usage.Snapshot
	sc := &sseScanner{on: func(snap *usage.Snapshot) { got = append(got, snap) }}

	sc.write([]byte("event: response.output_text.delta\n"))
	sc.write([]byte(`data: {"type":"response.output_text.delta","delta":"hello"}` + "\n"))
	sc.write([]byte("data: [DONE]\n"))
	sc.write([]byte("data: not json at all\n"))
	sc.write([]byte(`data: {"type":"codex.rate_limits"}` + "\n"))
	sc.write([]byte("\n"))
	sc.write([]byte(`data: {"type":"response.completed","rate_limits":{"primary":{"used_percent":99}}}` + "\n"))

	if len(got) != 0 {
		t.Fatalf("harvested %d samples from a stream with no rate-limit windows in it", len(got))
	}
}

func TestAnOverlongStreamLineIsDroppedRatherThanBuffered(t *testing.T) {
	var got []*usage.Snapshot
	sc := &sseScanner{on: func(snap *usage.Snapshot) { got = append(got, snap) }}

	huge := make([]byte, maxSSELine+1024)
	for i := range huge {
		huge[i] = 'x'
	}
	sc.write([]byte("data: "))
	// streamBack never hands the scanner more than streamChunk bytes at a time,
	// so the junk arrives here the way the network delivers it. The cap has to
	// trip on whichever piece crosses it, not on one enormous write.
	for off := 0; off < len(huge); off += streamChunk {
		end := off + streamChunk
		if end > len(huge) {
			end = len(huge)
		}
		sc.write(huge[off:end])
	}

	// The cap is asserted HERE, with the over-long line still in flight and no
	// newline yet. line() does `sc.buf, sc.over = sc.buf[:0], false`
	// unconditionally, so an assertion taken after the newline reads
	// len(sc.buf) == 0 whether or not append caps anything at all: with the
	// whole cap block deleted from append, a rebuild of this file passed the
	// post-newline form of this test, and the 66560 bytes of junk fail
	// json.Unmarshal on their own, so the sample count below caught nothing
	// either. In flight the two implementations differ by 66566 bytes.
	if len(sc.buf) > maxSSELine {
		t.Fatalf("mid-line the scanner is holding %d bytes, want at most %d", len(sc.buf), maxSSELine)
	}
	// And the cap has to LATCH, not merely truncate. An append that empties buf
	// without setting over keeps the byte count down and then feeds the tail of
	// the junk to line() as though it were a line of its own.
	if !sc.over {
		t.Fatal("the scanner buffered past the cap without marking the line over-long")
	}

	sc.write([]byte("\n"))

	if len(got) != 0 {
		t.Fatalf("an over-long line produced %d samples", len(got))
	}

	// And the scanner still works on the next line.
	sc.write([]byte(`data: {"type":"codex.rate_limits","rate_limits":{"primary":{"used_percent":5,"window_minutes":300}}}` + "\n"))
	if len(got) != 1 {
		t.Fatalf("harvested %d samples after an over-long line, want 1", len(got))
	}
}
