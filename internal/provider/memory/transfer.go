package memory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/applyinnovations/endlessfs/internal/domain"
)

func (p *Provider) CreateUpload(ctx context.Context, scope domain.Scope, request domain.CreateUploadRequest) (domain.UploadCapability, error) {
	if err := validateContextScope(ctx, scope); err != nil {
		return domain.UploadCapability{}, err
	}
	if !request.Path.Valid() || request.Path.IsRoot() || request.Size < 0 {
		return domain.UploadCapability{}, domain.NewError(domain.ErrorInvalid, "invalid upload destination or size")
	}
	mediaType, err := domain.NormalizeMediaType(request.MediaType)
	if err != nil {
		return domain.UploadCapability{}, err
	}
	conflict, err := domain.NormalizeConflictMode(request.Conflict)
	if err != nil {
		return domain.UploadCapability{}, err
	}
	if err := validateIdempotencyKey(request.IdempotencyKey); err != nil {
		return domain.UploadCapability{}, err
	}
	fingerprint := operationFingerprint(scope.UserID().String(), request.Path.String(), strconv.FormatInt(request.Size, 10), mediaType, string(conflict), string(request.ExpectedVersion), strconv.FormatBool(request.Resumable))

	p.mu.Lock()
	defer p.mu.Unlock()
	if request.IdempotencyKey != "" {
		key := idempotencyKey(scope.UserID(), OperationCreateUpload, request.IdempotencyKey)
		if prior, found := p.uploadIdempotency[key]; found {
			if prior.fingerprint != fingerprint {
				return domain.UploadCapability{}, domain.NewError(domain.ErrorConflict, "idempotency key was used for a different request")
			}
			return prior.capability, nil
		}
	}
	if err := p.beforeLocked(OperationCreateUpload); err != nil {
		return domain.UploadCapability{}, err
	}
	if p.baseURL == "" {
		return domain.UploadCapability{}, domain.NewError(domain.ErrorUnavailable, "mock data plane URL is not configured")
	}
	if err := p.requireParentLocked(scope, request.Path); err != nil {
		return domain.UploadCapability{}, err
	}
	path := request.Path
	existing, targetExisted := p.scopeObjectsLocked(scope)[path.String()]
	if targetExisted {
		switch conflict {
		case domain.ConflictFail:
			return domain.UploadCapability{}, domain.NewError(domain.ErrorConflict, "upload destination already exists")
		case domain.ConflictReplace:
			if request.ExpectedVersion == "" || request.ExpectedVersion != existing.entry.Version {
				return domain.UploadCapability{}, domain.NewError(domain.ErrorPreconditionFailed, "upload destination version does not match")
			}
		case domain.ConflictRename:
			path, err = p.availableRenamedPathLocked(scope, path)
			if err != nil {
				return domain.UploadCapability{}, err
			}
			targetExisted = false
		}
	}
	protocol := domain.UploadSingle
	method := http.MethodPut
	framing := domain.UploadFramingWholeObject
	var chunkRules *domain.ChunkRules
	if request.Resumable {
		protocol = domain.UploadResumable
		method = http.MethodPatch
		framing = domain.UploadFramingOffsetHeader
		rules := p.chunkRules
		chunkRules = &rules
	} else if request.Size > p.maxMaterializedBytes {
		return domain.UploadCapability{}, domain.NewError(domain.ErrorInvalid, "large uploads must be resumable")
	}
	uploadIDValue, err := p.ids.OpaqueID()
	if err != nil {
		return domain.UploadCapability{}, err
	}
	token, err := p.ids.BearerToken()
	if err != nil {
		return domain.UploadCapability{}, err
	}
	expiresAt := p.clock.Now().Add(p.uploadTTL).UTC()
	if p.consumeSpecificFaultLocked(OperationCreateUpload, FaultExpired) {
		expiresAt = p.clock.Now().Add(-time.Second).UTC()
	}
	uploadID := domain.UploadID(uploadIDValue)
	hash := tokenHash(token)
	p.uploads[uploadID] = &upload{
		id: uploadID, scope: scope, requestedPath: request.Path, path: path, size: request.Size, mediaType: mediaType,
		conflict: conflict, expectedVersion: request.ExpectedVersion, targetExisted: targetExisted,
		protocol: protocol, expiresAt: expiresAt, materialized: true, hasher: sha256.New(), capabilityHash: hash,
	}
	p.uploadTokens[hash] = uploadID
	headers := map[string]string{"Content-Type": mediaType}
	if request.Resumable {
		headers["Upload-Offset"] = "0"
	}
	capability := domain.UploadCapability{
		UploadID: uploadID, Protocol: protocol, URL: p.baseURL + "/cap/upload/" + token,
		Method: method, Headers: headers, ExpiresAt: expiresAt, ChunkRules: chunkRules, Framing: framing, DeclaredSize: request.Size,
	}
	if request.IdempotencyKey != "" {
		p.uploadIdempotency[idempotencyKey(scope.UserID(), OperationCreateUpload, request.IdempotencyKey)] = idempotentUpload{fingerprint: fingerprint, capability: capability}
	}
	return capability, nil
}

