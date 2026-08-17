package memory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
)

const (
	TransferUploadData       = "upload_data"
	TransferDownloadData     = "download_data"
	TransferFaultInterrupted = "interrupted_upload"
)

type uploadSession struct {
	id           string
	key          objectstore.Key
	size         int64
	mediaType    string
	protocol     domain.UploadProtocol
	expiresAt    time.Time
	offset       int64
	body         []byte
	materialized bool
	hasher       hash.Hash
	tokenHash    [sha256.Size]byte
	aborted      bool
	version      objectstore.NativeVersion
}

type downloadSession struct {
	key         objectstore.Key
	version     objectstore.NativeVersion
	filename    string
	mediaType   string
	disposition domain.Disposition
	expiresAt   time.Time
}

type TransferByteCounts struct {
	Upload   int64
	Download int64
}

func (b *Backend) ConfigureDataPlane(baseURL string, clock domain.Clock, ids *domain.IDGenerator) error {
	parsed, err := url.Parse(baseURL)
	ip := net.ParseIP(parsed.Hostname())
	if err != nil || parsed.Scheme != "http" || parsed.Port() == "" || ip == nil || !ip.IsLoopback() || parsed.User != nil || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" || clock == nil || ids == nil {
		return domain.NewError(domain.ErrorInvalid, "memory data plane requires a loopback URL, clock, and IDs")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.dataPlaneURL = strings.TrimRight(baseURL, "/")
	b.clock = clock
	b.ids = ids
	return nil
}

func (b *Backend) InjectTransferFault(operation, fault string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.transferFaults[operation] = append(b.transferFaults[operation], fault)
}

func (b *Backend) TransferByteCounts() TransferByteCounts {
	b.mu.Lock()
	defer b.mu.Unlock()
	return TransferByteCounts{Upload: b.uploadBytes, Download: b.downloadBytes}
}

