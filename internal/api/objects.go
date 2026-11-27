package api

import (
	"net/http"
	"time"

	"github.com/TheroDev-Corp/cdn-manager/internal/service"
	"github.com/TheroDev-Corp/cdn-manager/internal/storage"
)

func (a *API) buckets(w http.ResponseWriter, r *http.Request, actor service.Actor) error {
	source, err := a.backend.Authorize(actor, refFrom(r).Source, service.Read)
	if err != nil {
		return err
	}
	names, err := source.ListBuckets(r.Context())
	if err != nil {
		return err
	}
	type bucket struct {
		Name string `json:"name"`
	}
	buckets := make([]bucket, 0, len(names))
	for _, name := range names {
		buckets = append(buckets, bucket{Name: name})
	}
	writeJSON(w, http.StatusOK, map[string]any{"buckets": buckets})
	return nil
}

func (a *API) changeBucket(w http.ResponseWriter, r *http.Request, actor service.Actor) (err error) {
	ref := refFrom(r)
	ctx := r.Context()
	operation := "create_bucket"
	if r.Method == http.MethodDelete {
		operation = "delete_bucket"
	}
	defer func() { a.backend.Audit(ctx, actor, operation, ref, err) }()
	source, err := a.backend.Authorize(actor, ref.Source, service.Bucket)
	if err != nil {
		return err
	}
	if r.Method == http.MethodDelete {
		err = source.DeleteBucket(ctx, ref.Bucket)
	} else {
		err = source.CreateBucket(ctx, ref.Bucket)
	}
	if err == nil {
		w.WriteHeader(http.StatusNoContent)
	}
	return err
}

func (a *API) bucketPolicy(w http.ResponseWriter, r *http.Request, actor service.Actor) (err error) {
	ref := refFrom(r)
	ctx := r.Context()
	if r.Method != http.MethodGet {
		defer func() { a.backend.Audit(ctx, actor, "change_bucket_policy", ref, err) }()
	}
	source, err := a.backend.Authorize(actor, ref.Source, service.Policy)
	if err != nil {
		return err
	}
	switch r.Method {
	case http.MethodGet:
		policy, err := source.GetBucketPolicy(ctx, ref.Bucket)
		if err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, map[string]string{"policy": policy})
		return nil
	case http.MethodDelete:
		err = source.DeleteBucketPolicy(ctx, ref.Bucket)
	case http.MethodPut:
		var body struct{ Policy string }
		if err := decodeJSON(w, r, 128<<10, &body); err != nil {
			return err
		}
		err = source.PutBucketPolicy(ctx, ref.Bucket, body.Policy)
	}
	if err == nil {
		w.WriteHeader(http.StatusNoContent)
	}
	return err
}

func (a *API) list(w http.ResponseWriter, r *http.Request, actor service.Actor) error {
	limit, err := count(r, 100)
	if err != nil {
		return err
	}
	q := r.URL.Query()
	page, err := a.backend.List(r.Context(), actor, refFrom(r), q.Get("prefix"), "/", q.Get("cursor"), limit)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, struct {
		Objects    []storage.Object `json:"objects"`
		Prefixes   []string         `json:"prefixes"`
		NextCursor string           `json:"nextCursor,omitempty"`
	}{page.Objects, page.Prefixes, page.NextToken})
	return nil
}

func (a *API) stat(w http.ResponseWriter, r *http.Request, actor service.Actor) error {
	object, err := a.backend.Stat(r.Context(), actor, refFrom(r))
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, object)
	return nil
}

func (a *API) put(w http.ResponseWriter, r *http.Request, actor service.Actor) (err error) {
	ref := refFrom(r)
	ctx := r.Context()
	defer func() { a.backend.Audit(ctx, actor, "put_object", ref, err) }()
	source, err := a.backend.Authorize(actor, ref.Source, service.Write)
	if err != nil {
		return err
	}
	if ref.VersionID != "" {
		return storage.ErrInvalid
	}
	finish := a.backend.BeginTransfer(ref.Source, "upload")
	result, err := source.Put(ctx, ref.Bucket, ref.Key, r.Header.Get("Content-Type"), r.Body, r.ContentLength)
	finish(result.Size, err)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusCreated, result)
	return nil
}

