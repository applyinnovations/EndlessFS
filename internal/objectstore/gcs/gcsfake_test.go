package gcs_test

import (
	"crypto/md5"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
)

type fakeObject struct {
	body           []byte
	logicalSize    int64
	generation     int64
	metageneration int64
	cacheControl   string
}

type fakeResumableSession struct {
	name      string
	size      int64
	mediaType string
	body      []byte
}

type fakeGCS struct {
	t                         *testing.T
	mu                        sync.Mutex
	objects                   map[string]fakeObject
	nextGeneration            int64
	nextStatus                int
	failUploadAfterCommit     bool
	failUploadAfterCommitName string
	uploadRequests            int
	corruptNextDownloadCRC    bool
	wrongNextMetadataSizeBy   int
	baseURL                   string
	sessions                  map[string]*fakeResumableSession
	completedSessions         map[string]struct{}
	nextSession               int64
	sessionDeleteAttempts     int
	sessionDeleteProtocol     string
	sessionDeleteStatus       int
	sessionStatusAttempts     int
	rejectCompletedDelete     bool
	clock                     domain.Clock
	uploadBytes               int64
	downloadBytes             int64
	allowedOrigin             string
	signedGetCacheControl     string
	unavailable               bool
}

func newGCSServer(t *testing.T) *httptest.Server {
	server, _ := newGCSServerWithFake(t)
	return server
}

func newGCSServerWithFake(t *testing.T) (*httptest.Server, *fakeGCS) {
	t.Helper()
	fake := &fakeGCS{
		t: t, objects: make(map[string]fakeObject), sessions: make(map[string]*fakeResumableSession),
		completedSessions: make(map[string]struct{}), nextGeneration: 100, clock: domain.SystemClock{},
	}
	server := httptest.NewServer(fake)
	fake.baseURL = server.URL
	t.Cleanup(server.Close)
	return server, fake
}

func newGCSHTTP2ServerWithFake(t *testing.T) (*httptest.Server, *fakeGCS) {
	t.Helper()
	fake := &fakeGCS{
		t: t, objects: make(map[string]fakeObject), sessions: make(map[string]*fakeResumableSession),
		completedSessions: make(map[string]struct{}), nextGeneration: 100, clock: domain.SystemClock{},
	}
	server := httptest.NewUnstartedServer(fake)
	server.EnableHTTP2 = true
	server.StartTLS()
	fake.baseURL = server.URL
	t.Cleanup(server.Close)
	return server, fake
}

