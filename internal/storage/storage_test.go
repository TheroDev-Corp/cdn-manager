package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

func TestS3ListingEncodings(t *testing.T) {
	const key = "directory +%2B/file +%2B.txt"
	for _, test := range []struct{ mode, encoded string }{
		{"", "directory%20+%252B/file%20+%252B.txt"},
		{"path", "directory%20%2B%252B/file%20%2B%252B.txt"},
		{"form", "directory%20%2B%252B/file+%2B%252B.txt"},
	} {
		source := &Source{config: SourceConfig{ListingEncoding: test.mode}}
		decoded, err := source.decodeListed(test.encoded, "url")
		if err != nil || decoded != key {
			t.Errorf("mode %q decoded %q, %v, want %q", test.mode, decoded, err, key)
		}
	}
}

func TestCDNLink(t *testing.T) {
	const pattern = "https://cdn.example.test/media/{key}"
	link, err := makeCDNLink(pattern, "files", "nested/тест #%.txt")
	if err != nil || link != "https://cdn.example.test/media/nested/%D1%82%D0%B5%D1%81%D1%82%20%23%25.txt" {
		t.Fatalf("CDN link = %q, %v", link, err)
	}
	for _, key := range []string{"../x", "sub/./x", "sub/../x"} {
		if _, err := makeCDNLink(pattern, "files", key); !errors.Is(err, ErrInvalid) {
			t.Fatalf("normalized key %q: %v", key, err)
		}
	}
}

func TestCDNTemplatesAreDistinctPerBucket(t *testing.T) {
	source := SourceConfig{
		ID: "test", Region: "test", Buckets: []string{"one", "two"}, AccessKey: "fixture", SecretKey: "fixture",
		CDNURL: "https://cdn.example.test/{key}",
	}
	if _, err := New([]SourceConfig{source}, t.TempDir()); !errors.Is(err, ErrInvalid) {
		t.Fatalf("overlapping buckets returned %v", err)
	}
	source.CDNURL = "https://cdn.example.test/{bucket}/{key}"
	other := source
	other.ID, other.Buckets, other.CDNURL = "other", []string{"documents"}, "https://cdn.example.test/one/{key}"
	if _, err := New([]SourceConfig{source, other}, t.TempDir()); !errors.Is(err, ErrInvalid) {
		t.Fatalf("overlapping sources returned %v", err)
	}
	if _, err := New([]SourceConfig{source}, t.TempDir()); err != nil {
		t.Fatalf("distinct templates returned %v", err)
	}
}

