package storage

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"hash"
	"io"
	"sort"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

const (
	MinPartSize            = int64(8 << 20)
	MaxPartSize            = int64(64 << 20)
	MaxPartCount           = int64(10000)
	uploadOperationTimeout = 10 * time.Minute
	UploadLifetime         = 24 * time.Hour
	completedRetention     = 7 * 24 * time.Hour
	uploadMarker           = "cdn-manager-upload-id"
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
	CreateOnly  bool `json:",omitempty"`
}

func (s *Store) StartUpload(ctx context.Context, subject, sourceID, bucket, key, contentType string, size int64) (Upload, error) {
	return s.startUpload(ctx, subject, sourceID, bucket, key, contentType, size, false)
}

func (s *Store) StartNewUpload(ctx context.Context, subject, sourceID, bucket, key, contentType string, size int64) (Upload, error) {
	return s.startUpload(ctx, subject, sourceID, bucket, key, contentType, size, true)
}

func (s *Store) startUpload(ctx context.Context, subject, sourceID, bucket, key, contentType string, size int64, createOnly bool) (Upload, error) {
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
	if createOnly {
		if err := source.requireMissing(ctx, bucket, key); err != nil {
			return Upload{}, err
		}
	}

	partSize := max(MinPartSize, (size+MaxPartCount-1)/MaxPartCount)
	id := rand.Text()
	record := uploadRecord{
		Upload: Upload{ID: id, Source: sourceID, Bucket: bucket, Key: key, Size: size, ContentType: contentType,
			PartSize: partSize, PartCount: int32((size + partSize - 1) / partSize), ExpiresAt: time.Now().UTC().Add(UploadLifetime), Parts: []UploadPart{}},
		Subject: subject, CreateOnly: createOnly,
	}
	ctx, cancel := context.WithTimeout(ctx, uploadOperationTimeout)
	defer cancel()
	if err := s.uploads.reserve(ctx, record); err != nil {
		return Upload{}, err
	}
	published := false
	defer func() {
		if published {
			return
		}
		cleanup, stop := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
		defer stop()
		s.uploads.discardStart(cleanup, record.ID)
	}()

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
	saveCtx, cancelSave := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancelSave()
	if err := s.uploads.publish(saveCtx, record); err != nil {
		// The caller has never received the session ID, so it is safe
		// to clean up this new provider upload even if publication won.
		abortCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		_, abortErr := source.client.AbortMultipartUpload(abortCtx, record.abortRequest())
		return Upload{}, errors.Join(err, abortErr)
	}
	published = true
	return record.Upload, nil
}

