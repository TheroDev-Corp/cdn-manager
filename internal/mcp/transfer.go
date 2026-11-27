package mcp

import (
	"encoding/json"
	"net/http"

	"github.com/TheroDev-Corp/cdn-manager/internal/auth"
	"github.com/TheroDev-Corp/cdn-manager/internal/service"
	"github.com/TheroDev-Corp/cdn-manager/internal/storage"
)

func (s *Server) grantPart(token auth.AccessToken, input partArgs) (transferResult, error) {
	upload, err := s.backend.UploadForTransfer(token.Actor, input.ID)
	if err != nil {
		return transferResult{}, err
	}
	if upload.Status != "active" {
		return transferResult{}, storage.ErrConflict
	}
	if input.Number < 1 || input.Number > upload.PartCount {
		return transferResult{}, storage.ErrInvalid
	}
	size := min(upload.PartSize, upload.Size-int64(input.Number-1)*upload.PartSize)
	grant, expires, ok := s.provider.IssueUploadGrant(token, upload.ID, input.Number, size, upload.ExpiresAt)
	if !ok {
		return transferResult{}, storage.ErrBusy
	}
	return transferResult{
		URL: s.provider.PublicURL() + TransferPath, Method: http.MethodPut,
		Authorization: "Bearer " + grant, ContentLength: size, ExpiresAt: expires,
	}, nil
}

func (s *Server) Transfer(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !s.allowOrigin(r) {
		http.Error(w, "Forbidden origin", http.StatusForbidden)
		return
	}
	if r.Method != http.MethodPut {
		w.Header().Set("Allow", http.MethodPut)
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	grant, err := s.provider.UploadGrant(r)
	if err != nil {
		w.Header().Set("WWW-Authenticate", `Bearer realm="cdn-manager upload part"`)
		http.Error(w, "An upload grant is required", http.StatusUnauthorized)
		return
	}
	if r.ContentLength != grant.Size {
		http.Error(w, "Content-Length must equal the granted part size", http.StatusBadRequest)
		return
	}
	part, err := s.backend.UploadPart(r.Context(), grant.Actor, grant.UploadID, grant.Number, http.MaxBytesReader(w, r.Body, grant.Size), grant.Size)
	if err != nil {
		public := clientError(err)
		status := http.StatusBadGateway
		switch public {
		case service.ErrForbidden, storage.ErrUnsupported:
			status = http.StatusForbidden
		case storage.ErrInvalid:
			status = http.StatusBadRequest
		case storage.ErrNotFound:
			status = http.StatusNotFound
		case storage.ErrConflict:
			status = http.StatusConflict
		case storage.ErrTooLarge:
			status = http.StatusRequestEntityTooLarge
		case storage.ErrBusy:
			status = http.StatusTooManyRequests
			w.Header().Set("Retry-After", "1")
		}
		http.Error(w, public.Error(), status)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(part)
}
