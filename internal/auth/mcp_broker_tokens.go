package auth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"mime"
	"net/http"
	"slices"
	"strings"
	"time"
)

const (
	brokerAccessPrefix  = "cdnmcp_at_"
	brokerRefreshPrefix = "cdnmcp_rt_"
)

func validChallenge(challenge string) bool {
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(challenge)
	return len(challenge) == 43 && err == nil && len(decoded) == sha256.Size
}

func challengeFor(verifier string) string {
	if len(verifier) < 43 || len(verifier) > 128 || strings.ContainsFunc(verifier, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("-._~", r))
	}) {
		return ""
	}
	digest := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(digest[:])
}

func brokerForm(w http.ResponseWriter, r *http.Request) bool {
	contentType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	r.Body = http.MaxBytesReader(w, r.Body, brokerBodyLimit)
	return contentType == "application/x-www-form-urlencoded" && len(r.Header.Values("Authorization")) == 0 &&
		r.ParseForm() == nil && singleBrokerValues(r.PostForm) && r.PostForm.Get("client_id") != "" && !r.PostForm.Has("client_secret")
}

func (b *mcpBroker) token(w http.ResponseWriter, r *http.Request) {
	if !brokerForm(w, r) {
		brokerError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	if !b.validResource(r.PostForm.Get("resource")) {
		brokerError(w, http.StatusBadRequest, "invalid_target")
		return
	}
	var response brokerTokenResponse
	var err error
	switch r.PostForm.Get("grant_type") {
	case "authorization_code":
		response, err = b.exchangeCode(r.Context(), r.PostForm.Get("code"), r.PostForm.Get("client_id"),
			r.PostForm.Get("redirect_uri"), r.PostForm.Get("code_verifier"))
	case "refresh_token":
		response, err = b.rotate(r.Context(), r.PostForm.Get("refresh_token"), r.PostForm.Get("client_id"), r.PostForm.Get("scope"))
	default:
		brokerError(w, http.StatusBadRequest, "unsupported_grant_type")
		return
	}
	switch {
	case errors.Is(err, errBrokerScope):
		brokerError(w, http.StatusBadRequest, "invalid_scope")
	case errors.Is(err, ErrUnauthorized):
		brokerError(w, http.StatusBadRequest, "invalid_grant")
	case err != nil:
		brokerError(w, http.StatusServiceUnavailable, "temporarily_unavailable")
	default:
		brokerJSON(w, http.StatusOK, response)
	}
}

var errBrokerScope = errors.New("scope exceeds the grant")

type brokerTokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token,omitempty"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	Scope        string `json:"scope"`
}

func (b *mcpBroker) exchangeCode(ctx context.Context, raw, clientID, redirect, verifier string) (brokerTokenResponse, error) {
	code, err := b.codes.get(ctx, raw, "", false)
	if err != nil {
		return brokerTokenResponse{}, err
	}
	challenge := challengeFor(verifier)
	if !sameToken(code.Challenge, challenge) || code.ClientID != clientID || code.Redirect != redirect || code.Resource != b.provider.mcpAudience {
		return brokerTokenResponse{}, ErrUnauthorized
	}
	if _, err := b.provider.sessions.getHashed(ctx, code.Session, "", false); err != nil {
		return brokerTokenResponse{}, err
	}
	if _, err := b.codes.get(ctx, raw, challenge, true); err != nil {
		return brokerTokenResponse{}, err
	}
	grant := brokerGrant{ClientID: clientID, Session: code.Session, Resource: code.Resource,
		Expires: time.Now().Add(b.provider.sessionTTL)}
	if !code.Refresh {
		grant.Expires = time.Now().Add(min(brokerAccessTTL, b.provider.sessionTTL))
	}
	id, err := b.grants.issue(ctx, grant, grant.Expires, "", "")
	if err != nil {
		return brokerTokenResponse{}, err
	}
	return b.issueTokens(ctx, id, grant, code.Scopes, code.Refresh)
}

func (b *mcpBroker) issueTokens(ctx context.Context, id string, grant brokerGrant, scopes []string, refresh bool) (brokerTokenResponse, error) {
	now := time.Now()
	expires := minTime(now.Add(brokerAccessTTL), grant.Expires)
	access, err := b.access.issue(ctx, brokerToken{Grant: id, Expires: expires, Scopes: scopes}, expires, "", "")
	if err != nil {
		return brokerTokenResponse{}, err
	}
	out := brokerTokenResponse{AccessToken: brokerAccessPrefix + access, TokenType: "Bearer",
		Scope: strings.Join(scopes, " "), ExpiresIn: int(expires.Sub(now).Seconds())}
	if refresh {
		raw := newToken()
		// Retain the token's grant reference after consumption so that a replay
		// revokes the whole grant, including the rotated credential.
		entry := brokerRefresh{Grant: id, Scopes: scopes}
		if err := b.refreshHistory.put(ctx, raw, entry, grant.Expires, "", ""); err != nil {
			return brokerTokenResponse{}, err
		}
		if err := b.refresh.put(ctx, raw, struct{}{}, grant.Expires, "", raw); err != nil {
			return brokerTokenResponse{}, err
		}
		out.RefreshToken = brokerRefreshPrefix + raw
	}
	return out, nil
}

