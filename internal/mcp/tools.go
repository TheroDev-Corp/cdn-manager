package mcp

import (
	"context"
	"time"

	"github.com/TheroDev-Corp/cdn-manager/internal/auth"
	"github.com/TheroDev-Corp/cdn-manager/internal/service"
	"github.com/TheroDev-Corp/cdn-manager/internal/storage"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type listArgs struct {
	Source    string `json:"source"`
	Bucket    string `json:"bucket"`
	Prefix    string `json:"prefix,omitempty"`
	Delimiter string `json:"delimiter,omitempty" jsonschema:"Use / to group immediate folders, or omit for recursive listing."`
	Cursor    string `json:"cursor,omitempty"`
	Limit     int32  `json:"limit,omitempty"`
}

type objectPage struct {
	Objects    []storage.Object `json:"objects"`
	Prefixes   []string         `json:"prefixes"`
	NextCursor string           `json:"nextCursor,omitempty"`
}

type sourcesResult struct {
	Sources []service.Source `json:"sources"`
}

type urlResult struct {
	URL string `json:"url"`
}

type writeTextArgs struct {
	service.Ref
	Text        string `json:"text"`
	ETag        string `json:"etag" jsonschema:"ETag returned by read_text. A mismatch leaves the stored object unchanged."`
	ContentType string `json:"contentType"`
}

type startUploadArgs struct {
	service.Ref
	ContentType string `json:"contentType,omitempty"`
	Size        int64  `json:"size" jsonschema:"Total file length in bytes."`
}

type uploadArgs struct {
	ID string `json:"id" jsonschema:"Upload session ID returned by start_upload."`
}

type partArgs struct {
	uploadArgs
	Number int32 `json:"number" jsonschema:"Part number starting at 1."`
}

type transferResult struct {
	URL           string    `json:"url"`
	Method        string    `json:"method"`
	Authorization string    `json:"authorization"`
	ContentLength int64     `json:"contentLength"`
	ExpiresAt     time.Time `json:"expiresAt"`
}

type mutationResult struct {
	OK bool `json:"ok"`
}

func registerTools(s *Server, server *sdk.Server) {
	b := s.backend
	addTool(s, server, "list_sources", "List available S3 sources and capabilities.", service.Read,
		func(_ context.Context, token auth.AccessToken, _ struct{}) (sourcesResult, error) {
			return sourcesResult{Sources: b.Sources(token.Actor)}, nil
		})
	addTool(s, server, "list_objects", "List one page of S3 objects and optional grouped prefixes.", service.Read,
		func(ctx context.Context, token auth.AccessToken, input listArgs) (objectPage, error) {
			ref := service.Ref{Source: input.Source, Bucket: input.Bucket}
			page, err := b.List(ctx, token.Actor, ref, input.Prefix, input.Delimiter, input.Cursor, input.Limit)
			return objectPage{Objects: page.Objects, Prefixes: page.Prefixes, NextCursor: page.NextToken}, err
		})
	addTool(s, server, "stat_object", "Read object size, ETag and metadata.", service.Read,
		func(ctx context.Context, token auth.AccessToken, ref service.Ref) (storage.Object, error) {
			return b.Stat(ctx, token.Actor, ref)
		})
	addTool(s, server, "get_cdn_url", "Get the permanent CDN URL for an object key.", service.Read,
		func(ctx context.Context, token auth.AccessToken, ref service.Ref) (urlResult, error) {
			value, err := b.CDNURL(ctx, token.Actor, ref)
			return urlResult{URL: value}, err
		})
	addTool(s, server, "read_text", "Read a small UTF-8 text file and its current ETag.", service.Read,
		func(ctx context.Context, token auth.AccessToken, ref service.Ref) (storage.Text, error) {
			return b.ReadText(ctx, token.Actor, ref)
		})
	addTool(s, server, "write_text", "Save text using a matching ETag from read_text.", service.Write,
		func(ctx context.Context, token auth.AccessToken, input writeTextArgs) (storage.Object, error) {
			return b.WriteText(ctx, token.Actor, input.Ref, input.ContentType, input.ETag, input.Text)
		})
	addTool(s, server, "delete_object", "Delete the indicated object or version if permitted.", service.Delete,
		func(ctx context.Context, token auth.AccessToken, ref service.Ref) (mutationResult, error) {
			err := b.DeleteObject(ctx, token.Actor, ref)
			return mutationResult{OK: err == nil}, err
		})
	addTool(s, server, "start_upload", "Create a multipart upload session and get its part sizes.", service.Write,
		func(ctx context.Context, token auth.AccessToken, input startUploadArgs) (service.Upload, error) {
			return b.StartUpload(ctx, token.Actor, input.Ref, input.ContentType, input.Size)
		})
	addTool(s, server, "upload_status", "Check uploaded parts and completion; use it after reconnecting.", service.Write,
		func(ctx context.Context, token auth.AccessToken, input uploadArgs) (service.Upload, error) {
			return b.UploadStatus(ctx, token.Actor, input.ID)
		})
	addTool(s, server, "grant_upload_part", "Get an expiring HTTPS PUT credential for one multipart part.", service.Write,
		func(_ context.Context, token auth.AccessToken, input partArgs) (transferResult, error) {
			return s.grantPart(token, input)
		})
	addTool(s, server, "complete_upload", "Complete the upload after every part has been sent.", service.Write,
		func(ctx context.Context, token auth.AccessToken, input uploadArgs) (storage.Object, error) {
			return b.CompleteUpload(ctx, token.Actor, input.ID)
		})
	addTool(s, server, "abort_upload", "Discard an unfinished multipart upload.", service.Write,
		func(ctx context.Context, token auth.AccessToken, input uploadArgs) (mutationResult, error) {
			err := b.AbortUpload(ctx, token.Actor, input.ID)
			return mutationResult{OK: err == nil}, err
		})
}
