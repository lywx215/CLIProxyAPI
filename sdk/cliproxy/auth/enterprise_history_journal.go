package auth

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/filelock"
)

const (
	historyUpload   = "upload"
	historyResult   = "result"
	historyBaseline = "baseline"
	historyBatchMax = 128
	historyFrameMax = 4 << 20
)

type enterpriseHistoryEvent struct {
	Kind     string                      `json:"kind"`
	Account  string                      `json:"account_id"`
	Binding  string                      `json:"binding_id,omitempty"`
	At       time.Time                   `json:"at,omitzero"`
	Success  bool                        `json:"success,omitempty"`
	Status   int                         `json:"status_code,omitempty"`
	Baseline *enterpriseHistorySeedStats `json:"baseline,omitempty"`

	// Older journal readers treat an upload event with no At as a legacy account.
	InitializedAt time.Time `json:"initialized_at,omitzero"`
}

type enterpriseHistoryBatch struct {
	Sequence uint64                   `json:"sequence"`
	Events   []enterpriseHistoryEvent `json:"events"`
}

type enterpriseHistoryWrite struct {
	event enterpriseHistoryEvent
	done  chan error
}

type enterpriseHistoryStore struct {
	mu        sync.RWMutex
	lifetimes map[string]EnterpriseCredentialLifetime
	counts    map[enterpriseHistoryKey]*enterpriseHistoryCounts

	sendMu   sync.RWMutex
	closed   bool
	requests chan enterpriseHistoryWrite
	done     chan struct{}
	logError sync.Once

	file       *os.File
	unlockFile func()
	offset     int64
	sequence   uint64
	broken     error
}

func openEnterpriseHistory(path string) (*enterpriseHistoryStore, error) {
	_, unlock, err := filelock.Try(path + ".lock")
	if err != nil {
		return nil, fmt.Errorf("lock Enterprise credential history: %w", err)
	}
	file, err := os.OpenFile(path+".v1.wal", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		unlock()
		return nil, fmt.Errorf("open Enterprise credential history: %w", err)
	}
	store := &enterpriseHistoryStore{
		file: file, unlockFile: unlock, lifetimes: make(map[string]EnterpriseCredentialLifetime),
		counts:   make(map[enterpriseHistoryKey]*enterpriseHistoryCounts),
		requests: make(chan enterpriseHistoryWrite, historyBatchMax), done: make(chan struct{}),
	}
	if err = store.replay(); err == nil && store.offset == 0 {
		// Commit the file's directory entry before acknowledging the first event.
		err = syncEnterpriseHistoryDirectory(filepath.Dir(path))
	}
	if err != nil {
		return nil, fmt.Errorf("restore Enterprise credential history: %w", errors.Join(err, store.closeFile()))
	}
	return store, nil
}

func syncEnterpriseHistoryDirectory(path string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	return errors.Join(dir.Sync(), dir.Close())
}

func (s *enterpriseHistoryStore) closeFile() error {
	err := s.file.Close()
	s.unlockFile()
	return err
}

func (s *enterpriseHistoryStore) close() error {
	s.sendMu.Lock()
	s.closed = true
	close(s.requests)
	s.sendMu.Unlock()
	<-s.done
	return s.closeFile()
}

func (s *enterpriseHistoryStore) write(event enterpriseHistoryEvent) error {
	done := make(chan error, 1)
	s.sendMu.RLock()
	if s.closed {
		s.sendMu.RUnlock()
		return os.ErrClosed
	}
	s.requests <- enterpriseHistoryWrite{event: event, done: done}
	s.sendMu.RUnlock()
	return <-done
}

func (s *enterpriseHistoryStore) run() {
	defer close(s.done)
	for first := range s.requests {
		writes := []enterpriseHistoryWrite{first}
		for len(writes) < historyBatchMax {
			select {
			case next, ok := <-s.requests:
				if ok {
					writes = append(writes, next)
					continue
				}
			default:
			}
			break
		}
		events := make([]enterpriseHistoryEvent, len(writes))
		for i, write := range writes {
			events[i] = write.event
		}
		err := s.appendEvents(events)
		for _, write := range writes {
			write.done <- err
		}
	}
}

func (s *enterpriseHistoryStore) appendEvents(events []enterpriseHistoryEvent) error {
	if s.broken != nil {
		return s.broken
	}
	for len(events) > 0 {
		size := min(historyBatchMax, len(events))
		if err := s.appendBatch(events[:size]); err != nil {
			return err
		}
		events = events[size:]
	}
	return nil
}

