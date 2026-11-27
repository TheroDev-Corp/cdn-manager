package storage

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/redis/go-redis/v9"
)

func TestRedisS3Multipart(t *testing.T) {
	url, endpoint := os.Getenv("CDN_MANAGER_TEST_REDIS_URL"), os.Getenv("CDN_MANAGER_TEST_S3_ENDPOINT")
	if url == "" || endpoint == "" {
		t.Skip("set CDN_MANAGER_TEST_REDIS_URL and CDN_MANAGER_TEST_S3_ENDPOINT")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	opts, err := redis.ParseURL(url)
	if err != nil {
		t.Fatal("invalid test Redis URL")
	}
	opts.MaxRetries = -1
	bucket := os.Getenv("CDN_MANAGER_TEST_S3_BUCKET")
	cfg := SourceConfig{
		ID: "media", Endpoint: endpoint, Region: os.Getenv("CDN_MANAGER_TEST_S3_REGION"), Buckets: []string{bucket}, PathStyle: true,
		AccessKey: os.Getenv("CDN_MANAGER_TEST_S3_ACCESS_KEY"), SecretKey: os.Getenv("CDN_MANAGER_TEST_S3_SECRET_KEY"),
		Capabilities: Capabilities{Multipart: true},
	}
	prefix := "test:" + rand.Text()
	newStore := func() (*Store, *redisUploadJournal) {
		t.Helper()
		client := redis.NewClient(opts)
		t.Cleanup(func() { _ = client.Close() })
		store, err := NewRedis([]SourceConfig{cfg}, t.TempDir(), client, prefix)
		if err != nil {
			t.Fatal(err)
		}
		return store, store.uploads.(*redisUploadJournal)
	}
	first, journal := newStore()
	second, _ := newStore()
	key := "ha-test/" + rand.Text()
	source, err := first.Source("media")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = source.Delete(context.Background(), bucket, key, "") })
	data := []byte("the immutable part")
	upload, err := first.StartUpload(ctx, "alice", "media", bucket, key, "application/octet-stream", int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 3*time.Second)
		defer stop()
		_ = first.AbortUpload(cleanup, "alice", "media", upload.ID)
		_ = journal.client.Del(cleanup, journal.keys(upload.ID)...).Err()
	})
	if _, err := second.UploadStatus(ctx, "bob", "media", upload.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("other subject: %v", err)
	}
	if _, err := first.UploadPart(ctx, "alice", "media", upload.ID, 1, bytes.NewReader(data), int64(len(data))); err != nil {
		t.Fatal(err)
	}
	status, err := second.UploadStatus(ctx, "alice", "media", upload.ID)
	if err != nil || len(status.Parts) != 1 {
		t.Fatalf("parts across replicas: %+v, %v", status.Parts, err)
	}
	different := bytes.Repeat([]byte{42}, len(data))
	if _, err := second.UploadPart(ctx, "alice", "media", upload.ID, 1, bytes.NewReader(different), int64(len(different))); !errors.Is(err, ErrConflict) {
		t.Fatalf("same part, different bytes: %v", err)
	}
	lock, err := journal.lock(ctx, upload.ID, 0, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.release()
	if err := second.AbortUpload(ctx, "alice", "media", upload.ID); !errors.Is(err, ErrBusy) {
		t.Fatalf("abort under completion lease: %v", err)
	}
	manifest, err := json.Marshal(status.Parts)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(manifest)
	if err := lock.prepare(ctx, "completing", digest[:]); err != nil {
		t.Fatal(err)
	}
	if err := lock.prepare(ctx, "active", nil); err != nil || lock.phase() != "active" {
		t.Fatalf("release rejected completion: state=%s err=%v", lock.phase(), err)
	}
	if err := lock.prepare(ctx, "completing", digest[:]); err != nil {
		t.Fatal(err)
	}
	record, err := journal.read(ctx, upload.ID)
	if err != nil {
		t.Fatal(err)
	}
	committed, err := source.client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket: &bucket, Key: &key, UploadId: &record.ProviderID,
		MultipartUpload: &types.CompletedMultipartUpload{Parts: []types.CompletedPart{
			{ETag: &status.Parts[0].ETag, PartNumber: aws.Int32(1)},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	// The provider committed but the first process never saved the result.
	if err := journal.client.HSet(ctx, journal.keys(upload.ID)[0], "exclusive_until", 1).Err(); err != nil {
		t.Fatal(err)
	}
	if err := second.AbortUpload(ctx, "alice", "media", upload.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("abort could displace durable completion: %v", err)
	}
	result, err := second.CompleteUpload(ctx, "alice", "media", upload.ID)
	if err != nil || result.ETag != aws.ToString(committed.ETag) {
		t.Fatalf("recover across replicas: %+v, %v", result, err)
	}
	status, err = first.UploadStatus(ctx, "alice", "media", upload.ID)
	if err != nil || status.Result == nil || status.Result.ETag != result.ETag {
		t.Fatalf("persisted completion: %+v, %v", status, err)
	}

	t.Run("ImportFileJournal", func(t *testing.T) {
		legacyDir := t.TempDir()
		legacy, err := New([]SourceConfig{cfg}, legacyDir)
		if err != nil {
			t.Fatal(err)
		}
		key := key + ".legacy"
		t.Cleanup(func() { _ = source.Delete(context.Background(), bucket, key, "") })
		session, err := legacy.StartUpload(ctx, "alice", "media", bucket, key, "", int64(len(data)))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = legacy.AbortUpload(context.Background(), "alice", "media", session.ID) })
		if _, err := legacy.UploadPart(ctx, "alice", "media", session.ID, 1, bytes.NewReader(data), int64(len(data))); err != nil {
			t.Fatal(err)
		}
		count, err := ImportFileUploads(ctx, legacyDir, journal.client, prefix)
		if err != nil || count != 1 {
			t.Fatalf("imported %d records: %v", count, err)
		}
		if _, err := legacy.uploads.read(ctx, session.ID); err != nil {
			t.Fatalf("legacy state changed: %v", err)
		}
		recovered, err := second.UploadStatus(ctx, "alice", "media", session.ID)
		if err != nil || len(recovered.Parts) != 1 {
			t.Fatalf("imported data could not resume: %+v, %v", recovered.Parts, err)
		}
		if err := second.AbortUpload(ctx, "alice", "media", session.ID); err != nil {
			t.Fatal(err)
		}
		count, err = ImportFileUploads(ctx, legacyDir, journal.client, prefix)
		if err != nil || count != 0 {
			t.Fatalf("repeat import resurrected a removed record: %d, %v", count, err)
		}
		if _, err := second.UploadRef(ctx, "alice", session.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("removed session was visible after reimport: %v", err)
		}
		t.Cleanup(func() { _ = journal.client.Del(context.Background(), journal.prefix+"imported-file-v1").Err() })
	})

}