func (p *Provider) UploadStatus(ctx context.Context, scope domain.Scope, uploadID domain.UploadID) (domain.UploadStatus, error) {
	if err := validateContextScope(ctx, scope); err != nil {
		return domain.UploadStatus{}, err
	}
	if uploadID == "" {
		return domain.UploadStatus{}, domain.NewError(domain.ErrorInvalid, "upload ID is required")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	session, found := p.uploads[uploadID]
	if !found || session.scope != scope || session.aborted {
		return domain.UploadStatus{}, domain.NewError(domain.ErrorNotFound, "upload not found")
	}
	return domain.UploadStatus{
		UploadID: uploadID, Path: session.requestedPath, Protocol: session.protocol,
		ConfirmedOffset: session.offset, DeclaredSize: session.size, ExpiresAt: session.expiresAt,
	}, nil
}

func (p *Provider) CompleteUpload(ctx context.Context, scope domain.Scope, request domain.CompleteUploadRequest) (domain.Entry, error) {
	if err := validateContextScope(ctx, scope); err != nil {
		return domain.Entry{}, err
	}
	if request.UploadID == "" || !request.Path.Valid() || request.Size < 0 {
		return domain.Entry{}, domain.NewError(domain.ErrorInvalid, "invalid upload completion")
	}
	mediaType, err := domain.NormalizeMediaType(request.MediaType)
	if err != nil {
		return domain.Entry{}, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.beforeLocked(OperationCompleteUpload); err != nil {
		return domain.Entry{}, err
	}
	session, found := p.uploads[request.UploadID]
	if !found || session.scope != scope || session.aborted {
		return domain.Entry{}, domain.NewError(domain.ErrorNotFound, "upload not found")
	}
	if !p.clock.Now().Before(session.expiresAt) {
		return domain.Entry{}, domain.NewError(domain.ErrorPreconditionFailed, "upload capability expired")
	}
	if request.Path != session.requestedPath || request.Size != session.size || mediaType != session.mediaType {
		return domain.Entry{}, domain.NewError(domain.ErrorPreconditionFailed, "upload constraints do not match initiation")
	}
	if session.offset != session.size {
		return domain.Entry{}, domain.NewError(domain.ErrorPreconditionFailed, "upload is incomplete")
	}
	checksum := hex.EncodeToString(session.hasher.Sum(nil))
	if p.consumeSpecificFaultLocked(OperationCompleteUpload, FaultChecksumMismatch) || (request.ChecksumSHA256 != "" && !strings.EqualFold(request.ChecksumSHA256, checksum)) {
		return domain.Entry{}, domain.NewError(domain.ErrorPreconditionFailed, "upload checksum does not match")
	}
	current, exists := p.scopeObjectsLocked(scope)[session.path.String()]
	if session.targetExisted {
		if !exists || current.entry.Version != session.expectedVersion {
			return domain.Entry{}, domain.NewError(domain.ErrorPreconditionFailed, "upload destination changed")
		}
	} else if exists {
		return domain.Entry{}, domain.NewError(domain.ErrorConflict, "upload destination appeared during transfer")
	}
	data := append([]byte(nil), session.data...)
	storedMediaType := trustedMediaType(session.mediaType, data, session.materialized)
	entry, err := p.newFileEntryLocked(session.path, session.size, storedMediaType)
	if err != nil {
		return domain.Entry{}, err
	}
	if session.targetExisted {
		p.deleteTreeLocked(scope, session.path)
	}
	p.scopeObjectsLocked(scope)[session.path.String()] = object{entry: entry, data: data, materialized: session.materialized}
	delete(p.uploadTokens, session.capabilityHash)
	delete(p.uploads, request.UploadID)
	return entry, nil
}

func trustedMediaType(declared string, data []byte, materialized bool) string {
	switch declared {
	case "image/png", "image/jpeg", "image/gif", "image/webp", "application/pdf", "text/plain":
		if !materialized {
			return "application/octet-stream"
		}
		detected, _, err := mime.ParseMediaType(http.DetectContentType(data))
		if err != nil || detected != declared {
			return "application/octet-stream"
		}
		if declared == "text/plain" && !utf8.Valid(data) {
			return "application/octet-stream"
		}
	}
	return declared
}

func (p *Provider) AbortUpload(ctx context.Context, scope domain.Scope, uploadID domain.UploadID) error {
	if err := validateContextScope(ctx, scope); err != nil {
		return err
	}
	if uploadID == "" {
		return domain.NewError(domain.ErrorInvalid, "upload ID is required")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.beforeLocked(OperationAbortUpload); err != nil {
		return err
	}
	session, found := p.uploads[uploadID]
	if !found || session.scope != scope {
		return domain.NewError(domain.ErrorNotFound, "upload not found")
	}
	session.aborted = true
	delete(p.uploadTokens, session.capabilityHash)
	delete(p.uploads, uploadID)
	return nil
}

func (p *Provider) CreateDownload(ctx context.Context, scope domain.Scope, request domain.CreateDownloadRequest) (domain.DownloadCapability, error) {
	if err := validateContextScope(ctx, scope); err != nil {
		return domain.DownloadCapability{}, err
	}
	if !request.Path.Valid() || request.Path.IsRoot() {
		return domain.DownloadCapability{}, domain.NewError(domain.ErrorInvalid, "download path is invalid")
	}
	if request.Disposition == "" {
		request.Disposition = domain.DispositionAttachment
	}
	if request.Disposition != domain.DispositionAttachment && request.Disposition != domain.DispositionInline {
		return domain.DownloadCapability{}, domain.NewError(domain.ErrorInvalid, "invalid download disposition")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.beforeLocked(OperationCreateDownload); err != nil {
		return domain.DownloadCapability{}, err
	}
	item, found := p.scopeObjectsLocked(scope)[request.Path.String()]
	if !found || item.entry.Kind != domain.EntryFile {
		return domain.DownloadCapability{}, domain.NewError(domain.ErrorNotFound, "file not found")
	}
	if request.Version == "" || request.Version != item.entry.Version {
		return domain.DownloadCapability{}, domain.NewError(domain.ErrorPreconditionFailed, "download version does not match")
	}
	if p.baseURL == "" {
		return domain.DownloadCapability{}, domain.NewError(domain.ErrorUnavailable, "mock data plane URL is not configured")
	}
	token, err := p.ids.BearerToken()
	if err != nil {
		return domain.DownloadCapability{}, err
	}
	expiresAt := p.clock.Now().Add(min(p.downloadTTL, 10*time.Minute)).UTC()
	if p.consumeSpecificFaultLocked(OperationCreateDownload, FaultExpired) {
		expiresAt = p.clock.Now().Add(-time.Second).UTC()
	}
	p.downloads[tokenHash(token)] = download{
		scope: scope, path: request.Path, version: request.Version,
		disposition: request.Disposition, expiresAt: expiresAt,
	}
	return domain.DownloadCapability{
		URL: p.baseURL + "/cap/download/" + token, Method: http.MethodGet,
		Headers: map[string]string{}, ExpiresAt: expiresAt,
	}, nil
}

func (p *Provider) UploadOffset(ctx context.Context, scope domain.Scope, uploadID domain.UploadID) (int64, error) {
	if err := validateContextScope(ctx, scope); err != nil {
		return 0, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	session, found := p.uploads[uploadID]
	if !found || session.scope != scope {
		return 0, domain.NewError(domain.ErrorNotFound, "upload not found")
	}
	return session.offset, nil
}

// SimulateUploadOffset advances a mock upload without allocating the logical
// object. It exists solely to prove offsets above normal memory sizes.
func (p *Provider) SimulateUploadOffset(ctx context.Context, scope domain.Scope, uploadID domain.UploadID, offset int64) error {
	if err := validateContextScope(ctx, scope); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	session, found := p.uploads[uploadID]
	if !found || session.scope != scope {
		return domain.NewError(domain.ErrorNotFound, "upload not found")
	}
	if session.protocol != domain.UploadResumable || offset < session.offset || offset > session.size {
		return domain.NewError(domain.ErrorInvalid, "invalid simulated upload offset")
	}
	session.offset = offset
	session.data = nil
	session.materialized = false
	return nil
}

func (p *Provider) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("Referrer-Policy", "no-referrer")
	writer.Header().Set("X-Content-Type-Options", "nosniff")
	writer.Header().Set("Content-Security-Policy", "default-src 'none'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; sandbox")
	parts := strings.Split(strings.TrimPrefix(request.URL.Path, "/"), "/")
	if len(parts) != 3 || parts[0] != "cap" || parts[2] == "" {
		http.NotFound(writer, request)
		return
	}
	if origin := request.Header.Get("Origin"); origin != "" {
		if p.allowedOrigin == "" || origin != p.allowedOrigin {
			http.Error(writer, "origin is not allowed", http.StatusForbidden)
			return
		}
		writer.Header().Set("Access-Control-Allow-Origin", p.allowedOrigin)
		writer.Header().Set("Vary", "Origin")
		writer.Header().Set("Access-Control-Expose-Headers", "Content-Length, Content-Range, Content-Disposition, Upload-Offset")
	}
	if request.Method == http.MethodOptions {
		writer.Header().Set("Access-Control-Allow-Methods", "GET, PUT, PATCH, OPTIONS")
		writer.Header().Set("Access-Control-Allow-Headers", "Content-Type, Range, Upload-Offset")
		writer.Header().Set("Access-Control-Max-Age", "300")
		writer.WriteHeader(http.StatusNoContent)
		return
	}
	switch parts[1] {
	case "upload":
		p.serveUpload(writer, request, parts[2])
	case "download":
		p.serveDownload(writer, request, parts[2])
	default:
		http.NotFound(writer, request)
	}
}

func (p *Provider) serveUpload(writer http.ResponseWriter, request *http.Request, token string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.beforeLocked(OperationUploadData); err != nil {
		writeDataPlaneError(writer, err)
		return
	}
	uploadID, found := p.uploadTokens[tokenHash(token)]
	if !found {
		http.NotFound(writer, request)
		return
	}
	session, found := p.uploads[uploadID]
	if !found || session.aborted {
		http.NotFound(writer, request)
		return
	}
	expectedMethod := http.MethodPut
	if session.protocol == domain.UploadResumable {
		expectedMethod = http.MethodPatch
	}
	if request.Method != expectedMethod {
		writer.Header().Set("Allow", expectedMethod)
		http.Error(writer, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !p.clock.Now().Before(session.expiresAt) {
		http.Error(writer, "capability unavailable", http.StatusGone)
		return
	}
	mediaType, err := domain.NormalizeMediaType(request.Header.Get("Content-Type"))
	if err != nil || mediaType != session.mediaType {
		http.Error(writer, "upload constraint mismatch", http.StatusPreconditionFailed)
		return
	}
	if session.protocol == domain.UploadResumable {
		offset, err := parseOffset(request.Header.Get("Upload-Offset"))
		if err != nil || offset != session.offset {
			writer.Header().Set("Upload-Offset", strconv.FormatInt(session.offset, 10))
			http.Error(writer, "upload offset mismatch", http.StatusConflict)
			return
		}
	}
	limit := session.size - session.offset
	if session.protocol == domain.UploadResumable {
		limit = min(limit, p.chunkRules.MaximumSize)
	}
	data, err := io.ReadAll(io.LimitReader(request.Body, limit+1))
	p.metrics.UploadBytes += int64(len(data))
	if err != nil {
		http.Error(writer, "upload interrupted", http.StatusServiceUnavailable)
		return
	}
	if int64(len(data)) > limit {
		http.Error(writer, "upload chunk too large", http.StatusRequestEntityTooLarge)
		return
	}
	if session.protocol == domain.UploadSingle && int64(len(data)) != session.size {
		http.Error(writer, "single upload size mismatch", http.StatusPreconditionFailed)
		return
	}
	accepted := len(data)
	interrupted := p.consumeSpecificFaultLocked(OperationUploadData, FaultInterruptedUpload)
	if interrupted && accepted > 0 {
		accepted = max(1, accepted/2)
	}
	if session.protocol == domain.UploadResumable && accepted > 0 && session.offset+int64(accepted) < session.size {
		if int64(accepted) < p.chunkRules.MinimumSize || int64(accepted)%p.chunkRules.Multiple != 0 {
			http.Error(writer, "upload chunk violates chunk rules", http.StatusBadRequest)
			return
		}
	}
	acceptedData := data[:accepted]
	_, _ = session.hasher.Write(acceptedData)
	if session.materialized && session.offset+int64(accepted) <= p.maxMaterializedBytes {
		session.data = append(session.data, acceptedData...)
	} else {
		session.data = nil
		session.materialized = false
	}
	session.offset += int64(accepted)
	writer.Header().Set("Upload-Offset", strconv.FormatInt(session.offset, 10))
	if interrupted {
		http.Error(writer, "upload interrupted", http.StatusServiceUnavailable)
		return
	}
	writer.WriteHeader(http.StatusNoContent)
}

func (p *Provider) serveDownload(writer http.ResponseWriter, request *http.Request, token string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.beforeLocked(OperationDownloadData); err != nil {
		writeDataPlaneError(writer, err)
		return
	}
	capability, found := p.downloads[tokenHash(token)]
	if !found {
		http.NotFound(writer, request)
		return
	}
	if request.Method != http.MethodGet {
		writer.Header().Set("Allow", http.MethodGet)
		http.Error(writer, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !p.clock.Now().Before(capability.expiresAt) {
		http.Error(writer, "capability unavailable", http.StatusGone)
		return
	}
	item, found := p.scopeObjectsLocked(capability.scope)[capability.path.String()]
	if !found || item.entry.Kind != domain.EntryFile || item.entry.Version != capability.version {
		http.NotFound(writer, request)
		return
	}
	start, end, partial, err := parseRange(request.Header.Get("Range"), item.entry.Size)
	if err != nil {
		writer.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", item.entry.Size))
		http.Error(writer, "range not satisfiable", http.StatusRequestedRangeNotSatisfiable)
		return
	}
	length := end - start + 1
	writer.Header().Set("Accept-Ranges", "bytes")
	writer.Header().Set("Content-Type", item.entry.MediaType)
	writer.Header().Set("Content-Length", strconv.FormatInt(length, 10))
	writer.Header().Set("Content-Disposition", safeDisposition(capability.disposition, item.entry.Name))
	if partial {
		writer.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, item.entry.Size))
		writer.WriteHeader(http.StatusPartialContent)
	} else {
		writer.WriteHeader(http.StatusOK)
	}
	var written int64
	if item.materialized {
		count, _ := writer.Write(item.data[start : end+1])
		written = int64(count)
	} else {
		written = writeZeros(writer, length)
	}
	p.metrics.DownloadBytes += written
}

func parseRange(value string, size int64) (start, end int64, partial bool, err error) {
	if size < 0 {
		return 0, 0, false, fmt.Errorf("invalid size")
	}
	if value == "" {
		if size == 0 {
			return 0, -1, false, nil
		}
		return 0, size - 1, false, nil
	}
	if !strings.HasPrefix(value, "bytes=") || strings.Contains(value, ",") || size == 0 {
		return 0, 0, false, fmt.Errorf("invalid range")
	}
	parts := strings.Split(strings.TrimPrefix(value, "bytes="), "-")
	if len(parts) != 2 {
		return 0, 0, false, fmt.Errorf("invalid range")
	}
	if parts[0] == "" {
		suffix, parseErr := strconv.ParseInt(parts[1], 10, 64)
		if parseErr != nil || suffix <= 0 {
			return 0, 0, false, fmt.Errorf("invalid suffix range")
		}
		suffix = min(suffix, size)
		return size - suffix, size - 1, true, nil
	}
	start, err = strconv.ParseInt(parts[0], 10, 64)
	if err != nil || start < 0 || start >= size {
		return 0, 0, false, fmt.Errorf("invalid range start")
	}
	end = size - 1
	if parts[1] != "" {
		end, err = strconv.ParseInt(parts[1], 10, 64)
		if err != nil || end < start {
			return 0, 0, false, fmt.Errorf("invalid range end")
		}
		end = min(end, size-1)
	}
	return start, end, true, nil
}

func safeDisposition(disposition domain.Disposition, filename string) string {
	if disposition != domain.DispositionAttachment && disposition != domain.DispositionInline {
		disposition = domain.DispositionAttachment
	}
	value := mime.FormatMediaType(string(disposition), map[string]string{"filename": filename})
	if value == "" {
		return string(disposition)
	}
	return value
}

func writeZeros(writer io.Writer, size int64) int64 {
	buffer := make([]byte, 32<<10)
	var written int64
	for written < size {
		chunk := min(int64(len(buffer)), size-written)
		count, err := writer.Write(buffer[:chunk])
		written += int64(count)
		if err != nil || int64(count) != chunk {
			break
		}
	}
	return written
}

func writeDataPlaneError(writer http.ResponseWriter, err error) {
	switch domain.KindOf(err) {
	case domain.ErrorRateLimited:
		http.Error(writer, "temporarily unavailable", http.StatusTooManyRequests)
	case domain.ErrorUnavailable:
		http.Error(writer, "temporarily unavailable", http.StatusServiceUnavailable)
	case domain.ErrorConflict, domain.ErrorPreconditionFailed:
		http.Error(writer, "request conflict", http.StatusConflict)
	default:
		http.Error(writer, "request unavailable", http.StatusNotFound)
	}
}