func TestS3Integration(t *testing.T) {
	endpoint := os.Getenv("CDN_MANAGER_TEST_S3_ENDPOINT")
	if endpoint == "" {
		t.Skip("CDN_MANAGER_TEST_S3_ENDPOINT is unset")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	config := SourceConfig{ID: "media", Endpoint: endpoint, Region: os.Getenv("CDN_MANAGER_TEST_S3_REGION"),
		AccessKey: os.Getenv("CDN_MANAGER_TEST_S3_ACCESS_KEY"), SecretKey: os.Getenv("CDN_MANAGER_TEST_S3_SECRET_KEY"),
		Buckets: []string{os.Getenv("CDN_MANAGER_TEST_S3_BUCKET")}, PathStyle: true,
		ListingEncoding: os.Getenv("CDN_MANAGER_TEST_S3_LISTING_ENCODING"),
		CDNURL:          "https://cdn.example.test/media/{key}", Capabilities: Capabilities{Multipart: true, Presign: true}}
	other := config
	other.ID, other.CDNURL = "other", "https://cdn.example.test/other/{key}"
	stateDir := t.TempDir()
	newStore := func() (*Store, *Source) {
		t.Helper()
		store, err := New([]SourceConfig{config, other}, stateDir)
		if err != nil {
			t.Fatal(err)
		}
		source, err := store.Source("media")
		if err != nil {
			t.Fatal(err)
		}
		return store, source
	}
	store, source := newStore()
	bucket := config.Buckets[0]
	if err := source.Health(ctx); err != nil {
		t.Fatal(err)
	}
	buckets, err := source.ListBuckets(ctx)
	if err != nil || len(buckets) != 1 || buckets[0] != bucket {
		t.Fatalf("list buckets: %v, %v", buckets, err)
	}

	root := fmt.Sprintf("test/%d/", time.Now().UnixNano())
	prefix := root + "directory +%2B/"
	key := prefix + "тест #%2B+.bin"
	t.Cleanup(func() { _ = source.Delete(context.Background(), bucket, key, "") })
	head := bytes.Repeat([]byte{42}, int(MinPartSize))
	tail := []byte("last part")
	size := int64(len(head) + len(tail))
	upload, err := store.StartUpload(ctx, "alice", "media", bucket, key, "application/octet-stream", size)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.AbortUpload(context.Background(), "alice", "media", upload.ID) })
	if _, err := store.UploadPart(ctx, "alice", "media", upload.ID, 1, bytes.NewReader(head), int64(len(head))); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CompleteUpload(ctx, "alice", "media", upload.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("incomplete upload: %v", err)
	}

	// Reconstruct application state with only the mounted volume and the S3 provider.
	store, source = newStore()
	if _, err := store.UploadStatus(ctx, "bob", "media", upload.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("other subject: %v", err)
	}
	if _, err := store.UploadStatus(ctx, "alice", "other", upload.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("other source: %v", err)
	}
	if _, err := source.Stat(ctx, "unauthorized", key, ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("other bucket: %v", err)
	}
	status, err := store.UploadStatus(ctx, "alice", "media", upload.ID)
	if err != nil || len(status.Parts) != 1 || status.Parts[0].Number != 1 {
		t.Fatalf("resumed parts: %+v, %v", status.Parts, err)
	}
	if _, err := store.UploadPart(ctx, "alice", "media", upload.ID, 2, bytes.NewReader(tail), int64(len(tail))); err != nil {
		t.Fatal(err)
	}

	// Simulate the provider committing before the process can persist the success.
	record, err := store.readUpload(upload.ID)
	if err != nil {
		t.Fatal(err)
	}
	parts, err := source.listUploadParts(ctx, record)
	if err != nil {
		t.Fatal(err)
	}
	var committed []types.CompletedPart
	for _, part := range parts {
		committed = append(committed, types.CompletedPart{ETag: aws.String(part.ETag), PartNumber: aws.Int32(part.Number)})
	}
	lost, err := source.client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket: &bucket, Key: &key, UploadId: &record.ProviderID, MultipartUpload: &types.CompletedMultipartUpload{Parts: committed},
	})
	if err != nil {
		t.Fatal(err)
	}

	store, source = newStore()
	result, err := store.CompleteUpload(ctx, "alice", "media", upload.ID)
	if err != nil || result.ETag != aws.ToString(lost.ETag) {
		t.Fatalf("lost completion recovery: %+v, %v", result, err)
	}
	repeated, err := store.CompleteUpload(ctx, "alice", "media", upload.ID)
	if err != nil || repeated.ETag != result.ETag || repeated.Size != size {
		t.Fatalf("repeat completion: %+v, %v", repeated, err)
	}

	got, err := source.Get(ctx, bucket, key, "", "")
	if err != nil {
		t.Fatal(err)
	}
	actual := sha256.New()
	_, err = io.Copy(actual, got.Body)
	got.Body.Close()
	want := sha256.New()
	want.Write(head)
	want.Write(tail)
	if err != nil || !bytes.Equal(actual.Sum(nil), want.Sum(nil)) {
		t.Fatalf("checksum mismatch: %v", err)
	}
	list, err := source.List(ctx, bucket, key, "", "", 1)
	if err != nil || len(list.Objects) != 1 || list.Objects[0].Key != key {
		t.Fatalf("key round trip: %+v, %v", list, err)
	}
	list, err = source.List(ctx, bucket, root, "/", "", 10)
	if err != nil || !slices.Contains(list.Prefixes, prefix) {
		t.Fatalf("prefix round trip: %+v, %v", list, err)
	}

	link, err := source.CDNLink(bucket, key)
	if err != nil || !strings.HasSuffix(link, "/%D1%82%D0%B5%D1%81%D1%82%20%23%252B+.bin") {
		t.Fatalf("public link: %q, %v", link, err)
	}
	if _, err := source.WriteText(ctx, bucket, key, "text/plain", result.ETag, "cannot write"); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("conditional writes must be gated: %v", err)
	}

	t.Run("SweepExpired", func(t *testing.T) {
		upload, err := store.StartUpload(ctx, "alice", "media", bucket, key+".expired", "", 10)
		if err != nil {
			t.Fatal(err)
		}
		record, err := store.readUpload(upload.ID)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _, _ = source.client.AbortMultipartUpload(context.Background(), record.abortRequest()) })
		record.ExpiresAt = time.Now().Add(-time.Hour)
		if err := store.saveUpload(record); err != nil {
			t.Fatal(err)
		}
		if err := store.Sweep(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := store.readUpload(upload.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("expired session retained: %v", err)
		}
		if _, err := source.listUploadParts(ctx, record); !errorCode(err, "NoSuchUpload") {
			t.Fatalf("expired provider upload retained: %v", err)
		}
	})

	conditional := os.Getenv("CDN_MANAGER_TEST_S3_CONDITIONAL_WRITES")
	if conditional != "" {
		t.Run("ConditionalWrites", func(t *testing.T) {
			key := key + ".txt"
			t.Cleanup(func() { _ = source.Delete(context.Background(), bucket, key, "") })
			old, err := source.Put(ctx, bucket, key, "text/plain", strings.NewReader("old"), 3)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := source.Put(ctx, bucket, key, "text/plain", strings.NewReader("new"), 3); err != nil {
				t.Fatal(err)
			}
			// Submit the same atomic condition WriteText relies on, bypassing its
			// earlier read so provider implementations that ignore this header show up.
			_, err = source.client.PutObject(ctx, &s3.PutObjectInput{Bucket: &bucket, Key: &key, IfMatch: &old.ETag,
				Body: strings.NewReader("stale"), ContentLength: aws.Int64(5)})
			switch conditional {
			case "required":
				if !errors.Is(s3Error(err), ErrConflict) {
					t.Fatalf("provider did not reject stale If-Match: %v", err)
				}
			case "ignored":
				if err != nil {
					t.Fatalf("provider no longer silently ignores stale If-Match: %v", err)
				}
			default:
				t.Fatalf("unknown conditional writes expectation %q", conditional)
			}
		})
	}
}
