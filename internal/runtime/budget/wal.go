package budget

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
)

const maxRecordSize = 16 << 20

var walMagic = []byte("GBW1")

const (
	eventReserve           byte = 0x01
	eventCompensate        byte = 0x02
	eventUpstreamExhausted byte = 0x03
	eventCooldown          byte = 0x04
	eventInitialize        byte = 0x05
	eventCheckpoint        byte = 0x06
	eventRecover           byte = 0x07
	eventResume            byte = 0x08
	eventCredential        byte = 0x09
	eventBackup            byte = 0x0a
	eventRestore           byte = 0x0b
	eventCircuit           byte = 0x0c
)

type walRecord struct {
	Version   int             `json:"version"`
	Sequence  uint64          `json:"sequence"`
	Revision  uint64          `json:"revision"`
	Operation string          `json:"operation"`
	Data      json.RawMessage `json:"data"`
}

func encodeRecord(record walRecord) ([]byte, error) {
	event, err := eventForRecord(record)
	if err != nil {
		return nil, err
	}
	payload, err := json.Marshal(record)
	if err != nil {
		return nil, err
	}
	if len(payload)+1 > maxRecordSize {
		return nil, errors.New("budget journal record too large")
	}
	frame := make([]byte, len(payload)+9)
	binary.BigEndian.PutUint32(frame, uint32(len(payload)+1))
	frame[4] = event
	copy(frame[5:], payload)
	binary.BigEndian.PutUint32(frame[len(frame)-4:], crc32.ChecksumIEEE(frame[4:len(frame)-4]))
	return frame, nil
}

func eventForRecord(record walRecord) (byte, error) {
	switch record.Operation {
	case "reserve":
		return eventReserve, nil
	case "compensate":
		return eventCompensate, nil
	case "entry":
		var entry Entry
		if err := json.Unmarshal(record.Data, &entry); err != nil {
			return 0, err
		}
		if entry.DailyExhausted {
			return eventUpstreamExhausted, nil
		}
		return eventCooldown, nil
	case "initialize":
		return eventInitialize, nil
	case "checkpoint":
		return eventCheckpoint, nil
	case "recover":
		return eventRecover, nil
	case "resume":
		return eventResume, nil
	case "credential":
		return eventCredential, nil
	case "backup":
		return eventBackup, nil
	case "restore":
		return eventRestore, nil
	case "circuit":
		return eventCircuit, nil
	default:
		return 0, errors.New("unknown budget journal event")
	}
}

// replayWAL truncates an incomplete, unsynced append only. A complete record with
// an invalid CRC or sequence is corruption and must enter disaster recovery.
func replayWAL(file *os.File, apply func(walRecord) error) (uint64, error) {
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return 0, err
	}
	var magic [4]byte
	if _, err := io.ReadFull(file, magic[:]); err != nil || !bytes.Equal(magic[:], walMagic) {
		return 0, errors.New("invalid budget journal magic")
	}
	var offset int64 = 4
	var last uint64
	for {
		var size [4]byte
		_, err := io.ReadFull(file, size[:])
		if errors.Is(err, io.EOF) {
			break
		}
		if errors.Is(err, io.ErrUnexpectedEOF) {
			if err = truncateWAL(file, offset); err != nil {
				return last, err
			}
			break
		}
		if err != nil {
			return last, err
		}
		length := binary.BigEndian.Uint32(size[:])
		if length < 2 || length > maxRecordSize {
			return last, errors.New("invalid budget journal length")
		}
		payload := make([]byte, int(length)+4)
		if _, err = io.ReadFull(file, payload); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				if err = truncateWAL(file, offset); err != nil {
					return last, err
				}
				break
			}
			return last, err
		}
		if crc32.ChecksumIEEE(payload[:length]) != binary.BigEndian.Uint32(payload[length:]) {
			return last, errors.New("budget journal checksum mismatch")
		}
		var record walRecord
		decoder := json.NewDecoder(bytes.NewReader(payload[1:length]))
		decoder.DisallowUnknownFields()
		if err = decoder.Decode(&record); err != nil {
			return last, err
		}
		expected, err := eventForRecord(record)
		if err != nil || payload[0] != expected {
			return last, errors.New("budget journal event mismatch")
		}
		if record.Version != 1 || (last != 0 && record.Sequence != last+1) || record.Sequence == 0 {
			return last, errors.New("invalid budget journal sequence")
		}
		if last == 0 && record.Operation != "initialize" && record.Operation != "checkpoint" && record.Operation != "recover" {
			return last, errors.New("budget journal has no initialization record")
		}
		if err = apply(record); err != nil {
			return last, err
		}
		last = record.Sequence
		offset += int64(length) + 8
	}
	_, err := file.Seek(0, io.SeekEnd)
	return last, err
}

func truncateWAL(file *os.File, offset int64) error {
	if err := file.Truncate(offset); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	_, err := file.Seek(offset, io.SeekStart)
	return err
}

func appendWAL(file *os.File, record walRecord) error {
	frame, err := encodeRecord(record)
	if err != nil {
		return err
	}
	offset, err := file.Seek(0, io.SeekEnd)
	if err != nil {
		return err
	}
	if offset == 0 {
		frame = append(append([]byte(nil), walMagic...), frame...)
	}
	if _, err = io.Copy(file, bytes.NewReader(frame)); err != nil {
		return errors.Join(err, truncateWAL(file, offset))
	}
	if err = file.Sync(); err != nil {
		return errors.Join(err, truncateWAL(file, offset))
	}
	return nil
}

func atomicWrite(path string, data []byte) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".budget-*.tmp")
	if err != nil {
		return err
	}
	temp := file.Name()
	defer func() { _ = os.Remove(temp) }()
	if err = file.Chmod(0o600); err == nil {
		_, err = file.Write(data)
	}
	if err == nil {
		err = file.Sync()
	}
	err = errors.Join(err, file.Close())
	if err != nil {
		return err
	}
	if err = replaceFile(temp, path); err != nil {
		return fmt.Errorf("replace budget file: %w", err)
	}
	return syncDirectory(filepath.Dir(path))
}