func (f *fakeGCS) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	f.mu.Lock()
	unavailable := f.unavailable
	f.mu.Unlock()
	if unavailable {
		f.problem(writer, http.StatusForbidden, "accessDenied")
		return
	}
	if origin := request.Header.Get("Origin"); origin != "" {
		f.mu.Lock()
		allowedOrigin := f.allowedOrigin
		f.mu.Unlock()
		if origin != allowedOrigin {
			f.problem(writer, http.StatusForbidden, "corsOriginDenied")
			return
		}
		writer.Header().Set("Access-Control-Allow-Origin", allowedOrigin)
		writer.Header().Set("Access-Control-Expose-Headers", "Content-Range, Range, Upload-Offset, X-Goog-Generation")
		writer.Header().Set("Vary", "Origin")
		if request.Method == http.MethodOptions {
			if request.Header.Get("Access-Control-Request-Method") != http.MethodPut {
				f.problem(writer, http.StatusForbidden, "corsMethodDenied")
				return
			}
			writer.Header().Set("Access-Control-Allow-Methods", "PUT")
			writer.Header().Set("Access-Control-Allow-Headers", "Content-Type, Content-Range")
			writer.WriteHeader(http.StatusNoContent)
			return
		}
	}
	f.mu.Lock()
	status := f.nextStatus
	f.nextStatus = 0
	f.mu.Unlock()
	if status != 0 {
		f.problem(writer, status, "injected")
		return
	}
	if request.URL.Path == "/storage/v1/b/endlessfs-test/o" && request.Method == http.MethodGet {
		f.list(writer, request)
		return
	}
	if request.URL.Path == "/upload/storage/v1/b/endlessfs-test/o" && request.Method == http.MethodPost {
		f.upload(writer, request)
		return
	}
	if strings.HasPrefix(request.URL.Path, "/resumable/") {
		f.resumable(writer, request, strings.TrimPrefix(request.URL.Path, "/resumable/"))
		return
	}
	if strings.HasPrefix(request.URL.Path, "/endlessfs-test/") {
		name, err := url.PathUnescape(strings.TrimPrefix(request.URL.EscapedPath(), "/endlessfs-test/"))
		if err != nil {
			f.problem(writer, http.StatusForbidden, "signatureDoesNotMatch")
			return
		}
		if status := f.v4Status(request.URL.Query()); status != 0 {
			f.problem(writer, status, "signatureDoesNotMatch")
			return
		}
		switch request.Method {
		case http.MethodPost:
			f.startResumable(writer, request, name)
		case http.MethodPut:
			f.signedPut(writer, request, name)
		case http.MethodGet:
			f.signedGet(writer, request, name)
		default:
			f.problem(writer, http.StatusMethodNotAllowed, "methodNotAllowed")
		}
		return
	}
	prefix := "/storage/v1/b/endlessfs-test/o/"
	if strings.HasPrefix(request.URL.Path, prefix) {
		tail := strings.TrimPrefix(request.URL.Path, prefix)
		if request.Method == http.MethodPost && strings.Contains(tail, "/rewriteTo/b/") {
			f.rewrite(writer, request, tail)
			return
		}
		name, err := url.PathUnescape(tail)
		if err != nil {
			f.problem(writer, http.StatusBadRequest, "invalid")
			return
		}
		switch request.Method {
		case http.MethodGet:
			f.get(writer, request, name)
		case http.MethodDelete:
			f.delete(writer, request, name)
		default:
			f.problem(writer, http.StatusNotFound, "notFound")
		}
		return
	}
	f.problem(writer, http.StatusNotFound, "notFound")
}

func (f *fakeGCS) v4Status(query url.Values) int {
	if !(query.Get("X-Goog-Algorithm") == "GOOG4-RSA-SHA256" &&
		query.Get("X-Goog-Credential") != "" && query.Get("X-Goog-Date") != "" &&
		query.Get("X-Goog-Expires") != "" && query.Get("X-Goog-SignedHeaders") != "" &&
		query.Get("X-Goog-Signature") != "") {
		return http.StatusForbidden
	}
	issued, err := time.Parse("20060102T150405Z", query.Get("X-Goog-Date"))
	seconds, secondsErr := strconv.ParseInt(query.Get("X-Goog-Expires"), 10, 64)
	if err != nil || secondsErr != nil || seconds < 1 {
		return http.StatusForbidden
	}
	if !f.clock.Now().Before(issued.Add(time.Duration(seconds) * time.Second)) {
		return http.StatusGone
	}
	return 0
}

