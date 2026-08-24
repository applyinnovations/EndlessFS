package portable

import (
	"sort"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
)

const (
	uploadRecordSchema  = "upload-record-v1"
	transferLeaseSchema = "transfer-lease-v1"
)

func domainUploadCapability(uploadID string, capability objectstore.UploadCapability) domain.UploadCapability {
	return domain.UploadCapability{
		UploadID: domain.UploadID(uploadID), Protocol: capability.Protocol, URL: capability.URL,
		Method: capability.Method, Headers: copyHeaders(capability.Headers), ExpiresAt: capability.ExpiresAt, ChunkRules: capability.ChunkRules,
		Framing: capability.Framing, DeclaredSize: capability.DeclaredSize,
	}
}

func (s *FileStore) transferBackend() (objectstore.DirectTransferBackend, error) {
	transfers, ok := s.engine.fileBackend.(objectstore.DirectTransferBackend)
	if !ok {
		return nil, domain.NewError(domain.ErrorPreconditionFailed, "object backend has no direct transfer support")
	}
	return transfers, nil
}

func copyHeaders(headers map[string]string) map[string]string {
	result := make(map[string]string, len(headers))
	keys := make([]string, 0, len(headers))
	for key := range headers {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		result[key] = headers[key]
	}
	return result
}
