package storage

import (
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

var ErrJournalUnavailable = errors.New("upload journal unavailable")

const uploadLease = 15 * time.Second

//go:embed upload_redis.lua
var uploadRedisScript string

var uploadRedis = redis.NewScript(uploadRedisScript)

type redisUploadJournal struct {
	client redis.UniversalClient
	prefix string
}

type redisUploadLock struct {
	journal         *redisUploadJournal
	id, kind, token string
	part            int32
	state           string
	ctx             context.Context
	cancel          context.CancelFunc
	stopped         <-chan struct{}
}

func NewRedis(configs []SourceConfig, spoolDir string, client redis.UniversalClient, prefix string) (*Store, error) {
	return newStore(configs, spoolDir, newRedisUploadJournal(client, prefix), true)
}

func newRedisUploadJournal(client redis.UniversalClient, prefix string) *redisUploadJournal {
	return &redisUploadJournal{client: client, prefix: prefix + ":uploads:"}
}

func (j *redisUploadJournal) keys(id string) []string {
	return []string{j.prefix + "records:" + id, j.prefix + "expiry", j.prefix + "parts:" + id}
}

func (j *redisUploadJournal) run(ctx context.Context, action, id string, args ...any) (string, error) {
	params := append([]any{action, id}, args...)
	out, err := uploadRedis.Run(ctx, j.client, j.keys(id), params...).Text()
	if err != nil {
		return "", ErrJournalUnavailable
	}
	switch out {
	case "busy":
		return "", ErrBusy
	case "conflict":
		return "", ErrConflict
	case "missing":
		return "", ErrNotFound
	case "ok":
		return "", nil
	default:
		if state, ok := strings.CutPrefix(out, "ok:"); ok {
			return state, nil
		}
		return "", ErrJournalUnavailable
	}
}

func (j *redisUploadJournal) read(ctx context.Context, id string) (uploadRecord, error) {
	if !isUploadID(id) {
		return uploadRecord{}, ErrNotFound
	}
	body, err := j.client.HGet(ctx, j.keys(id)[0], "record").Bytes()
	switch {
	case errors.Is(err, redis.Nil):
		return uploadRecord{}, ErrNotFound
	case err != nil:
		return uploadRecord{}, ErrJournalUnavailable
	default:
		return decodeUpload(id, body)
	}
}

func (j *redisUploadJournal) reserve(ctx context.Context, record uploadRecord) error {
	return j.create(ctx, record, "starting")
}

func (j *redisUploadJournal) publish(ctx context.Context, record uploadRecord) error {
	body, err := json.Marshal(record)
	if err != nil {
		return err
	}
	_, err = j.run(ctx, "publish", record.ID, body)
	return err
}

func (j *redisUploadJournal) discardStart(ctx context.Context, id string) {
	_, _ = j.run(ctx, "discard-start", id)
}

func (j *redisUploadJournal) create(ctx context.Context, record uploadRecord, phase string) error {
	body, err := json.Marshal(record)
	if err != nil {
		return err
	}
	_, err = j.run(ctx, "create", record.ID, body, phase, record.purgeAt().UnixMilli())
	return err
}

func (r uploadRecord) purgeAt() time.Time {
	if r.Result == nil {
		return r.ExpiresAt
	}
	return r.CompletedAt.Add(completedRetention)
}

func (j *redisUploadJournal) expired(ctx context.Context, now time.Time) ([]string, error) {
	ids, err := j.client.ZRangeByScore(ctx, j.prefix+"expiry", &redis.ZRangeBy{
		Min: "-inf", Max: strconv.FormatInt(now.UnixMilli(), 10),
	}).Result()
	if err != nil {
		return nil, ErrJournalUnavailable
	}
	return ids, nil
}

func (j *redisUploadJournal) lock(ctx context.Context, id string, part int32, exclusive bool, digest []byte) (uploadLock, error) {
	if !isUploadID(id) {
		return nil, ErrNotFound
	}
	kind := "read"
	if exclusive {
		kind = "exclusive"
	} else if part > 0 {
		kind = "part"
	}
	token := rand.Text()
	state, err := j.run(ctx, "lock", id, kind, token, part, digest, uploadLease.Milliseconds())
	if err != nil {
		return nil, err
	}
	lock := &redisUploadLock{journal: j, id: id, kind: kind, token: token, part: part, state: state, ctx: ctx}
	if kind != "read" {
		lock.ctx, lock.cancel = context.WithCancel(ctx)
		stopped := make(chan struct{})
		lock.stopped = stopped
		go lock.keepAlive(stopped)
	}
	return lock, nil
}

func (l *redisUploadLock) keepAlive(stopped chan<- struct{}) {
	defer close(stopped)
	ticker := time.NewTicker(uploadLease / 3)
	defer ticker.Stop()
	for {
		select {
		case <-l.ctx.Done():
			return
		case <-ticker.C:
			_, err := l.journal.run(l.ctx, "renew", l.id, l.kind, l.token, l.part, uploadLease.Milliseconds())
			if err != nil {
				l.cancel()
				return
			}
		}
	}
}

func (l *redisUploadLock) requestContext() context.Context { return l.ctx }

func (l *redisUploadLock) read(ctx context.Context, id string) (uploadRecord, error) {
	return l.journal.read(ctx, id)
}

func (l *redisUploadLock) phase() string { return l.state }

func (l *redisUploadLock) prepare(ctx context.Context, state string, manifest []byte) error {
	_, err := l.journal.run(ctx, "prepare", l.id, l.token, state, manifest)
	if err == nil {
		l.state = state
	}
	return err
}

func (l *redisUploadLock) save(ctx context.Context, record uploadRecord) error {
	body, err := json.Marshal(record)
	if err != nil {
		return err
	}
	_, err = l.journal.run(ctx, "save", l.id, l.token, body, record.purgeAt().UnixMilli())
	return err
}

func (l *redisUploadLock) remove(ctx context.Context, id string) error {
	_, err := l.journal.run(ctx, "remove", id, l.token)
	return err
}

func (l *redisUploadLock) release() {
	if l.cancel == nil {
		return
	}
	l.cancel()
	<-l.stopped
	ctx, stop := context.WithTimeout(context.Background(), 2*time.Second)
	defer stop()
	_, _ = l.journal.run(ctx, "release", l.id, l.kind, l.token, l.part, 0)
}

// ImportFileUploads copies a stopped 0.1.0 journal. The caller must hold
// the legacy manager lock and keep both deployments stopped until it finishes.
func ImportFileUploads(ctx context.Context, dir string, client redis.UniversalClient, prefix string) (int, error) {
	journal := newRedisUploadJournal(client, prefix)
	marker := journal.prefix + "imported-file-v1"
	done, err := client.Exists(ctx, marker).Result()
	if err != nil {
		return 0, ErrJournalUnavailable
	}
	if done != 0 {
		return 0, nil
	}
	files := &fileUploadJournal{dir: dir}
	ids, err := files.ids(ctx)
	if err != nil {
		return 0, err
	}
	var imported int
	for _, id := range ids {
		record, err := files.read(ctx, id)
		if err != nil {
			return imported, fmt.Errorf("read legacy upload %s: %w", id, err)
		}
		phase := "active"
		if record.Result != nil {
			phase = "completed"
		}
		if err := journal.create(ctx, record, phase); errors.Is(err, ErrConflict) {
			// The retry of a partial import recognizes only identical records.
			existing, err := journal.read(ctx, id)
			if err != nil {
				return imported, err
			}
			if !reflect.DeepEqual(record, existing) {
				return imported, fmt.Errorf("legacy upload %s differs from the record in Redis: %w", id, ErrConflict)
			}
			continue
		} else if err != nil {
			return imported, err
		}
		imported++
	}
	if err := client.Set(ctx, marker, "complete", 0).Err(); err != nil {
		return imported, ErrJournalUnavailable
	}
	return imported, nil
}
