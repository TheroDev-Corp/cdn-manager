package storage

import (
	"context"
	"encoding/json"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

func (s *Source) Health(ctx context.Context) error {
	if s.config.Capabilities.BucketManagement {
		// An allowlisted name can be reserved for a future CreateBucket call.
		// Use the same provider operation as the bucket-management entry point.
		_, err := s.ListBuckets(ctx)
		return err
	}
	for _, bucket := range s.config.Buckets {
		if _, err := s.client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: &bucket}); err != nil {
			return s3Error(err)
		}
	}
	return nil
}

func (s *Source) ListBuckets(ctx context.Context) ([]string, error) {
	paginator := s3.NewListBucketsPaginator(s.client, &s3.ListBucketsInput{}, func(opts *s3.ListBucketsPaginatorOptions) {
		opts.StopOnDuplicateToken = true
	})
	buckets := make([]string, 0, len(s.config.Buckets))
	for paginator.HasMorePages() {
		out, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, s3Error(err)
		}
		for _, bucket := range out.Buckets {
			if name := aws.ToString(bucket.Name); s.checkBucket(name) == nil {
				buckets = append(buckets, name)
			}
		}
	}
	return buckets, nil
}

func (s *Source) CreateBucket(ctx context.Context, bucket string) error {
	if err := s.checkBucket(bucket); err != nil {
		return err
	}
	if !s.config.Capabilities.BucketManagement {
		return ErrUnsupported
	}
	in := &s3.CreateBucketInput{Bucket: &bucket}
	if s.config.Endpoint == "" && s.config.Region != "us-east-1" {
		in.CreateBucketConfiguration = &types.CreateBucketConfiguration{LocationConstraint: types.BucketLocationConstraint(s.config.Region)}
	}
	_, err := s.client.CreateBucket(ctx, in)
	return s3Error(err)
}

func (s *Source) DeleteBucket(ctx context.Context, bucket string) error {
	if err := s.checkBucket(bucket); err != nil {
		return err
	}
	if !s.config.Capabilities.BucketManagement {
		return ErrUnsupported
	}
	_, err := s.client.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: &bucket})
	return s3Error(err)
}

func (s *Source) GetBucketPolicy(ctx context.Context, bucket string) (string, error) {
	if err := s.checkBucket(bucket); err != nil {
		return "", err
	}
	if !s.config.Capabilities.BucketPolicies {
		return "", ErrUnsupported
	}
	out, err := s.client.GetBucketPolicy(ctx, &s3.GetBucketPolicyInput{Bucket: &bucket})
	if errorCode(err, "NoSuchBucketPolicy") {
		return "", nil
	}
	if err != nil {
		return "", s3Error(err)
	}
	return aws.ToString(out.Policy), nil
}

func (s *Source) PutBucketPolicy(ctx context.Context, bucket, policy string) error {
	if err := s.checkBucket(bucket); err != nil {
		return err
	}
	if !s.config.Capabilities.BucketPolicies {
		return ErrUnsupported
	}
	if !json.Valid([]byte(policy)) {
		return ErrInvalid
	}
	_, err := s.client.PutBucketPolicy(ctx, &s3.PutBucketPolicyInput{Bucket: &bucket, Policy: &policy})
	return s3Error(err)
}

func (s *Source) DeleteBucketPolicy(ctx context.Context, bucket string) error {
	if err := s.checkBucket(bucket); err != nil {
		return err
	}
	if !s.config.Capabilities.BucketPolicies {
		return ErrUnsupported
	}
	_, err := s.client.DeleteBucketPolicy(ctx, &s3.DeleteBucketPolicyInput{Bucket: &bucket})
	return s3Error(err)
}

func (s *Source) Presign(ctx context.Context, bucket, key, version string, ttl time.Duration) (string, error) {
	if err := s.checkObject(bucket, key, version); err != nil {
		return "", err
	}
	if !s.config.Capabilities.Presign {
		return "", ErrUnsupported
	}
	if ttl < time.Second || ttl > 7*24*time.Hour {
		return "", ErrInvalid
	}
	out, err := s3.NewPresignClient(s.client).PresignGetObject(ctx, &s3.GetObjectInput{Bucket: &bucket, Key: &key, VersionId: optional(version)}, s3.WithPresignExpires(ttl))
	if err != nil {
		return "", s3Error(err)
	}
	return out.URL, nil
}

type Version struct {
	Object
	IsLatest     bool `json:"isLatest"`
	DeleteMarker bool `json:"deleteMarker"`
}

