package portable

import (
	"bufio"
	"container/heap"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

const (
	checkpointReachabilityChunkEntries = 16 * 1024
	checkpointReachabilityMergeWidth   = 64
)

// checkpointReachabilityCollector is a bounded-memory external sorter. A
// checkpoint may name millions of immutable pages and blobs, while namespace
// copy-on-write can encounter the same physical object through many logical
// paths. Local ephemeral sorting prevents either case from making process
// memory proportional to bucket size and costs no provider requests.
type checkpointReachabilityCollector struct {
	directory string
	buffer    []string
	chunks    []string
	sequence  uint64
}

func newCheckpointReachabilityCollector() (*checkpointReachabilityCollector, error) {
	directory, err := os.MkdirTemp("", "endlessfs-checkpoint-reachability-")
	if err != nil {
		return nil, domain.WrapError(domain.ErrorUnavailable, "create checkpoint reachability workspace", err)
	}
	return &checkpointReachabilityCollector{directory: directory, buffer: make([]string, 0, checkpointReachabilityChunkEntries)}, nil
}

func (collector *checkpointReachabilityCollector) Close() error {
	if collector == nil || collector.directory == "" {
		return nil
	}
	err := os.RemoveAll(collector.directory)
	collector.directory = ""
	return err
}

func (collector *checkpointReachabilityCollector) Add(key objectstore.Key) error {
	if collector == nil || !key.Valid() {
		return domain.NewError(domain.ErrorInvalid, "invalid checkpoint reachability key")
	}
	collector.buffer = append(collector.buffer, key.String())
	if len(collector.buffer) == cap(collector.buffer) {
		return collector.flush()
	}
	return nil
}

func (collector *checkpointReachabilityCollector) flush() error {
	if len(collector.buffer) == 0 {
		return nil
	}
	sort.Strings(collector.buffer)
	path := filepath.Join(collector.directory, checkpointReachabilityChunkName(collector.sequence))
	collector.sequence++
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return domain.WrapError(domain.ErrorUnavailable, "create checkpoint reachability chunk", err)
	}
	writer := bufio.NewWriter(file)
	previous := ""
	for _, key := range collector.buffer {
		if key == previous {
			continue
		}
		if err := writeCheckpointReachabilityKey(writer, key); err != nil {
			_ = file.Close()
			return err
		}
		previous = key
	}
	if err := writer.Flush(); err != nil {
		_ = file.Close()
		return domain.WrapError(domain.ErrorUnavailable, "flush checkpoint reachability chunk", err)
	}
	if err := file.Close(); err != nil {
		return domain.WrapError(domain.ErrorUnavailable, "close checkpoint reachability chunk", err)
	}
	collector.chunks = append(collector.chunks, path)
	collector.buffer = collector.buffer[:0]
	return nil
}

func checkpointReachabilityChunkName(sequence uint64) string {
	var body [8]byte
	binary.BigEndian.PutUint64(body[:], sequence)
	const alphabet = "0123456789abcdef"
	name := make([]byte, 16)
	for index, value := range body {
		name[index*2], name[index*2+1] = alphabet[value>>4], alphabet[value&15]
	}
	return string(name) + ".keys"
}

func writeCheckpointReachabilityKey(writer io.Writer, key string) error {
	if len(key) == 0 || len(key) > objectstore.MaxKeyBytes {
		return domain.NewError(domain.ErrorInvalid, "invalid checkpoint reachability key length")
	}
	var size [2]byte
	binary.BigEndian.PutUint16(size[:], uint16(len(key)))
	if _, err := writer.Write(size[:]); err != nil {
		return domain.WrapError(domain.ErrorUnavailable, "write checkpoint reachability key length", err)
	}
	if _, err := io.WriteString(writer, key); err != nil {
		return domain.WrapError(domain.ErrorUnavailable, "write checkpoint reachability key", err)
	}
	return nil
}

type checkpointReachabilityReader struct {
	file    *os.File
	reader  *bufio.Reader
	current string
}

func openCheckpointReachabilityReader(path string) (*checkpointReachabilityReader, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, domain.WrapError(domain.ErrorUnavailable, "open checkpoint reachability chunk", err)
	}
	return &checkpointReachabilityReader{file: file, reader: bufio.NewReader(file)}, nil
}

func (reader *checkpointReachabilityReader) advance() (bool, error) {
	var size [2]byte
	if _, err := io.ReadFull(reader.reader, size[:]); errors.Is(err, io.EOF) {
		reader.current = ""
		return false, nil
	} else if err != nil {
		return false, domain.WrapError(domain.ErrorUnavailable, "read checkpoint reachability key length", err)
	}
	length := int(binary.BigEndian.Uint16(size[:]))
	if length == 0 || length > objectstore.MaxKeyBytes {
		return false, domain.NewError(domain.ErrorInvalid, "invalid checkpoint reachability chunk")
	}
	value := make([]byte, length)
	if _, err := io.ReadFull(reader.reader, value); err != nil {
		return false, domain.WrapError(domain.ErrorUnavailable, "read checkpoint reachability key", err)
	}
	if _, err := objectstore.ParseKey(string(value)); err != nil {
		return false, err
	}
	reader.current = string(value)
	return true, nil
}

