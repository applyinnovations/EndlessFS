// Package memory implements deterministic provider semantics for local v1 proof.
package memory

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"hash"
	"math"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/applyinnovations/endlessfs/internal/domain"
)

const (
	OperationList            = "list"
	OperationLookupChildren  = "lookup_children"
	OperationStat            = "stat"
	OperationCreateDirectory = "create_directory"
	OperationCreateUpload    = "create_upload"
	OperationCompleteUpload  = "complete_upload"
	OperationAbortUpload     = "abort_upload"
	OperationCreateDownload  = "create_download"
	OperationUploadData      = "upload_data"
	OperationDownloadData    = "download_data"
	OperationCopy            = "copy"
	OperationMove            = "move"
	OperationDelete          = "delete"
)

type Fault string

const (
	FaultExpired           Fault = "expired"
	FaultNotFound          Fault = "not_found"
	FaultConflict          Fault = "conflict"
	FaultPartialOperation  Fault = "partial_operation"
	FaultRateLimited       Fault = "rate_limited"
	FaultUnavailable       Fault = "unavailable"
	FaultChecksumMismatch  Fault = "checksum_mismatch"
	FaultInterruptedUpload Fault = "interrupted_upload"
	FaultStaleVersion      Fault = "stale_version"
)

type Options struct {
	Clock                domain.Clock
	IDs                  *domain.IDGenerator
	UploadTTL            time.Duration
	DownloadTTL          time.Duration
	MaxMaterializedBytes int64
	ChunkRules           domain.ChunkRules
	AllowedOrigin        string
}

type Instrumentation struct {
	ProviderCalls     map[string]uint64
	ControlPlaneBytes int64
	UploadBytes       int64
	DownloadBytes     int64
}

type object struct {
	entry        domain.Entry
	data         []byte
	materialized bool
}

type upload struct {
	id              domain.UploadID
	scope           domain.Scope
	requestedPath   domain.UserPath
	path            domain.UserPath
	size            int64
	mediaType       string
	conflict        domain.ConflictMode
	expectedVersion domain.Version
	targetExisted   bool
	protocol        domain.UploadProtocol
	expiresAt       time.Time
	offset          int64
	data            []byte
	materialized    bool
	hasher          hash.Hash
	aborted         bool
	capabilityHash  [sha256.Size]byte
}

type download struct {
	scope       domain.Scope
	path        domain.UserPath
	version     domain.Version
	disposition domain.Disposition
	expiresAt   time.Time
}

type listSnapshot struct {
	scope      domain.Scope
	directory  domain.UserPath
	pageSize   int
	sort       domain.SortField
	descending bool
	entries    []domain.Entry
	current    domain.Entry
	index      int
}

type idempotentResult struct {
	fingerprint string
	operation   domain.Operation
}

type idempotentUpload struct {
	fingerprint string
	capability  domain.UploadCapability
}

type Provider struct {
	mu sync.Mutex

	clock                domain.Clock
	ids                  *domain.IDGenerator
	uploadTTL            time.Duration
	downloadTTL          time.Duration
	maxMaterializedBytes int64
	chunkRules           domain.ChunkRules
	baseURL              string
	allowedOrigin        string

	objects           map[domain.Scope]map[string]object
	uploads           map[domain.UploadID]*upload
	uploadTokens      map[[sha256.Size]byte]domain.UploadID
	downloads         map[[sha256.Size]byte]download
	listSnapshots     map[string]*listSnapshot
	operations        map[string]domain.Operation
	idempotency       map[string]idempotentResult
	uploadIdempotency map[string]idempotentUpload
	faults            map[string][]Fault
	metrics           Instrumentation
	versions          uint64
}

