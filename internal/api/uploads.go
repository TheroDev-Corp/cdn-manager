package api

import (
	"net/http"
	"strconv"

	"github.com/TheroDev-Corp/cdn-manager/internal/service"
	"github.com/TheroDev-Corp/cdn-manager/internal/storage"
	"github.com/go-chi/chi/v5"
)

func (a *API) start(w http.ResponseWriter, r *http.Request, actor service.Actor) error {
	var body struct {
		Size        int64
		ContentType string
		CreateOnly  bool
	}
	if err := decodeJSON(w, r, 8192, &body); err != nil {
		return err
	}
	upload, err := a.backend.StartUploadWithPolicy(r.Context(), actor, refFrom(r), body.ContentType, body.Size, body.CreateOnly)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusCreated, upload)
	return nil
}

func (a *API) uploadStatus(w http.ResponseWriter, r *http.Request, actor service.Actor) error {
	upload, err := a.backend.UploadStatus(r.Context(), actor, chi.URLParam(r, "id"))
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, upload)
	return nil
}

func (a *API) uploadPart(w http.ResponseWriter, r *http.Request, actor service.Actor) error {
	number, err := strconv.ParseInt(chi.URLParam(r, "number"), 10, 32)
	if err != nil || number < 1 || number > storage.MaxPartCount {
		return storage.ErrInvalid
	}
	part, err := a.backend.UploadPart(r.Context(), actor, chi.URLParam(r, "id"), int32(number), r.Body, r.ContentLength)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, part)
	return nil
}

func (a *API) complete(w http.ResponseWriter, r *http.Request, actor service.Actor) error {
	result, err := a.backend.CompleteUpload(r.Context(), actor, chi.URLParam(r, "id"))
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, result)
	return nil
}

func (a *API) abort(w http.ResponseWriter, r *http.Request, actor service.Actor) error {
	if err := a.backend.AbortUpload(r.Context(), actor, chi.URLParam(r, "id")); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}
