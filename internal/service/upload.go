package service

import (
	"context"
	"io"

	"github.com/TheroDev-Corp/cdn-manager/internal/storage"
)

type Upload struct {
	storage.Upload
	Status string `json:"status"`
}

func uploadResponse(upload storage.Upload) Upload {
	status := "active"
	if upload.Result != nil {
		status = "completed"
	}
	return Upload{Upload: upload, Status: status}
}

func (b *Backend) StartUpload(ctx context.Context, actor Actor, ref Ref, contentType string, size int64) (_ Upload, err error) {
	return b.StartUploadWithPolicy(ctx, actor, ref, contentType, size, false)
}

func (b *Backend) StartUploadWithPolicy(ctx context.Context, actor Actor, ref Ref, contentType string, size int64, createOnly bool) (_ Upload, err error) {
	defer func() { b.Audit(ctx, actor, "start_upload", ref, err) }()
	if _, err := b.Authorize(actor, ref.Source, Write); err != nil {
		return Upload{}, err
	}
	if ref.VersionID != "" {
		return Upload{}, storage.ErrInvalid
	}
	start := b.store.StartUpload
	if createOnly {
		start = b.store.StartNewUpload
	}
	upload, err := start(ctx, actor.Subject, ref.Source, ref.Bucket, ref.Key, contentType, size)
	return uploadResponse(upload), err
}

func (b *Backend) uploadRef(ctx context.Context, actor Actor, id string) (Ref, error) {
	upload, err := b.UploadForTransfer(ctx, actor, id)
	return Ref{Source: upload.Source, Bucket: upload.Bucket, Key: upload.Key}, err
}

func (b *Backend) UploadForTransfer(ctx context.Context, actor Actor, id string) (Upload, error) {
	upload, err := b.store.UploadRef(ctx, actor.Subject, id)
	if err != nil {
		return Upload{}, err
	}
	_, err = b.Authorize(actor, upload.Source, Write)
	return uploadResponse(upload), err
}

func (b *Backend) UploadStatus(ctx context.Context, actor Actor, id string) (Upload, error) {
	ref, err := b.uploadRef(ctx, actor, id)
	if err != nil {
		return Upload{}, err
	}
	upload, err := b.store.UploadStatus(ctx, actor.Subject, ref.Source, id)
	return uploadResponse(upload), err
}

func (b *Backend) UploadPart(ctx context.Context, actor Actor, id string, number int32, body io.Reader, size int64) (result storage.UploadPart, err error) {
	ref, err := b.uploadRef(ctx, actor, id)
	defer func() { b.Audit(ctx, actor, "upload_part", ref, err) }()
	if err != nil {
		return storage.UploadPart{}, err
	}
	finish := b.BeginTransfer(ref.Source, "upload")
	defer func() { finish(result.Size, err) }()
	return b.store.UploadPart(ctx, actor.Subject, ref.Source, id, number, body, size)
}

func (b *Backend) CompleteUpload(ctx context.Context, actor Actor, id string) (_ storage.Object, err error) {
	ref, err := b.uploadRef(ctx, actor, id)
	defer func() { b.Audit(ctx, actor, "complete_upload", ref, err) }()
	if err != nil {
		return storage.Object{}, err
	}
	return b.store.CompleteUpload(ctx, actor.Subject, ref.Source, id)
}

func (b *Backend) AbortUpload(ctx context.Context, actor Actor, id string) (err error) {
	ref, err := b.uploadRef(ctx, actor, id)
	defer func() { b.Audit(ctx, actor, "abort_upload", ref, err) }()
	if err != nil {
		return err
	}
	return b.store.AbortUpload(ctx, actor.Subject, ref.Source, id)
}