func (a *API) deleteObject(w http.ResponseWriter, r *http.Request, actor service.Actor) error {
	if err := a.backend.DeleteObject(r.Context(), actor, refFrom(r)); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (a *API) deleteMany(w http.ResponseWriter, r *http.Request, actor service.Actor) (err error) {
	ref := refFrom(r)
	ctx := r.Context()
	defer func() {
		if err != nil {
			a.backend.Audit(ctx, actor, "delete_objects", ref, err)
		}
	}()
	source, err := a.backend.Authorize(actor, ref.Source, service.Delete)
	if err != nil {
		return err
	}
	var body struct {
		Keys []string
	}
	if err := decodeJSON(w, r, 7<<20, &body); err != nil {
		return err
	}
	if len(body.Keys) == 0 || len(body.Keys) > 1000 {
		return storage.ErrInvalid
	}
	targets := make([]storage.DeleteTarget, 0, len(body.Keys))
	for _, key := range body.Keys {
		targets = append(targets, storage.DeleteTarget{Key: key})
	}
	deleted, err := source.DeleteMany(ctx, ref.Bucket, targets)
	if err != nil {
		return err
	}
	type result struct {
		Key   string `json:"key"`
		Error string `json:"error,omitempty"`
	}
	results := make([]result, 0, len(deleted))
	for _, item := range deleted {
		ref.Key = item.Key
		entry := result{Key: item.Key}
		if item.Deleted {
			a.backend.Audit(ctx, actor, "delete_objects", ref, nil)
		} else {
			entry.Error = "The storage service could not delete this object."
			a.backend.Audit(ctx, actor, "delete_objects", ref, storage.ErrConflict)
		}
		results = append(results, entry)
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": results})
	return nil
}

func (a *API) cdn(w http.ResponseWriter, r *http.Request, actor service.Actor) error {
	link, err := a.backend.CDNURL(r.Context(), actor, refFrom(r))
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]string{"url": link})
	return nil
}

func (a *API) cdnStatus(w http.ResponseWriter, r *http.Request, actor service.Actor) error {
	availability, err := a.backend.CheckCDN(r.Context(), actor, refFrom(r))
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, availability)
	return nil
}

func (a *API) presign(w http.ResponseWriter, r *http.Request, actor service.Actor) (err error) {
	ref := refFrom(r)
	ctx := r.Context()
	defer func() { a.backend.Audit(ctx, actor, "presign_object", ref, err) }()
	source, err := a.backend.Authorize(actor, ref.Source, service.Presign)
	if err != nil {
		return err
	}
	var body struct {
		TTLSeconds int64 `json:"ttlSeconds"`
	}
	if err := decodeJSON(w, r, 4096, &body); err != nil {
		return err
	}
	if body.TTLSeconds < 1 || body.TTLSeconds > 604800 {
		return storage.ErrInvalid
	}
	link, err := source.Presign(ctx, ref.Bucket, ref.Key, ref.VersionID, time.Duration(body.TTLSeconds)*time.Second)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]string{"url": link})
	return nil
}

func (a *API) text(w http.ResponseWriter, r *http.Request, actor service.Actor) error {
	ref := refFrom(r)
	ctx := r.Context()
	if r.Method == http.MethodGet {
		body, err := a.backend.ReadText(ctx, actor, ref)
		if err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, struct {
			storage.Text
			MaxBytes int `json:"maxBytes"`
		}{body, storage.MaxTextBytes})
		return nil
	}
	var body struct {
		Text        *string
		ETag        string
		ContentType string
	}
	if err := decodeJSON(w, r, 7<<20, &body); err != nil {
		return err
	}
	if body.Text == nil {
		return storage.ErrInvalid
	}
	result, err := a.backend.WriteText(ctx, actor, ref, body.ContentType, body.ETag, *body.Text)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, result)
	return nil
}

func (a *API) versions(w http.ResponseWriter, r *http.Request, actor service.Actor) error {
	ref := refFrom(r)
	if ref.Key == "" {
		return storage.ErrInvalid
	}
	source, err := a.backend.Authorize(actor, ref.Source, service.Read)
	if err != nil {
		return err
	}
	limit, err := count(r, 100)
	if err != nil {
		return err
	}
	cursor := r.URL.Query().Get("cursor")
	keyMarker := ""
	if cursor != "" {
		keyMarker = ref.Key
	}
	page, err := source.ListVersions(r.Context(), ref.Bucket, ref.Key, keyMarker, cursor, limit)
	if err != nil {
		return err
	}
	type entry struct {
		storage.Version
		ID string `json:"id"`
	}
	versions := make([]entry, 0, len(page.Versions))
	for _, version := range page.Versions {
		if version.Key == ref.Key {
			versions = append(versions, entry{Version: version, ID: version.VersionID})
		}
	}
	nextCursor := ""
	if page.NextKeyMarker == ref.Key {
		nextCursor = page.NextVersionMarker
	}
	writeJSON(w, http.StatusOK, struct {
		Versions   []entry `json:"versions"`
		NextCursor string  `json:"nextCursor,omitempty"`
	}{versions, nextCursor})
	return nil
}
