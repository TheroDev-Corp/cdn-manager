package api

import (
	"archive/zip"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"
	"unicode"

	"github.com/TheroDev-Corp/cdn-manager/internal/service"
	"github.com/TheroDev-Corp/cdn-manager/internal/storage"
)

func (a *API) download(w http.ResponseWriter, r *http.Request, actor service.Actor) (err error) {
	ref := refFrom(r)
	source, err := a.backend.Authorize(actor, ref.Source, service.Read)
	if err != nil {
		return err
	}
	finish := a.backend.BeginTransfer(ref.Source, "download")
	var bytes int64
	defer func() { finish(bytes, err) }()
	object, err := source.Get(r.Context(), ref.Bucket, ref.Key, ref.VersionID, r.Header.Get("Range"))
	if err != nil {
		return err
	}
	defer object.Body.Close()

	contentType := "application/octet-stream"
	w.Header().Set("Content-Security-Policy", "sandbox; default-src 'none'")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.URL.Path == "/api/v1/preview" {
		contentType, _, err = mime.ParseMediaType(object.ContentType)
		if err != nil || !safePreview(contentType) {
			err = storage.ErrUnsupported
			return err
		}
	} else {
		w.Header().Set("Content-Disposition", attachment(filename(ref.Key)))
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Accept-Ranges", "bytes")
	if object.Size >= 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(object.Size, 10))
	}
	if object.ContentRange != "" {
		w.Header().Set("Content-Range", object.ContentRange)
		w.WriteHeader(http.StatusPartialContent)
	}
	if r.Method != http.MethodHead {
		bytes, err = io.Copy(w, object.Body)
		if err != nil {
			panic(http.ErrAbortHandler)
		}
	}
	return nil
}

func safePreview(contentType string) bool {
	switch contentType {
	case "image/png", "image/jpeg", "image/gif", "image/webp", "image/avif", "image/bmp",
		"video/mp4", "video/webm", "video/ogg", "video/quicktime",
		"audio/mpeg", "audio/mp4", "audio/ogg", "audio/wav", "audio/webm", "audio/aac", "audio/flac":
		return true
	default:
		return false
	}
}

func filename(key string) string {
	name := strings.TrimRight(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || strings.ContainsRune("\\:*?\"<>|", r) {
			return '_'
		}
		return r
	}, path.Base(key)), " .")
	if name == "" || name == "/" {
		return "download"
	}
	if windowsDevice(name) {
		return "_" + name
	}
	return name
}

func windowsDevice(name string) bool {
	device, _, _ := strings.Cut(name, ".")
	device = strings.ToUpper(strings.TrimRight(device, " "))
	switch device {
	case "CON", "PRN", "AUX", "NUL", "CONIN$", "CONOUT$":
		return true
	}
	for _, prefix := range []string{"COM", "LPT"} {
		if suffix, ok := strings.CutPrefix(device, prefix); ok && len([]rune(suffix)) == 1 && strings.Contains("0123456789¹²³", suffix) {
			return true
		}
	}
	return false
}

func attachment(filename string) string {
	return mime.FormatMediaType("attachment", map[string]string{"filename": filename})
}

func (a *API) downloadZIP(w http.ResponseWriter, r *http.Request, actor service.Actor) (err error) {
	ref := refFrom(r)
	source, err := a.backend.Authorize(actor, ref.Source, service.Read)
	if err != nil {
		return err
	}
	var body struct{ Keys []string }
	if r.PostForm != nil {
		body.Keys = r.PostForm["keys"]
	} else if err := decodeJSON(w, r, 7<<20, &body); err != nil {
		return err
	}
	if len(body.Keys) == 0 || len(body.Keys) > 1000 {
		return storage.ErrInvalid
	}
	for _, key := range body.Keys {
		if key == "" || len(key) > 1024 {
			return storage.ErrInvalid
		}
	}
	names := archivePaths(body.Keys)

	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", attachment("objects.zip"))
	finish := a.backend.BeginTransfer(ref.Source, "download")
	var bytes int64
	defer func() { finish(bytes, err) }()
	archive := zip.NewWriter(w)
	for i, key := range body.Keys {
		var object storage.Download
		object, err = source.Get(r.Context(), ref.Bucket, key, "", "")
		if err != nil {
			if i == 0 {
				w.Header().Del("Content-Disposition")
				return err
			}
			panic(http.ErrAbortHandler)
		}
		header := &zip.FileHeader{Name: names[i], Method: zip.Store, Modified: object.LastModified}
		var entry io.Writer
		entry, err = archive.CreateHeader(header)
		if err == nil {
			var copied int64
			copied, err = io.Copy(entry, object.Body)
			bytes += copied
		}
		object.Body.Close()
		if err != nil {
			panic(http.ErrAbortHandler)
		}
	}
	if err = archive.Close(); err != nil {
		panic(http.ErrAbortHandler)
	}
	return nil
}

func archivePaths(keys []string) []string {
	names := make([]string, len(keys))
	files := make(map[string]bool, len(keys))
	directories := make(map[string]bool)
	reserve := func(name string) bool {
		folded := strings.ToLower(name)
		if files[folded] || directories[folded] {
			return false
		}
		for dir := path.Dir(folded); dir != "."; dir = path.Dir(dir) {
			if files[dir] {
				return false
			}
		}
		for dir := path.Dir(folded); dir != "."; dir = path.Dir(dir) {
			directories[dir] = true
		}
		files[folded] = true
		return true
	}
	for i, key := range keys {
		if safeZIPPath(key) && reserve(key) {
			names[i] = key
		}
	}
	index := 1
	for i, key := range keys {
		if names[i] != "" {
			continue
		}
		for {
			candidate := fmt.Sprintf("object-%d-%s", index, filename(key))
			index++
			if reserve(candidate) {
				names[i] = candidate
				break
			}
		}
	}
	return names
}

func safeZIPPath(key string) bool {
	if key == "." || !fs.ValidPath(key) {
		return false
	}
	for _, part := range strings.Split(key, "/") {
		if filename(part) != part {
			return false
		}
	}
	return true
}