func (f *fakeGCS) startResumable(writer http.ResponseWriter, request *http.Request, name string) {
	if request.Header.Get("x-goog-resumable") != "start" || request.URL.Query().Get("ifGenerationMatch") != "0" {
		f.problem(writer, http.StatusBadRequest, "invalid")
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, exists := f.objects[name]; exists {
		f.problem(writer, http.StatusPreconditionFailed, "conditionNotMet")
		return
	}
	f.nextSession++
	id := strconv.FormatInt(f.nextSession, 10)
	f.sessions[id] = &fakeResumableSession{name: name, size: -1, mediaType: request.Header.Get("Content-Type")}
	writer.Header().Set("Location", f.baseURL+"/resumable/"+id)
	writer.WriteHeader(http.StatusCreated)
}

func (f *fakeGCS) signedPut(writer http.ResponseWriter, request *http.Request, name string) {
	if request.URL.Query().Get("ifGenerationMatch") != "0" {
		f.problem(writer, http.StatusBadRequest, "invalid")
		return
	}
	body, err := io.ReadAll(request.Body)
	if err != nil {
		f.problem(writer, http.StatusBadRequest, "invalid")
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.uploadBytes += int64(len(body))
	if _, exists := f.objects[name]; exists {
		f.problem(writer, http.StatusPreconditionFailed, "conditionNotMet")
		return
	}
	f.nextGeneration++
	object := fakeObject{body: body, generation: f.nextGeneration, metageneration: 1}
	f.objects[name] = object
	writer.Header().Set("x-goog-generation", strconv.FormatInt(object.generation, 10))
	writer.WriteHeader(http.StatusOK)
}

func (f *fakeGCS) signedGet(writer http.ResponseWriter, request *http.Request, name string) {
	f.mu.Lock()
	object, exists := f.objects[name]
	f.mu.Unlock()
	if !exists || request.URL.Query().Get("generation") != strconv.FormatInt(object.generation, 10) || request.URL.Query().Get("ifGenerationMatch") != strconv.FormatInt(object.generation, 10) {
		f.problem(writer, http.StatusPreconditionFailed, "conditionNotMet")
		return
	}
	writer.Header().Set("Content-Type", request.URL.Query().Get("response-content-type"))
	writer.Header().Set("Content-Disposition", request.URL.Query().Get("response-content-disposition"))
	cacheControl := object.cacheControl
	if f.signedGetCacheControl != "" {
		cacheControl = f.signedGetCacheControl
	}
	writer.Header().Set("Cache-Control", cacheControl)
	size := fakeObjectSize(object)
	if rangeHeader := request.Header.Get("Range"); rangeHeader != "" {
		start, end, ok := parseFakeRange(rangeHeader, size)
		if !ok {
			writer.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", size))
			writer.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}
		writer.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, size))
		writer.WriteHeader(http.StatusPartialContent)
		length := end - start + 1
		f.mu.Lock()
		f.downloadBytes += length
		f.mu.Unlock()
		if object.logicalSize > 0 {
			_, _ = writer.Write(make([]byte, length))
		} else {
			_, _ = writer.Write(object.body[start : end+1])
		}
		return
	}
	writer.WriteHeader(http.StatusOK)
	f.mu.Lock()
	f.downloadBytes += size
	f.mu.Unlock()
	_, _ = writer.Write(object.body)
}

func (f *fakeGCS) resumable(writer http.ResponseWriter, request *http.Request, id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if request.Method == http.MethodDelete {
		f.sessionDeleteAttempts++
		f.sessionDeleteProtocol = request.Proto
		if request.Header.Get("Content-Length") != "0" {
			f.problem(writer, http.StatusLengthRequired, "lengthRequired")
			return
		}
		if f.sessionDeleteStatus != 0 {
			f.problem(writer, f.sessionDeleteStatus, "injected")
			return
		}
	}
	if request.Method == http.MethodPut && strings.HasPrefix(request.Header.Get("Content-Range"), "bytes */") {
		f.sessionStatusAttempts++
	}
	session, exists := f.sessions[id]
	if !exists {
		if _, completed := f.completedSessions[id]; completed && request.Method == http.MethodDelete && f.rejectCompletedDelete {
			f.problem(writer, http.StatusMethodNotAllowed, "methodNotAllowed")
			return
		}
		f.problem(writer, http.StatusNotFound, "notFound")
		return
	}
	if request.Method == http.MethodDelete {
		delete(f.sessions, id)
		writer.WriteHeader(http.StatusNoContent)
		return
	}
	if request.Method != http.MethodPut {
		f.problem(writer, http.StatusMethodNotAllowed, "methodNotAllowed")
		return
	}
	contentRange := request.Header.Get("Content-Range")
	if strings.HasPrefix(contentRange, "bytes */") {
		total, err := strconv.ParseInt(strings.TrimPrefix(contentRange, "bytes */"), 10, 64)
		if err != nil || total < 0 {
			f.problem(writer, http.StatusBadRequest, "invalid")
			return
		}
		session.size = total
		if total == 0 {
			f.nextGeneration++
			f.objects[session.name] = fakeObject{body: []byte{}, generation: f.nextGeneration, metageneration: 1}
			f.completedSessions[id] = struct{}{}
			delete(f.sessions, id)
			writer.WriteHeader(http.StatusOK)
			return
		}
		if len(session.body) > 0 {
			writer.Header().Set("Range", fmt.Sprintf("bytes=0-%d", len(session.body)-1))
		}
		writer.WriteHeader(http.StatusPermanentRedirect)
		return
	}
	var start, end, total int64
	if _, err := fmt.Sscanf(contentRange, "bytes %d-%d/%d", &start, &end, &total); err != nil || start != int64(len(session.body)) || end < start || total < end+1 {
		f.problem(writer, http.StatusBadRequest, "invalid")
		return
	}
	body, err := io.ReadAll(request.Body)
	if err != nil || int64(len(body)) != end-start+1 {
		f.problem(writer, http.StatusBadRequest, "invalid")
		return
	}
	session.size = total
	session.body = append(session.body, body...)
	f.uploadBytes += int64(len(body))
	if int64(len(session.body)) < total {
		writer.Header().Set("Range", fmt.Sprintf("bytes=0-%d", len(session.body)-1))
		writer.WriteHeader(http.StatusPermanentRedirect)
		return
	}
	if int64(len(session.body)) != total {
		f.problem(writer, http.StatusBadRequest, "invalid")
		return
	}
	f.nextGeneration++
	f.objects[session.name] = fakeObject{body: append([]byte(nil), session.body...), generation: f.nextGeneration, metageneration: 1}
	f.completedSessions[id] = struct{}{}
	delete(f.sessions, id)
	writer.WriteHeader(http.StatusOK)
}