func (b *Backend) BeginUpload(ctx context.Context, request objectstore.UploadRequest) (objectstore.UploadCapability, error) {
	if err := objectstore.ContextError(ctx); err != nil {
		return objectstore.UploadCapability{}, err
	}
	if request.UploadID == "" || !request.Key.Valid() || request.Size < 0 || request.MediaType == "" || !request.ExpiresAt.After(b.clock.Now()) {
		return objectstore.UploadCapability{}, domain.NewError(domain.ErrorInvalid, "invalid direct upload request")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.dataPlaneURL == "" {
		return objectstore.UploadCapability{}, domain.NewError(domain.ErrorUnavailable, "memory data plane is not configured")
	}
	if _, exists := b.uploads[request.UploadID]; exists {
		return objectstore.UploadCapability{}, domain.NewError(domain.ErrorConflict, "upload already exists")
	}
	token, err := b.ids.BearerToken()
	if err != nil {
		return objectstore.UploadCapability{}, err
	}
	protocol := domain.UploadSingle
	method := http.MethodPut
	framing := domain.UploadFramingWholeObject
	var chunkRules *domain.ChunkRules
	if request.Resumable {
		protocol = domain.UploadResumable
		method = http.MethodPatch
		framing = domain.UploadFramingOffsetHeader
		rules := domain.ChunkRules{MinimumSize: 1, MaximumSize: 8 << 20, Multiple: 1}
		chunkRules = &rules
	}
	hash := sha256.Sum256([]byte(token))
	session := &uploadSession{
		id: request.UploadID, key: request.Key, size: request.Size, mediaType: request.MediaType,
		protocol: protocol, expiresAt: request.ExpiresAt.UTC(), materialized: true, hasher: sha256.New(), tokenHash: hash,
	}
	b.uploads[request.UploadID] = session
	b.uploadTokens[hash] = request.UploadID
	headers := map[string]string{"Content-Type": request.MediaType}
	if request.Resumable {
		headers["Upload-Offset"] = "0"
	}
	return objectstore.UploadCapability{
		Protocol: protocol, URL: b.dataPlaneURL + "/cap/upload/" + token, Method: method,
		Headers: headers, ExpiresAt: request.ExpiresAt.UTC(), ChunkRules: chunkRules, Framing: framing, DeclaredSize: request.Size,
	}, nil
}

func (b *Backend) UploadProgress(ctx context.Context, uploadID string) (objectstore.UploadProgress, error) {
	if err := objectstore.ContextError(ctx); err != nil {
		return objectstore.UploadProgress{}, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	session, found := b.uploads[uploadID]
	if !found || session.aborted {
		return objectstore.UploadProgress{}, domain.NewError(domain.ErrorNotFound, "upload not found")
	}
	checksum := ""
	if session.offset == session.size && session.materialized {
		checksum = hex.EncodeToString(session.hasher.Sum(nil))
	}
	return objectstore.UploadProgress{
		Offset: session.offset, Size: session.size, ExpiresAt: session.expiresAt,
		Complete: session.offset == session.size, Version: session.version, SHA256: checksum, Materialized: session.materialized,
	}, nil
}

func (b *Backend) AbortUpload(ctx context.Context, uploadID string) error {
	if err := objectstore.ContextError(ctx); err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	session, found := b.uploads[uploadID]
	if !found {
		return domain.NewError(domain.ErrorNotFound, "upload not found")
	}
	session.aborted = true
	delete(b.uploadTokens, session.tokenHash)
	delete(b.uploads, uploadID)
	if current, exists := b.records[session.key.String()]; exists && current.version == session.version {
		delete(b.records, session.key.String())
	}
	return nil
}

func (b *Backend) CreateDownload(ctx context.Context, request objectstore.DownloadRequest) (objectstore.DownloadCapability, error) {
	if err := objectstore.ContextError(ctx); err != nil {
		return objectstore.DownloadCapability{}, err
	}
	if !request.Key.Valid() || request.Version == "" || request.Filename == "" || request.MediaType == "" || !request.ExpiresAt.After(b.clock.Now()) || (request.Disposition != domain.DispositionAttachment && request.Disposition != domain.DispositionInline) {
		return objectstore.DownloadCapability{}, domain.NewError(domain.ErrorInvalid, "invalid direct download request")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	record, found := b.records[request.Key.String()]
	if !found {
		return objectstore.DownloadCapability{}, domain.NewError(domain.ErrorNotFound, "download object not found")
	}
	if record.version != request.Version {
		return objectstore.DownloadCapability{}, domain.NewError(domain.ErrorPreconditionFailed, "download object version does not match")
	}
	if b.dataPlaneURL == "" {
		return objectstore.DownloadCapability{}, domain.NewError(domain.ErrorUnavailable, "memory data plane is not configured")
	}
	token, err := b.ids.BearerToken()
	if err != nil {
		return objectstore.DownloadCapability{}, err
	}
	hash := sha256.Sum256([]byte(token))
	b.downloads[hash] = downloadSession{
		key: request.Key, version: request.Version, filename: request.Filename, mediaType: request.MediaType,
		disposition: request.Disposition, expiresAt: request.ExpiresAt.UTC(),
	}
	return objectstore.DownloadCapability{URL: b.dataPlaneURL + "/cap/download/" + token, Method: http.MethodGet, Headers: map[string]string{}, ExpiresAt: request.ExpiresAt.UTC()}, nil
}

func (b *Backend) UploadOffset(ctx context.Context, uploadID string) (int64, error) {
	progress, err := b.UploadProgress(ctx, uploadID)
	return progress.Offset, err
}

func (b *Backend) SimulateUploadOffset(ctx context.Context, uploadID string, offset int64) error {
	if err := objectstore.ContextError(ctx); err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	session, found := b.uploads[uploadID]
	if !found || session.protocol != domain.UploadResumable || offset < session.offset || offset > session.size {
		return domain.NewError(domain.ErrorInvalid, "invalid simulated upload offset")
	}
	session.offset = offset
	session.body = nil
	session.materialized = false
	if offset == session.size {
		b.versions++
		session.version = objectstore.VersionString("m", b.versions)
		b.records[session.key.String()] = record{version: session.version, size: session.size, materialized: false}
	}
	return nil
}

func (b *Backend) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("Referrer-Policy", "no-referrer")
	writer.Header().Set("X-Content-Type-Options", "nosniff")
	writer.Header().Set("Content-Security-Policy", "default-src 'none'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; sandbox")
	parts := strings.Split(strings.TrimPrefix(request.URL.Path, "/"), "/")
	if len(parts) != 3 || parts[0] != "cap" || parts[2] == "" {
		http.NotFound(writer, request)
		return
	}
	switch parts[1] {
	case "upload":
		b.serveUpload(writer, request, parts[2])
	case "download":
		b.serveDownload(writer, request, parts[2])
	default:
		http.NotFound(writer, request)
	}
}

func (b *Backend) serveUpload(writer http.ResponseWriter, request *http.Request, token string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	hash := sha256.Sum256([]byte(token))
	uploadID, found := b.uploadTokens[hash]
	session := b.uploads[uploadID]
	if !found || session == nil || session.aborted {
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
	if !b.clock.Now().Before(session.expiresAt) {
		http.Error(writer, "capability unavailable", http.StatusGone)
		return
	}
	mediaType, err := domain.NormalizeMediaType(request.Header.Get("Content-Type"))
	if err != nil || mediaType != session.mediaType {
		http.Error(writer, "upload constraint mismatch", http.StatusPreconditionFailed)
		return
	}
	if session.protocol == domain.UploadResumable {
		offset, parseErr := strconv.ParseInt(request.Header.Get("Upload-Offset"), 10, 64)
		if parseErr != nil || offset != session.offset {
			writer.Header().Set("Upload-Offset", strconv.FormatInt(session.offset, 10))
			http.Error(writer, "upload offset mismatch", http.StatusConflict)
			return
		}
	}
	limit := session.size - session.offset
	if session.protocol == domain.UploadResumable {
		limit = min(limit, int64(8<<20))
	}
	body, err := io.ReadAll(io.LimitReader(request.Body, limit+1))
	b.uploadBytes += int64(len(body))
	if err != nil || int64(len(body)) > limit {
		http.Error(writer, "upload body is invalid", http.StatusRequestEntityTooLarge)
		return
	}
	if session.protocol == domain.UploadSingle && int64(len(body)) != session.size {
		http.Error(writer, "single upload size mismatch", http.StatusPreconditionFailed)
		return
	}
	accepted := len(body)
	if b.consumeTransferFault(TransferUploadData, TransferFaultInterrupted) && accepted > 0 {
		accepted = max(1, accepted/2)
	}
	acceptedBody := body[:accepted]
	_, _ = session.hasher.Write(acceptedBody)
	if session.materialized && session.offset+int64(accepted) <= 16<<20 {
		session.body = append(session.body, acceptedBody...)
	} else {
		session.body = nil
		session.materialized = false
	}
	session.offset += int64(accepted)
	writer.Header().Set("Upload-Offset", strconv.FormatInt(session.offset, 10))
	if session.offset == session.size {
		b.versions++
		session.version = objectstore.VersionString("m", b.versions)
		b.records[session.key.String()] = record{body: append([]byte(nil), session.body...), version: session.version, size: session.size, materialized: session.materialized}
	}
	if accepted != len(body) {
		http.Error(writer, "upload interrupted", http.StatusServiceUnavailable)
		return
	}
	writer.WriteHeader(http.StatusNoContent)
}

func (b *Backend) serveDownload(writer http.ResponseWriter, request *http.Request, token string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	hash := sha256.Sum256([]byte(token))
	session, found := b.downloads[hash]
	if !found {
		http.NotFound(writer, request)
		return
	}
	if request.Method != http.MethodGet {
		writer.Header().Set("Allow", http.MethodGet)
		http.Error(writer, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !b.clock.Now().Before(session.expiresAt) {
		http.Error(writer, "capability unavailable", http.StatusGone)
		return
	}
	record, exists := b.records[session.key.String()]
	if !exists || record.version != session.version {
		http.NotFound(writer, request)
		return
	}
	start, end, partial, err := parseRange(request.Header.Get("Range"), record.size)
	if err != nil {
		writer.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", record.size))
		writer.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
		return
	}
	length := end - start + 1
	writer.Header().Set("Accept-Ranges", "bytes")
	writer.Header().Set("Content-Type", session.mediaType)
	writer.Header().Set("Content-Disposition", fmt.Sprintf(`%s; filename=%q`, session.disposition, session.filename))
	writer.Header().Set("Content-Length", strconv.FormatInt(length, 10))
	if partial {
		writer.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, record.size))
		writer.WriteHeader(http.StatusPartialContent)
	}
	var body []byte
	if record.materialized {
		body = record.body[start : end+1]
	} else {
		body = make([]byte, length)
	}
	_, _ = writer.Write(body)
	b.downloadBytes += length
}

func (b *Backend) consumeTransferFault(operation, fault string) bool {
	faults := b.transferFaults[operation]
	if len(faults) == 0 || faults[0] != fault {
		return false
	}
	b.transferFaults[operation] = faults[1:]
	return true
}

func parseRange(header string, size int64) (int64, int64, bool, error) {
	if size == 0 {
		if header == "" {
			return 0, -1, false, nil
		}
		return 0, 0, false, domain.ErrInvalid
	}
	if header == "" {
		return 0, size - 1, false, nil
	}
	if !strings.HasPrefix(header, "bytes=") || strings.Contains(header, ",") {
		return 0, 0, false, domain.ErrInvalid
	}
	parts := strings.Split(strings.TrimPrefix(header, "bytes="), "-")
	if len(parts) != 2 || parts[0] == "" {
		return 0, 0, false, domain.ErrInvalid
	}
	start, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || start < 0 || start >= size {
		return 0, 0, false, domain.ErrInvalid
	}
	end := size - 1
	if parts[1] != "" {
		end, err = strconv.ParseInt(parts[1], 10, 64)
		if err != nil || end < start {
			return 0, 0, false, domain.ErrInvalid
		}
		end = min(end, size-1)
	}
	return start, end, true, nil
}
