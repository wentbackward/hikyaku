package capture

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestNew_DisabledWhenNotEnabled(t *testing.T) {
	c, err := New(Config{Enabled: false, OutputFolder: t.TempDir()})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c != nil {
		t.Error("capture should be nil when Enabled=false")
	}
}

func TestNew_DisabledWhenFolderEmpty(t *testing.T) {
	c, err := New(Config{Enabled: true, OutputFolder: ""})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c != nil {
		t.Error("capture should be nil when OutputFolder is empty (security: no default location)")
	}
}

func TestNew_DefaultsMaxMessages(t *testing.T) {
	c, err := New(Config{Enabled: true, OutputFolder: t.TempDir()})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.MaxMessages() != DefaultMaxMessages {
		t.Errorf("max_messages: got %d, want %d", c.MaxMessages(), DefaultMaxMessages)
	}
}

func TestNew_CreatesOutputFolder(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "new", "nested")
	c, err := New(Config{Enabled: true, OutputFolder: dir, MaxMessages: 3})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c == nil {
		t.Fatal("capture should be created")
	}
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("output folder not created: %v", err)
	}
}

func TestNilSafe(t *testing.T) {
	// Every method on *Capture must be nil-safe so callers can pass
	// a nil handle without guards.
	var c *Capture
	if c.Configured() {
		t.Error("nil.Configured() should be false")
	}
	if c.OutputFolder() != "" {
		t.Error("nil.OutputFolder() should be empty")
	}
	if c.MaxMessages() != 0 {
		t.Error("nil.MaxMessages() should be 0")
	}
	if c.Arm() != 0 {
		t.Error("nil.Arm() should return 0")
	}
	if c.ArmWith(3, nil) != 0 {
		t.Error("nil.ArmWith() should return 0")
	}
	c.Disarm()
	if c.Remaining() != 0 || c.WindowSize() != 0 {
		t.Error("nil.Remaining()/WindowSize() should be 0")
	}
	if c.Reserve(Request{}) != nil {
		t.Error("nil.Reserve() should return nil")
	}

	var s *Slot
	if err := s.Write(Payload{}); err != nil {
		t.Errorf("nil slot Write should be no-op, got %v", err)
	}
}

func TestArmAndReserve_RespectsMax(t *testing.T) {
	c, err := New(Config{Enabled: true, OutputFolder: t.TempDir(), MaxMessages: 3})
	if err != nil {
		t.Fatal(err)
	}

	// Before Arm, window is closed.
	if s := c.Reserve(Request{}); s != nil {
		t.Error("Reserve should return nil before Arm")
	}

	c.Arm()
	for i := 0; i < 3; i++ {
		if s := c.Reserve(Request{}); s == nil {
			t.Errorf("reserve %d should succeed", i+1)
		}
	}
	if s := c.Reserve(Request{}); s != nil {
		t.Error("4th reserve should fail — window is exhausted")
	}
}

func TestArm_RearmsAfterExhaustion(t *testing.T) {
	c, _ := New(Config{Enabled: true, OutputFolder: t.TempDir(), MaxMessages: 2})
	c.Arm()
	c.Reserve(Request{})
	c.Reserve(Request{})
	if c.Reserve(Request{}) != nil {
		t.Fatal("should be exhausted")
	}
	c.Arm()
	if c.Reserve(Request{}) == nil {
		t.Error("should be re-armed after second Arm")
	}
}