func (f *fakeGCS) upload(writer http.ResponseWriter, request *http.Request) {
	f.mu.Lock()
	f.uploadRequests++
	f.mu.Unlock()
	if request.URL.Query().Get("uploadType") != "multipart" {
		f.problem(writer, http.StatusBadRequest, "invalid")
		return
	}
	name := request.URL.Query().Get("name")
	mediaType, parameters, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
	if err != nil || mediaType != "multipart/related" || parameters["boundary"] == "" {
		f.problem(writer, http.StatusBadRequest, "invalid")
		return
	}
	reader := multipart.NewReader(request.Body, parameters["boundary"])
	metadataPart, err := reader.NextPart()
	if err != nil {
		f.problem(writer, http.StatusBadRequest, "invalid")
		return
	}
	var metadata struct {
		Name         string `json:"name"`
		CRC32C       string `json:"crc32c"`
		CacheControl string `json:"cacheControl"`
	}
	if err := json.NewDecoder(metadataPart).Decode(&metadata); err != nil {
		f.problem(writer, http.StatusBadRequest, "invalid")
		return
	}
	bodyPart, err := reader.NextPart()
	if err != nil {
		f.problem(writer, http.StatusBadRequest, "invalid")
		return
	}
	body, err := io.ReadAll(bodyPart)
	if err != nil {
		f.problem(writer, http.StatusBadRequest, "invalid")
		return
	}
	if metadata.Name != "" && metadata.Name != name || metadata.CRC32C != "" && metadata.CRC32C != crc32c(body) {
		f.problem(writer, http.StatusBadRequest, "invalid")
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	current, exists := f.objects[name]
	if !generationMatch(request.URL.Query().Get("ifGenerationMatch"), current.generation, exists) {
		f.problem(writer, http.StatusPreconditionFailed, "conditionNotMet")
		return
	}
	f.nextGeneration++
	created := fakeObject{body: append([]byte(nil), body...), generation: f.nextGeneration, metageneration: 1, cacheControl: metadata.CacheControl}
	f.objects[name] = created
	if f.failUploadAfterCommit || f.failUploadAfterCommitName == name {
		f.failUploadAfterCommit = false
		f.failUploadAfterCommitName = ""
		f.problem(writer, http.StatusServiceUnavailable, "backendError")
		return
	}
	f.writeObject(writer, name, created)
}

func (f *fakeGCS) get(writer http.ResponseWriter, request *http.Request, name string) {
	f.mu.Lock()
	object, exists := f.objects[name]
	f.mu.Unlock()
	if !exists {
		f.problem(writer, http.StatusNotFound, "notFound")
		return
	}
	if generation := request.URL.Query().Get("generation"); generation != "" && generation != strconv.FormatInt(object.generation, 10) {
		f.problem(writer, http.StatusNotFound, "notFound")
		return
	}
	if match := request.URL.Query().Get("ifGenerationMatch"); match != "" && match != strconv.FormatInt(object.generation, 10) {
		f.problem(writer, http.StatusPreconditionFailed, "conditionNotMet")
		return
	}
	if request.URL.Query().Get("alt") == "media" {
		f.mu.Lock()
		corruptCRC := f.corruptNextDownloadCRC
		f.corruptNextDownloadCRC = false
		f.mu.Unlock()
		writer.Header().Set("Content-Type", "application/octet-stream")
		writer.Header().Set("Content-Length", strconv.Itoa(len(object.body)))
		writer.Header().Set("X-Goog-Generation", strconv.FormatInt(object.generation, 10))
		writer.Header().Set("X-Goog-Metageneration", strconv.FormatInt(object.metageneration, 10))
		checksum := crc32c(object.body)
		if corruptCRC {
			checksum = "AAAAAA=="
		}
		writer.Header().Set("X-Goog-Hash", "crc32c="+checksum)
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write(object.body)
		return
	}
	f.mu.Lock()
	sizeDelta := f.wrongNextMetadataSizeBy
	f.wrongNextMetadataSizeBy = 0
	f.mu.Unlock()
	if sizeDelta != 0 {
		value := objectJSON(name, object)
		value["size"] = strconv.Itoa(len(object.body) + sizeDelta)
		f.writeJSON(writer, http.StatusOK, value)
		return
	}
	f.writeObject(writer, name, object)
}

func (f *fakeGCS) delete(writer http.ResponseWriter, request *http.Request, name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	object, exists := f.objects[name]
	if !exists {
		f.problem(writer, http.StatusNotFound, "notFound")
		return
	}
	if !generationMatch(request.URL.Query().Get("ifGenerationMatch"), object.generation, true) {
		f.problem(writer, http.StatusPreconditionFailed, "conditionNotMet")
		return
	}
	delete(f.objects, name)
	writer.WriteHeader(http.StatusNoContent)
}

func (f *fakeGCS) list(writer http.ResponseWriter, request *http.Request) {
	prefix := request.URL.Query().Get("prefix")
	limit := 1000
	if raw := request.URL.Query().Get("maxResults"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 {
			f.problem(writer, http.StatusBadRequest, "invalid")
			return
		}
		limit = parsed
	}
	startAfter := ""
	if startOffset := request.URL.Query().Get("startOffset"); startOffset != "" {
		startAfter = strings.TrimSuffix(startOffset, "\x00")
	}
	if token := request.URL.Query().Get("pageToken"); token != "" {
		decoded, err := base64.RawURLEncoding.DecodeString(token)
		if err != nil {
			f.problem(writer, http.StatusBadRequest, "invalidPageToken")
			return
		}
		startAfter = string(decoded)
	}
	f.mu.Lock()
	keys := make([]string, 0, len(f.objects))
	for key := range f.objects {
		if strings.HasPrefix(key, prefix) && key > startAfter {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	if len(keys) > limit {
		keys = keys[:limit]
	}
	items := make([]map[string]any, 0, len(keys))
	for _, key := range keys {
		items = append(items, objectJSON(key, f.objects[key]))
	}
	more := false
	if len(keys) > 0 {
		last := keys[len(keys)-1]
		for key := range f.objects {
			if strings.HasPrefix(key, prefix) && key > last {
				more = true
				break
			}
		}
	}
	f.mu.Unlock()
	response := map[string]any{"kind": "storage#objects", "items": items}
	if more {
		response["nextPageToken"] = base64.RawURLEncoding.EncodeToString([]byte(keys[len(keys)-1]))
	}
	f.writeJSON(writer, http.StatusOK, response)
}

func (f *fakeGCS) rewrite(writer http.ResponseWriter, request *http.Request, tail string) {
	parts := strings.SplitN(tail, "/rewriteTo/b/endlessfs-test/o/", 2)
	if len(parts) != 2 {
		f.problem(writer, http.StatusBadRequest, "invalid")
		return
	}
	source, sourceErr := url.PathUnescape(parts[0])
	destination, destinationErr := url.PathUnescape(parts[1])
	if sourceErr != nil || destinationErr != nil {
		f.problem(writer, http.StatusBadRequest, "invalid")
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	sourceObject, sourceExists := f.objects[source]
	if !sourceExists {
		f.problem(writer, http.StatusNotFound, "notFound")
		return
	}
	if !generationMatch(request.URL.Query().Get("ifSourceGenerationMatch"), sourceObject.generation, true) {
		f.problem(writer, http.StatusPreconditionFailed, "conditionNotMet")
		return
	}
	destinationObject, destinationExists := f.objects[destination]
	if !generationMatch(request.URL.Query().Get("ifGenerationMatch"), destinationObject.generation, destinationExists) {
		f.problem(writer, http.StatusPreconditionFailed, "conditionNotMet")
		return
	}
	f.nextGeneration++
	created := fakeObject{body: append([]byte(nil), sourceObject.body...), logicalSize: sourceObject.logicalSize, generation: f.nextGeneration, metageneration: 1}
	f.objects[destination] = created
	f.writeJSON(writer, http.StatusOK, map[string]any{
		"kind": "storage#rewriteResponse", "totalBytesRewritten": strconv.FormatInt(fakeObjectSize(created), 10),
		"objectSize": strconv.FormatInt(fakeObjectSize(created), 10), "done": true, "resource": objectJSON(destination, created),
	})
}

func generationMatch(raw string, generation int64, exists bool) bool {
	if raw == "" {
		return true
	}
	if raw == "0" {
		return !exists
	}
	return exists && raw == strconv.FormatInt(generation, 10)
}

func objectJSON(name string, object fakeObject) map[string]any {
	return map[string]any{
		"kind": "storage#object", "bucket": "endlessfs-test", "name": name,
		"size": strconv.FormatInt(fakeObjectSize(object), 10), "generation": strconv.FormatInt(object.generation, 10),
		"metageneration": strconv.FormatInt(object.metageneration, 10), "crc32c": crc32c(object.body), "md5Hash": md5Hash(object.body),
		"contentType": "application/octet-stream",
	}
}

func md5Hash(body []byte) string {
	digest := md5.Sum(body)
	return base64.StdEncoding.EncodeToString(digest[:])
}

func fakeObjectSize(object fakeObject) int64 {
	if object.logicalSize > 0 {
		return object.logicalSize
	}
	return int64(len(object.body))
}

func parseFakeRange(value string, size int64) (int64, int64, bool) {
	if !strings.HasPrefix(value, "bytes=") || strings.Contains(value, ",") {
		return 0, 0, false
	}
	parts := strings.Split(strings.TrimPrefix(value, "bytes="), "-")
	if len(parts) != 2 || parts[0] == "" {
		return 0, 0, false
	}
	start, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || start < 0 || start >= size {
		return 0, 0, false
	}
	end := size - 1
	if parts[1] != "" {
		end, err = strconv.ParseInt(parts[1], 10, 64)
		if err != nil || end < start {
			return 0, 0, false
		}
		end = min(end, size-1)
	}
	return start, end, true
}

func crc32c(body []byte) string {
	value := crc32.Checksum(body, crc32.MakeTable(crc32.Castagnoli))
	encoded := make([]byte, 4)
	binary.BigEndian.PutUint32(encoded, value)
	return base64.StdEncoding.EncodeToString(encoded)
}

func (f *fakeGCS) writeObject(writer http.ResponseWriter, name string, object fakeObject) {
	f.writeJSON(writer, http.StatusOK, objectJSON(name, object))
}

func (f *fakeGCS) problem(writer http.ResponseWriter, status int, reason string) {
	f.writeJSON(writer, status, map[string]any{"error": map[string]any{
		"code": status, "message": http.StatusText(status),
		"errors": []map[string]string{{"domain": "global", "reason": reason, "message": http.StatusText(status)}},
	}})
}

func (f *fakeGCS) writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	writer.WriteHeader(status)
	if err := json.NewEncoder(writer).Encode(value); err != nil {
		f.t.Errorf("encode fake GCS response: %v", err)
	}
}
