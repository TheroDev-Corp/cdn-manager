package auth

import (
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

func (p *Provider) IssueUploadGrant(token AccessToken, id string, part int32, size int64, uploadExpiry time.Time) (string, time.Time, bool) {
	if token.Actor.Subject == "" || !slices.Contains(token.Actor.Scopes, service.Write) {
		return "", time.Time{}, false
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
	grant, ok := p.uploadGrants.issue(UploadGrant{Actor: actor, UploadID: id, Number: part, Size: size}, expires)
	return grant, expires.UTC(), ok
}

func (p *Provider) UploadGrant(r *http.Request) (UploadGrant, error) {
	value, ok := p.uploadGrants.get(bearerToken(r), nil)
	if !ok {
		return UploadGrant{}, ErrUnauthorized
	}
	return value, nil
}
