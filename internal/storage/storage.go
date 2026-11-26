package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
	"github.com/aws/smithy-go/middleware"
	smithyhttp "github.com/aws/smithy-go/transport/http"
)

const MaxTextBytes = 1 << 20
const MaxSinglePutBytes = 64 << 20

var (
	ErrInvalid     = errors.New("invalid storage request")
	ErrNotFound    = errors.New("storage resource not found")
	ErrConflict    = errors.New("storage resource conflict")
	ErrUnsupported = errors.New("storage operation is disabled")
	ErrTooLarge    = errors.New("storage request is too large")
	ErrBusy        = errors.New("storage transfer capacity reached")
)

type Capabilities struct {
	Multipart         bool `json:"multipart"`
	ConditionalWrites bool `json:"conditionalWrites"`
	Versions          bool `json:"versions"`
	BucketManagement  bool `json:"bucketManagement"`
	BucketPolicies    bool `json:"bucketPolicies"`
	Presign           bool `json:"presign"`
}

type SourceConfig struct {
	ID              string                                                 `json:"id"`
	Endpoint        string                                                 `json:"endpoint"`
	Region          string                                                 `json:"region"`
	AccessKey       string                                                 `json:"-"`
	SecretKey       string                                                 `json:"-"`
	SessionToken    string                                                 `json:"-"`
	Buckets         []string                                               `json:"buckets"`
	PathStyle       bool                                                   `json:"pathStyle"`
	CDNURL          string                                                 `json:"cdnUrl"`
	CDNProbeURL     string                                                 `json:"cdnProbeUrl"`
	ListingEncoding string                                                 `json:"listingEncoding"`
	Capabilities    Capabilities                                           `json:"capabilities"`
	Observe         func(source, operation string, retries int, err error) `json:"-"`
}

type Source struct {
	id        string
	config    SourceConfig
	client    *s3.Client
	stateDir  string
	transfers chan struct{}
}

type Store struct {
	sources  map[string]*Source
	stateDir string
	mu       sync.Mutex
	active   map[string]*uploadActivity
}

func New(configs []SourceConfig, stateDir string) (*Store, error) {
	if stateDir == "" {
		return nil, fmt.Errorf("upload state directory: %w", ErrInvalid)
	}
	if err := os.MkdirAll(stateDir, 0700); err != nil {
		return nil, fmt.Errorf("create upload state directory: %w", err)
	}
	// A state directory belongs to one running manager. Files left by a process
	// killed during a transfer or a journal write are safe to remove on startup.
	entries, err := os.ReadDir(stateDir)
	if err != nil {
		return nil, fmt.Errorf("open upload state directory: %w", err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if !entry.Type().IsRegular() || !strings.HasSuffix(name, ".tmp") {
			continue
		}
		if strings.HasPrefix(name, ".transfer-") || strings.HasPrefix(name, ".record-") {
			if err := os.Remove(filepath.Join(stateDir, name)); err != nil {
				return nil, fmt.Errorf("remove abandoned upload temporary file: %w", err)
			}
		}
	}
	store := &Store{sources: make(map[string]*Source, len(configs)), stateDir: stateDir, active: make(map[string]*uploadActivity)}
	globalTransfers := make(chan struct{}, 4)
	links := make(map[string]struct{})
	for _, config := range configs {
		if config.ID == "" || config.Region == "" || len(config.Buckets) == 0 || store.sources[config.ID] != nil {
			return nil, fmt.Errorf("source ID, region, or buckets: %w", ErrInvalid)
		}
		if config.AccessKey == "" || config.SecretKey == "" {
			return nil, fmt.Errorf("S3 credentials missing for source %q: %w", config.ID, ErrInvalid)
		}
		switch config.ListingEncoding {
		case "", "path", "form":
		default:
			return nil, fmt.Errorf("invalid listing encoding for source %q: %w", config.ID, ErrInvalid)
		}
		if config.Endpoint != "" {
			endpoint, err := url.Parse(config.Endpoint)
			if err != nil || endpoint.Host == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" || (endpoint.Scheme != "https" && endpoint.Scheme != "http") {
				return nil, fmt.Errorf("invalid endpoint for source %q: %w", config.ID, ErrInvalid)
			}
		}
		config.Buckets = slices.Clone(config.Buckets)
		opts := aws.Config{
			Region:                     config.Region,
			Credentials:                credentials.NewStaticCredentialsProvider(config.AccessKey, config.SecretKey, config.SessionToken),
			RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
			ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
		}
		client := s3.NewFromConfig(opts, func(options *s3.Options) {
			options.UsePathStyle = config.PathStyle
			if config.Endpoint != "" {
				options.BaseEndpoint = &config.Endpoint
			}
			if config.Observe != nil {
				options.APIOptions = append(options.APIOptions, func(stack *middleware.Stack) error {
					return stack.Initialize.Add(middleware.InitializeMiddlewareFunc("CDNManagerMetrics",
						func(ctx context.Context, input middleware.InitializeInput, next middleware.InitializeHandler) (middleware.InitializeOutput, middleware.Metadata, error) {
							output, metadata, err := next.HandleInitialize(ctx, input)
							attempts, _ := retry.GetAttemptResults(metadata)
							config.Observe(config.ID, middleware.GetOperationName(ctx), max(0, len(attempts.Results)-1), err)
							return output, metadata, err
						}), middleware.Before)
				})
			}
		})
		if config.CDNURL != "" {
			for _, bucket := range config.Buckets {
				link, err := makeCDNLink(config.CDNURL, bucket, "cdn-manager-collision-placeholder")
				if err != nil {
					return nil, fmt.Errorf("invalid CDN URL for source %q: %w", config.ID, err)
				}
				parsed, _ := url.Parse(link)
				parsed.Host = strings.ToLower(parsed.Host)
				link = parsed.String()
				if _, found := links[link]; found {
					return nil, fmt.Errorf("CDN URL overlaps another source or bucket at source %q: %w", config.ID, ErrInvalid)
				}
				links[link] = struct{}{}
			}
		}
		if config.CDNProbeURL != "" {
			if config.CDNURL == "" {
				return nil, fmt.Errorf("CDN probe requires a public URL for source %q: %w", config.ID, ErrInvalid)
			}
			for _, bucket := range config.Buckets {
				if _, err := makeCDNLink(config.CDNProbeURL, bucket, "test"); err != nil {
					return nil, fmt.Errorf("invalid CDN probe URL for source %q: %w", config.ID, ErrInvalid)
				}
			}
		}
		store.sources[config.ID] = &Source{id: config.ID, config: config, client: client, stateDir: stateDir, transfers: globalTransfers}
	}
	return store, nil
}

