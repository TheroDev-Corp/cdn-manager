package storage

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

const (
	MinPartSize        = int64(8 << 20)
	MaxPartSize        = int64(64 << 20)
	MaxPartCount       = int64(10000)
	UploadLifetime     = 24 * time.Hour
	completedRetention = 7 * 24 * time.Hour
	uploadMarker       = "cdn-manager-upload-id"
)

type UploadPart struct {
	Number int32  `json:"number"`
	ETag   string `json:"etag"`
	Size   int64  `json:"size"`
}

type Upload struct {
	ID          string       `json:"id"`
	Source      string       `json:"source"`
	Bucket      string       `json:"bucket"`
	Key         string       `json:"key"`
	ContentType string       `json:"contentType,omitempty"`
	Size        int64        `json:"size"`
	PartSize    int64        `json:"partSize"`
	PartCount   int32        `json:"partCount"`
	ExpiresAt   time.Time    `json:"expiresAt"`
	Parts       []UploadPart `json:"parts"`
	Result      *Object      `json:"result,omitempty"`
}

type uploadRecord struct {
	Upload
	Subject     string
	ProviderID  string
	CompletedAt time.Time
}

type uploadActivity struct {
	exclusive bool
	parts     map[int32]bool
	readers   int
}

func (s *Store) StartUpload(ctx context.Context, subject, sourceID, bucket, key, contentType string, size int64) (Upload, error) {
	source, err := s.Source(sourceID)
	if err != nil {
		return Upload{}, err
	}
	if err := source.checkObject(bucket, key, ""); err != nil {
		return Upload{}, err
	}
	if !source.config.Capabilities.Multipart {
		return Upload{}, ErrUnsupported
	}
	if subject == "" || size <= 0 {
		return Upload{}, ErrInvalid
	}
	if size > MaxPartSize*MaxPartCount {
		return Upload{}, ErrTooLarge
	}

	partSize := max(MinPartSize, (size+MaxPartCount-1)/MaxPartCount)
	id := rand.Text()
	record := uploadRecord{
		Upload: Upload{ID: id, Source: sourceID, Bucket: bucket, Key: key, Size: size, ContentType: contentType,
			PartSize: partSize, PartCount: int32((size + partSize - 1) / partSize), ExpiresAt: time.Now().UTC().Add(UploadLifetime), Parts: []UploadPart{}},
		Subject: subject,
	}
	out, err := source.client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket: &bucket, Key: &key, ContentType: optional(contentType), Metadata: map[string]string{uploadMarker: id},
	})
	if err != nil {
		return Upload{}, s3Error(err)
	}
	record.ProviderID = aws.ToString(out.UploadId)
	if record.ProviderID == "" {
		return Upload{}, ErrConflict
	}
	if err := s.saveUpload(record); err != nil {
		abortCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		_, abortErr := source.client.AbortMultipartUpload(abortCtx, record.abortRequest())
		return Upload{}, errors.Join(err, abortErr)
	}
	return record.Upload, nil
}

func (s *Store) UploadStatus(ctx context.Context, subject, sourceID, id string) (Upload, error) {
	release, err := s.acquireUpload(id, 0, false)
	if err != nil {
		return Upload{}, err
	}
	defer release()
	record, source, err := s.openUpload(subject, sourceID, id)
	if err != nil {
		return Upload{}, err
	}
	if record.Result == nil {
		record.Parts, err = source.listUploadParts(ctx, record)
		if errorCode(err, "NoSuchUpload") {
			if found, ok, lookupErr := source.completedObject(ctx, record); lookupErr != nil {
				return Upload{}, lookupErr
			} else if ok {
				record.Result = &found
				return record.Upload, nil
			}
		}
		if err != nil {
			return Upload{}, err
		}
	}
	return record.Upload, nil
}

// UploadRef identifies a subject's session before consulting S3. Callers can
// recheck its current source permissions before invoking any upload operation.
func (s *Store) UploadRef(subject, id string) (Upload, error) {
	record, err := s.readUpload(id)
	if err != nil {
		return Upload{}, err
	}
	if subject == "" || record.Subject != subject ||
		(record.Result == nil && !time.Now().Before(record.ExpiresAt)) {
		return Upload{}, ErrNotFound
	}
	return record.Upload, nil
}