func TestReserve_ConcurrentSafe(t *testing.T) {
	c, _ := New(Config{Enabled: true, OutputFolder: t.TempDir(), MaxMessages: 100})
	c.Arm()

	var wg sync.WaitGroup
	var mu sync.Mutex
	reserved := 0
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if c.Reserve(Request{}) != nil {
				mu.Lock()
				reserved++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if reserved != 100 {
		t.Errorf("concurrent reserve: got %d slots, want exactly 100", reserved)
	}
}

func TestWrite_FileContents(t *testing.T) {
	dir := t.TempDir()
	c, _ := New(Config{Enabled: true, OutputFolder: dir, MaxMessages: 1})
	c.Arm()
	slot := c.Reserve(Request{})
	if slot == nil {
		t.Fatal("expected slot")
	}

	now := time.Now()
	p := Payload{
		RequestID: "abc12345",
		Timestamp: now.UTC().Format(time.RFC3339Nano),
		Request: RequestSnapshot{
			Method:       "POST",
			Path:         "/v1/chat/completions",
			VirtualModel: "gresh-general",
			RealModel:    "Qwen/Qwen3",
			Backend:      "vllm",
			Protocol:     "openai",
			Streaming:    true,
			Headers:      map[string]string{"Authorization": "[redacted]", "Content-Type": "application/json"},
			Incoming:     json.RawMessage(`{"model":"gresh-general","messages":[{"role":"user","content":"hi"}]}`),
			Resolved:     json.RawMessage(`{"model":"Qwen/Qwen3","messages":[{"role":"user","content":"hi"}],"temperature":0.7}`),
		},
		Response: ResponseSnapshot{StatusCode: 200, SSE: "data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\ndata: [DONE]\n\n"},
		Timing:   TimingSnapshot{StartedAt: now.UTC().Format(time.RFC3339Nano), DurationMs: 123.4},
	}

	if err := slot.Write(p); err != nil {
		t.Fatalf("write: %v", err)
	}

	entries, _ := os.ReadDir(dir)
	var jsonFiles []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".json") {
			jsonFiles = append(jsonFiles, e.Name())
		}
	}
	if len(jsonFiles) != 1 {
		t.Fatalf("expected 1 json file, got %d: %v", len(jsonFiles), jsonFiles)
	}
	if !strings.Contains(jsonFiles[0], "abc12345") {
		t.Errorf("file name should contain request_id, got %q", jsonFiles[0])
	}
	if strings.Contains(jsonFiles[0], ":") {
		t.Errorf("file name should not contain colons (filesystem unsafe), got %q", jsonFiles[0])
	}

	data, err := os.ReadFile(filepath.Join(dir, jsonFiles[0]))
	if err != nil {
		t.Fatal(err)
	}
	var got Payload
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("payload not valid JSON: %v", err)
	}
	if got.RequestID != "abc12345" {
		t.Errorf("request_id round-trip: got %q", got.RequestID)
	}
	if got.Request.VirtualModel != "gresh-general" {
		t.Errorf("virtual_model round-trip: got %q", got.Request.VirtualModel)
	}
	if got.Request.Headers["Authorization"] != "[redacted]" {
		t.Errorf("auth header should be preserved as [redacted]: %q", got.Request.Headers["Authorization"])
	}
}

func TestWrite_FilePermissions(t *testing.T) {
	dir := t.TempDir()
	c, _ := New(Config{Enabled: true, OutputFolder: dir, MaxMessages: 1})
	c.Arm()
	slot := c.Reserve(Request{})

	p := Payload{RequestID: "x", Timestamp: time.Now().UTC().Format(time.RFC3339Nano)}
	if err := slot.Write(p); err != nil {
		t.Fatal(err)
	}

	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		info, _ := e.Info()
		mode := info.Mode().Perm()
		if mode != 0o600 {
			t.Errorf("capture file %s: got mode %o, want 0600", e.Name(), mode)
		}
	}
}

func TestCappedBuffer(t *testing.T) {
	b := &CappedBuffer{Max: 10}
	n, _ := b.Write([]byte("hello"))
	if n != 5 || b.Truncated {
		t.Errorf("first write: n=%d truncated=%v", n, b.Truncated)
	}
	n, _ = b.Write([]byte(" world!!!!!"))
	if n != 11 {
		t.Errorf("overflow write should report full input length, got %d", n)
	}
	if !b.Truncated {
		t.Error("should be marked truncated")
	}
	if b.String() != "hello worl" {
		t.Errorf("stored %q, want %q", b.String(), "hello worl")
	}

	// Further writes after truncation drop silently
	n, _ = b.Write([]byte("more"))
	if n != 4 {
		t.Errorf("post-truncate write should claim success, got %d", n)
	}
	if b.String() != "hello worl" {
		t.Errorf("truncated buffer mutated: got %q", b.String())
	}
}

// ── Programmatic arming and filters (the embedder seam) ──────────────────────

func TestArmWith_FilterSkipsNonMatchingWithoutConsuming(t *testing.T) {
	c, _ := New(Config{Enabled: true, OutputFolder: t.TempDir(), MaxMessages: 2})
	onlyRouteA := func(r Request) bool { return r.VirtualModel == "a" }
	if n := c.ArmWith(2, onlyRouteA); n != 2 {
		t.Fatalf("ArmWith = %d, want 2", n)
	}
	// Non-matching requests never consume a slot, however many arrive.
	for i := 0; i < 10; i++ {
		if s := c.Reserve(Request{VirtualModel: "b"}); s != nil {
			t.Fatalf("request %d for route b must not be captured", i)
		}
	}
	if c.Remaining() != 2 {
		t.Fatalf("Remaining = %d after non-matching traffic, want 2", c.Remaining())
	}
	for i := 0; i < 2; i++ {
		if c.Reserve(Request{VirtualModel: "a"}) == nil {
			t.Fatalf("matching request %d must be captured", i)
		}
	}
	if c.Reserve(Request{VirtualModel: "a"}) != nil {
		t.Fatal("window must close after the count is reached")
	}
	if c.Remaining() != 0 {
		t.Fatalf("Remaining = %d, want 0", c.Remaining())
	}
}