func (s *Store) UploadStatus(ctx context.Context, subject, sourceID, id string) (Upload, error) {
	ctx, cancel := context.WithTimeout(ctx, uploadOperationTimeout)
	defer cancel()
	lock, err := s.uploads.lock(ctx, id, 0, false, nil)
	if err != nil {
		return Upload{}, err
	}
	defer lock.release()
	ctx = lock.requestContext()
	record, source, err := s.openUpload(ctx, lock, subject, sourceID, id)
	if err != nil {
		return Upload{}, err
	}
	if record.Result == nil {
		record.Parts, err = source.listUploadParts(ctx, record)
		if record.missingProviderUpload(err) {
			if found, ok, lookupErr := source.completedObject(ctx, record); lookupErr != nil {
				return Upload{}, lookupErr
			} else if ok {
				record.Result = &found
				return record.Upload, nil
			} else if record.CreateOnly && found.Key != "" {
				return Upload{}, ErrObjectExists
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
func (s *Store) UploadRef(ctx context.Context, subject, id string) (Upload, error) {
	record, err := s.uploads.read(ctx, id)
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
	record, source, err := s.openUpload(ctx, s.uploads, subject, sourceID, id)
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
	var checksum hash.Hash
	if s.sharedUploads && body != nil {
		checksum = sha256.New()
		body = io.TeeReader(body, checksum)
	}
	file, err := spool(s.spoolDir, body, size)
	if err != nil {
		return UploadPart{}, err
	}
	defer removeSpool(file)

	// Slow client bodies should not hold a database connection or a lock.
	ctx, cancel := context.WithTimeout(ctx, uploadOperationTimeout)
	defer cancel()
	lock, err := s.uploads.lock(ctx, id, number, false, partDigest(checksum))
	if err != nil {
		return UploadPart{}, err
	}
	defer lock.release()
	ctx = lock.requestContext()
	record, source, err = s.openUpload(ctx, lock, subject, sourceID, id)
	if err != nil {
		return UploadPart{}, err
	}
	if record.Result != nil {
		return UploadPart{}, ErrConflict
	}
	out, err := source.client.UploadPart(ctx, &s3.UploadPartInput{
		Bucket: &record.Bucket, Key: &record.Key, UploadId: &record.ProviderID, PartNumber: &number, ContentLength: &size, Body: file,
	})
	if err != nil {
		return UploadPart{}, s3Error(err)
	}
	return UploadPart{Number: number, ETag: aws.ToString(out.ETag), Size: size}, nil
}

func partDigest(checksum hash.Hash) []byte {
	if checksum == nil {
		return nil
	}
	return checksum.Sum(nil)
}

func (s *Store) CompleteUpload(ctx context.Context, subject, sourceID, id string) (Object, error) {
	ctx, cancel := context.WithTimeout(ctx, uploadOperationTimeout)
	defer cancel()
	lock, err := s.uploads.lock(ctx, id, 0, true, nil)
	if err != nil {
		return Object{}, err
	}
	defer lock.release()
	ctx = lock.requestContext()
	record, source, err := s.openUpload(ctx, lock, subject, sourceID, id)
	if err != nil {
		return Object{}, err
	}
	return s.completeUpload(ctx, lock, record, source)
}

func (s *Store) completeUpload(ctx context.Context, lock uploadLock, record uploadRecord, source *Source) (Object, error) {
	if record.Result != nil {
		return *record.Result, nil
	}

	if lock.phase() == "aborting" {
		return Object{}, ErrConflict
	}

	parts, err := source.listUploadParts(ctx, record)
	if err != nil {
		if !record.missingProviderUpload(err) {
			return Object{}, err
		}
		return s.recoverCompletion(ctx, lock, source, record, err)
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
	manifest, err := json.Marshal(parts)
	if err != nil {
		return Object{}, err
	}
	digest := sha256.Sum256(manifest)
	condition, err := source.createCondition(ctx, record.Bucket, record.Key, record.CreateOnly)
	if err != nil {
		if lock.phase() == "completing" && errors.Is(err, ErrObjectExists) {
			return s.recoverCompletion(ctx, lock, source, record, err)
		}
		return Object{}, err
	}
	if err := lock.prepare(ctx, "completing", digest[:]); err != nil {
		return Object{}, err
	}
	out, err := source.client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket: &record.Bucket, Key: &record.Key, UploadId: &record.ProviderID,
		MultipartUpload: &types.CompletedMultipartUpload{Parts: completed}, IfNoneMatch: condition,
	})
	if err != nil {
		return s.recoverCompletion(ctx, lock, source, record, createError(err, condition))
	}
	return s.storeCompletion(ctx, lock, record, Object{
		Key: record.Key, ETag: aws.ToString(out.ETag), VersionID: aws.ToString(out.VersionId), Size: record.Size, ContentType: record.ContentType,
	})
}

func (s *Store) recoverCompletion(ctx context.Context, lock uploadLock, source *Source, record uploadRecord, requestErr error) (Object, error) {
	object, ok, err := source.completedObject(ctx, record)
	if err != nil {
		if errors.Is(requestErr, ErrObjectExists) {
			return Object{}, err
		}
		return Object{}, errors.Join(requestErr, err)
	}
	if ok {
		return s.storeCompletion(ctx, lock, record, object)
	}
	if record.CreateOnly && object.Key != "" && record.missingProviderUpload(requestErr) {
		requestErr = ErrObjectExists
	}
	if errors.Is(requestErr, ErrObjectExists) {
		// The provider rejected the write and the key belongs to another upload.
		// Release the completion intent so the user can discard the old parts.
		if lock.phase() == "completing" {
			if err := lock.prepare(ctx, "active", nil); err != nil {
				return Object{}, err
			}
		}
	}
	return Object{}, requestErr
}

func (s *Store) storeCompletion(ctx context.Context, lock uploadLock, record uploadRecord, result Object) (Object, error) {
	record.Result = &result
	record.CompletedAt = time.Now().UTC()
	saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := lock.save(saveCtx, record); err != nil {
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
		return object, false, nil
	}
	// Keep the result stable across a successful completion and a recovered response.
	return Object{Key: object.Key, Size: object.Size, ETag: object.ETag, VersionID: object.VersionID, ContentType: record.ContentType}, true, nil
}

func (s *Store) AbortUpload(ctx context.Context, subject, sourceID, id string) error {
	ctx, cancel := context.WithTimeout(ctx, uploadOperationTimeout)
	defer cancel()
	lock, err := s.uploads.lock(ctx, id, 0, true, nil)
	if err != nil {
		return err
	}
	defer lock.release()
	ctx = lock.requestContext()
	record, source, err := s.openUpload(ctx, lock, subject, sourceID, id)
	if err != nil {
		return err
	}
	if record.Result != nil {
		return ErrConflict
	}
	if _, ok, err := source.completedObject(ctx, record); err != nil || ok {
		return errors.Join(ErrConflict, err)
	}
	return s.abortUpload(ctx, lock, source, record)
}

func (s *Store) abortUpload(ctx context.Context, lock uploadLock, source *Source, record uploadRecord) error {
	if err := lock.prepare(ctx, "aborting", nil); err != nil {
		return err
	}
	_, err := source.client.AbortMultipartUpload(ctx, record.abortRequest())
	if err = s3Error(err); err != nil && !record.missingProviderUpload(err) {
		return err
	}
	return lock.remove(ctx, record.ID)
}

func (r uploadRecord) missingProviderUpload(err error) bool {
	return errorCode(err, "NoSuchUpload") || (r.CreateOnly && errors.Is(err, ErrNotFound))
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

func (s *Store) openUpload(ctx context.Context, reader uploadReader, subject, sourceID, id string) (uploadRecord, *Source, error) {
	record, err := reader.read(ctx, id)
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

// Sweep removes expired records and aborts their S3 uploads. Call periodically.
func (s *Store) Sweep(ctx context.Context) error {
	ids, err := s.uploads.expired(ctx, time.Now().UTC())
	if err != nil {
		return err
	}
	var failures []error
	for _, id := range ids {
		if ctx.Err() != nil {
			return errors.Join(append(failures, ctx.Err())...)
		}
		err := s.sweepUpload(ctx, id)
		if err != nil && !errors.Is(err, ErrBusy) && !errors.Is(err, ErrNotFound) {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

func (s *Store) sweepUpload(ctx context.Context, id string) error {
	lock, err := s.uploads.lock(ctx, id, 0, true, nil)
	if err != nil {
		return err
	}
	defer lock.release()
	ctx = lock.requestContext()
	if lock.phase() == "starting" {
		return lock.remove(ctx, id)
	}
	record, err := lock.read(ctx, id)
	if err != nil {
		return err
	}
	now := time.Now()
	if record.Result != nil {
		if now.Sub(record.CompletedAt) > completedRetention {
			return lock.remove(ctx, id)
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
		_, err := s.storeCompletion(ctx, lock, record, object)
		return err
	}
	if lock.phase() == "completing" {
		// Once a complete was dispatched, retry the same manifest rather
		// than racing the provider with a conflicting abort request.
		_, err := s.completeUpload(ctx, lock, record, source)
		if err != nil && now.After(record.ExpiresAt.Add(completedRetention)) &&
			(errors.Is(err, ErrConflict) || errorCode(err, "NoSuchUpload")) {
			// Provider lifecycle cleanup also removes an orphan left by
			// a process that died during creation or a missing manifest.
			return lock.remove(ctx, id)
		}
		return err
	}
	return s.abortUpload(ctx, lock, source, record)
}