func (s *Store) UploadPart(ctx context.Context, subject, sourceID, id string, number int32, body io.Reader, size int64) (UploadPart, error) {
	if number < 1 {
		return UploadPart{}, ErrInvalid
	}
	releaseSession, err := s.acquireUpload(id, number, false)
	if err != nil {
		return UploadPart{}, err
	}
	defer releaseSession()
	record, source, err := s.openUpload(subject, sourceID, id)
	if err != nil {
		return UploadPart{}, err
	}
	if record.Result != nil {
		return UploadPart{}, ErrConflict
	}
	if number > record.PartCount || size != record.expectedSize(number) {
		return UploadPart{}, ErrInvalid
	}
	releaseTransfer, err := source.acquireTransfer(ctx)
	if err != nil {
		return UploadPart{}, err
	}
	defer releaseTransfer()
	file, err := spool(s.stateDir, body, size)
	if err != nil {
		return UploadPart{}, err
	}
	defer removeSpool(file)
	out, err := source.client.UploadPart(ctx, &s3.UploadPartInput{
		Bucket: &record.Bucket, Key: &record.Key, UploadId: &record.ProviderID, PartNumber: &number, ContentLength: &size, Body: file,
	})
	if err != nil {
		return UploadPart{}, s3Error(err)
	}
	return UploadPart{Number: number, ETag: aws.ToString(out.ETag), Size: size}, nil
}

func (s *Store) CompleteUpload(ctx context.Context, subject, sourceID, id string) (Object, error) {
	release, err := s.acquireUpload(id, 0, true)
	if err != nil {
		return Object{}, err
	}
	defer release()
	record, source, err := s.openUpload(subject, sourceID, id)
	if err != nil {
		return Object{}, err
	}
	if record.Result != nil {
		return *record.Result, nil
	}

	parts, err := source.listUploadParts(ctx, record)
	if err != nil {
		if !errorCode(err, "NoSuchUpload") {
			return Object{}, err
		}
		return s.recoverCompletion(ctx, source, record, err)
	}
	if len(parts) != int(record.PartCount) {
		return Object{}, ErrConflict
	}
	completed := make([]types.CompletedPart, 0, len(parts))
	for index, part := range parts {
		if part.Number != int32(index+1) || part.Size != record.expectedSize(part.Number) || part.ETag == "" {
			return Object{}, ErrConflict
		}
		completed = append(completed, types.CompletedPart{ETag: aws.String(part.ETag), PartNumber: aws.Int32(part.Number)})
	}
	out, err := source.client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket: &record.Bucket, Key: &record.Key, UploadId: &record.ProviderID,
		MultipartUpload: &types.CompletedMultipartUpload{Parts: completed},
	})
	if err != nil {
		return s.recoverCompletion(ctx, source, record, s3Error(err))
	}
	return s.storeCompletion(record, Object{
		Key: record.Key, ETag: aws.ToString(out.ETag), VersionID: aws.ToString(out.VersionId), Size: record.Size, ContentType: record.ContentType,
	})
}

func (s *Store) recoverCompletion(ctx context.Context, source *Source, record uploadRecord, requestErr error) (Object, error) {
	object, ok, err := source.completedObject(ctx, record)
	if err != nil || !ok {
		return Object{}, errors.Join(requestErr, err)
	}
	return s.storeCompletion(record, object)
}

func (s *Store) storeCompletion(record uploadRecord, result Object) (Object, error) {
	record.Result = &result
	record.CompletedAt = time.Now().UTC()
	if err := s.saveUpload(record); err != nil {
		return Object{}, err
	}
	return result, nil
}

func (s *Source) completedObject(ctx context.Context, record uploadRecord) (Object, bool, error) {
	object, err := s.Stat(ctx, record.Bucket, record.Key, "")
	if errors.Is(err, ErrNotFound) {
		return Object{}, false, nil
	}
	if err != nil {
		return Object{}, false, err
	}
	if object.Metadata[uploadMarker] != record.ID || object.Size != record.Size {
		return Object{}, false, nil
	}
	// Keep the result stable across a successful completion and a recovered response.
	return Object{Key: object.Key, Size: object.Size, ETag: object.ETag, VersionID: object.VersionID, ContentType: record.ContentType}, true, nil
}

func (s *Store) AbortUpload(ctx context.Context, subject, sourceID, id string) error {
	release, err := s.acquireUpload(id, 0, true)
	if err != nil {
		return err
	}
	defer release()
	record, source, err := s.openUpload(subject, sourceID, id)
	if err != nil {
		return err
	}
	if record.Result != nil {
		return ErrConflict
	}
	if _, ok, err := source.completedObject(ctx, record); err != nil || ok {
		return errors.Join(ErrConflict, err)
	}
	return s.abortUpload(ctx, source, record)
}

func (s *Store) abortUpload(ctx context.Context, source *Source, record uploadRecord) error {
	_, err := source.client.AbortMultipartUpload(ctx, record.abortRequest())
	if err != nil && !errorCode(err, "NoSuchUpload") {
		return s3Error(err)
	}
	return os.Remove(s.uploadPath(record.ID))
}

