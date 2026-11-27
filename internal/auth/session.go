package auth

import (
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"sync"
	"time"

	"github.com/TheroDev-Corp/cdn-manager/internal/service"
	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

type entry[T any] struct {
	value   T
	expires time.Time
}

type expiringStore[T any] struct {
	mu        sync.Mutex
	entries   map[string]entry[T]
	limit     int
	nextPrune time.Time
}

func newExpiringStore[T any](limit int) expiringStore[T] {
	return expiringStore[T]{entries: make(map[string]entry[T]), limit: limit}
}

func newToken() string {
	var entropy [32]byte
	rand.Read(entropy[:])
	return base64.RawURLEncoding.EncodeToString(entropy[:])
}

func (s *expiringStore[T]) issue(value T, until time.Time) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	if !now.Before(s.nextPrune) || len(s.entries) == s.limit {
		for id, item := range s.entries {
			if !now.Before(item.expires) {
				delete(s.entries, id)
			}
		}
		s.nextPrune = now.Add(time.Minute)
	}
	if len(s.entries) == s.limit || !now.Before(until) {
		return "", false
	}
	id := newToken()
	s.entries[id] = entry[T]{value: value, expires: until}
	return id, true
}

func (s *expiringStore[T]) get(id string, consume func(T) bool) (T, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if item, exists := s.entries[id]; exists {
		if !time.Now().Before(item.expires) {
			delete(s.entries, id)
		} else if consume == nil || consume(item.value) {
			if consume != nil {
				delete(s.entries, id)
			}
			return item.value, true
		}
	}
	var empty T
	return empty, false
}

func (s *expiringStore[T]) remove(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.entries, id)
}

type login struct {
	state, nonce, verifier string
}

type session struct {
	actor service.Actor
	csrf  string
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
	p.logins.remove(p.cookie(r, "login"))
	transaction := login{state: newToken(), nonce: newToken(), verifier: oauth2.GenerateVerifier()}
	id, ok := p.logins.issue(transaction, time.Now().Add(loginTTL))
	if !ok {
		http.Error(w, "Sign-in temporarily unavailable", http.StatusServiceUnavailable)
		return
	}
	p.setCookie(w, "login", id, int(loginTTL.Seconds()))
	http.Redirect(w, r, p.oauth.AuthCodeURL(transaction.state, oidc.Nonce(transaction.nonce), oauth2.S256ChallengeOption(transaction.verifier)), http.StatusSeeOther)
}

func (p *Provider) Callback(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	query := r.URL.Query()
	transaction, ok := p.logins.get(p.cookie(r, "login"), func(l login) bool { return sameToken(l.state, query.Get("state")) })
	if !ok {
		http.Error(w, "Invalid sign-in transaction", http.StatusBadRequest)
		return
	}
	p.setCookie(w, "login", "", -1)
	if query.Get("code") == "" || query.Get("error") != "" {
		http.Error(w, "Sign-in was declined", http.StatusUnauthorized)
		return
	}
	ctx := oidc.ClientContext(r.Context(), p.httpClient)
	token, err := p.oauth.Exchange(ctx, query.Get("code"), oauth2.VerifierOption(transaction.verifier))
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
	if err != nil || verified.Subject == "" || !sameToken(transaction.nonce, verified.Nonce) {
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
	p.sessions.remove(p.cookie(r, "session"))
	now := time.Now()
	expires := now.Add(p.sessionTTL)
	if verified.Expiry.Before(expires) {
		expires = verified.Expiry
	}
	id, ok := p.sessions.issue(session{actor: claims.actor(verified.Subject, "web"), csrf: newToken()}, expires)
	if !ok {
		http.Error(w, "Sign-in temporarily unavailable", http.StatusServiceUnavailable)
		return
	}
	p.setCookie(w, "session", id, max(1, int(expires.Sub(now).Seconds())))
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (p *Provider) Web(r *http.Request) (service.Actor, string, error) {
	value, ok := p.sessions.get(p.cookie(r, "session"), nil)
	if !ok {
		return service.Actor{}, "", ErrUnauthorized
	}
	return value.actor, value.csrf, nil
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
	if err != nil {
		http.Error(w, "Authentication required", http.StatusUnauthorized)
		return
	}
	if !p.CSRF(r, csrf) {
		http.Error(w, "Invalid CSRF token", http.StatusForbidden)
		return
	}
	p.sessions.remove(p.cookie(r, "session"))
	p.setCookie(w, "session", "", -1)
	w.WriteHeader(http.StatusNoContent)
}
