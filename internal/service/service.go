package service

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/TheroDev-Corp/cdn-manager/internal/storage"
	"github.com/TheroDev-Corp/cdn-manager/internal/telemetry"
)

const (
	Read    = "read"
	Write   = "write"
	Delete  = "delete"
	Presign = "presign"
	Policy  = "policy"
	Bucket  = "bucket"
)

var ErrForbidden = errors.New("operation is forbidden")

type Actor struct {
	Subject string
	Name    string
	Client  string
	Groups  []string
	Scopes  []string
}

type Ref struct {
	Source    string `json:"source"`
	Bucket    string `json:"bucket"`
	Key       string `json:"key"`
	VersionID string `json:"versionId,omitempty"`
}

type Roles struct {
	Viewer []string `json:"viewer"`
	Editor []string `json:"editor"`
	Admin  []string `json:"admin"`
}

type SourceConfig struct {
	storage.SourceConfig
	Label           string `json:"label"`
	AccessKeyEnv    string `json:"accessKeyEnv"`
	SecretKeyEnv    string `json:"secretKeyEnv"`
	SessionTokenEnv string `json:"sessionTokenEnv"`
	Roles           Roles  `json:"roles"`
}

type Source struct {
	ID           string          `json:"id"`
	Label        string          `json:"label,omitempty"`
	Region       string          `json:"region"`
	Capabilities map[string]bool `json:"capabilities"`
	Health       string          `json:"health"`
}

type Backend struct {
	store   *storage.Store
	sources map[string]SourceConfig
	ids     []string
	logger  *slog.Logger
	Metrics *telemetry.Metrics
	mu      sync.RWMutex
	health  map[string]string
}

func New(store *storage.Store, configs []SourceConfig, logger *slog.Logger) *Backend {
	b := &Backend{
		store: store, sources: make(map[string]SourceConfig, len(configs)),
		health: make(map[string]string, len(configs)), logger: logger,
	}
	for _, config := range configs {
		b.ids = append(b.ids, config.ID)
		b.sources[config.ID] = config
		b.health[config.ID] = "unknown"
	}
	return b
}

func allowed(actor Actor, roles Roles, permission string) bool {
	if actor.Subject == "" || (actor.Client != "web" && !slices.Contains(actor.Scopes, permission)) {
		return false
	}
	for _, group := range actor.Groups {
		if slices.Contains(roles.Admin, group) ||
			(slices.Contains(roles.Editor, group) && (permission == Read || permission == Write)) ||
			(slices.Contains(roles.Viewer, group) && permission == Read) {
			return true
		}
	}
	return false
}

func (b *Backend) Sources(actor Actor) []Source {
	sources := make([]Source, 0, len(b.ids))
	b.mu.RLock()
	defer b.mu.RUnlock()
	for _, id := range b.ids {
		config := b.sources[id]
		if !allowed(actor, config.Roles, Read) {
			continue
		}
		canWrite := allowed(actor, config.Roles, Write)
		canManageBuckets := config.Capabilities.BucketManagement && allowed(actor, config.Roles, Bucket)
		sources = append(sources, Source{
			ID: id, Label: config.Label, Region: config.Region, Health: b.health[id],
			Capabilities: map[string]bool{
				"upload": canWrite, "multipart": canWrite && config.Capabilities.Multipart,
				"metadata": true, "versions": config.Capabilities.Versions,
				"editText":     config.Capabilities.ConditionalWrites && canWrite,
				"createBucket": canManageBuckets, "deleteBucket": canManageBuckets,
				"policy":  config.Capabilities.BucketPolicies && allowed(actor, config.Roles, Policy),
				"presign": config.Capabilities.Presign && allowed(actor, config.Roles, Presign),
				"delete":  allowed(actor, config.Roles, Delete),
			},
		})
	}
	return sources
}

func (b *Backend) Authorize(actor Actor, sourceID, permission string) (*storage.Source, error) {
	config, ok := b.sources[sourceID]
	if !ok || (!allowed(actor, config.Roles, permission) && !allowed(actor, config.Roles, Read)) {
		return nil, storage.ErrNotFound
	}
	if !allowed(actor, config.Roles, permission) {
		return nil, ErrForbidden
	}
	return b.store.Source(sourceID)
}

