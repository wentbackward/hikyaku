package proxy

// The streamed lanes' line buffer: a partial line is carried between reads
// with amortized growth, up to the line cap; a longer line is a
// responseTooLargeError. Both parsers (SSE and Ollama NDJSON) share it.

import (
	"errors"
	"io"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/wentbackward/hikyaku/internal/telemetry"
)

// feedInChunks feeds s to feed in chunks of size n and returns the first
// limit error the parser reports.
func feedInChunks(s string, n int, feed func([]byte) (int, []byte), limitErr func() error) error {
	for i := 0; i < len(s); i += n {
		feed([]byte(s[i:min(i+n, len(s))]))
		if err := limitErr(); err != nil {
			return err
		}
	}
	return nil
}

// totalAlloc runs f and returns the bytes the process allocated meanwhile.
func totalAlloc(f func()) uint64 {
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	f()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

// A line carried across many small reads costs memory linear in its length.
// The buffer grows by append (Go grows a large slice by about 1.25x per
// reallocation, so the copies sum to about 5x the line), never by copying
// everything buffered so far on each read, which allocates about n²/2c bytes
// for a line of n bytes in reads of c bytes: 1 GiB for this 1 MiB line, about
// 1024x. The bound of 16x leaves room for growth-policy changes between Go
// releases and is still far below the quadratic cost. The lineBuffer is
// measured on its own (a no-op callback, input slices built beforehand), so
// nothing else in the process inflates the count.
func TestLineBuffer_AmortizedGrowth(t *testing.T) {
	const lineLen, chunk = 1 << 20, 512
	line := []byte(strings.Repeat("x", lineLen) + "\n")
	var lines int
	var b lineBuffer
	alloc := totalAlloc(func() {
		for i := 0; i < len(line); i += chunk {
			if _, _, err := b.split(line[i:min(i+chunk, len(line))], func([]byte) bool { lines++; return true }); err != nil {
				t.Fatalf("limit error on a line under the cap: %v", err)
			}
		}
	})
	if lines != 1 {
		t.Fatalf("split handled %d lines, want 1", lines)
	}
	t.Logf("allocated %.2fx the line", float64(alloc)/lineLen)
	if alloc > 16*lineLen {
		t.Errorf("carrying a %d-byte line in %d-byte reads allocated %d bytes (%.1fx), want at most 16x (linear)", lineLen, chunk, alloc, float64(alloc)/lineLen)
	}
}

// Both parsers parse a line carried across many reads: the counts on a
// 1 MiB line arrive.
func TestLineBuffer_LongLineParsedSSE(t *testing.T) {
	line := `data: {"choices":[],"usage":{"prompt_tokens":11,"completion_tokens":7},"pad":"` +
		strings.Repeat("x", 1<<20) + `"}` + "\n"
	p := makeParser(t, "openai")
	if err := feedInChunks(line, 512, p.feed, p.limitErr); err != nil {
		t.Fatalf("limit error on a line under the cap: %v", err)
	}
	if got := p.usage(); got.prompt != 11 || got.completion != 7 {
		t.Errorf("usage on the long line = %+v, want 11/7 (the line is parsed)", got)
	}
}

func TestLineBuffer_LongLineParsedOllama(t *testing.T) {
	line := `{"done":true,"prompt_eval_count":11,"eval_count":7,"context":[` +
		strings.Repeat("1,", 1<<19) + `1]}` + "\n"
	p := makeOllamaParser(t)
	if err := feedInChunks(line, 512, p.feed, p.limitErr); err != nil {
		t.Fatalf("limit error on a line under the cap: %v", err)
	}
	if got := p.usage(); got.prompt != 11 || got.completion != 7 {
		t.Errorf("usage on the long line = %+v, want 11/7 (the line is parsed)", got)
	}
}

// When an oversized line follows complete lines in the same read, those
// lines are parsed and forwarded: feed reports their length, and the limit
// error stops only what follows.
func TestLineBuffer_LinesBeforeOversizedLineAreForwarded(t *testing.T) {
	const lineCap = 4096
	usage := `data: {"choices":[],"usage":{"prompt_tokens":11,"completion_tokens":7}}` + "\n"
	in := []byte(usage + "\n:" + strings.Repeat("x", lineCap) + "\n")
	p := makeParser(t, "openai")
	p.lines.max = lineCap
	fwd, tail := p.feed(in)
	assertLineTooLarge(t, p.limitErr(), lineCap)
	if fwd != len(usage)+1 || tail != nil {
		t.Errorf("feed = %d, %q; want %d (the two complete lines), nil", fwd, tail, len(usage)+1)
	}
	if got := p.usage(); got.prompt != 11 || got.completion != 7 {
		t.Errorf("usage = %+v, want 11/7 (the line before the oversized one is parsed)", got)
	}

	var b lineBuffer
	b.max = lineCap
	n, stopped, err := b.split([]byte("a\nbb\n"+strings.Repeat("y", lineCap+1)), func([]byte) bool { return true })
	if n != 5 || stopped || err == nil {
		t.Errorf("split = %d, %v, %v; want 5, false, a limit error", n, stopped, err)
	}
}

// Ollama's /api/generate puts the whole context (token ids) on its final
// object with the counts; a long context makes that line far longer than a
// chunk. It is counted, not dropped, as long as it fits the line cap.
func TestOllama_LongFinalLineIsCounted(t *testing.T) {
	p := makeOllamaParser(t)
	final := `{"done":true,"response":"","context":[` + strings.Repeat("123456,", 40000) + `1],"prompt_eval_count":11,"eval_count":3}`
	if err := feedInChunks(final+"\n", 4096, p.feed, p.limitErr); err != nil {
		t.Fatalf("limit error: %v", err)
	}
	if got, want := p.usage(), (usageCounts{prompt: 11, completion: 3}); got != want {
		t.Errorf("usage() = %+v, want %+v", got, want)
	}
}

// On both parsers a line of exactly the cap is accepted and a line one byte
// over it is a limit error, whether it arrives split or in one read, ended
// or not; the buffer is released and nothing more is parsed.
func TestLineBuffer_CapIsInclusive(t *testing.T) {
	const lineCap = 4096
	sseLine := func(n int) string { // an SSE comment line of exactly n bytes before '\n'
		return ":" + strings.Repeat("x", n-1)
	}
	ndLine := func(n int) string { // an NDJSON object of exactly n bytes before '\n'
		return `{"pad":"` + strings.Repeat("x", n-10) + `"}`
	}
	type parser struct {
		feed     func([]byte) (int, []byte)
		limitErr func() error
		pending  func() int
	}
	newSSE := func() parser {
		p := makeParser(t, "openai")
		p.lines.max = lineCap
		return parser{p.feed, p.limitErr, func() int { return len(p.lines.partial) }}
	}
	newOllama := func() parser {
		p := makeOllamaParser(t)
		p.lines.max = lineCap
		return parser{p.feed, p.limitErr, func() int { return len(p.lines.partial) }}
	}
	for _, lane := range []struct {
		name string
		mk   func() parser
		line func(int) string
	}{{"sse", newSSE, sseLine}, {"ollama", newOllama, ndLine}} {
		t.Run(lane.name+"/at-cap", func(t *testing.T) {
			p := lane.mk()
			if len(lane.line(lineCap)) != lineCap {
				t.Fatalf("test line is %d bytes", len(lane.line(lineCap)))
			}
			if err := feedInChunks(lane.line(lineCap)+"\n", 100, p.feed, p.limitErr); err != nil {
				t.Fatalf("a line of exactly the cap was refused: %v", err)
			}
			if p.pending() != 0 {
				t.Errorf("%d bytes still buffered after the line ended", p.pending())
			}
		})
		t.Run(lane.name+"/over-cap-split", func(t *testing.T) {
			p := lane.mk()
			err := feedInChunks(lane.line(lineCap+1)+"\n", 100, p.feed, p.limitErr)
			assertLineTooLarge(t, err, lineCap)
			if p.pending() != 0 {
				t.Errorf("%d bytes still buffered after the limit error", p.pending())
			}
			if fwd, tail := p.feed([]byte("more\n")); fwd != 0 || tail != nil {
				t.Errorf("feed after the limit error = %d, %q; want 0, nil", fwd, tail)
			}
		})
		t.Run(lane.name+"/over-cap-one-read", func(t *testing.T) {
			p := lane.mk()
			err := feedInChunks(lane.line(lineCap+1)+"\n", lineCap+100, p.feed, p.limitErr)
			assertLineTooLarge(t, err, lineCap)
		})
		t.Run(lane.name+"/over-cap-unterminated", func(t *testing.T) {
			p := lane.mk()
			err := feedInChunks(lane.line(lineCap+1), 100, p.feed, p.limitErr)
			assertLineTooLarge(t, err, lineCap)
		})
	}
}

func assertLineTooLarge(t *testing.T, err error, lineCap int64) {
	t.Helper()
	var tl *responseTooLargeError
	if !errors.As(err, &tl) {
		t.Fatalf("err = %v, want a *responseTooLargeError", err)
	}
	if tl.limit != limitLine || tl.max != lineCap {
		t.Errorf("err = %+v, want the line limit of %d bytes", tl, lineCap)
	}
}

// The default line cap (no config) is the config default.
func TestLineBuffer_DefaultCap(t *testing.T) {
	var b lineBuffer
	if b.limit() != 16<<20 {
		t.Errorf("default line cap = %d, want %d", b.limit(), 16<<20)
	}
}

// BenchmarkSSEParser_LongLine reports the cost of carrying one 1 MiB line
// across 512-byte reads; allocated bytes per op stay a small multiple of the
// line length.
func BenchmarkSSEParser_LongLine(b *testing.B) {
	line := "data: {\"pad\":\"" + strings.Repeat("x", 1<<20) + "\"}\n"
	m, _, err := telemetry.Init()
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.SetBytes(int64(len(line)))
	for b.Loop() {
		p := newSSEParser("be", "m", "openai", time.Now(), m, b.Context())
		_ = feedInChunks(line, 512, p.feed, p.limitErr)
	}
}

// eofAfter returns its chunks one per Read, then (0, io.EOF) forever.
type eofAfter struct{ chunks [][]byte }

func (r *eofAfter) Read(p []byte) (int, error) {
	if len(r.chunks) == 0 {
		return 0, io.EOF
	}
	n := copy(p, r.chunks[0])
	r.chunks = r.chunks[1:]
	return n, nil
}

func (r *eofAfter) Close() error { return nil }

// The line-cap error is latched: a Read after the one that returned it
// returns it again, even when the upstream would now report a clean EOF, so
// the stream's close still sees the cap hit (EndResponseTooLarge, a backend
// failure, the metric) and never a complete stream.
func TestInterceptedBody_LineCapErrorIsLatched(t *testing.T) {
	const lineCap = 64
	p := makeParser(t, "openai")
	p.lines.max = lineCap
	var closedWith error
	b := &interceptedBody{
		ReadCloser: &eofAfter{chunks: [][]byte{[]byte(":" + strings.Repeat("x", lineCap) + "\n")}},
		parser:     p,
		onClose:    func(err error) { closedWith = err },
	}
	buf := make([]byte, 1024)
	if _, err := b.Read(buf); err == nil {
		t.Fatal("the read of the oversized line returned no error")
	}
	for range 2 {
		if n, err := b.Read(buf); n != 0 || !errors.Is(err, p.limitErr()) {
			t.Errorf("a later Read = %d, %v; want 0 and the latched limit error", n, err)
		}
	}
	_ = b.Close()
	assertLineTooLarge(t, closedWith, lineCap)
}