func New(options Options) *Provider {
	if options.Clock == nil {
		options.Clock = domain.SystemClock{}
	}
	if options.IDs == nil {
		options.IDs = domain.SystemIDGenerator()
	}
	if options.UploadTTL == 0 {
		options.UploadTTL = 5 * time.Minute
	}
	if options.DownloadTTL == 0 {
		options.DownloadTTL = time.Minute
	}
	if options.MaxMaterializedBytes == 0 {
		options.MaxMaterializedBytes = 16 << 20
	}
	if options.ChunkRules.MaximumSize == 0 {
		options.ChunkRules = domain.ChunkRules{MinimumSize: 1, MaximumSize: 8 << 20, Multiple: 1}
	}
	return &Provider{
		clock:                options.Clock,
		ids:                  options.IDs,
		uploadTTL:            options.UploadTTL,
		downloadTTL:          options.DownloadTTL,
		maxMaterializedBytes: options.MaxMaterializedBytes,
		chunkRules:           options.ChunkRules,
		allowedOrigin:        options.AllowedOrigin,
		objects:              make(map[domain.Scope]map[string]object),
		uploads:              make(map[domain.UploadID]*upload),
		uploadTokens:         make(map[[sha256.Size]byte]domain.UploadID),
		downloads:            make(map[[sha256.Size]byte]download),
		listSnapshots:        make(map[string]*listSnapshot),
		operations:           make(map[string]domain.Operation),
		idempotency:          make(map[string]idempotentResult),
		uploadIdempotency:    make(map[string]idempotentUpload),
		faults:               make(map[string][]Fault),
		metrics:              Instrumentation{ProviderCalls: make(map[string]uint64)},
	}
}

func (p *Provider) SetDataPlaneBaseURL(baseURL string) error {
	parsed, err := url.Parse(baseURL)
	ip := net.ParseIP(parsed.Hostname())
	if err != nil || parsed.Scheme != "http" || parsed.Port() == "" || ip == nil || !ip.IsLoopback() || parsed.User != nil || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return domain.NewError(domain.ErrorInvalid, "mock data plane must use a loopback URL")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.baseURL = strings.TrimRight(baseURL, "/")
	return nil
}

func (p *Provider) InjectFault(operation string, fault Fault) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.faults[operation] = append(p.faults[operation], fault)
}

func (p *Provider) Instrumentation() Instrumentation {
	p.mu.Lock()
	defer p.mu.Unlock()
	calls := make(map[string]uint64, len(p.metrics.ProviderCalls))
	for operation, count := range p.metrics.ProviderCalls {
		calls[operation] = count
	}
	return Instrumentation{
		ProviderCalls:     calls,
		ControlPlaneBytes: p.metrics.ControlPlaneBytes,
		UploadBytes:       p.metrics.UploadBytes,
		DownloadBytes:     p.metrics.DownloadBytes,
	}
}

func (p *Provider) RecordControlPlaneBytes(count int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.metrics.ControlPlaneBytes += count
}