type brokerRefresh struct {
	Grant  string
	Scopes []string
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func (b *mcpBroker) rotate(ctx context.Context, presented, clientID, requested string) (brokerTokenResponse, error) {
	raw, ok := strings.CutPrefix(presented, brokerRefreshPrefix)
	if !ok {
		return brokerTokenResponse{}, ErrUnauthorized
	}
	previous, err := b.refreshHistory.get(ctx, raw, "", false)
	if err != nil {
		return brokerTokenResponse{}, err
	}
	grant, err := b.grants.get(ctx, previous.Grant, "", false)
	if err != nil {
		return brokerTokenResponse{}, err
	}
	if clientID != grant.ClientID || grant.Resource != b.provider.mcpAudience {
		return brokerTokenResponse{}, ErrUnauthorized
	}
	if _, err := b.provider.sessions.getHashed(ctx, grant.Session, "", false); err != nil {
		return brokerTokenResponse{}, err
	}
	scopes := previous.Scopes
	if wanted := strings.Fields(requested); len(wanted) > 0 {
		for _, scope := range wanted {
			if !slices.Contains(previous.Scopes, scope) {
				return brokerTokenResponse{}, errBrokerScope
			}
		}
		scopes = slices.DeleteFunc(slices.Clone(previous.Scopes), func(scope string) bool { return !slices.Contains(wanted, scope) })
	}
	if _, err := b.refresh.get(ctx, raw, raw, true); err != nil {
		if errors.Is(err, ErrUnauthorized) {
			if revokeErr := b.grants.remove(ctx, previous.Grant); revokeErr != nil {
				return brokerTokenResponse{}, revokeErr
			}
		}
		return brokerTokenResponse{}, err
	}
	return b.issueTokens(ctx, previous.Grant, grant, scopes, true)
}

func (b *mcpBroker) revoke(w http.ResponseWriter, r *http.Request) {
	if !brokerForm(w, r) || r.PostForm.Get("token") == "" {
		brokerError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	ctx := r.Context()
	token := r.PostForm.Get("token")
	var grantID string
	var err error
	if raw, ok := strings.CutPrefix(token, brokerAccessPrefix); ok {
		var entry brokerToken
		entry, err = b.access.get(ctx, raw, "", false)
		grantID = entry.Grant
	} else if raw, ok := strings.CutPrefix(token, brokerRefreshPrefix); ok {
		var entry brokerRefresh
		entry, err = b.refreshHistory.get(ctx, raw, "", false)
		grantID = entry.Grant
	}
	if err == nil && grantID != "" {
		var grant brokerGrant
		grant, err = b.grants.get(ctx, grantID, "", false)
		if err == nil && grant.ClientID == r.PostForm.Get("client_id") && grant.Resource == b.provider.mcpAudience {
			err = b.grants.remove(ctx, grantID)
		}
	}
	if err != nil && !errors.Is(err, ErrUnauthorized) {
		brokerError(w, http.StatusServiceUnavailable, "temporarily_unavailable")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.WriteHeader(http.StatusOK)
}

func (b *mcpBroker) accessToken(ctx context.Context, presented string) (AccessToken, error) {
	raw, ok := strings.CutPrefix(presented, brokerAccessPrefix)
	if !ok {
		return AccessToken{}, ErrUnauthorized
	}
	token, err := b.access.get(ctx, raw, "", false)
	if err != nil {
		return AccessToken{}, err
	}
	grant, err := b.grants.get(ctx, token.Grant, "", false)
	if err != nil {
		return AccessToken{}, err
	}
	if grant.Resource != b.provider.mcpAudience {
		return AccessToken{}, ErrUnauthorized
	}
	session, err := b.provider.sessions.getHashed(ctx, grant.Session, "", false)
	if err != nil {
		return AccessToken{}, err
	}
	actor := session.Actor
	actor.Client = "mcp"
	actor.Scopes = token.Scopes
	return AccessToken{Actor: actor, Expires: token.Expires}, nil
}
