package service

import (
	"context"
	"net/http"
	"time"

	"github.com/TheroDev-Corp/cdn-manager/internal/storage"
)

var (
	probeSlots  = make(chan struct{}, 8)
	probeClient = &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			MaxIdleConns:           8,
			MaxIdleConnsPerHost:    2,
			IdleConnTimeout:        30 * time.Second,
			MaxResponseHeaderBytes: 64 << 10,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
)

type CDNAvailability struct {
	Available bool `json:"available"`
	Status    int  `json:"status,omitempty"`
}

func (b *Backend) CheckCDN(ctx context.Context, actor Actor, ref Ref) (CDNAvailability, error) {
	source, err := b.Authorize(actor, ref.Source, Read)
	if err != nil {
		return CDNAvailability{}, err
	}
	if ref.VersionID != "" {
		return CDNAvailability{}, storage.ErrUnsupported
	}
	target, err := source.CDNProbeLink(ref.Bucket, ref.Key)
	if err != nil {
		return CDNAvailability{}, err
	}
	select {
	case probeSlots <- struct{}{}:
		defer func() { <-probeSlots }()
	default:
		return CDNAvailability{}, storage.ErrBusy
	}
	check, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	status := probe(check, http.MethodHead, target)
	if status == http.StatusMethodNotAllowed || status == http.StatusNotImplemented {
		status = probe(check, http.MethodGet, target)
	}
	return CDNAvailability{Status: status, Available: status >= 200 && status < 300}, nil
}

func probe(ctx context.Context, method, target string) int {
	req, err := http.NewRequestWithContext(ctx, method, target, nil)
	if err != nil {
		return 0
	}
	if method == http.MethodGet {
		req.Header.Set("Range", "bytes=0-0")
	}
	response, err := probeClient.Do(req)
	if err != nil {
		return 0
	}
	_ = response.Body.Close()
	return response.StatusCode
}
