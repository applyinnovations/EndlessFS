package gcs

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/storage"
	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
)

const (
	backendKind         = "gcs"
	leaseAssociatedData = "endlessfs-gcs-upload-lease-v1"
	resumableChunkSize  = int64(8 << 20)
)

type TransferOptions struct {
	HTTPClient     *http.Client
	GoogleAccessID string
	SignBytes      func([]byte) ([]byte, error)
	Hostname       string
	Insecure       bool
	LeaseKey       []byte
	Random         io.Reader
	Clock          domain.Clock
}

type transferConfiguration struct {
	httpClient     *http.Client
	googleAccessID string
	signBytes      func([]byte) ([]byte, error)
	hostname       string
	insecure       bool
	aead           cipher.AEAD
	random         io.Reader
	clock          domain.Clock
}

type uploadLease struct {
	SchemaVersion int                   `json:"schemaVersion"`
	UploadID      string                `json:"uploadID"`
	Key           string                `json:"key"`
	Size          int64                 `json:"size"`
	MediaType     string                `json:"mediaType"`
	Protocol      domain.UploadProtocol `json:"protocol"`
	SessionURL    string                `json:"sessionURL,omitempty"`
	ExpiresAt     time.Time             `json:"expiresAt"`
}

func newTransferConfiguration(options TransferOptions) (*transferConfiguration, error) {
	if len(options.LeaseKey) != 32 || (options.SignBytes != nil && options.GoogleAccessID == "") {
		return nil, domain.NewError(domain.ErrorInvalid, "invalid GCS transfer configuration")
	}
	block, err := aes.NewCipher(options.LeaseKey)
	if err != nil {
		return nil, domain.NewError(domain.ErrorInvalid, "invalid GCS transfer lease key")
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, domain.WrapError(domain.ErrorInternal, "initialize GCS transfer lease protection", err)
	}
	if options.HTTPClient == nil {
		options.HTTPClient = http.DefaultClient
	}
	if options.Random == nil {
		options.Random = rand.Reader
	}
	if options.Clock == nil {
		options.Clock = domain.SystemClock{}
	}
	return &transferConfiguration{
		httpClient: options.HTTPClient, googleAccessID: options.GoogleAccessID,
		signBytes: options.SignBytes, hostname: options.Hostname, insecure: options.Insecure,
		aead: aead, random: options.Random,
		clock: options.Clock,
	}, nil
}

func (b *Backend) BackendKind() string { return backendKind }