func (s *Store) Source(id string) (*Source, error) {
	source := s.sources[id]
	if source == nil {
		return nil, ErrNotFound
	}
	return source, nil
}

func (s *Source) ID() string                 { return s.id }
func (s *Source) Buckets() []string          { return slices.Clone(s.config.Buckets) }
func (s *Source) Capabilities() Capabilities { return s.config.Capabilities }

func (s *Source) checkBucket(bucket string) error {
	if bucket == "" || !slices.Contains(s.config.Buckets, bucket) {
		return ErrNotFound
	}
	return nil
}

func (s *Source) checkObject(bucket, key, version string) error {
	if err := s.checkBucket(bucket); err != nil {
		return err
	}
	if key == "" || len(key) > 1024 || !utf8.ValidString(key) {
		return ErrInvalid
	}
	if version != "" && !s.config.Capabilities.Versions {
		return ErrUnsupported
	}
	return nil
}

func (s *Source) CDNLink(bucket, key string) (string, error) {
	return s.cdnLink(bucket, key, s.config.CDNURL)
}

func (s *Source) CDNProbeLink(bucket, key string) (string, error) {
	if s.config.CDNProbeURL == "" {
		return s.CDNLink(bucket, key)
	}
	return s.cdnLink(bucket, key, s.config.CDNProbeURL)
}

func (s *Source) cdnLink(bucket, key, pattern string) (string, error) {
	if err := s.checkObject(bucket, key, ""); err != nil {
		return "", err
	}
	if pattern == "" {
		return "", ErrUnsupported
	}
	return makeCDNLink(pattern, bucket, key)
}

func makeCDNLink(pattern, bucket, key string) (string, error) {
	const marker = "cdn-manager-key-placeholder"
	if strings.Count(pattern, "{key}") != 1 || strings.Contains(pattern, marker) {
		return "", ErrInvalid
	}
	parts := strings.Split(key, "/")
	for i, part := range parts {
		if part == "." || part == ".." {
			// Browsers normalize dot-only path components even when percent encoded.
			return "", ErrInvalid
		}
		parts[i] = url.PathEscape(part)
	}
	template, err := url.Parse(strings.NewReplacer("{key}", marker, "{bucket}", url.PathEscape(bucket)).Replace(pattern))
	if err != nil || template.Hostname() == "" || template.User != nil || template.RawQuery != "" || template.ForceQuery ||
		template.Fragment != "" || (template.Scheme != "https" && template.Scheme != "http") || !strings.Contains(template.EscapedPath(), marker) {
		return "", ErrInvalid
	}
	return strings.NewReplacer("{key}", strings.Join(parts, "/"), "{bucket}", url.PathEscape(bucket)).Replace(pattern), nil
}

