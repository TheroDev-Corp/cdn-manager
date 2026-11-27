package api

import (
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"

	"github.com/TheroDev-Corp/cdn-manager/internal/auth"
	"github.com/TheroDev-Corp/cdn-manager/internal/service"
	"github.com/TheroDev-Corp/cdn-manager/internal/storage"
	"github.com/aws/smithy-go"
	"github.com/go-chi/chi/v5"
)

type WebAuth interface {
	Web(*http.Request) (service.Actor, string, error)
	ValidCSRF(*http.Request, string, string) bool
	MCPConnection() auth.MCPConnection
}

type API struct {
	backend *service.Backend
	auth    WebAuth
}

type action func(http.ResponseWriter, *http.Request, service.Actor) error

func Mount(router chi.Router, backend *service.Backend, auth WebAuth) {
	a := &API{backend: backend, auth: auth}
	router.Route("/api/v1", func(router chi.Router) {
		router.Use(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Cache-Control", "no-store")
				next.ServeHTTP(w, r)
			})
		})
		router.Get("/session", a.session)
		router.Get("/mcp-settings", a.handle(a.mcpSettings))
		router.Get("/sources", a.handle(a.sources))
		router.Get("/buckets", a.handle(a.buckets))
		router.Post("/buckets", a.handle(a.changeBucket))
		router.Delete("/buckets", a.handle(a.changeBucket))
		router.Get("/bucket-policy", a.handle(a.bucketPolicy))
		router.Put("/bucket-policy", a.handle(a.bucketPolicy))
		router.Delete("/bucket-policy", a.handle(a.bucketPolicy))
		router.Get("/objects", a.handle(a.list))
		router.Get("/object", a.handle(a.stat))
		router.Put("/object", a.handle(a.put))
		router.Delete("/object", a.handle(a.deleteObject))
		router.Post("/delete", a.handle(a.deleteMany))
		router.Get("/cdn-url", a.handle(a.cdn))
		router.Get("/cdn-status", a.handle(a.cdnStatus))
		router.Post("/presign", a.handle(a.presign))
		router.Get("/text", a.handle(a.text))
		router.Put("/text", a.handle(a.text))
		router.Get("/versions", a.handle(a.versions))
		router.Get("/download", a.handle(a.download))
		router.Get("/preview", a.handle(a.download))
		router.Head("/download", a.handle(a.download))
		router.Head("/preview", a.handle(a.download))
		router.Post("/zip", a.handle(a.downloadZIP))
		router.Post("/uploads", a.handle(a.start))
		router.Get("/uploads/{id}", a.handle(a.uploadStatus))
		router.Delete("/uploads/{id}", a.handle(a.abort))
		router.Put("/uploads/{id}/parts/{number}", a.handle(a.uploadPart))
		router.Post("/uploads/{id}/complete", a.handle(a.complete))
	})
}

func (a *API) handle(run action) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor, csrf, err := a.auth.Web(r)
		if errors.Is(err, auth.ErrStoreUnavailable) {
			writeFailure(w, err)
			return
		}
		if err != nil || actor.Subject == "" {
			writeError(w, http.StatusUnauthorized, "Sign in to continue.")
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			token := r.Header.Get("X-CSRF-Token")
			kind, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
			if r.URL.Path == "/api/v1/zip" && kind == "application/x-www-form-urlencoded" {
				r.Body = http.MaxBytesReader(w, r.Body, 4<<20)
				if err := r.ParseForm(); err != nil {
					writeFailure(w, invalidBody(err))
					return
				}
				token = r.PostForm.Get("csrfToken")
			}
			if !a.auth.ValidCSRF(r, csrf, token) {
				writeError(w, http.StatusForbidden, "The security token is missing or expired.")
				return
			}
		}
		if err := run(w, r, actor); err != nil {
			writeFailure(w, err)
		}
	}
}

func (a *API) session(w http.ResponseWriter, r *http.Request) {
	actor, csrf, err := a.auth.Web(r)
	if errors.Is(err, auth.ErrStoreUnavailable) {
		writeFailure(w, err)
		return
	}
	if err != nil || actor.Subject == "" {
		writeError(w, http.StatusUnauthorized, "Sign in to continue.")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"subject": actor.Subject, "name": actor.Name, "csrfToken": csrf})
}

func (a *API) sources(w http.ResponseWriter, _ *http.Request, actor service.Actor) error {
	writeJSON(w, http.StatusOK, map[string]any{"sources": a.backend.Sources(actor)})
	return nil
}

func (a *API) mcpSettings(w http.ResponseWriter, _ *http.Request, _ service.Actor) error {
	writeJSON(w, http.StatusOK, a.auth.MCPConnection())
	return nil
}

func refFrom(r *http.Request) service.Ref {
	q := r.URL.Query()
	return service.Ref{Source: q.Get("source"), Bucket: q.Get("bucket"), Key: q.Get("key"), VersionID: q.Get("versionId")}
}

func count(r *http.Request, fallback int32) (int32, error) {
	s := r.URL.Query().Get("limit")
	if s == "" {
		return fallback, nil
	}
	n, err := strconv.ParseInt(s, 10, 32)
	if err != nil || n < 1 || n > 1000 {
		return 0, storage.ErrInvalid
	}
	return int32(n), nil
}

func decodeJSON(w http.ResponseWriter, r *http.Request, limit int64, target any) error {
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return invalidBody(err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return invalidBody(err)
	}
	return nil
}

func invalidBody(err error) error {
	if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
		return storage.ErrTooLarge
	}
	return storage.ErrInvalid
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"message": message})
}

func writeFailure(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, storage.ErrInvalid):
		writeError(w, http.StatusBadRequest, "The request contains an invalid value.")
	case errors.Is(err, service.ErrForbidden):
		writeError(w, http.StatusForbidden, "You do not have permission for this operation.")
	case errors.Is(err, storage.ErrNotFound):
		writeError(w, http.StatusNotFound, "The requested resource was not found.")
	case errors.Is(err, storage.ErrConflict):
		writeError(w, http.StatusConflict, "The object or upload changed. Refresh its status before retrying.")
	case errors.Is(err, storage.ErrUnsupported):
		writeError(w, http.StatusUnprocessableEntity, "This operation is unavailable for the selected resource.")
	case errors.Is(err, storage.ErrTooLarge):
		writeError(w, http.StatusRequestEntityTooLarge, "The request exceeds the size limit.")
	case errors.Is(err, auth.ErrStoreUnavailable):
		writeError(w, http.StatusServiceUnavailable, "Session verification is temporarily unavailable.")
	case errors.Is(err, storage.ErrJournalUnavailable):
		writeError(w, http.StatusServiceUnavailable, "Upload state is temporarily unavailable.")
	case errors.Is(err, storage.ErrBusy):
		w.Header().Set("Retry-After", "2")
		writeError(w, http.StatusTooManyRequests, "The transfer is busy. Try again shortly.")
	default:
		if apiError, ok := errors.AsType[smithy.APIError](err); ok && apiError.ErrorCode() == "InvalidRange" {
			writeError(w, http.StatusRequestedRangeNotSatisfiable, "The range is outside this object.")
			return
		}
		writeError(w, http.StatusBadGateway, "The storage request could not be completed.")
	}
}
