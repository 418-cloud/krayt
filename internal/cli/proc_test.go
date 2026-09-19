package cli

import (
	"os"
	"os/exec"
	"testing"
)

func TestSupervisorAlive(t *testing.T) {
	if !supervisorAlive(os.Getpid()) {
		t.Error("supervisorAlive(own pid) = false, want true")
	}
	for _, pid := range []int{0, -1} {
		if supervisorAlive(pid) {
			t.Errorf("supervisorAlive(%d) = true, want false", pid)
		}
	}

	// A child that has exited and been reaped no longer exists.
	child := exec.Command(os.Args[0], "-test.run=^$")
	if err := child.Run(); err != nil {
		t.Fatalf("run child: %v", err)
	}
	if supervisorAlive(child.Process.Pid) {
		t.Errorf("supervisorAlive(%d) = true for an exited, reaped child", child.Process.Pid)
	}
}
