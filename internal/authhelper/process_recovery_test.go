package authhelper

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

func childEnvironment(path, mode string) []string {
	result := []string{"AISTUDIO_FIXTURE_CHILD=" + mode, "AISTUDIO_FIXTURE_STATE=" + path}
	for _, key := range []string{"SystemRoot", "WINDIR", "TEMP", "TMP", "ProgramData", "USERPROFILE"} {
		if value := os.Getenv(key); value != "" {
			result = append(result, key+"="+value)
		}
	}
	return result
}
func TestAuthHelperFixtureChild(t *testing.T) {
	mode := os.Getenv("AISTUDIO_FIXTURE_CHILD")
	if mode == "" {
		t.Skip("parent-only fixture child")
	}
	path := os.Getenv("AISTUDIO_FIXTURE_STATE")
	if mode == "lock" {
		ctx, cancel := context.WithTimeout(t.Context(), 150*time.Millisecond)
		defer cancel()
		release, err := aistudio.AcquireSessionAccountLease(ctx, path)
		if err == nil {
			release()
			t.Fatal("another process acquired held lease")
		}
		return
	}
	if mode != "crash" {
		t.Fatal("invalid fixture child")
	}
	mapping := Mapping{Handle: "fixture_handle_0001", Consumer: "consumer.fixture.invalid", GaiaID: "fixture_gaia", Email: "fixture@example.invalid", StateFile: path, DirectAuth: true}
	broker, err := NewBroker(t.Context(), []Mapping{mapping}, func(context.Context, aistudio.StorageState, Mapping, bool) (aistudio.StorageState, error) {
		os.Exit(92)
		return aistudio.StorageState{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	broker.Refresh(t.Context(), mapping.Consumer, fixtureRequest(mapping, "process_crash_op_0001", 0))
	t.Fatal("fixture crash not reached")
}
func TestAccountLeaseExcludesAnotherNativeProcess(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	release, err := aistudio.AcquireSessionAccountLease(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestAuthHelperFixtureChild$")
	cmd.Env = childEnvironment(path, "lock")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("fixture child: %s %v", output, err)
	}
}
func TestProcessDeathAfterDurablePendingCannotAuthenticateAgain(t *testing.T) {
	broker, mapping, count := fixtureBroker(t, nil)
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestAuthHelperFixtureChild$")
	cmd.Env = childEnvironment(mapping.StateFile, "crash")
	err := cmd.Run()
	exit, ok := err.(*exec.ExitError)
	if !ok || exit.ExitCode() != 92 {
		t.Fatal("fixture crash failed", err)
	}
	state, loadErr := aistudio.LoadStorageState(mapping.StateFile)
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	meta, metaErr := state.Session()
	if metaErr != nil || meta == nil || meta.Authority.Operation == nil {
		t.Fatal("pending not durable")
	}
	if _, err := broker.Refresh(t.Context(), mapping.Consumer, operationRequest(meta.Authority.Operation, mapping.Handle)); safeCode(err) != "INDETERMINATE" {
		t.Fatal(err)
	}
	if count.Load() != 0 {
		t.Fatal("restart repeated unknown authentication")
	}
}