func (reader *checkpointReachabilityReader) close() error { return reader.file.Close() }

type checkpointReachabilityHeap []*checkpointReachabilityReader

func (values checkpointReachabilityHeap) Len() int { return len(values) }
func (values checkpointReachabilityHeap) Less(i, j int) bool {
	return values[i].current < values[j].current
}
func (values checkpointReachabilityHeap) Swap(i, j int) { values[i], values[j] = values[j], values[i] }
func (values *checkpointReachabilityHeap) Push(value any) {
	*values = append(*values, value.(*checkpointReachabilityReader))
}
func (values *checkpointReachabilityHeap) Pop() any {
	old := *values
	last := old[len(old)-1]
	*values = old[:len(old)-1]
	return last
}

func (collector *checkpointReachabilityCollector) merge(paths []string, output string) error {
	writers, err := os.OpenFile(output, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return domain.WrapError(domain.ErrorUnavailable, "create merged checkpoint reachability chunk", err)
	}
	writer := bufio.NewWriter(writers)
	readers := make([]*checkpointReachabilityReader, 0, len(paths))
	queue := checkpointReachabilityHeap{}
	closeAll := func() {
		for _, reader := range readers {
			_ = reader.close()
		}
	}
	for _, path := range paths {
		reader, openErr := openCheckpointReachabilityReader(path)
		if openErr != nil {
			closeAll()
			_ = writers.Close()
			return openErr
		}
		readers = append(readers, reader)
		if found, advanceErr := reader.advance(); advanceErr != nil {
			closeAll()
			_ = writers.Close()
			return advanceErr
		} else if found {
			heap.Push(&queue, reader)
		}
	}
	previous := ""
	for queue.Len() > 0 {
		reader := heap.Pop(&queue).(*checkpointReachabilityReader)
		if reader.current != previous {
			if err := writeCheckpointReachabilityKey(writer, reader.current); err != nil {
				closeAll()
				_ = writers.Close()
				return err
			}
			previous = reader.current
		}
		if found, advanceErr := reader.advance(); advanceErr != nil {
			closeAll()
			_ = writers.Close()
			return advanceErr
		} else if found {
			heap.Push(&queue, reader)
		}
	}
	closeAll()
	if err := writer.Flush(); err != nil {
		_ = writers.Close()
		return domain.WrapError(domain.ErrorUnavailable, "flush merged checkpoint reachability chunk", err)
	}
	if err := writers.Close(); err != nil {
		return domain.WrapError(domain.ErrorUnavailable, "close merged checkpoint reachability chunk", err)
	}
	return nil
}

func (collector *checkpointReachabilityCollector) consolidate() error {
	if err := collector.flush(); err != nil {
		return err
	}
	for len(collector.chunks) > checkpointReachabilityMergeWidth {
		merged := make([]string, 0, (len(collector.chunks)+checkpointReachabilityMergeWidth-1)/checkpointReachabilityMergeWidth)
		for start := 0; start < len(collector.chunks); start += checkpointReachabilityMergeWidth {
			end := min(start+checkpointReachabilityMergeWidth, len(collector.chunks))
			path := filepath.Join(collector.directory, "merged-"+checkpointReachabilityChunkName(collector.sequence))
			collector.sequence++
			if err := collector.merge(collector.chunks[start:end], path); err != nil {
				return err
			}
			for _, old := range collector.chunks[start:end] {
				if err := os.Remove(old); err != nil {
					return domain.WrapError(domain.ErrorUnavailable, "remove checkpoint reachability chunk", err)
				}
			}
			merged = append(merged, path)
		}
		collector.chunks = merged
	}
	return nil
}

type checkpointReachabilityStream struct {
	collector *checkpointReachabilityCollector
	readers   []*checkpointReachabilityReader
	queue     checkpointReachabilityHeap
	previous  string
}

func (collector *checkpointReachabilityCollector) Stream() (*checkpointReachabilityStream, error) {
	if err := collector.consolidate(); err != nil {
		return nil, err
	}
	stream := &checkpointReachabilityStream{collector: collector}
	for _, path := range collector.chunks {
		reader, err := openCheckpointReachabilityReader(path)
		if err != nil {
			_ = stream.Close()
			return nil, err
		}
		stream.readers = append(stream.readers, reader)
		if found, err := reader.advance(); err != nil {
			_ = stream.Close()
			return nil, err
		} else if found {
			heap.Push(&stream.queue, reader)
		}
	}
	return stream, nil
}