func (b *Backend) BeginUpload(ctx context.Context, request objectstore.UploadRequest) (objectstore.UploadHandle, error) {
	if err := objectstore.ContextError(ctx); err != nil {
		return objectstore.UploadHandle{}, err
	}
	if b.transfer == nil {
		return objectstore.UploadHandle{}, domain.NewError(domain.ErrorPreconditionFailed, "GCS direct transfers are not configured")
	}
	if request.UploadID == "" || !request.Key.Valid() || request.Size < 0 || request.MediaType == "" || !request.ExpiresAt.After(b.transfer.clock.Now()) {
		return objectstore.UploadHandle{}, domain.NewError(domain.ErrorInvalid, "invalid GCS upload request")
	}
	lease := uploadLease{
		SchemaVersion: 1, UploadID: request.UploadID, Key: request.Key.String(), Size: request.Size,
		MediaType: request.MediaType, Protocol: domain.UploadSingle, ExpiresAt: request.ExpiresAt.UTC(),
	}
	capability := objectstore.UploadCapability{
		Protocol: domain.UploadSingle, Method: http.MethodPut,
		Headers: map[string]string{"Content-Type": request.MediaType}, ExpiresAt: request.ExpiresAt.UTC(),
		Framing: domain.UploadFramingWholeObject, DeclaredSize: request.Size,
	}
	// Every browser upload uses a server-initiated session. A one-request upload
	// is still reported as UploadSingle, but unlike a bare signed PUT its
	// capability can be revoked when the operation is aborted.
	initiationURL, err := b.signedURL(request.Key, http.MethodPost, request.ExpiresAt, request.MediaType, []string{"x-goog-resumable:start"}, url.Values{"ifGenerationMatch": {"0"}})
	if err != nil {
		return objectstore.UploadHandle{}, err
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, initiationURL, http.NoBody)
	if err != nil {
		return objectstore.UploadHandle{}, domain.NewError(domain.ErrorInternal, "create GCS resumable initiation request")
	}
	httpRequest.Header.Set("Content-Type", request.MediaType)
	httpRequest.Header.Set("x-goog-resumable", "start")
	response, err := b.transfer.httpClient.Do(httpRequest)
	if err != nil {
		return objectstore.UploadHandle{}, domain.WrapError(domain.ErrorUnavailable, "GCS resumable initiation failed", err)
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	_ = response.Body.Close()
	if response.StatusCode != http.StatusCreated && response.StatusCode != http.StatusOK {
		return objectstore.UploadHandle{}, classifyHTTPStatus("GCS resumable initiation failed", response.StatusCode)
	}
	sessionURL, err := validateSessionURL(response.Header.Get("Location"), initiationURL)
	if err != nil {
		return objectstore.UploadHandle{}, err
	}
	lease.SessionURL = sessionURL
	capability.URL = sessionURL
	capability.Method = http.MethodPut
	capability.Framing = domain.UploadFramingContentRange
	if request.Resumable {
		lease.Protocol = domain.UploadResumable
		capability.Protocol = domain.UploadResumable
		capability.ChunkRules = &domain.ChunkRules{MinimumSize: 256 << 10, MaximumSize: resumableChunkSize, Multiple: 256 << 10}
	}
	sealed, err := b.sealLease(lease)
	if err != nil {
		return objectstore.UploadHandle{}, err
	}
	return objectstore.UploadHandle{Capability: capability, Lease: sealed}, nil
}

func (b *Backend) UploadProgress(ctx context.Context, sealed []byte) (objectstore.UploadProgress, error) {
	lease, err := b.openLease(sealed)
	if err != nil {
		return objectstore.UploadProgress{}, err
	}
	if err := objectstore.ContextError(ctx); err != nil {
		return objectstore.UploadProgress{}, err
	}
	if !b.transfer.clock.Now().Before(lease.ExpiresAt) {
		return objectstore.UploadProgress{}, domain.NewError(domain.ErrorPreconditionFailed, "GCS upload lease expired")
	}
	key := objectstore.MustKey(lease.Key)
	info, headErr := b.Head(ctx, key)
	if headErr == nil {
		if info.Size != lease.Size {
			return objectstore.UploadProgress{}, domain.NewError(domain.ErrorPreconditionFailed, "GCS uploaded object size mismatch")
		}
		return objectstore.UploadProgress{Offset: info.Size, Size: info.Size, ExpiresAt: lease.ExpiresAt, Complete: true, Version: info.Version, Materialized: true}, nil
	}
	if !errors.Is(headErr, domain.ErrNotFound) {
		return objectstore.UploadProgress{}, headErr
	}
	if lease.Protocol == domain.UploadSingle {
		return objectstore.UploadProgress{Size: lease.Size, ExpiresAt: lease.ExpiresAt}, nil
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPut, lease.SessionURL, http.NoBody)
	if err != nil {
		return objectstore.UploadProgress{}, domain.NewError(domain.ErrorInternal, "create GCS resumable status request")
	}
	request.Header.Set("Content-Length", "0")
	request.Header.Set("Content-Range", fmt.Sprintf("bytes */%d", lease.Size))
	response, err := b.transfer.httpClient.Do(request)
	if err != nil {
		return objectstore.UploadProgress{}, domain.WrapError(domain.ErrorUnavailable, "GCS resumable status failed", err)
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	_ = response.Body.Close()
	if response.StatusCode == http.StatusOK || response.StatusCode == http.StatusCreated {
		info, err := b.Head(ctx, key)
		if err != nil {
			return objectstore.UploadProgress{}, err
		}
		if info.Size != lease.Size {
			return objectstore.UploadProgress{}, domain.NewError(domain.ErrorPreconditionFailed, "GCS uploaded object size mismatch")
		}
		return objectstore.UploadProgress{Offset: info.Size, Size: info.Size, ExpiresAt: lease.ExpiresAt, Complete: true, Version: info.Version, Materialized: true}, nil
	}
	if response.StatusCode != http.StatusPermanentRedirect {
		return objectstore.UploadProgress{}, classifyHTTPStatus("GCS resumable status failed", response.StatusCode)
	}
	offset, err := confirmedOffset(response.Header.Get("Range"), lease.Size)
	if err != nil {
		return objectstore.UploadProgress{}, err
	}
	return objectstore.UploadProgress{Offset: offset, Size: lease.Size, ExpiresAt: lease.ExpiresAt}, nil
}

func (b *Backend) ResumeUpload(ctx context.Context, sealed []byte) (objectstore.UploadCapability, error) {
	lease, err := b.openLease(sealed)
	if err != nil {
		return objectstore.UploadCapability{}, err
	}
	if err := objectstore.ContextError(ctx); err != nil {
		return objectstore.UploadCapability{}, err
	}
	if !b.transfer.clock.Now().Before(lease.ExpiresAt) {
		return objectstore.UploadCapability{}, domain.NewError(domain.ErrorNotFound, "GCS upload lease expired")
	}
	capability := objectstore.UploadCapability{
		Protocol: lease.Protocol, URL: lease.SessionURL, Method: http.MethodPut,
		Headers: map[string]string{"Content-Type": lease.MediaType}, ExpiresAt: lease.ExpiresAt,
		Framing: domain.UploadFramingContentRange, DeclaredSize: lease.Size,
	}
	if lease.Protocol == domain.UploadResumable {
		capability.ChunkRules = &domain.ChunkRules{MinimumSize: 256 << 10, MaximumSize: resumableChunkSize, Multiple: 256 << 10}
	}
	return capability, nil
}

func (b *Backend) AbortUpload(ctx context.Context, sealed []byte) error {
	lease, err := b.openLease(sealed)
	if err != nil {
		return err
	}
	key := objectstore.MustKey(lease.Key)
	materialized, err := b.deleteMaterializedUpload(ctx, key, lease.Size)
	if err != nil || materialized {
		return err
	}
	if lease.SessionURL != "" {
		request, requestErr := http.NewRequestWithContext(ctx, http.MethodDelete, lease.SessionURL, http.NoBody)
		if requestErr != nil {
			return domain.NewError(domain.ErrorInternal, "create GCS resumable cancellation request")
		}
		response, requestErr := b.transfer.httpClient.Do(request)
		if requestErr != nil {
			return domain.WrapError(domain.ErrorUnavailable, "GCS resumable cancellation failed", requestErr)
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		_ = response.Body.Close()
		if response.StatusCode != http.StatusNoContent && response.StatusCode != http.StatusNotFound && response.StatusCode != http.StatusGone && response.StatusCode != 499 {
			materialized, err := b.deleteMaterializedUpload(ctx, key, lease.Size)
			if err != nil || materialized {
				return err
			}
			return classifyHTTPStatus("GCS resumable cancellation failed", response.StatusCode)
		}
	}
	_, err = b.deleteMaterializedUpload(ctx, key, lease.Size)
	return err
}

func (b *Backend) deleteMaterializedUpload(ctx context.Context, key objectstore.Key, size int64) (bool, error) {
	info, err := b.Head(ctx, key)
	if errors.Is(err, domain.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if info.Size != size {
		return true, domain.NewError(domain.ErrorPreconditionFailed, "GCS upload abort target mismatch")
	}
	if err := b.Delete(ctx, key, objectstore.DeleteCondition{Version: info.Version}); err != nil && !errors.Is(err, domain.ErrNotFound) {
		return true, err
	}
	return true, nil
}

func (b *Backend) CreateDownload(ctx context.Context, request objectstore.DownloadRequest) (objectstore.DownloadCapability, error) {
	if b.transfer == nil || !request.Key.Valid() || request.Version == "" || request.Filename == "" || request.MediaType == "" || !request.ExpiresAt.After(b.transfer.clock.Now()) {
		return objectstore.DownloadCapability{}, domain.NewError(domain.ErrorInvalid, "invalid GCS download request")
	}
	generation, err := decodeVersion(request.Version)
	if err != nil {
		return objectstore.DownloadCapability{}, err
	}
	disposition := mime.FormatMediaType(string(request.Disposition), map[string]string{"filename": request.Filename})
	if disposition == "" {
		return objectstore.DownloadCapability{}, domain.NewError(domain.ErrorInvalid, "invalid GCS download filename")
	}
	query := url.Values{
		"generation":                   {strconv.FormatInt(generation, 10)},
		"ifGenerationMatch":            {strconv.FormatInt(generation, 10)},
		"response-content-type":        {request.MediaType},
		"response-content-disposition": {disposition},
	}
	signedURL, err := b.signedURL(request.Key, http.MethodGet, request.ExpiresAt, "", nil, query)
	if err != nil {
		return objectstore.DownloadCapability{}, err
	}
	return objectstore.DownloadCapability{URL: signedURL, Method: http.MethodGet, Headers: map[string]string{}, ExpiresAt: request.ExpiresAt.UTC()}, nil
}

func (b *Backend) signedURL(key objectstore.Key, method string, expires time.Time, contentType string, headers []string, query url.Values) (string, error) {
	options := &storage.SignedURLOptions{
		GoogleAccessID: b.transfer.googleAccessID, SignBytes: b.transfer.signBytes,
		Method: method, Expires: expires.UTC(), ContentType: contentType, Headers: headers,
		QueryParameters: query, Scheme: storage.SigningSchemeV4, Hostname: b.transfer.hostname, Insecure: b.transfer.insecure,
	}
	value, err := b.bucket.SignedURL(key.String(), options)
	if err != nil {
		return "", domain.WrapError(domain.ErrorUnavailable, "GCS capability signing failed", err)
	}
	return value, nil
}

func (b *Backend) sealLease(lease uploadLease) ([]byte, error) {
	plaintext, err := json.Marshal(lease)
	if err != nil {
		return nil, domain.NewError(domain.ErrorInternal, "encode GCS upload lease")
	}
	nonce := make([]byte, b.transfer.aead.NonceSize())
	if _, err := io.ReadFull(b.transfer.random, nonce); err != nil {
		return nil, domain.WrapError(domain.ErrorInternal, "generate GCS upload lease nonce", err)
	}
	return b.transfer.aead.Seal(nonce, nonce, plaintext, []byte(leaseAssociatedData)), nil
}

func (b *Backend) openLease(sealed []byte) (uploadLease, error) {
	if b.transfer == nil || len(sealed) <= b.transfer.aead.NonceSize() {
		return uploadLease{}, domain.NewError(domain.ErrorInvalid, "invalid GCS upload lease")
	}
	nonceSize := b.transfer.aead.NonceSize()
	plaintext, err := b.transfer.aead.Open(nil, sealed[:nonceSize], sealed[nonceSize:], []byte(leaseAssociatedData))
	if err != nil {
		return uploadLease{}, domain.NewError(domain.ErrorInvalid, "invalid GCS upload lease")
	}
	var lease uploadLease
	decoder := json.NewDecoder(strings.NewReader(string(plaintext)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&lease); err != nil || lease.SchemaVersion != 1 || lease.UploadID == "" || lease.Size < 0 || lease.MediaType == "" || lease.ExpiresAt.IsZero() || (lease.Protocol != domain.UploadSingle && lease.Protocol != domain.UploadResumable) || (lease.Protocol == domain.UploadResumable && lease.SessionURL == "") {
		return uploadLease{}, domain.NewError(domain.ErrorInvalid, "invalid GCS upload lease")
	}
	key, err := objectstore.ParseKey(lease.Key)
	if err != nil || key.String() != lease.Key {
		return uploadLease{}, domain.NewError(domain.ErrorInvalid, "invalid GCS upload lease")
	}
	return lease, nil
}

func validateSessionURL(value, initiation string) (string, error) {
	session, err := url.Parse(value)
	base, baseErr := url.Parse(initiation)
	if err != nil || baseErr != nil || session.Scheme == "" || session.Host == "" || session.User != nil || session.Fragment != "" || session.Scheme != base.Scheme || !strings.EqualFold(session.Host, base.Host) {
		return "", domain.NewError(domain.ErrorInternal, "GCS returned invalid resumable session")
	}
	return session.String(), nil
}

func confirmedOffset(value string, size int64) (int64, error) {
	if value == "" {
		return 0, nil
	}
	if !strings.HasPrefix(value, "bytes=0-") {
		return 0, domain.NewError(domain.ErrorInternal, "GCS returned invalid resumable range")
	}
	last, err := strconv.ParseInt(strings.TrimPrefix(value, "bytes=0-"), 10, 64)
	if err != nil || last < 0 || last >= size {
		return 0, domain.NewError(domain.ErrorInternal, "GCS returned invalid resumable range")
	}
	return last + 1, nil
}

func classifyHTTPStatus(message string, status int) error {
	switch status {
	case http.StatusBadRequest:
		return domain.NewError(domain.ErrorInvalid, message)
	case http.StatusUnauthorized:
		return domain.NewError(domain.ErrorUnauthenticated, message)
	case http.StatusForbidden:
		return domain.NewError(domain.ErrorUnauthorized, message)
	case http.StatusNotFound, http.StatusGone:
		return domain.NewError(domain.ErrorNotFound, message)
	case http.StatusConflict, http.StatusPreconditionFailed:
		return domain.NewError(domain.ErrorPreconditionFailed, message)
	case http.StatusTooManyRequests, http.StatusRequestTimeout, http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return domain.NewError(domain.ErrorUnavailable, message)
	default:
		return domain.NewError(domain.ErrorInternal, message)
	}
}
