package orchestrator_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/418-cloud/krayt/internal/orchestrator"
)

// Env vars that turn a re-exec of this test binary into a slot-acquiring helper process, so the
// cross-process limit can be proven with real separate processes (not just goroutines).
const (
	slotHelperDir  = "KRAYT_TEST_SLOT_DIR"
	slotHelperTag  = "KRAYT_TEST_SLOT_TAG"
	slotHelperHold = "KRAYT_TEST_SLOT_HOLD_MS"
)

// TestMain doubles as the slot helper: when slotHelperDir is set it acquires one slot (max=1),
// records the held interval to its tag file, holds, releases, and exits — never running the
// suite. Otherwise it runs the tests normally.
func TestMain(m *testing.M) {
	if dir := os.Getenv(slotHelperDir); dir != "" {
		hold, _ := strconv.Atoi(os.Getenv(slotHelperHold))
		rel, err := orchestrator.AcquireSlot(context.Background(), dir, 1)
		if err != nil {
			os.Exit(3)
		}
		start := time.Now().UnixNano()
		time.Sleep(time.Duration(hold) * time.Millisecond)
		end := time.Now().UnixNano()
		_ = os.WriteFile(filepath.Join(dir, os.Getenv(slotHelperTag)), []byte(fmt.Sprintf("%d %d", start, end)), 0o644)
		rel()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// TestAcquireSlotLimits proves the file-lock semaphore caps concurrency at max and actually
// reaches it (not accidentally serialized), using goroutines whose separate flock fds contend
// exactly as separate processes would (§6.2).
func TestAcquireSlotLimits(t *testing.T) {
	dir := t.TempDir()
	const limit = 2
	var mu sync.Mutex
	var inFlight, peak int
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rel, err := orchestrator.AcquireSlot(context.Background(), dir, limit)
			if err != nil {
				t.Errorf("acquire: %v", err)
				return
			}
			mu.Lock()
			inFlight++
			if inFlight > peak {
				peak = inFlight
			}
			mu.Unlock()
			time.Sleep(80 * time.Millisecond)
			mu.Lock()
			inFlight--
			mu.Unlock()
			rel()
		}()
	}
	wg.Wait()
	if peak > limit {
		t.Errorf("peak concurrency %d exceeded limit %d", peak, limit)
	}
	if peak < limit {
		t.Errorf("peak concurrency %d never reached limit %d (limiter too strict)", peak, limit)
	}
}

// TestAcquireSlotUnbounded: max <= 0 imposes no limit and its release is a safe no-op.
func TestAcquireSlotUnbounded(t *testing.T) {
	dir := t.TempDir()
	rel, err := orchestrator.AcquireSlot(context.Background(), dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	rel()
	if _, err := os.Stat(filepath.Join(dir, "slots")); !os.IsNotExist(err) {
		t.Errorf("unbounded should not create a slots dir (err=%v)", err)
	}
}

// TestAcquireSlotCrossProcess is the headline §6.2 proof: two independent processes sharing one
// .krayt with max=1 hold the slot in non-overlapping intervals — the limit really is enforced
// across processes, not just within one.
func TestAcquireSlotCrossProcess(t *testing.T) {
	dir := t.TempDir()
	const holdMS = 400
	launch := func(tag string) *exec.Cmd {
		c := exec.Command(os.Args[0], "-test.run=^$")
		c.Env = append(os.Environ(),
			slotHelperDir+"="+dir, slotHelperTag+"="+tag, slotHelperHold+"="+strconv.Itoa(holdMS))
		return c
	}
	a, b := launch("a"), launch("b")
	if err := a.Start(); err != nil {
		t.Fatal(err)
	}
	if err := b.Start(); err != nil {
		t.Fatal(err)
	}
	if err := a.Wait(); err != nil {
		t.Fatalf("helper a: %v", err)
	}
	if err := b.Wait(); err != nil {
		t.Fatalf("helper b: %v", err)
	}
	as, ae := readInterval(t, filepath.Join(dir, "a"))
	bs, be := readInterval(t, filepath.Join(dir, "b"))
	if as < be && bs < ae { // intervals overlap
		t.Errorf("held intervals overlap across processes: a=[%d,%d] b=[%d,%d]", as, ae, bs, be)
	}
}

func readInterval(t *testing.T, path string) (int64, int64) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var s, e int64
	if _, err := fmt.Sscanf(string(b), "%d %d", &s, &e); err != nil {
		t.Fatalf("parse %s (%q): %v", path, b, err)
	}
	return s, e
}
