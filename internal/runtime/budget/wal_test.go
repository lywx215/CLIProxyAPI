package budget

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"hash/crc32"
	"os"
	"path/filepath"
	"testing"
)

func TestWALProtocolMagicEventsAndCompensationObject(t *testing.T) {
	path := t.TempDir()
	e := openTest(t, newClock(), path, false)
	initializeTest(t, e)
	scope := Scope{Project: "one", Model: "gemini-3-flash-preview"}
	ticket, err := e.Reserve(WithRequest(context.Background()), scope)
	if err != nil {
		t.Fatal(err)
	}
	if err = ticket.Compensate(context.Background()); err != nil {
		t.Fatal(err)
	}
	ticket.Release()
	if err = e.Observe(context.Background(), scope, Signal{Kind: SignalRPM}); err != nil {
		t.Fatal(err)
	}
	if err = e.Observe(context.Background(), scope, Signal{Kind: SignalRPD}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(path, "ledger.wal"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data[:4], []byte("GBW1")) {
		t.Fatal("missing protocol magic")
	}
	var events []byte
	for offset := 4; offset < len(data); {
		length := int(binary.BigEndian.Uint32(data[offset : offset+4]))
		events = append(events, data[offset+4])
		if data[offset+4] == eventCompensate {
			var record walRecord
			if err = json.Unmarshal(data[offset+5:offset+4+length], &record); err != nil {
				t.Fatal(err)
			}
			var payload compensation
			if err = json.Unmarshal(record.Data, &payload); err != nil || payload.TargetAttemptID != ticket.ID() {
				t.Fatalf("compensation payload: %s", record.Data)
			}
		}
		offset += length + 8
	}
	if !bytes.Equal(events, []byte{eventInitialize, eventReserve, eventCompensate, eventCooldown, eventUpstreamExhausted}) {
		t.Fatalf("events=%v", events)
	}
}

func TestUnknownMagicAndEventMismatchEnterDisaster(t *testing.T) {
	for _, damage := range []string{"magic", "unknown-event", "mismatched-event"} {
		t.Run(damage, func(t *testing.T) {
			path := t.TempDir()
			clock := newClock()
			e := openTest(t, clock, path, false)
			initializeTest(t, e)
			if err := e.Close(); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(filepath.Join(path, "ledger.wal"))
			if err != nil {
				t.Fatal(err)
			}
			if damage == "magic" {
				copy(data[:4], []byte("GBW9"))
			} else {
				if damage == "unknown-event" {
					data[8] = 0xff
				} else {
					data[8] = eventReserve
				}
				length := int(binary.BigEndian.Uint32(data[4:8]))
				binary.BigEndian.PutUint32(data[8+length:12+length], crc32.ChecksumIEEE(data[8:8+length]))
			}
			if err = os.WriteFile(filepath.Join(path, "ledger.wal"), data, 0o600); err != nil {
				t.Fatal(err)
			}
			e = openTest(t, clock, path, false)
			if e.Snapshot().State != StateDisaster {
				t.Fatal("unsupported WAL was treated as fresh configuration")
			}
		})
	}
}