func (p *Provider) List(ctx context.Context, scope domain.Scope, request domain.ListRequest) (domain.ListPage, error) {
	if err := validateContextScope(ctx, scope); err != nil {
		return domain.ListPage{}, err
	}
	if !request.Directory.Valid() {
		return domain.ListPage{}, domain.NewError(domain.ErrorInvalid, "directory path is required")
	}
	pageSize, err := normalizePageSize(request.PageSize)
	if err != nil {
		return domain.ListPage{}, err
	}
	if request.Sort == "" {
		request.Sort = domain.SortName
	}
	if request.Sort != domain.SortName && request.Sort != domain.SortModified && request.Sort != domain.SortSize && request.Sort != domain.SortKind {
		return domain.ListPage{}, domain.NewError(domain.ErrorInvalid, "invalid list sort")
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.beforeLocked(OperationList); err != nil {
		return domain.ListPage{}, err
	}
	if request.Cursor != "" {
		snapshot, found := p.listSnapshots[request.Cursor]
		if !found || snapshot.scope != scope || snapshot.directory != request.Directory || snapshot.pageSize != pageSize || snapshot.sort != request.Sort || snapshot.descending != request.Descending {
			return domain.ListPage{}, domain.NewError(domain.ErrorInvalid, "invalid or out-of-scope list cursor")
		}
		return p.listPageLocked(request.Cursor, snapshot), nil
	}
	current, err := p.statLocked(scope, request.Directory)
	if err != nil || current.Kind != domain.EntryDirectory {
		if err != nil {
			return domain.ListPage{}, err
		}
		return domain.ListPage{}, domain.NewError(domain.ErrorNotFound, "directory not found")
	}
	entries := make([]domain.Entry, 0)
	for _, item := range p.scopeObjectsLocked(scope) {
		if item.entry.Path.Parent() == request.Directory {
			entries = append(entries, item.entry)
		}
	}
	sortEntries(entries, request.Sort, request.Descending)
	if len(entries) <= pageSize {
		return domain.ListPage{Current: current, Entries: entries}, nil
	}
	cursor, err := p.ids.OpaqueID()
	if err != nil {
		return domain.ListPage{}, err
	}
	snapshot := &listSnapshot{
		scope: scope, directory: request.Directory, pageSize: pageSize,
		sort: request.Sort, descending: request.Descending, entries: entries, current: current,
	}
	p.listSnapshots[cursor] = snapshot
	return p.listPageLocked(cursor, snapshot), nil
}

func (p *Provider) listPageLocked(cursor string, snapshot *listSnapshot) domain.ListPage {
	end := min(snapshot.index+snapshot.pageSize, len(snapshot.entries))
	entries := append([]domain.Entry(nil), snapshot.entries[snapshot.index:end]...)
	snapshot.index = end
	if end == len(snapshot.entries) {
		delete(p.listSnapshots, cursor)
		cursor = ""
	}
	return domain.ListPage{Current: snapshot.current, Entries: entries, NextCursor: cursor}
}

func (p *Provider) LookupChildren(ctx context.Context, scope domain.Scope, request domain.ChildLookupRequest) (domain.ChildLookup, error) {
	if err := validateContextScope(ctx, scope); err != nil {
		return domain.ChildLookup{}, err
	}
	if !request.Directory.Valid() || len(request.Names) < 1 || len(request.Names) > 1000 {
		return domain.ChildLookup{}, domain.NewError(domain.ErrorInvalid, "child lookup request is invalid")
	}
	paths := make([]domain.UserPath, 0, len(request.Names))
	seen := make(map[string]struct{}, len(request.Names))
	for _, name := range request.Names {
		path, err := request.Directory.Join(name)
		if err != nil {
			return domain.ChildLookup{}, domain.NewError(domain.ErrorInvalid, "child lookup name is invalid")
		}
		if _, duplicate := seen[name]; duplicate {
			return domain.ChildLookup{}, domain.NewError(domain.ErrorInvalid, "child lookup contains duplicate names")
		}
		seen[name] = struct{}{}
		paths = append(paths, path)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.beforeLocked(OperationLookupChildren); err != nil {
		return domain.ChildLookup{}, err
	}
	current, err := p.statLocked(scope, request.Directory)
	if err != nil {
		return domain.ChildLookup{}, err
	}
	if current.Kind != domain.EntryDirectory {
		return domain.ChildLookup{}, domain.NewError(domain.ErrorNotFound, "directory not found")
	}
	result := domain.ChildLookup{Current: current, Entries: make([]domain.Entry, 0, len(paths))}
	objects := p.scopeObjectsLocked(scope)
	for _, path := range paths {
		item, found := objects[path.String()]
		if !found || item.entry.Path.Parent() != request.Directory {
			return domain.ChildLookup{}, domain.NewError(domain.ErrorNotFound, "entry not found")
		}
		result.Entries = append(result.Entries, item.entry)
	}
	return result, nil
}

func (p *Provider) Stat(ctx context.Context, scope domain.Scope, path domain.UserPath) (domain.Entry, error) {
	if err := validateContextScope(ctx, scope); err != nil {
		return domain.Entry{}, err
	}
	if !path.Valid() {
		return domain.Entry{}, domain.NewError(domain.ErrorInvalid, "path is required")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.beforeLocked(OperationStat); err != nil {
		return domain.Entry{}, err
	}
	return p.statLocked(scope, path)
}

func (p *Provider) statLocked(scope domain.Scope, path domain.UserPath) (domain.Entry, error) {
	if path.IsRoot() {
		recursiveBytes, err := p.rootRecursiveBytesLocked(scope)
		if err != nil {
			return domain.Entry{}, err
		}
		fileCount := p.rootRecursiveFileCountLocked(scope)
		return domain.Entry{
			Path: path, Kind: domain.EntryDirectory, Size: recursiveBytes, FileCount: fileCount, ModifiedAt: time.Unix(0, 0).UTC(), Version: "root",
		}, nil
	}
	item, found := p.scopeObjectsLocked(scope)[path.String()]
	if !found {
		return domain.Entry{}, domain.NewError(domain.ErrorNotFound, "entry not found")
	}
	return item.entry, nil
}

func (p *Provider) CreateDirectory(ctx context.Context, scope domain.Scope, request domain.CreateDirectoryRequest) (domain.Entry, error) {
	if err := validateContextScope(ctx, scope); err != nil {
		return domain.Entry{}, err
	}
	if !request.Path.Valid() || request.Path.IsRoot() {
		return domain.Entry{}, domain.NewError(domain.ErrorInvalid, "directory path is invalid")
	}
	conflict, err := domain.NormalizeConflictMode(request.Conflict)
	if err != nil {
		return domain.Entry{}, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.beforeLocked(OperationCreateDirectory); err != nil {
		return domain.Entry{}, err
	}
	if err := p.requireParentLocked(scope, request.Path); err != nil {
		return domain.Entry{}, err
	}
	path, err := p.resolveDestinationLocked(scope, request.Path, conflict, request.ExpectedVersion)
	if err != nil {
		return domain.Entry{}, err
	}
	original := cloneObjects(p.scopeObjectsLocked(scope))
	entry := p.newEntryLocked(path, domain.EntryDirectory, 0, "")
	p.scopeObjectsLocked(scope)[path.String()] = object{entry: entry, materialized: true}
	if err := p.recomputeRecursiveBytesLocked(scope); err != nil {
		p.objects[scope] = original
		return domain.Entry{}, err
	}
	return entry, nil
}

func (p *Provider) scopeObjectsLocked(scope domain.Scope) map[string]object {
	objects, found := p.objects[scope]
	if !found {
		objects = make(map[string]object)
		p.objects[scope] = objects
	}
	return objects
}

func (p *Provider) requireParentLocked(scope domain.Scope, path domain.UserPath) error {
	parent := path.Parent()
	if parent.IsRoot() {
		return nil
	}
	item, found := p.scopeObjectsLocked(scope)[parent.String()]
	if !found || item.entry.Kind != domain.EntryDirectory {
		return domain.NewError(domain.ErrorNotFound, "parent directory not found")
	}
	return nil
}

func (p *Provider) resolveDestinationLocked(scope domain.Scope, path domain.UserPath, conflict domain.ConflictMode, expected domain.Version) (domain.UserPath, error) {
	item, exists := p.scopeObjectsLocked(scope)[path.String()]
	if !exists {
		return path, nil
	}
	switch conflict {
	case domain.ConflictFail:
		return domain.UserPath{}, domain.NewError(domain.ErrorConflict, "destination already exists")
	case domain.ConflictReplace:
		if expected == "" || expected != item.entry.Version {
			return domain.UserPath{}, domain.NewError(domain.ErrorPreconditionFailed, "destination version does not match")
		}
		p.deleteTreeLocked(scope, path)
		return path, nil
	case domain.ConflictRename:
		return p.availableRenamedPathLocked(scope, path)
	default:
		return domain.UserPath{}, domain.NewError(domain.ErrorInvalid, "invalid conflict mode")
	}
}

func (p *Provider) availableRenamedPathLocked(scope domain.Scope, path domain.UserPath) (domain.UserPath, error) {
	name := path.Name()
	extensionIndex := strings.LastIndexByte(name, '.')
	base, extension := name, ""
	if extensionIndex > 0 {
		base, extension = name[:extensionIndex], name[extensionIndex:]
	}
	for index := 1; index <= 10_000; index++ {
		suffix := fmt.Sprintf(" (%d)", index)
		candidateBase := base
		for len(candidateBase)+len(suffix)+len(extension) > 255 && candidateBase != "" {
			_, size := utf8.DecodeLastRuneInString(candidateBase)
			candidateBase = candidateBase[:len(candidateBase)-size]
		}
		candidate, err := path.Parent().Join(candidateBase + suffix + extension)
		for err != nil && candidateBase != "" {
			_, size := utf8.DecodeLastRuneInString(candidateBase)
			candidateBase = candidateBase[:len(candidateBase)-size]
			candidate, err = path.Parent().Join(candidateBase + suffix + extension)
		}
		if err != nil {
			return domain.UserPath{}, err
		}
		if _, exists := p.scopeObjectsLocked(scope)[candidate.String()]; !exists {
			return candidate, nil
		}
	}
	return domain.UserPath{}, domain.NewError(domain.ErrorConflict, "unable to generate a conflict-free name")
}

func (p *Provider) newEntryLocked(path domain.UserPath, kind domain.EntryKind, size int64, mediaType string) domain.Entry {
	p.versions++
	fileCount := int64(0)
	if kind == domain.EntryFile {
		fileCount = 1
	}
	return domain.Entry{
		Path: path, Name: path.Name(), Kind: kind, Size: size, FileCount: fileCount, MediaType: mediaType,
		ModifiedAt: p.clock.Now().UTC(), Version: domain.Version(fmt.Sprintf("p%016x", p.versions)),
	}
}

func (p *Provider) newFileEntryLocked(path domain.UserPath, size int64, mediaType string) (domain.Entry, error) {
	value, err := p.ids.OpaqueID()
	if err != nil {
		return domain.Entry{}, err
	}
	contentID := domain.ContentID(value)
	contentVersion, err := p.ids.OpaqueID()
	if err != nil {
		return domain.Entry{}, err
	}
	entry := p.newEntryLocked(path, domain.EntryFile, size, mediaType)
	entry.ContentID = contentID
	entry.ContentVersion = domain.ContentVersion(contentVersion)
	entry.ContentModifiedAt = p.clock.Now().UTC()
	return entry, nil
}

func (p *Provider) deleteTreeLocked(scope domain.Scope, path domain.UserPath) {
	objects := p.scopeObjectsLocked(scope)
	for candidate := range objects {
		candidatePath, err := domain.ParseUserPath(candidate)
		if err == nil && (candidatePath == path || candidatePath.IsDescendantOf(path)) {
			delete(objects, candidate)
		}
	}
}

func (p *Provider) rootRecursiveBytesLocked(scope domain.Scope) (int64, error) {
	var total int64
	for _, item := range p.scopeObjectsLocked(scope) {
		if item.entry.Kind != domain.EntryFile {
			continue
		}
		if item.entry.Size < 0 || item.entry.Size > math.MaxInt64-total {
			return 0, domain.NewError(domain.ErrorInvalid, "directory recursive byte aggregate overflows")
		}
		total += item.entry.Size
	}
	return total, nil
}

func (p *Provider) rootRecursiveFileCountLocked(scope domain.Scope) int64 {
	var total int64
	for _, item := range p.scopeObjectsLocked(scope) {
		if item.entry.Kind == domain.EntryFile {
			total++
		}
	}
	return total
}

func (p *Provider) recomputeRecursiveBytesLocked(scope domain.Scope) error {
	if _, err := p.rootRecursiveBytesLocked(scope); err != nil {
		return err
	}
	objects := p.scopeObjectsLocked(scope)
	totals := make(map[string]int64)
	counts := make(map[string]int64)
	for _, item := range objects {
		if item.entry.Kind != domain.EntryFile {
			continue
		}
		parent := item.entry.Path.Parent()
		for !parent.IsRoot() {
			current := totals[parent.String()]
			if item.entry.Size < 0 || item.entry.Size > math.MaxInt64-current {
				return domain.NewError(domain.ErrorInvalid, "directory recursive byte aggregate overflows")
			}
			totals[parent.String()] = current + item.entry.Size
			counts[parent.String()]++
			parent = parent.Parent()
		}
	}
	for path, item := range objects {
		if item.entry.Kind != domain.EntryDirectory || item.entry.Size == totals[path] && item.entry.FileCount == counts[path] {
			continue
		}
		item.entry.Size = totals[path]
		item.entry.FileCount = counts[path]
		p.versions++
		item.entry.Version = domain.Version(fmt.Sprintf("p%016x", p.versions))
		objects[path] = item
	}
	return nil
}

func cloneObjects(objects map[string]object) map[string]object {
	cloned := make(map[string]object, len(objects))
	for path, item := range objects {
		cloned[path] = item
	}
	return cloned
}

func (p *Provider) beforeLocked(operation string) error {
	p.metrics.ProviderCalls[operation]++
	faults := p.faults[operation]
	if len(faults) == 0 {
		return nil
	}
	fault := faults[0]
	p.faults[operation] = faults[1:]
	switch fault {
	case FaultNotFound:
		return domain.NewError(domain.ErrorNotFound, "injected not found")
	case FaultConflict:
		return domain.NewError(domain.ErrorConflict, "injected conflict")
	case FaultRateLimited:
		return domain.NewError(domain.ErrorRateLimited, "injected rate limit")
	case FaultUnavailable:
		return domain.NewError(domain.ErrorUnavailable, "injected unavailability")
	case FaultStaleVersion:
		return domain.NewError(domain.ErrorPreconditionFailed, "injected stale version")
	case FaultExpired, FaultChecksumMismatch, FaultInterruptedUpload, FaultPartialOperation:
		p.faults[operation] = append([]Fault{fault}, p.faults[operation]...)
		return nil
	default:
		return domain.NewError(domain.ErrorInternal, "unknown injected fault")
	}
}

func (p *Provider) consumeSpecificFaultLocked(operation string, wanted Fault) bool {
	faults := p.faults[operation]
	if len(faults) == 0 || faults[0] != wanted {
		return false
	}
	p.faults[operation] = faults[1:]
	return true
}

func validateContextScope(ctx context.Context, scope domain.Scope) error {
	if err := ctx.Err(); err != nil {
		return domain.WrapError(domain.ErrorUnavailable, "provider request canceled", err)
	}
	if !scope.Valid() {
		return domain.NewError(domain.ErrorUnauthorized, "invalid storage scope")
	}
	return nil
}

func normalizePageSize(size int) (int, error) {
	if size == 0 {
		return 200, nil
	}
	if size < 1 || size > 1000 {
		return 0, domain.NewError(domain.ErrorInvalid, "page size must be between 1 and 1000")
	}
	return size, nil
}

func sortEntries(entries []domain.Entry, field domain.SortField, descending bool) {
	sort.Slice(entries, func(left, right int) bool {
		a, b := entries[left], entries[right]
		comparison := 0
		switch field {
		case domain.SortModified:
			comparison = a.ModifiedAt.Compare(b.ModifiedAt)
		case domain.SortSize:
			comparison = compare(a.Size, b.Size)
		case domain.SortKind:
			comparison = strings.Compare(string(a.Kind), string(b.Kind))
		default:
			comparison = strings.Compare(a.Name, b.Name)
		}
		if comparison == 0 {
			comparison = strings.Compare(a.Path.String(), b.Path.String())
		}
		if descending {
			return comparison > 0
		}
		return comparison < 0
	})
}

func compare[T ~int64](left, right T) int {
	switch {
	case left < right:
		return -1
	case left > right:
		return 1
	default:
		return 0
	}
}

func tokenHash(token string) [sha256.Size]byte {
	return sha256.Sum256([]byte(token))
}

func operationKey(userID domain.UserID, operationID domain.OperationID) string {
	return userID.String() + "\x00" + string(operationID)
}

func idempotencyKey(userID domain.UserID, operation, key string) string {
	return userID.String() + "\x00" + operation + "\x00" + key
}

func operationFingerprint(values ...string) string {
	hash := sha256.Sum256([]byte(strings.Join(values, "\x00")))
	return base64.RawURLEncoding.EncodeToString(hash[:])
}

func parseOffset(value string) (int64, error) {
	offset, err := strconv.ParseInt(value, 10, 64)
	if err != nil || offset < 0 {
		return 0, errors.New("invalid upload offset")
	}
	return offset, nil
}

var _ http.Handler = (*Provider)(nil)