type VersionPage struct {
	Versions          []Version `json:"versions"`
	NextKeyMarker     string    `json:"nextKeyMarker,omitempty"`
	NextVersionMarker string    `json:"nextVersionMarker,omitempty"`
}

func (s *Source) ListVersions(ctx context.Context, bucket, prefix, keyMarker, versionMarker string, limit int32) (VersionPage, error) {
	if err := s.checkBucket(bucket); err != nil {
		return VersionPage{}, err
	}
	if !s.config.Capabilities.Versions {
		return VersionPage{}, ErrUnsupported
	}
	if limit < 1 || limit > 1000 || (keyMarker == "" && versionMarker != "") {
		return VersionPage{}, ErrInvalid
	}
	out, err := s.client.ListObjectVersions(ctx, &s3.ListObjectVersionsInput{
		Bucket: &bucket, Prefix: optional(prefix), KeyMarker: optional(keyMarker), VersionIdMarker: optional(versionMarker), MaxKeys: &limit,
		EncodingType: types.EncodingTypeUrl,
	})
	if err != nil {
		return VersionPage{}, s3Error(err)
	}
	encoding := string(out.EncodingType)
	nextKey, err := s.decodeListed(aws.ToString(out.NextKeyMarker), encoding)
	if err != nil {
		return VersionPage{}, err
	}
	page := VersionPage{Versions: make([]Version, 0, len(out.Versions)+len(out.DeleteMarkers)),
		NextKeyMarker: nextKey, NextVersionMarker: aws.ToString(out.NextVersionIdMarker)}
	for _, item := range out.Versions {
		key, err := s.decodeListed(aws.ToString(item.Key), encoding)
		if err != nil {
			return VersionPage{}, err
		}
		page.Versions = append(page.Versions, Version{Object: Object{Key: key, Size: aws.ToInt64(item.Size),
			ETag: aws.ToString(item.ETag), VersionID: aws.ToString(item.VersionId), LastModified: aws.ToTime(item.LastModified)}, IsLatest: aws.ToBool(item.IsLatest)})
	}
	for _, item := range out.DeleteMarkers {
		key, err := s.decodeListed(aws.ToString(item.Key), encoding)
		if err != nil {
			return VersionPage{}, err
		}
		page.Versions = append(page.Versions, Version{Object: Object{Key: key, VersionID: aws.ToString(item.VersionId),
			LastModified: aws.ToTime(item.LastModified)}, IsLatest: aws.ToBool(item.IsLatest), DeleteMarker: true})
	}
	return page, nil
}

type DeleteTarget struct {
	Key       string `json:"key"`
	VersionID string `json:"versionId,omitempty"`
}

type DeleteResult struct {
	DeleteTarget
	Deleted bool   `json:"deleted"`
	Code    string `json:"code,omitempty"`
}

func (s *Source) DeleteMany(ctx context.Context, bucket string, targets []DeleteTarget) ([]DeleteResult, error) {
	if err := s.checkBucket(bucket); err != nil {
		return nil, err
	}
	if len(targets) == 0 || len(targets) > 1000 {
		return nil, ErrInvalid
	}
	objects := make([]types.ObjectIdentifier, 0, len(targets))
	seen := make(map[DeleteTarget]bool, len(targets))
	for _, target := range targets {
		if err := s.checkObject(bucket, target.Key, target.VersionID); err != nil {
			return nil, err
		}
		if seen[target] {
			return nil, ErrInvalid
		}
		seen[target] = true
		objects = append(objects, types.ObjectIdentifier{Key: aws.String(target.Key), VersionId: optional(target.VersionID)})
	}
	out, err := s.client.DeleteObjects(ctx, &s3.DeleteObjectsInput{Bucket: &bucket, Delete: &types.Delete{Objects: objects}})
	if err != nil {
		return nil, s3Error(err)
	}
	byTarget := make(map[DeleteTarget]DeleteResult, len(targets))
	for _, item := range out.Deleted {
		target := DeleteTarget{Key: aws.ToString(item.Key), VersionID: aws.ToString(item.VersionId)}
		byTarget[target] = DeleteResult{DeleteTarget: target, Deleted: true}
	}
	for _, item := range out.Errors {
		target := DeleteTarget{Key: aws.ToString(item.Key), VersionID: aws.ToString(item.VersionId)}
		byTarget[target] = DeleteResult{DeleteTarget: target, Code: aws.ToString(item.Code)}
	}
	results := make([]DeleteResult, 0, len(targets))
	for _, target := range targets {
		result, ok := byTarget[target]
		if !ok {
			result = DeleteResult{DeleteTarget: target, Code: "UnknownResult"}
		}
		results = append(results, result)
	}
	return results, nil
}