func (stream *checkpointReachabilityStream) Next() (objectstore.Key, bool, error) {
	for stream.queue.Len() > 0 {
		reader := heap.Pop(&stream.queue).(*checkpointReachabilityReader)
		value := reader.current
		if found, err := reader.advance(); err != nil {
			return objectstore.Key{}, false, err
		} else if found {
			heap.Push(&stream.queue, reader)
		}
		if value == stream.previous {
			continue
		}
		stream.previous = value
		key, err := objectstore.ParseKey(value)
		return key, err == nil, err
	}
	return objectstore.Key{}, false, nil
}

func (stream *checkpointReachabilityStream) Close() error {
	for _, reader := range stream.readers {
		_ = reader.close()
	}
	stream.readers = nil
	if stream.collector != nil {
		return stream.collector.Close()
	}
	return nil
}

type checkpointReachabilityWalker struct {
	engine    *Engine
	collector *checkpointReachabilityCollector
	visited   *checkpointVisitSet
}

func (e *Engine) collectSchema008CheckpointReachability(ctx context.Context) (*checkpointReachabilityStream, error) {
	collector, err := newCheckpointReachabilityCollector()
	if err != nil {
		return nil, err
	}
	visited, err := newCheckpointVisitSet()
	if err != nil {
		_ = collector.Close()
		return nil, err
	}
	fail := func(err error) (*checkpointReachabilityStream, error) {
		_ = collector.Close()
		_ = visited.Close()
		return nil, err
	}
	walker := &checkpointReachabilityWalker{engine: e, collector: collector, visited: visited}
	for _, key := range []objectstore.Key{storageformat.SuperblockKey(), storageformat.WriterSetKey(), storageformat.WriteGateKey(), storageformat.DomainCatalogHeadKey()} {
		if err := collector.Add(key); err != nil {
			return fail(err)
		}
	}
	catalogSnapshot, found, err := e.readDomainCatalogIfPresent(ctx)
	if err != nil {
		return fail(err)
	}
	if !found {
		return fail(domain.NewError(domain.ErrorPreconditionFailed, "schema-008 checkpoint has no domain catalog"))
	}
	catalogSession := newDomainCatalogTreeSession(e.stateDomainStore())
	if err := walker.walkTree(ctx, catalogSession, catalogSnapshot.head.Root, "catalog", nil); err != nil {
		return fail(err)
	}
	catalog := newDomainCatalog(e.backend, e.scheduler)
	if err := catalog.visitEntries(ctx, catalogSnapshot.head, func(entry storageformat.DomainCatalogEntry) error {
		reference := consistencyDomainRef{Kind: entry.Kind, ID: entry.DomainID}
		if err := collector.Add(storageformat.DomainHeadKey(reference.Kind, reference.ID)); err != nil {
			return err
		}
		snapshot, err := e.stateDomainStore().loadHead(ctx, reference)
		if err != nil {
			return err
		}
		session := newConsistencyDomainTreeSession(e.stateDomainStore(), reference)
		if err := walker.walkTree(ctx, session, snapshot.head.Base, "base", nil); err != nil {
			return err
		}
		if err := walker.walkTree(ctx, session, snapshot.head.Outcomes, "outcomes", func(value storageformat.DomainEntry) error {
			var outcome storageformat.DomainOutcome
			if err := decodeCanonicalValue(value.Value, &outcome); err != nil {
				return err
			}
			return walker.walkNamespaceMutationResult(ctx, session, outcome.Result)
		}); err != nil {
			return err
		}
		if err := walker.walkTree(ctx, session, snapshot.head.OutcomeExpiry, "outcome-expiry", nil); err != nil {
			return err
		}
		if reference.Kind == storageformat.DomainNamespace {
			owner, err := domain.ParseUserID(reference.ID)
			if err != nil {
				return err
			}
			for _, area := range []domain.Area{domain.AreaLive, domain.AreaTrash} {
				value, found, err := e.stateDomainStore().lookupAtHead(ctx, reference, snapshot.head, namespaceRootKey(area))
				if err != nil {
					return err
				}
				if !found {
					continue
				}
				root, err := decodeNamespaceEntry(value.Data)
				if err != nil {
					return err
				}
				if err := walker.walkNamespaceEntry(ctx, session, owner, root); err != nil {
					return err
				}
			}
			for _, delta := range snapshot.head.Deltas {
				if err := walker.walkNamespaceMutationResult(ctx, session, delta.Result); err != nil {
					return err
				}
			}
		}
		return nil
	}); err != nil {
		return fail(err)
	}
	if err := visited.Close(); err != nil {
		return fail(err)
	}
	stream, err := collector.Stream()
	if err != nil {
		return fail(err)
	}
	return stream, nil
}