func TestArmWith_CountDefaultsAndClamps(t *testing.T) {
	c, _ := New(Config{Enabled: true, OutputFolder: t.TempDir(), MaxMessages: 4})
	if n := c.ArmWith(0, nil); n != 4 {
		t.Errorf("ArmWith(0) = %d, want the configured window 4", n)
	}
	if n := c.ArmWith(-7, nil); n != 4 {
		t.Errorf("ArmWith(-7) = %d, want the configured window 4", n)
	}
	if n := c.ArmWith(9, nil); n != 9 || c.WindowSize() != 9 || c.Remaining() != 9 {
		t.Errorf("ArmWith(9) = %d (window %d, remaining %d), want 9: a programmatic window is not bound by max_messages", n, c.WindowSize(), c.Remaining())
	}
	if n := c.ArmWith(MaxAllowedMessages+1, nil); n != MaxAllowedMessages {
		t.Errorf("ArmWith(max+1) = %d, want the clamp %d", n, MaxAllowedMessages)
	}
}

func TestArm_IsTheConfiguredWindowWithoutFilter(t *testing.T) {
	c, _ := New(Config{Enabled: true, OutputFolder: t.TempDir(), MaxMessages: 2})
	c.ArmWith(5, func(Request) bool { return false })
	// SIGUSR1 re-arms the configured window and drops any filter: every
	// request is captured again, exactly as before the seam existed.
	if n := c.Arm(); n != 2 {
		t.Fatalf("Arm = %d, want 2", n)
	}
	if c.Reserve(Request{VirtualModel: "anything"}) == nil {
		t.Fatal("Arm must capture everything (no filter)")
	}
}

func TestDisarm_ClosesWindow(t *testing.T) {
	c, _ := New(Config{Enabled: true, OutputFolder: t.TempDir(), MaxMessages: 5})
	c.Arm()
	c.Disarm()
	if c.Reserve(Request{}) != nil {
		t.Fatal("Reserve must fail after Disarm")
	}
	if c.Remaining() != 0 || c.WindowSize() != 0 {
		t.Fatalf("Remaining/WindowSize = %d/%d after Disarm, want 0/0", c.Remaining(), c.WindowSize())
	}
	c.Arm()
	if c.Reserve(Request{}) == nil {
		t.Fatal("re-arm after Disarm must work")
	}
}

func TestRemaining_ReportsTheOpenWindow(t *testing.T) {
	c, _ := New(Config{Enabled: true, OutputFolder: t.TempDir(), MaxMessages: 3})
	if c.Remaining() != 0 || c.WindowSize() != 0 {
		t.Fatal("no window before Arm")
	}
	c.Arm()
	c.Reserve(Request{})
	if c.Remaining() != 2 || c.WindowSize() != 3 {
		t.Fatalf("Remaining/WindowSize = %d/%d, want 2/3", c.Remaining(), c.WindowSize())
	}
}

func TestSlot_SequenceCountsWithinTheArmedWindow(t *testing.T) {
	c, _ := New(Config{Enabled: true, OutputFolder: t.TempDir(), MaxMessages: 2})
	c.ArmWith(3, nil)
	for want := 1; want <= 3; want++ {
		s := c.Reserve(Request{})
		if s == nil || s.seq != want || s.total != 3 {
			t.Fatalf("slot %d = %+v, want seq %d of 3", want, s, want)
		}
	}
}

func TestReserve_FilterConcurrentSafe(t *testing.T) {
	c, _ := New(Config{Enabled: true, OutputFolder: t.TempDir(), MaxMessages: 1})
	c.ArmWith(50, func(r Request) bool { return r.Backend == "yes" })
	var wg sync.WaitGroup
	var mu sync.Mutex
	reserved := 0
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			b := "no"
			if i%2 == 0 {
				b = "yes"
			}
			if c.Reserve(Request{Backend: b}) != nil {
				mu.Lock()
				reserved++
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()
	if reserved != 50 {
		t.Errorf("concurrent filtered reserve: got %d slots, want exactly 50", reserved)
	}
}
