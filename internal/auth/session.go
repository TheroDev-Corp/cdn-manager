package auth

import (
	"errors"
	"net/http"
	"time"

	"github.com/TheroDev-Corp/cdn-manager/internal/service"
	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

type login struct {
	Nonce      string
	Verifier   string
	MCPConsent string `json:",omitempty"`
}

type session struct {
	Actor service.Actor
	CSRF  string
}

func (p *Provider) cookieName(name string) string {
	if p.secureCookies {
		return "__Host-cdn-manager-" + name
	}
	return "cdn-manager-" + name
}

func (p *Provider) cookie(r *http.Request, name string) string {
	cookie, err := r.Cookie(p.cookieName(name))
	if err != nil {
		return ""
	}
	return cookie.Value
}

func (p *Provider) setCookie(w http.ResponseWriter, name, value string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name: p.cookieName(name), Value: value, Path: "/", MaxAge: maxAge,
		Secure: p.secureCookies, HttpOnly: true, SameSite: http.SameSiteLaxMode,
	})
}

func (p *Provider) Login(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	state := newToken()
	transaction := login{Nonce: newToken(), Verifier: oauth2.GenerateVerifier()}
	if consent := r.URL.Query().Get("mcp_consent"); p.broker != nil && consent != "" {
		if _, err := p.broker.requests.get(r.Context(), consent, "", false); err != nil {
			http.Error(w, "MCP authorization request is unavailable", http.StatusBadRequest)
			return
		}
		transaction.MCPConsent = consent
	}
	id, err := p.logins.issue(r.Context(), transaction, time.Now().Add(loginTTL), p.cookie(r, "login"), state)
	if err != nil {
		http.Error(w, "Sign-in temporarily unavailable", http.StatusServiceUnavailable)
		return
	}
	p.setCookie(w, "login", id, int(loginTTL.Seconds()))
	http.Redirect(w, r, p.oauth.AuthCodeURL(state, oidc.Nonce(transaction.Nonce), oauth2.S256ChallengeOption(transaction.Verifier)), http.StatusSeeOther)
}

func (p *Provider) Callback(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	query := r.URL.Query()
	transaction, err := p.logins.get(r.Context(), p.cookie(r, "login"), query.Get("state"), true)
	if errors.Is(err, ErrStoreUnavailable) {
		http.Error(w, "Sign-in temporarily unavailable", http.StatusServiceUnavailable)
		return
	}
	if err != nil {
		http.Error(w, "Invalid sign-in transaction", http.StatusBadRequest)
		return
	}
	p.setCookie(w, "login", "", -1)
	if query.Get("code") == "" || query.Get("error") != "" {
		http.Error(w, "Sign-in was declined", http.StatusUnauthorized)
		return
	}
	ctx := oidc.ClientContext(r.Context(), p.httpClient)
	token, err := p.oauth.Exchange(ctx, query.Get("code"), oauth2.VerifierOption(transaction.Verifier))
	if err != nil {
		http.Error(w, "Sign-in could not be completed", http.StatusUnauthorized)
		return
	}
	raw, ok := token.Extra("id_token").(string)
	if !ok {
		http.Error(w, "Invalid sign-in token", http.StatusUnauthorized)
		return
	}
	verified, err := p.webVerifier.Verify(ctx, raw)
	if err != nil || verified.Subject == "" || !sameToken(transaction.Nonce, verified.Nonce) {
		http.Error(w, "Invalid sign-in token", http.StatusUnauthorized)
		return
	}
	var claims identityClaims
	if err := verified.Claims(&claims); err != nil ||
		(claims.AuthorizedParty != "" && claims.AuthorizedParty != p.oauth.ClientID) ||
		(len(verified.Audience) > 1 && claims.AuthorizedParty != p.oauth.ClientID) {
		http.Error(w, "Invalid sign-in token", http.StatusUnauthorized)
		return
	}
	// The verified ID token authenticates this sign-in. The resulting web
	// session uses its own configured lifetime in both the store and cookie.
	expires := time.Now().Add(p.sessionTTL)
	value := session{Actor: claims.actor(verified.Subject, "web"), CSRF: newToken()}
	id, err := p.sessions.issue(r.Context(), value, expires, p.cookie(r, "session"), "")
	if err != nil {
		http.Error(w, "Sign-in temporarily unavailable", http.StatusServiceUnavailable)
		return
	}
	p.setCookie(w, "session", id, int(p.sessionTTL.Seconds()))
	if transaction.MCPConsent != "" && p.broker != nil {
		http.Redirect(w, r, MCPBrokerPath+"/consent?request="+transaction.MCPConsent, http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (p *Provider) Web(r *http.Request) (service.Actor, string, error) {
	value, err := p.sessions.get(r.Context(), p.cookie(r, "session"), "", false)
	if err != nil {
		return service.Actor{}, "", err
	}
	return value.Actor, value.CSRF, nil
}

func (p *Provider) CSRF(r *http.Request, expected string) bool {
	return p.ValidCSRF(r, expected, r.Header.Get("X-CSRF-Token"))
}

func (p *Provider) ValidCSRF(r *http.Request, expected, submitted string) bool {
	origin := r.Header.Get("Origin")
	return (origin == "" || origin == p.publicURL) && sameToken(expected, submitted)
}

func (p *Provider) Logout(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	_, csrf, err := p.Web(r)
	if errors.Is(err, ErrStoreUnavailable) {
		http.Error(w, "Sign-out temporarily unavailable", http.StatusServiceUnavailable)
		return
	}
	if err != nil {
		http.Error(w, "Authentication required", http.StatusUnauthorized)
		return
	}
	if !p.CSRF(r, csrf) {
		http.Error(w, "Invalid CSRF token", http.StatusForbidden)
		return
	}
	if err := p.sessions.remove(r.Context(), p.cookie(r, "session")); err != nil {
		http.Error(w, "Sign-out temporarily unavailable", http.StatusServiceUnavailable)
		return
	}
	p.setCookie(w, "session", "", -1)
	w.WriteHeader(http.StatusNoContent)
}