func (b *Backend) Audit(ctx context.Context, actor Actor, operation string, ref Ref, err error) {
	b.audit(ctx, actor, operation, ref, err, false)
}

func (b *Backend) AuditPartial(ctx context.Context, actor Actor, operation string, ref Ref) {
	b.audit(ctx, actor, operation, ref, nil, true)
}

func (b *Backend) audit(ctx context.Context, actor Actor, operation string, ref Ref, err error, partial bool) {
	outcome := "success"
	if err != nil {
		outcome = "failure"
	} else if partial {
		outcome = "partial"
	}
	b.logger.InfoContext(ctx, "storage audit", "subject", actor.Subject, "client", actor.Client,
		"source", ref.Source, "operation", operation, "bucket", ref.Bucket, "object", ref.Key, "outcome", outcome)
}

func (b *Backend) RefreshHealth(ctx context.Context) {
	var wg sync.WaitGroup
	for _, id := range b.ids {
		wg.Go(func() {
			source, err := b.store.Source(id)
			checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			if err == nil {
				err = source.Health(checkCtx)
			}
			status := "healthy"
			if err != nil {
				status = "unavailable"
			}
			b.mu.Lock()
			b.health[id] = status
			b.mu.Unlock()
			if b.Metrics != nil {
				b.Metrics.SetHealth(id, status)
			}
		})
	}
	wg.Wait()
}

func (b *Backend) BeginTransfer(source, direction string) func(int64, error) {
	if b.Metrics == nil {
		return func(int64, error) {}
	}
	return b.Metrics.BeginTransfer(source, direction)
}

func (b *Backend) Health() map[string]string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	health := make(map[string]string, len(b.health))
	for id, status := range b.health {
		health[id] = status
	}
	return health
}

func (b *Backend) List(ctx context.Context, actor Actor, ref Ref, prefix, delimiter, cursor string, limit int32) (storage.ObjectPage, error) {
	source, err := b.Authorize(actor, ref.Source, Read)
	if err != nil {
		return storage.ObjectPage{}, err
	}
	return source.List(ctx, ref.Bucket, prefix, delimiter, cursor, limit)
}

func (b *Backend) Stat(ctx context.Context, actor Actor, ref Ref) (storage.Object, error) {
	source, err := b.Authorize(actor, ref.Source, Read)
	if err != nil {
		return storage.Object{}, err
	}
	return source.Stat(ctx, ref.Bucket, ref.Key, ref.VersionID)
}

func (b *Backend) CDNURL(_ context.Context, actor Actor, ref Ref) (string, error) {
	source, err := b.Authorize(actor, ref.Source, Read)
	if err != nil {
		return "", err
	}
	if ref.VersionID != "" {
		return "", storage.ErrUnsupported
	}
	return source.CDNLink(ref.Bucket, ref.Key)
}

func (b *Backend) ReadText(ctx context.Context, actor Actor, ref Ref) (storage.Text, error) {
	source, err := b.Authorize(actor, ref.Source, Read)
	if err != nil {
		return storage.Text{}, err
	}
	if ref.VersionID != "" {
		return storage.Text{}, storage.ErrUnsupported
	}
	return source.ReadText(ctx, ref.Bucket, ref.Key)
}

func (b *Backend) WriteText(ctx context.Context, actor Actor, ref Ref, contentType, etag, text string) (_ storage.Object, err error) {
	defer func() { b.Audit(ctx, actor, "write_text", ref, err) }()
	source, err := b.Authorize(actor, ref.Source, Write)
	if err != nil {
		return storage.Object{}, err
	}
	if ref.VersionID != "" {
		return storage.Object{}, storage.ErrUnsupported
	}
	return source.WriteText(ctx, ref.Bucket, ref.Key, contentType, etag, text)
}

func (b *Backend) DeleteObject(ctx context.Context, actor Actor, ref Ref) (err error) {
	defer func() { b.Audit(ctx, actor, "delete_object", ref, err) }()
	source, err := b.Authorize(actor, ref.Source, Delete)
	if err != nil {
		return err
	}
	return source.Delete(ctx, ref.Bucket, ref.Key, ref.VersionID)
}
