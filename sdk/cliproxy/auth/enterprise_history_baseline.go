package auth

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"time"
)

type enterpriseHistorySeedBucket struct {
	At      time.Time `json:"at"`
	Success int64     `json:"success"`
	Failed  int64     `json:"failed"`
}

type enterpriseHistorySeedStats struct {
	Success int64                         `json:"success"`
	Failed  int64                         `json:"failed"`
	Recent  []enterpriseHistorySeedBucket `json:"recent_requests,omitempty"`
}

type enterpriseHistorySeedCollection struct {
	BindingID string `json:"binding_id"`
	enterpriseHistorySeedStats
}

type enterpriseHistorySeedAccount struct {
	AccountID       string                            `json:"account_id"`
	FirstUploadedAt time.Time                         `json:"first_uploaded_at,omitzero"`
	Collections     []enterpriseHistorySeedCollection `json:"collections"`
}

type enterpriseHistorySeed struct {
	Version    int                            `json:"version"`
	CapturedAt time.Time                      `json:"captured_at"`
	Accounts   []enterpriseHistorySeedAccount `json:"accounts"`
}

func (s enterpriseHistorySeedStats) valid() bool {
	if s.Success < 0 || s.Failed < 0 || len(s.Recent) > recentRequestBucketCount {
		return false
	}
	success, failed := s.Success, s.Failed
	var last time.Time
	for _, bucket := range s.Recent {
		if bucket.At.IsZero() || !bucket.At.After(last) || bucket.At.Unix()%recentRequestBucketSeconds != 0 ||
			bucket.Success < 0 || bucket.Failed < 0 || bucket.Success > success || bucket.Failed > failed {
			return false
		}
		success -= bucket.Success
		failed -= bucket.Failed
		last = bucket.At
	}
	return true
}

// Import one offline baseline before a service accepts requests. A persisted
// baseline can be replayed after an interrupted startup without double counting.
func (s *enterpriseHistoryStore) importBaseline(path string, now time.Time) (err error) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, file.Close()) }()
	const maxBytes = 32 << 20
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if info.Size() > maxBytes {
		return errors.New("Enterprise history baseline is too large")
	}
	decoder := json.NewDecoder(io.LimitReader(file, maxBytes+1))
	decoder.DisallowUnknownFields()
	var seed enterpriseHistorySeed
	if err := decoder.Decode(&seed); err != nil {
		return err
	}
	if decoder.Decode(new(json.RawMessage)) != io.EOF || seed.Version != 1 || seed.CapturedAt.IsZero() || seed.CapturedAt.After(now) {
		return errors.New("invalid Enterprise history baseline version or capture time")
	}
	var events []enterpriseHistoryEvent
	seenAccounts := make(map[string]bool, len(seed.Accounts))
	seenCollections := make(map[enterpriseHistoryKey]bool)
	for _, account := range seed.Accounts {
		if !validEnterpriseHistoryID(account.AccountID) || seenAccounts[account.AccountID] || account.FirstUploadedAt.After(seed.CapturedAt) {
			return errors.New("invalid or repeated Enterprise history baseline account")
		}
		seenAccounts[account.AccountID] = true
		at, known := s.firstUploaded[account.AccountID]
		if !known || (!account.FirstUploadedAt.IsZero() && (at.IsZero() || account.FirstUploadedAt.Before(at))) {
			events = append(events, enterpriseHistoryEvent{Kind: historyUpload, Account: account.AccountID, At: account.FirstUploadedAt})
		}
		for _, collection := range account.Collections {
			key := enterpriseHistoryKey{account: account.AccountID, binding: collection.BindingID}
			if !validEnterpriseHistoryID(collection.BindingID) || seenCollections[key] || !collection.valid() ||
				(len(collection.Recent) > 0 && collection.Recent[len(collection.Recent)-1].At.After(seed.CapturedAt)) {
				return errors.New("invalid or repeated Enterprise history baseline collection")
			}
			seenCollections[key] = true
			if stored := s.counts[key]; stored != nil {
				if !stored.seeded {
					return errors.New("Enterprise history baseline must be imported before live traffic")
				}
				continue
			}
			events = append(events, enterpriseHistoryEvent{Kind: historyBaseline, Account: account.AccountID, Binding: collection.BindingID, Baseline: &collection.enterpriseHistorySeedStats})
		}
	}
	return s.appendEvents(events)
}