func (s *enterpriseHistoryStore) appendBatch(events []enterpriseHistoryEvent) error {
	batch := enterpriseHistoryBatch{Sequence: s.sequence + 1, Events: events}
	payload, err := json.Marshal(batch)
	if err != nil {
		return err
	}
	if len(payload) > historyFrameMax {
		return errors.New("Enterprise history batch exceeds storage frame size")
	}
	frame := make([]byte, 0, 8+len(payload))
	frame = binary.BigEndian.AppendUint32(frame, uint32(len(payload)))
	frame = binary.BigEndian.AppendUint32(frame, crc32.ChecksumIEEE(payload))
	frame = append(frame, payload...)
	if _, err = s.file.WriteAt(frame, s.offset); err == nil {
		err = s.file.Sync()
	}
	if err != nil {
		if errRestore := s.removePartialTail(); errRestore != nil {
			s.broken = errors.Join(err, errRestore)
			return s.broken
		}
		return err
	}
	s.offset += int64(len(frame))
	s.apply(batch)
	return nil
}

func (s *enterpriseHistoryStore) removePartialTail() error {
	if err := s.file.Truncate(s.offset); err != nil {
		return err
	}
	return s.file.Sync()
}

func (s *enterpriseHistoryStore) replay() error {
	reader := bufio.NewReaderSize(s.file, 64<<10)
	for {
		var header [8]byte
		_, err := io.ReadFull(reader, header[:])
		if errors.Is(err, io.EOF) {
			return nil
		}
		if errors.Is(err, io.ErrUnexpectedEOF) {
			return s.removePartialTail()
		}
		if err != nil {
			return err
		}
		length := binary.BigEndian.Uint32(header[:4])
		if length == 0 || length > historyFrameMax {
			return errors.New("invalid Enterprise history frame length")
		}
		payload := make([]byte, length)
		if _, err = io.ReadFull(reader, payload); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return s.removePartialTail()
			}
			return err
		}
		if crc32.ChecksumIEEE(payload) != binary.BigEndian.Uint32(header[4:]) {
			return errors.New("Enterprise history frame checksum mismatch")
		}
		var batch enterpriseHistoryBatch
		if err := json.Unmarshal(payload, &batch); err != nil {
			return err
		}
		if batch.Sequence != s.sequence+1 || len(batch.Events) == 0 || len(batch.Events) > historyBatchMax {
			return errors.New("invalid Enterprise history batch sequence")
		}
		for _, event := range batch.Events {
			if !validEnterpriseHistoryID(event.Account) || !event.valid() {
				return errors.New("invalid Enterprise history event")
			}
		}
		s.apply(batch)
		s.offset += int64(8 + length)
	}
}

func (e enterpriseHistoryEvent) valid() bool {
	if !e.InitializedAt.IsZero() && (e.Kind != historyUpload || !e.At.IsZero()) {
		return false
	}
	switch e.Kind {
	case historyUpload:
		return e.Binding == "" && e.Baseline == nil
	case historyBaseline:
		return validEnterpriseHistoryID(e.Binding) && e.Baseline != nil && e.Baseline.valid()
	case historyResult:
		return validEnterpriseHistoryID(e.Binding) && !e.At.IsZero() && e.Baseline == nil && (e.Status == 0 || e.Status >= 100 && e.Status <= 599)
	default:
		return false
	}
}

func (s *enterpriseHistoryStore) apply(batch enterpriseHistoryBatch) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, event := range batch.Events {
		if event.Kind == historyUpload {
			lifetime := s.lifetimes[event.Account]
			if !event.At.IsZero() && (lifetime.FirstUploadedAt.IsZero() || event.At.Before(lifetime.FirstUploadedAt)) {
				lifetime.FirstUploadedAt = event.At.UTC()
			}
			if lifetime.InitializedAt.IsZero() && lifetime.FirstUploadedAt.IsZero() {
				lifetime.InitializedAt = event.InitializedAt.UTC()
			}
			s.lifetimes[event.Account] = lifetime
			continue
		}
		key := enterpriseHistoryKey{account: event.Account, binding: event.Binding}
		counts := s.counts[key]
		if counts == nil {
			counts = &enterpriseHistoryCounts{}
			s.counts[key] = counts
		}
		if event.Kind == historyBaseline {
			if !counts.seeded {
				counts.seeded = true
				counts.success += event.Baseline.Success
				counts.failed += event.Baseline.Failed
				for _, bucket := range event.Baseline.Recent {
					addEnterpriseHistoryBucket(&counts.recent, recentRequestBucketID(bucket.At), bucket.Success, bucket.Failed)
				}
			}
			continue
		}
		var success, failed int64
		if event.Success {
			success = 1
		} else {
			failed = 1
		}
		counts.success += success
		counts.failed += failed
		addEnterpriseHistoryBucket(&counts.recent, recentRequestBucketID(event.At), success, failed)
	}
	s.sequence = batch.Sequence
}
