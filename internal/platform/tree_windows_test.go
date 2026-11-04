//go:build windows

package platform

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// The test binary stands in for an app, by what it is told to do.
func init() {
	switch os.Getenv("MOORAGE_TREE_TEST") {
	case "":
		return
	case "memory":
		// Touching all of it, so that it is really asked for. The runtime stops the program if it is refused.
		blocks := make([][]byte, 0, 64)
		for range 64 {
			b := make([]byte, 8<<20)
			for i := range b {
				b[i] = 1
			}
			blocks = append(blocks, b)
		}
		fmt.Println(len(blocks))
		os.Exit(0)
	case "child":
		child := exec.Command(os.Args[0])
		child.Env = append(os.Environ(), "MOORAGE_TREE_TEST=wait")
		if err := child.Start(); err != nil {
			fmt.Println("refused:", err)
			os.Exit(7)
		}
		fmt.Println(child.Process.Pid)
		child.Wait()
		os.Exit(0)
	case "wait":
		time.Sleep(5 * time.Minute)
		os.Exit(0)
	case "hello":
		fmt.Println("hello")
		os.Exit(0)
	}
}

func helper(t *testing.T, mode string) *exec.Cmd {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(self)
	cmd.Env = append(os.Environ(), "MOORAGE_TREE_TEST="+mode)
	return cmd
}

func run(t *testing.T, cmd *exec.Cmd, limits TreeLimits) (string, error) {
	t.Helper()
	var out strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &out
	tree, err := StartTreeLimited(cmd, limits)
	if err != nil {
		t.Fatal(err)
	}
	defer tree.Close()
	err = cmd.Wait()
	return out.String(), err
}

func TestStartsInItsJobAndRuns(t *testing.T) {
	out, err := run(t, helper(t, "hello"), TreeLimits{})
	if err != nil || !strings.Contains(out, "hello") {
		t.Errorf("%q %v", out, err)
	}
}

func TestMemoryLimit(t *testing.T) {
	if out, err := run(t, helper(t, "memory"), TreeLimits{Memory: 1 << 30}); err != nil {
		t.Errorf("with room: %q %v", out, err)
	}
	out, err := run(t, helper(t, "memory"), TreeLimits{Memory: 128 << 20})
	if err == nil {
		t.Errorf("it got more than it was allowed: %q", out)
	}
}

func TestProcessLimit(t *testing.T) {
	out, err := run(t, helper(t, "child"), TreeLimits{Processes: 1})
	if err == nil || !strings.Contains(out, "refused") {
		t.Errorf("it started another: %q %v", out, err)
	}
}

func TestCPULimitIsAccepted(t *testing.T) {
	if out, err := run(t, helper(t, "hello"), TreeLimits{CPU: 0.5}); err != nil || !strings.Contains(out, "hello") {
		t.Errorf("%q %v", out, err)
	}
}

// TestWhatItStartsGoesWithIt: the whole point of the job is that nothing gets away.
func TestWhatItStartsGoesWithIt(t *testing.T) {
	cmd := helper(t, "child")
	var out strings.Builder
	cmd.Stdout = &out
	tree, err := StartTree(cmd)
	if err != nil {
		t.Fatal(err)
	}
	defer tree.Close()
	deadline := time.Now().Add(20 * time.Second)
	for strings.TrimSpace(out.String()) == "" && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	var child int
	if _, err := fmt.Sscan(strings.TrimSpace(out.String()), &child); err != nil {
		t.Fatalf("%q %v", out.String(), err)
	}
	tree.Kill()
	cmd.Wait()
	time.Sleep(200 * time.Millisecond)
	if alive(child) {
		t.Errorf("process %d got away", child)
	}
}

func alive(pid int) bool {
	out, err := exec.Command("tasklist", "/FI", fmt.Sprintf("PID eq %d", pid), "/NH").Output()
	return err == nil && strings.Contains(string(out), fmt.Sprint(pid))
}