type Object struct {
	Key          string            `json:"key"`
	Size         int64             `json:"size"`
	ETag         string            `json:"etag,omitempty"`
	VersionID    string            `json:"versionId,omitempty"`
	ContentType  string            `json:"contentType,omitempty"`
	ContentRange string            `json:"contentRange,omitempty"`
	LastModified time.Time         `json:"lastModified,omitzero"`
	Metadata     map[string]string `json:"metadata,omitempty"`
}

type Download struct {
	Object
	Body io.ReadCloser `json:"-"`
}

type ObjectPage struct {
	Objects   []Object `json:"objects"`
	Prefixes  []string `json:"prefixes"`
	NextToken string   `json:"nextToken,omitempty"`
}

func (s *Source) List(ctx context.Context, bucket, prefix, delimiter, token string, limit int32) (ObjectPage, error) {
	if err := s.checkBucket(bucket); err != nil {
		return ObjectPage{}, err
	}
	if limit < 1 || limit > 1000 {
		return ObjectPage{}, ErrInvalid
	}
	out, err := s.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
		Bucket: &bucket, Prefix: optional(prefix), Delimiter: optional(delimiter), ContinuationToken: optional(token), MaxKeys: &limit,
		EncodingType: "url",
	})
	if err != nil {
		return ObjectPage{}, s3Error(err)
	}
	page := ObjectPage{Objects: make([]Object, 0, len(out.Contents)), Prefixes: make([]string, 0, len(out.CommonPrefixes)), NextToken: aws.ToString(out.NextContinuationToken)}
	for _, item := range out.Contents {
		key, err := s.decodeListed(aws.ToString(item.Key), string(out.EncodingType))
		if err != nil {
			return ObjectPage{}, err
		}
		page.Objects = append(page.Objects, Object{Key: key, Size: aws.ToInt64(item.Size), ETag: aws.ToString(item.ETag), LastModified: aws.ToTime(item.LastModified)})
	}
	for _, item := range out.CommonPrefixes {
		prefix, err := s.decodeListed(aws.ToString(item.Prefix), string(out.EncodingType))
		if err != nil {
			return ObjectPage{}, err
		}
		page.Prefixes = append(page.Prefixes, prefix)
	}
	return page, nil
}

func (s *Source) Stat(ctx context.Context, bucket, key, version string) (Object, error) {
	if err := s.checkObject(bucket, key, version); err != nil {
		return Object{}, err
	}
	out, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: &bucket, Key: &key, VersionId: optional(version)})
	if err != nil {
		return Object{}, s3Error(err)
	}
	return Object{Key: key, Size: aws.ToInt64(out.ContentLength), ETag: aws.ToString(out.ETag), VersionID: aws.ToString(out.VersionId), ContentType: aws.ToString(out.ContentType), LastModified: aws.ToTime(out.LastModified), Metadata: out.Metadata}, nil
}

func (s *Source) Get(ctx context.Context, bucket, key, version, rangeHeader string) (Download, error) {
	if err := s.checkObject(bucket, key, version); err != nil {
		return Download{}, err
	}
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{Bucket: &bucket, Key: &key, VersionId: optional(version), Range: optional(rangeHeader)})
	if err != nil {
		return Download{}, s3Error(err)
	}
	return Download{Object: Object{Key: key, Size: aws.ToInt64(out.ContentLength), ETag: aws.ToString(out.ETag), ContentType: aws.ToString(out.ContentType), ContentRange: aws.ToString(out.ContentRange), VersionID: aws.ToString(out.VersionId), LastModified: aws.ToTime(out.LastModified), Metadata: out.Metadata}, Body: out.Body}, nil
}

func (s *Source) Put(ctx context.Context, bucket, key, contentType string, body io.Reader, size int64) (Object, error) {
	if err := s.checkObject(bucket, key, ""); err != nil {
		return Object{}, err
	}
	if size < 0 {
		return Object{}, ErrInvalid
	}
	if size > MaxSinglePutBytes {
		return Object{}, ErrTooLarge
	}
	release, err := s.acquireTransfer(ctx)
	if err != nil {
		return Object{}, err
	}
	defer release()
	file, err := spool(s.stateDir, body, size)
	if err != nil {
		return Object{}, err
	}
	defer removeSpool(file)
	out, err := s.client.PutObject(ctx, &s3.PutObjectInput{Bucket: &bucket, Key: &key, ContentType: optional(contentType), Body: file, ContentLength: &size})
	if err != nil {
		return Object{}, s3Error(err)
	}
	return Object{Key: key, Size: size, ContentType: contentType, ETag: aws.ToString(out.ETag), VersionID: aws.ToString(out.VersionId)}, nil
}

