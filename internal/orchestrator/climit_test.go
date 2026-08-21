package orchestrator_test

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/418-cloud/krayt/internal/orchestrator"
	"github.com/418-cloud/krayt/internal/proxy"
)

// Env vars that turn a re-exec of this test binary into a slot-acquiring helper process, so the
// cross-process limit can be proven with real separate processes (not just goroutines).
const (
	slotHelperDir  = "KRAYT_TEST_SLOT_DIR"
	slotHelperTag  = "KRAYT_TEST_SLOT_TAG"
	slotHelperHold = "KRAYT_TEST_SLOT_HOLD_MS"
)

// egressHelperArg is the first argv element spawnEgressProxy gives a KRAYT_EGRESS_PROXY_BIN
// replacement (`--mode <mode> …`), and so is how a re-exec of this test binary recognizes itself as
// the egress-proxy child rather than the slot helper or the test suite — see the TestMain doc
// below. It is argv and not an env var on purpose: spawnEgressProxy hands the child an explicit,
// minimal environment (egressProxyChildEnvKeys), so no test-only marker can ride along in it.
const egressHelperArg = "--mode"

// TestMain triples as: (1) the slot-acquiring helper process used by
// TestAcquireSlotCrossProcess, (2) the egress-proxy child process EVERY orchestrator.Run call
// in this package's tests spawns for real (§4/§6 of move-egress-proxy-to-host.md — there is no
// fake/no-op path in production, so there should not be one in tests either), or (3) the test
// suite itself.
//
// For (2): rather than hand every orchestrator.Run call site a purpose-built stub binary, this
// test binary points orchestrator.EgressProxyBinEnv at itself before running the suite. Every
// child spawned that way reruns this same TestMain, which recognizes the proxy contract's leading
// `--mode` argument (egressHelperArg) and behaves as the child (adopt fd 3, run the real
// proxy.Serve loop) instead of recursing into the suite. The marker has to be argv: spawnEgressProxy
// gives the child an explicit, minimal environment (egressProxyChildEnvKeys), so an env-var flag
// set on this process before m.Run() would NOT reach it. This exercises the genuine fd-passing +
// allowlist-enforcement path end to end in every orchestrator test, not a mock.
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
	if len(os.Args) > 1 && os.Args[1] == egressHelperArg {
		os.Exit(runEgressHelper())
	}
	if self, err := os.Executable(); err == nil {
		_ = os.Setenv(orchestrator.EgressProxyBinEnv, self)
	}
	os.Exit(m.Run())
}

// runEgressHelper is this test binary re-exec'd as a `krayt __egress-proxy` stand-in: it
// behaves exactly like the real hidden subcommand (internal/cli/egressproxy.go) — adopt fd 3,
// read the stdin config, report the CA cert (if any) over fd 4, serve — but lives here so every
// orchestrator-package test that calls orchestrator.Run gets a REAL child process without
// repeating this wiring per test.
func runEgressHelper() int {
	fs := flag.NewFlagSet("egress-helper", flag.ContinueOnError)
	mode := fs.String("mode", proxy.ModeAllowlist, "")
	allowCSV := fs.String("allow", "", "")
	mitm := fs.Bool("mitm", false, "")
	runID := fs.String("run-id", "", "")
	if err := fs.Parse(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "egress-helper: parse flags:", err)
		return 1
	}
	lis, err := proxy.ListenerFromFD(3)
	if err != nil {
		fmt.Fprintln(os.Stderr, "egress-helper:", err)
		return 1
	}
	var allow []string
	if *allowCSV != "" {
		allow = strings.Split(*allowCSV, ",")
	}
	stdinCfg, err := proxy.ReadStdinConfig(os.Stdin)
	if err != nil {
		fmt.Fprintln(os.Stderr, "egress-helper:", err)
		return 1
	}
	policy := proxy.Policy{
		Mode: *mode, Allow: allow, MITM: *mitm,
		Passthrough: stdinCfg.Passthrough, Inject: stdinCfg.Inject,
		// Mirrors internal/cli/egressproxy.go's envEnabled(EgressProxyLogRequestsEnv): the
		// observation log is the one feature that reaches the child through its environment, so a
		// stand-in that ignored it would let spawnEgressProxy quietly stop forwarding the variable
		// without any test noticing (TestSpawnEgressProxyForwardsLogRequestsEnv).
		LogRequests: os.Getenv("KRAYT_PROXY_LOG_REQUESTS") == "1",
	}
	h, ca, err := proxy.BuildHandler(policy, "", *runID)
	if err != nil {
		fmt.Fprintln(os.Stderr, "egress-helper:", err)
		return 1
	}
	if caW := os.NewFile(4, "ca-cert"); caW != nil {
		if ca != nil {
			_, _ = caW.Write(ca.CACertPEM())
		}
		_ = caW.Close()
	}
	if err := proxy.ServeHandler(context.Background(), lis, h); err != nil {
		fmt.Fprintln(os.Stderr, "egress-helper:", err)
		return 1
	}
	return 0
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
