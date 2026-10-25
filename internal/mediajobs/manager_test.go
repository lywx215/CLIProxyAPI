package mediajobs

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func testManager(t *testing.T) *Manager {
	t.Helper()
	m := New(time.Hour)
	t.Cleanup(func() { m.Close(context.Background()) })
	return m
}

func awaitTask(t *testing.T, m *Manager, task Task) Task {
	t.Helper()
	select {
	case <-task.done:
	case <-t.Context().Done():
		t.Fatal("task did not finish")
	}
	got, ok := m.Get("alice", task.ID)
	if !ok {
		t.Fatal("completed task missing")
	}
	return got
}

func TestQueueConcurrencyAndShutdown(t *testing.T) {
	m := testManager(t)
	started := make(chan struct{}, 34)
	var calls atomic.Int32
	generate := func(ctx context.Context) (Result, error) {
		calls.Add(1)
		started <- struct{}{}
		<-ctx.Done()
		return Result{}, ctx.Err()
	}
	for i := 0; i < 2; i++ {
		if _, err := m.Create("alice", "model", generate); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 2; i++ {
		<-started
	}
	for i := 0; i < 32; i++ {
		if _, err := m.Create("alice", "model", generate); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := m.Create("alice", "model", generate); !errors.Is(err, ErrFull) {
		t.Fatal("queue limit not enforced")
	}
	m.Close(context.Background())
	if calls.Load() != 2 {
		t.Fatalf("started %d tasks", calls.Load())
	}
	if _, err := m.Create("alice", "model", generate); !errors.Is(err, ErrClosed) {
		t.Fatal("accepted work after shutdown")
	}
}

func TestIdentityAndCompletionBasedExpiry(t *testing.T) {
	m := testManager(t)
	var clock atomic.Int64
	clock.Store(time.Now().UnixNano())
	m.now = func() time.Time { return time.Unix(0, clock.Load()) }
	gate := make(chan struct{})
	task, err := m.Create("alice", "model", func(context.Context) (Result, error) {
		<-gate
		return Result{MIME: "video/mp4", Data: []byte("video")}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	clock.Add(int64(5 * time.Hour))
	if _, ok := m.Get("alice", task.ID); !ok {
		t.Fatal("expired a running task")
	}
	if _, ok := m.Get("bob", task.ID); ok {
		t.Fatal("cross identity disclosure")
	}
	close(gate)
	got := awaitTask(t, m, task)
	if got.Status != "completed" || got.CompletedAt.Before(got.CreatedAt.Add(5*time.Hour)) {
		t.Fatal("incorrect completion timestamp")
	}
	clock.Add(int64(time.Hour - time.Nanosecond))
	if _, ok := m.Get("alice", task.ID); !ok {
		t.Fatal("expired too early")
	}
	clock.Add(1)
	if _, ok := m.Get("alice", task.ID); ok {
		t.Fatal("expired result still accessible")
	}
	m.Close(context.Background())
	if _, ok := m.Get("alice", task.ID); ok {
		t.Fatal("shutdown retained jobs")
	}
}

func TestFailureAndTerminalCapacity(t *testing.T) {
	m := testManager(t)
	m.maxFile = 5
	m.maxBytes = 8
	m.maxItems = 2
	var clock atomic.Int64
	clock.Store(time.Now().UnixNano())
	m.now = func() time.Time { return time.Unix(0, clock.Load()) }
	var ids []string
	for i := 0; i < 4; i++ {
		clock.Add(int64(time.Second))
		index := i
		task, err := m.Create("alice", "model", func(context.Context) (Result, error) {
			if index == 0 {
				return Result{}, errors.New("private upstream error")
			}
			if index == 1 {
				return Result{MIME: "video/mp4", Data: []byte("too large")}, nil
			}
			return Result{MIME: "video/mp4", Data: []byte("video")}, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		got := awaitTask(t, m, task)
		if i < 2 && got.Status != "failed" || i >= 2 && got.Status != "completed" {
			t.Fatal("incorrect terminal state")
		}
		ids = append(ids, task.ID)
	}
	for _, id := range ids[:3] {
		if _, ok := m.Get("alice", id); ok {
			t.Fatal("oldest terminal entry was not evicted")
		}
	}
	if _, ok := m.Get("alice", ids[3]); !ok {
		t.Fatal("newest result missing")
	}
}
