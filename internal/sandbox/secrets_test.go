package sandbox

import (
	"context"
	"fmt"
	"math/rand"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/418-cloud/krayt/internal/task"
)

func TestSecretArgsGoldenAndDeterministicOrder(t *testing.T) {
	// Two calls with the SAME set of specs in different input orders must produce byte-identical
	// argv (SecretArgs's own determinism guarantee) — so build the input already shuffled and pin
	// the sorted-by-key output.
	specs := []task.SecretSpec{
		{Key: "GH_TOKEN", Hosts: []string{"api.github.com", "github.com"}},
		{Key: "ANTHROPIC_API_KEY", Hosts: []string{"api.anthropic.com"}},
	}
	want := []string{
		"--secret", "ANTHROPIC_API_KEY@api.anthropic.com",
		"--secret", "GH_TOKEN@api.github.com,github.com",
	}
	got := SecretArgs(specs)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("SecretArgs = %q, want %q", got, want)
	}

	reversed := []task.SecretSpec{specs[1], specs[0]}
	if got2 := SecretArgs(reversed); !reflect.DeepEqual(got2, want) {
		t.Fatalf("SecretArgs on reversed input = %q, want the same %q", got2, want)
	}
}

func TestSecretArgsEmpty(t *testing.T) {
	if got := SecretArgs(nil); len(got) != 0 {
		t.Fatalf("SecretArgs(nil) = %v, want empty", got)
	}
}

func TestSecretEnvOnlyDeclaredKeys(t *testing.T) {
	specs := []task.SecretSpec{{Key: "ANTHROPIC_API_KEY", Hosts: []string{"api.anthropic.com"}}}
	vals := map[string]string{
		"ANTHROPIC_API_KEY": "sk-ant-real-value",
		"GH_TOKEN":          "ghp-real-value", // present in the secrets file but NOT declared
	}
	env, err := SecretEnv(specs, vals)
	if err != nil {
		t.Fatalf("SecretEnv: %v", err)
	}
	want := []string{"ANTHROPIC_API_KEY=sk-ant-real-value"}
	if !reflect.DeepEqual(env, want) {
		t.Fatalf("SecretEnv = %v, want %v (only the declared key, never GH_TOKEN)", env, want)
	}
}

func TestSecretEnvErrorsOnUndeclaredKeyMissingFromSecretsFile(t *testing.T) {
	specs := []task.SecretSpec{{Key: "ANTHROPIC_API_KEY", Hosts: []string{"api.anthropic.com"}}}
	_, err := SecretEnv(specs, map[string]string{"GH_TOKEN": "ghp-real-value"})
	if err == nil {
		t.Fatal("expected an error: ANTHROPIC_API_KEY is declared but the secrets file has no value for it")
	}
	if !strings.Contains(err.Error(), "ANTHROPIC_API_KEY") {
		t.Errorf("error %q does not name the missing key", err)
	}
}

func TestSecretEnvEmpty(t *testing.T) {
	env, err := SecretEnv(nil, map[string]string{"UNUSED": "value"})
	if err != nil {
		t.Fatalf("SecretEnv(nil, ...): %v", err)
	}
	if len(env) != 0 {
		t.Fatalf("SecretEnv(nil, ...) = %v, want empty", env)
	}
}

// TestNoSecretValueEverAppearsInRenderedArgv is the property test hand-secrets-to-msb.md's "Done
// when" requires: over many randomly-built CreateSpec and ExecSpec values (every field populated,
// not one hand-picked case), a secret value that was fed into SecretEnv/SecretArgs must never
// appear as a substring of the rendered argv. CreateSpec.Secrets/ExecSpec structurally have no
// field capable of holding a value (SecretRef and task.SecretSpec are both name+hosts only), so
// this also guards against a future field addition silently reopening that hole.
func TestNoSecretValueEverAppearsInRenderedArgv(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 200; i++ {
		spec, secretVals := randomCreateSpec(rng, i)
		args := spec.Args()
		assertNoValueLeaked(t, "CreateSpec.Args()", args, secretVals)

		// SecretEnv itself must never leak into argv-shaped output either — it doesn't render
		// argv, but proving the values it returns are exactly what's absent from Args() ties the
		// two functions' contracts together.
		specKeys := make([]task.SecretSpec, 0, len(secretVals))
		for k, v := range secretVals {
			specKeys = append(specKeys, task.SecretSpec{Key: k, Hosts: []string{"h.example"}})
			_ = v
		}
		if _, err := SecretEnv(specKeys, secretVals); err != nil {
			t.Fatalf("SecretEnv: %v", err)
		}

		execSpec := randomExecSpec(rng, i)
		eargs := execSpec.Args()
		assertNoValueLeaked(t, "ExecSpec.Args()", eargs, secretVals)
	}
}

func assertNoValueLeaked(t *testing.T, label string, args []string, secretVals map[string]string) {
	t.Helper()
	joined := strings.Join(args, "\x00")
	for k, v := range secretVals {
		if strings.Contains(joined, v) {
			t.Fatalf("%s leaked secret value for %q: %v", label, k, args)
		}
	}
}