type Text struct {
	Object
	Text string `json:"text"`
}

func (s *Source) ReadText(ctx context.Context, bucket, key string) (Text, error) {
	found, err := s.Get(ctx, bucket, key, "", "")
	if err != nil {
		return Text{}, err
	}
	defer found.Body.Close()
	if found.Size > MaxTextBytes {
		return Text{}, ErrTooLarge
	}
	body, err := io.ReadAll(io.LimitReader(found.Body, MaxTextBytes+1))
	if err != nil {
		return Text{}, err
	}
	if len(body) > MaxTextBytes {
		return Text{}, ErrTooLarge
	}
	if !utf8.Valid(body) || slices.Contains(body, byte(0)) {
		return Text{}, ErrUnsupported
	}
	return Text{Object: found.Object, Text: string(body)}, nil
}

func (s *Source) WriteText(ctx context.Context, bucket, key, contentType, etag, text string) (Object, error) {
	if err := s.checkObject(bucket, key, ""); err != nil {
		return Object{}, err
	}
	if !s.config.Capabilities.ConditionalWrites {
		return Object{}, ErrUnsupported
	}
	if len(text) > MaxTextBytes {
		return Object{}, ErrTooLarge
	}
	if !utf8.ValidString(text) || strings.ContainsRune(text, 0) || len(etag) < 3 || etag[0] != '"' || etag[len(etag)-1] != '"' || strings.ContainsAny(etag[1:len(etag)-1], "\r\n\",") {
		return Object{}, ErrInvalid
	}
	previous, err := s.Stat(ctx, bucket, key, "")
	if err != nil {
		return Object{}, err
	}
	if previous.ETag != etag {
		return Object{}, ErrConflict
	}
	if previous.Size > MaxTextBytes {
		return Object{}, ErrTooLarge
	}
	if contentType == "" {
		contentType = previous.ContentType
	}
	size := int64(len(text))
	out, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: &bucket, Key: &key, ContentType: optional(contentType), IfMatch: &etag,
		Metadata: previous.Metadata, Body: strings.NewReader(text), ContentLength: &size,
	})
	if err != nil {
		return Object{}, s3Error(err)
	}
	return Object{Key: key, Size: size, ContentType: contentType, ETag: aws.ToString(out.ETag), VersionID: aws.ToString(out.VersionId), Metadata: previous.Metadata}, nil
}

func (s *Source) Delete(ctx context.Context, bucket, key, version string) error {
	if err := s.checkObject(bucket, key, version); err != nil {
		return err
	}
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &bucket, Key: &key, VersionId: optional(version)})
	return s3Error(err)
}

func (s *Source) acquireTransfer(ctx context.Context) (func(), error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case s.transfers <- struct{}{}:
		return func() { <-s.transfers }, nil
	default:
		return nil, ErrBusy
	}
}

func spool(dir string, body io.Reader, size int64) (*os.File, error) {
	if body == nil {
		return nil, ErrInvalid
	}
	file, err := os.CreateTemp(dir, ".transfer-*.tmp")
	if err != nil {
		return nil, err
	}
	n, err := io.Copy(file, io.LimitReader(body, size+1))
	if err == nil && n != size {
		err = ErrInvalid
	}
	if err == nil {
		_, err = file.Seek(0, io.SeekStart)
	}
	if err != nil {
		removeSpool(file)
		return nil, err
	}
	return file, nil
}

func removeSpool(file *os.File) {
	_ = file.Close()
	_ = os.Remove(file.Name())
}

func (s *Source) decodeListed(value, encoding string) (string, error) {
	if encoding != "url" {
		return value, nil
	}
	if s.config.ListingEncoding == "form" {
		return url.QueryUnescape(value)
	}
	return url.PathUnescape(value)
}

func optional(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func s3Error(err error) error {
	if err == nil {
		return nil
	}
	var response *smithyhttp.ResponseError
	if errors.As(err, &response) {
		switch response.HTTPStatusCode() {
		case 404:
			return errors.Join(ErrNotFound, err)
		case 409, 412:
			return errors.Join(ErrConflict, err)
		}
	}
	return err
}

func errorCode(err error, code string) bool {
	var api smithy.APIError
	return errors.As(err, &api) && api.ErrorCode() == code
}
