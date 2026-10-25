// Package mediajobs owns bounded, identity-scoped asynchronous video results.
package mediajobs

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

var ErrFull = errors.New("video queue is full")
var ErrClosed = errors.New("video service is closed")

const IDPrefix = "video_ge_"

type Result struct {
	MIME string
	Data []byte
}
type Generate func(context.Context) (Result, error)

type Task struct {
	ID, Model, Status      string
	CreatedAt, CompletedAt time.Time
	Result                 Result
	owner                  string
	done                   chan struct{}
}

type work struct {
	id       string
	generate Generate
}

type Manager struct {
	mu                          sync.Mutex
	tasks                       map[string]*Task
	queue                       chan work
	ctx                         context.Context
	cancel                      context.CancelFunc
	wg                          sync.WaitGroup
	closed                      bool
	now                         func() time.Time
	ttl                         time.Duration
	maxBytes, maxItems, maxFile int
}

func New(ttl time.Duration) *Manager {
	if ttl <= 0 || ttl > 3*time.Hour {
		ttl = 3 * time.Hour
	}
	ctx, cancel := context.WithCancel(context.Background())
	m := &Manager{tasks: map[string]*Task{}, queue: make(chan work, 32), ctx: ctx, cancel: cancel, now: time.Now, ttl: ttl, maxBytes: 128 << 20, maxItems: 128, maxFile: 32 << 20}
	for i := 0; i < 2; i++ {
		m.wg.Add(1)
		go m.worker()
	}
	return m
}

func IsID(id string) bool { return strings.HasPrefix(id, IDPrefix) }

func (m *Manager) Create(owner, model string, generate Generate) (Task, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return Task{}, ErrClosed
	}
	m.cleanupLocked()
	t := &Task{ID: IDPrefix + strings.ReplaceAll(uuid.NewString(), "-", ""), Model: model, Status: "queued", CreatedAt: m.now(), owner: owner, done: make(chan struct{})}
	m.tasks[t.ID] = t
	select {
	case m.queue <- work{t.ID, generate}:
		return *t, nil
	default:
		delete(m.tasks, t.ID)
		return Task{}, ErrFull
	}
}

func (m *Manager) Get(owner, id string) (Task, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cleanupLocked()
	t, ok := m.tasks[id]
	if !ok || t.owner != owner || m.closed {
		return Task{}, false
	}
	return *t, true
}

func (m *Manager) worker() {
	defer m.wg.Done()
	for {
		select {
		case <-m.ctx.Done():
			return
		case w := <-m.queue:
			m.mu.Lock()
			t, ok := m.tasks[w.id]
			if !ok || m.closed {
				m.mu.Unlock()
				continue
			}
			t.Status = "in_progress"
			m.mu.Unlock()
			result, err := w.generate(m.ctx)
			m.mu.Lock()
			t.Status = "failed"
			if err == nil && result.MIME == "video/mp4" && len(result.Data) > 0 && len(result.Data) <= m.maxFile && m.ctx.Err() == nil {
				t.Status = "completed"
				t.Result = result
			}
			t.CompletedAt = m.now()
			close(t.done)
			m.cleanupLocked()
			m.mu.Unlock()
		}
	}
}

func (m *Manager) cleanupLocked() {
	now := m.now()
	for id, t := range m.tasks {
		if !t.CompletedAt.IsZero() && !now.Before(t.CompletedAt.Add(m.ttl)) {
			delete(m.tasks, id)
		}
	}
	for {
		count, size := 0, 0
		var oldest *Task
		for _, t := range m.tasks {
			if t.CompletedAt.IsZero() {
				continue
			}
			count++
			size += len(t.Result.Data)
			if oldest == nil || t.CompletedAt.Before(oldest.CompletedAt) || t.CompletedAt.Equal(oldest.CompletedAt) && t.ID < oldest.ID {
				oldest = t
			}
		}
		if count <= m.maxItems && size <= m.maxBytes || oldest == nil {
			return
		}
		delete(m.tasks, oldest.ID)
	}
}

func (m *Manager) Close(ctx context.Context) {
	m.mu.Lock()
	m.closed = true
	m.cancel()
	m.tasks = map[string]*Task{}
	for draining := true; draining; {
		select {
		case <-m.queue:
		default:
			draining = false
		}
	}
	m.mu.Unlock()
	done := make(chan struct{})
	go func() { m.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
	}
}