// randomCreateSpec builds a CreateSpec with every field populated from random data (the "whole
// struct" the Done-when bullet asks for), plus a set of secret values that are fed to
// SecretArgs/SecretEnv's keys but never assigned anywhere in the returned CreateSpec — that
// separation is exactly what the property test asserts holds.
func randomCreateSpec(rng *rand.Rand, seed int) (CreateSpec, map[string]string) {
	n := rng.Intn(4) + 1
	secretVals := make(map[string]string, n)
	secretSpecs := make([]task.SecretSpec, n)
	for j := 0; j < n; j++ {
		key := fmt.Sprintf("SECRET_%d_%d", seed, j)
		val := "sk-real-" + randomString(rng, 24)
		hosts := randomStrings(rng, rng.Intn(2)+1)
		secretVals[key] = val
		secretSpecs[j] = task.SecretSpec{Key: key, Hosts: hosts}
	}
	refs := make([]SecretRef, len(secretSpecs))
	for j, s := range secretSpecs {
		refs[j] = SecretRef{Name: s.Key, Hosts: s.Hosts}
	}

	spec := CreateSpec{
		Image:             randomString(rng, 10),
		Name:              randomString(rng, 10),
		User:              randomString(rng, 6),
		CPUs:              rng.Intn(16),
		MemoryMiB:         uint64(rng.Intn(65536)),
		DiskGiB:           uint64(rng.Intn(200)),
		RootDisk:          randomString(rng, 12),
		MaxDuration:       time.Duration(rng.Intn(3600)) * time.Second,
		Env:               randomEnvVars(rng),
		Vsock:             randomVsockRoutes(rng),
		NetRules:          randomStrings(rng, rng.Intn(4)),
		NetDefault:        randomString(rng, 5),
		NetDefaultEgress:  randomString(rng, 5),
		NetDefaultIngress: randomString(rng, 5),
		NoNet:             rng.Intn(2) == 0,
		Secrets:           refs,
		TLSIntercept:      rng.Intn(2) == 0,
		TLSBypass:         randomStrings(rng, rng.Intn(3)),
		Security:          randomString(rng, 8),
		ExtraArgs:         randomStrings(rng, rng.Intn(3)),
	}
	return spec, secretVals
}

func randomExecSpec(rng *rand.Rand, _ int) ExecSpec {
	return ExecSpec{
		Name:    randomString(rng, 10),
		User:    randomString(rng, 6),
		Command: randomStrings(rng, rng.Intn(5)+1),
	}
}

func randomEnvVars(rng *rand.Rand) []EnvVar {
	n := rng.Intn(3)
	out := make([]EnvVar, n)
	for i := range out {
		out[i] = EnvVar{Name: randomString(rng, 6), Value: randomString(rng, 10)}
	}
	return out
}

func randomVsockRoutes(rng *rand.Rand) []VsockRoute {
	n := rng.Intn(2)
	out := make([]VsockRoute, n)
	for i := range out {
		out[i] = VsockRoute{HostPath: randomString(rng, 8), Port: uint32(rng.Intn(65535))}
	}
	return out
}

func randomStrings(rng *rand.Rand, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = randomString(rng, 5+rng.Intn(10))
	}
	return out
}

const randomStringAlphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-._"

func randomString(rng *rand.Rand, n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = randomStringAlphabet[rng.Intn(len(randomStringAlphabet))]
	}
	return string(b)
}

// TestExecEnvNeverCarriesASecretEvenAfterCreateSetOne is the Timing rule's driver-level assertion
// (hand-secrets-to-msb.md, "Assert this with a test on the driver"): msb reads a secret's value
// "at start time", so it is set on Create (the invocation that starts the sandbox) and MUST NOT
// be set again on a later Exec against the same, already-running sandbox. Uses the fake msb
// harness so the recorded child env is the real exec.Cmd.Env the driver built, not a simulation.
func TestExecEnvNeverCarriesASecretEvenAfterCreateSetOne(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	c := newFakeClient(t, home, fakeScript{Default: fakeResponse{ExitCode: 0}})

	secretEnv := []string{"ANTHROPIC_API_KEY=sk-ant-the-real-secret-value"}
	createSpec := CreateSpec{
		Image:   "agent-image:latest",
		Name:    "krayt-run-1",
		Secrets: []SecretRef{{Name: "ANTHROPIC_API_KEY", Hosts: []string{"api.anthropic.com"}}},
	}
	if err := c.Create(context.Background(), createSpec, secretEnv); err != nil {
		t.Fatalf("Create: %v", err)
	}
	createCall := lastFakeCall(t, home)
	if createCall.Env["ANTHROPIC_API_KEY"] != "sk-ant-the-real-secret-value" {
		t.Fatalf("Create's own child env = %v, want it to carry the secret (this is the ONE invocation that must)", createCall.Env)
	}
	if strings.Contains(strings.Join(createCall.Args, "\x00"), "sk-ant-the-real-secret-value") {
		t.Fatalf("Create argv leaked the secret value: %v", createCall.Args)
	}

	if _, err := c.Exec(context.Background(), ExecSpec{Name: "krayt-run-1", Command: []string{"true"}}); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	execCall := lastFakeCall(t, home)
	if v, leaked := execCall.Env["ANTHROPIC_API_KEY"]; leaked {
		t.Fatalf("Exec's child env leaked the secret set on Create: %q (env=%v)", v, execCall.Env)
	}
	if strings.Contains(strings.Join(execCall.Args, "\x00"), "sk-ant-the-real-secret-value") {
		t.Fatalf("Exec argv leaked the secret value: %v", execCall.Args)
	}
}
