package storage

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type uploadReader interface {
	read(context.Context, string) (uploadRecord, error)
}

type uploadJournal interface {
	uploadReader
	reserve(context.Context, uploadRecord) error
	publish(context.Context, uploadRecord) error
	discardStart(context.Context, string)
	expired(context.Context, time.Time) ([]string, error)
	lock(context.Context, string, int32, bool, []byte) (uploadLock, error)
}

type uploadLock interface {
	uploadReader
	phase() string
	requestContext() context.Context
	prepare(context.Context, string, []byte) error
	save(context.Context, uploadRecord) error
	remove(context.Context, string) error
	release()
}

type fileUploadJournal struct {
	dir    string
	mu     sync.Mutex
	active map[string]*uploadActivity
}

type uploadActivity struct {
	exclusive bool
	parts     map[int32]bool
	readers   int
}

type fileUploadLock struct {
	*fileUploadJournal
	unlock func()
	ctx    context.Context
}

func (l fileUploadLock) release()                                      { l.unlock() }
func (l fileUploadLock) requestContext() context.Context               { return l.ctx }
func (l fileUploadLock) phase() string                                 { return "" }
func (l fileUploadLock) prepare(context.Context, string, []byte) error { return nil }

func (s *fileUploadJournal) reserve(context.Context, uploadRecord) error { return nil }
func (s *fileUploadJournal) discardStart(context.Context, string)        {}

func (s *fileUploadJournal) publish(ctx context.Context, record uploadRecord) error {
	return s.save(ctx, record)
}

func (s *fileUploadJournal) remove(_ context.Context, id string) error {
	return os.Remove(s.uploadPath(id))
}

func (s *fileUploadJournal) expired(ctx context.Context, _ time.Time) ([]string, error) {
	// File records are filtered for expiry under an exclusive lock.
	return s.ids(ctx)
}

func (s *fileUploadJournal) ids(ctx context.Context) ([]string, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		id, ok := strings.CutSuffix(entry.Name(), ".json")
		if ok && isUploadID(id) {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

func decodeUpload(id string, body []byte) (uploadRecord, error) {
	var record uploadRecord
	if err := json.Unmarshal(body, &record); err != nil {
		return record, err
	}
	if record.ID != id || record.ProviderID == "" {
		return record, ErrConflict
	}
	return record, nil
}
func (s *fileUploadJournal) lock(ctx context.Context, id string, part int32, exclusive bool, _ []byte) (uploadLock, error) {
	if !isUploadID(id) {
		return nil, ErrNotFound
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	activity := s.active[id]
	if activity == nil {
		activity = &uploadActivity{parts: make(map[int32]bool)}
		s.active[id] = activity
	}
	if activity.exclusive || (exclusive && (activity.readers > 0 || len(activity.parts) > 0)) || activity.parts[part] {
		return nil, ErrBusy
	}
	switch {
	case exclusive:
		activity.exclusive = true
	case part > 0:
		activity.parts[part] = true
	default:
		activity.readers++
	}
	return fileUploadLock{fileUploadJournal: s, ctx: ctx, unlock: func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		switch {
		case exclusive:
			activity.exclusive = false
		case part > 0:
			delete(activity.parts, part)
		default:
			activity.readers--
		}
		if !activity.exclusive && activity.readers == 0 && len(activity.parts) == 0 {
			delete(s.active, id)
		}
	}}, nil
}

func (s *fileUploadJournal) uploadPath(id string) string { return filepath.Join(s.dir, id+".json") }

func (s *fileUploadJournal) read(_ context.Context, id string) (uploadRecord, error) {
	if !isUploadID(id) {
		return uploadRecord{}, ErrNotFound
	}
	var record uploadRecord
	body, err := os.ReadFile(s.uploadPath(id))
	if errors.Is(err, os.ErrNotExist) {
		return record, ErrNotFound
	}
	if err != nil {
		return record, err
	}
	return decodeUpload(id, body)
}

func (s *fileUploadJournal) save(_ context.Context, record uploadRecord) error {
	file, err := os.CreateTemp(s.dir, ".record-*.tmp")
	if err != nil {
		return err
	}
	defer removeSpool(file)
	if err := json.NewEncoder(file).Encode(record); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(file.Name(), s.uploadPath(record.ID))
}
