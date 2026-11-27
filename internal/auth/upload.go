package auth

import (
	"context"
	"net/http"
	"slices"
	"time"

	"github.com/TheroDev-Corp/cdn-manager/internal/service"
)

const uploadGrantTTL = 5 * time.Minute

// UploadGrant authorizes retries of exactly one part of one upload session.
type UploadGrant struct {
	Actor    service.Actor
	UploadID string
	Number   int32
	Size     int64
}

func (p *Provider) IssueUploadGrant(ctx context.Context, token AccessToken, id string, part int32, size int64, uploadExpiry time.Time) (string, time.Time, error) {
	if token.Actor.Subject == "" || !slices.Contains(token.Actor.Scopes, service.Write) {
		return "", time.Time{}, ErrUnauthorized
	}
	expires := time.Now().Add(uploadGrantTTL)
	for _, deadline := range []time.Time{token.Expires, uploadExpiry} {
		if deadline.Before(expires) {
			expires = deadline
		}
	}
	// The delegated credential can only upload a part, even if the OAuth
	// credential originally granted broader permissions.
	actor := token.Actor
	actor.Scopes = []string{service.Write}
	grant, err := p.uploadGrants.issue(ctx, UploadGrant{Actor: actor, UploadID: id, Number: part, Size: size}, expires, "", "")
	if err != nil {
		return "", time.Time{}, err
	}
	return grant, expires.UTC(), nil
}

func (p *Provider) UploadGrant(r *http.Request) (UploadGrant, error) {
	return p.uploadGrants.get(r.Context(), bearerToken(r), "", false)
}
