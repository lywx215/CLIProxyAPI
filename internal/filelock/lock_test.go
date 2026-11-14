package filelock

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestLockProcessHelper(t *testing.T) {
	path := os.Getenv("GE_LOCK_TEST_PATH")
	if path == "" {
		return
	}
	_, release, err := Try(path)
	if os.Getenv("GE_LOCK_TEST_EXPECT_BUSY") == "true" {
		if err == nil {
			release()
			os.Exit(2)
		}
		return
	}
	if err != nil {
		os.Exit(3)
	}
	release()
}

func TestLockExcludesAnotherProcessAndReleases(t *testing.T) {
	path := filepath.Join(t.TempDir(), "principal.lock")
	file, release, err := Try(path)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if _, err = file.WriteAt([]byte("durable-intent"), 0); err != nil {
		t.Fatal(err)
	}
	child := func(busy string) {
		command := exec.Command(os.Args[0], "-test.run=^TestLockProcessHelper$")
		command.Env = append(os.Environ(), "GE_LOCK_TEST_PATH="+path, "GE_LOCK_TEST_EXPECT_BUSY="+busy)
		if result, errRun := command.CombinedOutput(); errRun != nil {
			t.Fatalf("lock child: %s %v", result, errRun)
		}
	}
	child("true")
	release()
	child("false")
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "durable-intent" {
		t.Fatal("lock owner marker was erased", err)
	}
}