func (r uploadRecord) abortRequest() *s3.AbortMultipartUploadInput {
	return &s3.AbortMultipartUploadInput{Bucket: &r.Bucket, Key: &r.Key, UploadId: &r.ProviderID}
}

func (r uploadRecord) expectedSize(number int32) int64 {
	return min(r.PartSize, r.Size-int64(number-1)*r.PartSize)
}

func (s *Source) listUploadParts(ctx context.Context, record uploadRecord) ([]UploadPart, error) {
	paginator := s3.NewListPartsPaginator(s.client, &s3.ListPartsInput{
		Bucket: &record.Bucket, Key: &record.Key, UploadId: &record.ProviderID,
	}, func(opts *s3.ListPartsPaginatorOptions) { opts.StopOnDuplicateToken = true })
	parts := make([]UploadPart, 0, min(record.PartCount, 128))
	for paginator.HasMorePages() {
		out, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, s3Error(err)
		}
		if len(out.Parts) > int(record.PartCount)-len(parts) {
			return nil, ErrConflict
		}
		for _, part := range out.Parts {
			parts = append(parts, UploadPart{Number: aws.ToInt32(part.PartNumber), ETag: aws.ToString(part.ETag), Size: aws.ToInt64(part.Size)})
		}
	}
	sort.Slice(parts, func(i, j int) bool { return parts[i].Number < parts[j].Number })
	return parts, nil
}

func (s *Store) acquireUpload(id string, part int32, exclusive bool) (func(), error) {
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
	return func() {
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
	}, nil
}

func (s *Store) openUpload(subject, sourceID, id string) (uploadRecord, *Source, error) {
	record, err := s.readUpload(id)
	if err != nil {
		return uploadRecord{}, nil, err
	}
	if subject == "" || record.Subject != subject || record.Source != sourceID || (record.Result == nil && !time.Now().Before(record.ExpiresAt)) {
		return uploadRecord{}, nil, ErrNotFound
	}
	source, err := s.sourceForUpload(record)
	return record, source, err
}

func (s *Store) sourceForUpload(record uploadRecord) (*Source, error) {
	source, err := s.Source(record.Source)
	if err != nil {
		return nil, err
	}
	if !source.config.Capabilities.Multipart {
		return nil, ErrUnsupported
	}
	if err := source.checkBucket(record.Bucket); err != nil {
		return nil, err
	}
	return source, nil
}

func isUploadID(id string) bool {
	if len(id) != 26 {
		return false
	}
	for _, ch := range id {
		if (ch < 'A' || ch > 'Z') && (ch < '2' || ch > '7') {
			return false
		}
	}
	return true
}

func (s *Store) uploadPath(id string) string { return filepath.Join(s.stateDir, id+".json") }

func (s *Store) readUpload(id string) (uploadRecord, error) {
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
	if err := json.Unmarshal(body, &record); err != nil {
		return record, err
	}
	if record.ID != id || record.ProviderID == "" {
		return record, ErrConflict
	}
	return record, nil
}

func (s *Store) saveUpload(record uploadRecord) error {
	file, err := os.CreateTemp(s.stateDir, ".record-*.tmp")
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

// Sweep removes expired records and aborts their S3 uploads. Call periodically.
func (s *Store) Sweep(ctx context.Context) error {
	entries, err := os.ReadDir(s.stateDir)
	if err != nil {
		return err
	}
	var failures []error
	for _, entry := range entries {
		if ctx.Err() != nil {
			return errors.Join(append(failures, ctx.Err())...)
		}
		id, ok := strings.CutSuffix(entry.Name(), ".json")
		if !ok || !isUploadID(id) {
			continue
		}
		err := s.sweepUpload(ctx, id)
		if err != nil && !errors.Is(err, ErrBusy) && !errors.Is(err, ErrNotFound) {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

func (s *Store) sweepUpload(ctx context.Context, id string) error {
	release, err := s.acquireUpload(id, 0, true)
	if err != nil {
		return err
	}
	defer release()
	record, err := s.readUpload(id)
	if err != nil {
		return err
	}
	now := time.Now()
	if record.Result != nil {
		if now.Sub(record.CompletedAt) > completedRetention {
			return os.Remove(s.uploadPath(id))
		}
		return nil
	}
	if now.Before(record.ExpiresAt) {
		return nil
	}
	source, err := s.sourceForUpload(record)
	if err != nil {
		return err
	}
	object, ok, err := source.completedObject(ctx, record)
	if err != nil {
		return err
	}
	if ok {
		_, err := s.storeCompletion(record, object)
		return err
	}
	return s.abortUpload(ctx, source, record)
}