func (walker *checkpointReachabilityWalker) walkTree(ctx context.Context, session *consistencyDomainTreeSession, root storageformat.DomainTreeRoot, purpose string, visit func(storageformat.DomainEntry) error) error {
	if root.Digest == "" {
		return nil
	}
	return walker.walkTreePage(ctx, session, domainPageRef{root: root}, purpose, visit)
}

func (walker *checkpointReachabilityWalker) walkTreePage(ctx context.Context, session *consistencyDomainTreeSession, reference domainPageRef, purpose string, visit func(storageformat.DomainEntry) error) error {
	key := session.pageKey(reference.root.Digest)
	seenKey := purpose + "\x00" + key.String()
	seen, err := walker.visited.Seen(seenKey)
	if err != nil {
		return err
	}
	if seen {
		return nil
	}
	page, err := session.readPage(ctx, reference)
	if err != nil {
		return err
	}
	if err := walker.collector.Add(key); err != nil {
		return err
	}
	if page.Level == 0 {
		if visit == nil {
			return nil
		}
		for _, entry := range page.Entries {
			if err := visit(entry); err != nil {
				return err
			}
		}
		return nil
	}
	for _, child := range page.Children {
		childReference := domainPageRef{root: storageformat.DomainTreeRoot{Digest: child.Digest, Level: child.Level, EntryCount: child.EntryCount, ByteCount: child.ByteCount}, firstKey: child.FirstKey, lastKey: child.LastKey}
		if err := walker.walkTreePage(ctx, session, childReference, purpose, visit); err != nil {
			return err
		}
	}
	return nil
}

func (walker *checkpointReachabilityWalker) walkNamespaceEntry(ctx context.Context, session *consistencyDomainTreeSession, owner domain.UserID, directory storageformat.NamespaceEntry) error {
	if directory.Entry.Kind != domain.EntryDirectory {
		return domain.NewError(domain.ErrorInvalid, "checkpoint namespace root is not a directory")
	}
	return walker.walkTree(ctx, session, directory.Children, "namespace", func(value storageformat.DomainEntry) error {
		entry, err := decodeNamespaceEntry(value.Value)
		if err != nil {
			return err
		}
		if entry.Entry.Kind == domain.EntryDirectory {
			return walker.walkNamespaceEntry(ctx, session, owner, entry)
		}
		return walker.collector.Add(storageformat.BlobKey(owner.String(), entry.Entry.BlobID))
	})
}

func (walker *checkpointReachabilityWalker) walkNamespaceMutationResult(ctx context.Context, session *consistencyDomainTreeSession, body []byte) error {
	var result storageformat.NamespaceMutationResult
	if err := decodeCanonicalValue(body, &result); err != nil {
		return err
	}
	if result.Batch == nil {
		return nil
	}
	return walker.walkTree(ctx, session, result.Batch.Items, "namespace-batch", nil)
}

type schema008CheckpointMetadataStream struct {
	reachable *checkpointReachabilityStream
	metadata  *checkpointMetadataStream
	key       objectstore.Key
	found     bool
}

func newSchema008CheckpointMetadataStream(ctx context.Context, engine *Engine) (*schema008CheckpointMetadataStream, error) {
	reachable, err := engine.collectSchema008CheckpointReachability(ctx)
	if err != nil {
		return nil, err
	}
	return &schema008CheckpointMetadataStream{reachable: reachable, metadata: newCheckpointMetadataStream(engine, true)}, nil
}

func (stream *schema008CheckpointMetadataStream) Close() error { return stream.reachable.Close() }

func (stream *schema008CheckpointMetadataStream) next(ctx context.Context) (objectstore.ObjectInfo, bool, bool, error) {
	if !stream.found {
		key, found, err := stream.reachable.Next()
		if err != nil || !found {
			return objectstore.ObjectInfo{}, false, false, err
		}
		stream.key, stream.found = key, true
	}
	for {
		info, fileData, found, err := stream.metadata.next(ctx)
		if err != nil {
			return objectstore.ObjectInfo{}, false, false, err
		}
		if !found {
			return objectstore.ObjectInfo{}, false, false, domain.NewError(domain.ErrorPreconditionFailed, "checkpoint authority object is missing")
		}
		switch {
		case info.Key.String() < stream.key.String():
			// Content-addressed objects not named by a current authority root are
			// harmless collectable garbage and never enter a portable checkpoint.
			continue
		case info.Key != stream.key:
			return objectstore.ObjectInfo{}, false, false, domain.NewError(domain.ErrorPreconditionFailed, "checkpoint authority object is missing")
		default:
			stream.found = false
			return info, fileData, true, nil
		}
	}
}
