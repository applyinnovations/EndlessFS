package portable

import (
	"bufio"
	"container/heap"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"os"
	"sort"
	"strings"

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
	root      *os.Root
	buffer    []string
	chunks    []string
	sequence  uint64
}

func newCheckpointReachabilityCollector() (*checkpointReachabilityCollector, error) {
	directory, err := os.MkdirTemp("", "endlessfs-checkpoint-reachability-")
	if err != nil {
		return nil, domain.WrapError(domain.ErrorUnavailable, "create checkpoint reachability workspace", err)
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		_ = os.RemoveAll(directory)
		return nil, domain.WrapError(domain.ErrorUnavailable, "open checkpoint reachability workspace", err)
	}
	return &checkpointReachabilityCollector{directory: directory, root: root, buffer: make([]string, 0, checkpointReachabilityChunkEntries)}, nil
}

func (collector *checkpointReachabilityCollector) Close() error {
	if collector == nil || collector.directory == "" {
		return nil
	}
	rootErr := collector.root.Close()
	removeErr := os.RemoveAll(collector.directory)
	collector.root = nil
	collector.directory = ""
	return errors.Join(rootErr, removeErr)
}

func (collector *checkpointReachabilityCollector) Add(key objectstore.Key) error {
	if collector == nil || collector.root == nil || collector.directory == "" || !key.Valid() {
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
	name := checkpointReachabilityChunkName(collector.sequence)
	collector.sequence++
	file, err := collector.root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
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
	collector.chunks = append(collector.chunks, name)
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
	if len(key) == 0 || len(key) > objectstore.MaxKeyBytes || len(key) > math.MaxUint16 {
		return domain.NewError(domain.ErrorInvalid, "invalid checkpoint reachability key length")
	}
	var size [2]byte
	encodedSize := uint16(len(key)) // #nosec G115 -- the preceding MaxUint16 bound proves this conversion lossless.
	binary.BigEndian.PutUint16(size[:], encodedSize)
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

func openCheckpointReachabilityReader(root *os.Root, name string) (*checkpointReachabilityReader, error) {
	if root == nil {
		return nil, domain.NewError(domain.ErrorInvalid, "checkpoint reachability workspace is closed")
	}
	file, err := root.Open(name)
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

func (collector *checkpointReachabilityCollector) merge(names []string, output string) error {
	writers, err := collector.root.OpenFile(output, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return domain.WrapError(domain.ErrorUnavailable, "create merged checkpoint reachability chunk", err)
	}
	writer := bufio.NewWriter(writers)
	readers := make([]*checkpointReachabilityReader, 0, len(names))
	queue := checkpointReachabilityHeap{}
	closeAll := func() {
		for _, reader := range readers {
			_ = reader.close()
		}
	}
	for _, name := range names {
		reader, openErr := openCheckpointReachabilityReader(collector.root, name)
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
			name := "merged-" + checkpointReachabilityChunkName(collector.sequence)
			collector.sequence++
			if err := collector.merge(collector.chunks[start:end], name); err != nil {
				return err
			}
			for _, old := range collector.chunks[start:end] {
				if err := collector.root.Remove(old); err != nil {
					return domain.WrapError(domain.ErrorUnavailable, "remove checkpoint reachability chunk", err)
				}
			}
			merged = append(merged, name)
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
	for _, name := range collector.chunks {
		reader, err := openCheckpointReachabilityReader(collector.root, name)
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
	return e.collectConsistencyDomainCheckpointReachability(ctx, false)
}

func (e *Engine) collectConsistencyDomainCheckpointReachability(ctx context.Context, schema009 bool) (*checkpointReachabilityStream, error) {
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
		var outcomeVisitor func(storageformat.DomainEntry) error
		if reference.Kind == storageformat.DomainNamespace {
			outcomeVisitor = func(value storageformat.DomainEntry) error {
				var outcome storageformat.DomainOutcome
				if err := decodeCanonicalValue(value.Value, &outcome); err != nil {
					return err
				}
				if schema009 && !isRecognizedNamespaceMutationResult(outcome.Result) {
					return nil
				}
				return walker.walkNamespaceMutationResult(ctx, session, outcome.Result)
			}
		}
		if err := walker.walkTree(ctx, session, snapshot.head.Outcomes, "outcomes", outcomeVisitor); err != nil {
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
				if schema009 && schema009NamespaceDeltaHasOpaqueResult(delta) {
					continue
				}
				if err := walker.walkNamespaceMutationResult(ctx, session, delta.Result); err != nil {
					return err
				}
			}
		}
		return nil
	}); err != nil {
		return fail(err)
	}
	if err := e.collectTransitionReachability009(ctx, collector); err != nil {
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

func (e *Engine) collectTransitionReachability009(ctx context.Context, collector *checkpointReachabilityCollector) error {
	if err := e.visitTransitionPlans009(ctx, func(_ storageformat.TransitionPlan009, object objectstore.Object) error {
		return collector.Add(object.Key)
	}); err != nil {
		return err
	}
	return visitObjectPages(ctx, e.backend, storageformat.TransitionPrefix()+"decisions/", func(info objectstore.ObjectInfo) error {
		object, err := e.backend.Get(ctx, info.Key)
		if err != nil {
			return err
		}
		var envelope storageformat.Envelope
		var decision storageformat.TransitionDecision009
		if err := storageformat.DecodeEnvelope(object.Body, object.Key, transitionDecisionSchema009, &envelope, &decision); err != nil || storageformat.ValidateTransitionDecision009(decision) != nil || storageformat.TransitionDecisionKey(decision.TransitionID) != object.Key {
			return domain.NewError(domain.ErrorInvalid, "invalid transition decision authority")
		}
		plan, err := e.readTransitionPlan009(ctx, decision.TransitionID)
		if err != nil || plan.plan.Fingerprint != decision.Fingerprint {
			return domain.NewError(domain.ErrorInvalid, "transition decision has no matching plan")
		}
		return collector.Add(object.Key)
	})
}

func (walker *checkpointReachabilityWalker) walkTree(ctx context.Context, session *consistencyDomainTreeSession, root storageformat.DomainTreeRoot, purpose string, visit func(storageformat.DomainEntry) error) error {
	if root.Digest == "" {
		return nil
	}
	return walker.walkTreePage(ctx, session, domainPageRef{root: root}, purpose, visit)
}

func (walker *checkpointReachabilityWalker) walkTreePage(ctx context.Context, session *consistencyDomainTreeSession, reference domainPageRef, purpose string, visit func(storageformat.DomainEntry) error) error {
	key := session.pageKey(reference.root.Digest)
	if reference.root.PackID != "" {
		key = storageformat.DomainPagePackKey(session.reference.Kind, session.reference.ID, reference.root.PackID)
	}
	seenKey := purpose + "\x00" + key.String() + "\x00" + reference.root.Digest
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
		childReference := domainPageRef{root: storageformat.DomainTreeRoot{Digest: child.Digest, PackID: child.PackID, Level: child.Level, EntryCount: child.EntryCount, ByteCount: child.ByteCount}, firstKey: child.FirstKey, lastKey: child.LastKey}
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
	return newConsistencyDomainCheckpointMetadataStream(ctx, engine, false)
}

func newConsistencyDomainCheckpointMetadataStream(ctx context.Context, engine *Engine, schema009 bool) (*schema008CheckpointMetadataStream, error) {
	reachable, err := engine.collectConsistencyDomainCheckpointReachability(ctx, schema009)
	if err != nil {
		return nil, err
	}
	return &schema008CheckpointMetadataStream{reachable: reachable, metadata: newCheckpointMetadataStream(engine, true)}, nil
}

func (stream *schema008CheckpointMetadataStream) Close() error { return stream.reachable.Close() }

func (stream *schema008CheckpointMetadataStream) next(ctx context.Context) (objectstore.ObjectInfo, bool, bool, error) {
	if !stream.found {
		key, found, err := stream.reachable.Next()
		if err != nil {
			return objectstore.ObjectInfo{}, false, false, err
		}
		if !found {
			for {
				info, fileData, metadataFound, metadataErr := stream.metadata.next(ctx)
				if metadataErr != nil || !metadataFound {
					return objectstore.ObjectInfo{}, false, false, metadataErr
				}
				collectable, collectErr := stream.collectableMetadata(ctx, info, fileData)
				if collectErr != nil {
					return objectstore.ObjectInfo{}, false, false, collectErr
				}
				if !collectable {
					return objectstore.ObjectInfo{}, false, false, domain.NewError(domain.ErrorPreconditionFailed, "checkpoint contains unenumerated mutable authority")
				}
			}
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
			// Only immutable pages and unreachable file blobs are collectable.
			// A mutable domain head omitted from the catalog is unenumerated
			// authority and must make checkpoint creation fail closed.
			collectable, collectErr := stream.collectableMetadata(ctx, info, fileData)
			if collectErr != nil {
				return objectstore.ObjectInfo{}, false, false, collectErr
			}
			if !collectable {
				return objectstore.ObjectInfo{}, false, false, domain.NewError(domain.ErrorPreconditionFailed, "checkpoint contains unenumerated mutable authority")
			}
			continue
		case info.Key != stream.key:
			return objectstore.ObjectInfo{}, false, false, domain.NewError(domain.ErrorPreconditionFailed, "checkpoint authority object is missing")
		default:
			stream.found = false
			return info, fileData, true, nil
		}
	}
}

func (stream *schema008CheckpointMetadataStream) collectableMetadata(ctx context.Context, info objectstore.ObjectInfo, fileData bool) (bool, error) {
	if fileData || isSchema008CollectableAuthorityGarbageKey(info.Key.String()) {
		return true, nil
	}
	segments := strings.Split(info.Key.String(), "/")
	if len(segments) != 6 || segments[0] != "endlessfs" || segments[1] != "v1" || segments[2] != "domains" || segments[3] == "catalog" || segments[4] == "" || segments[5] != "head.json" {
		return false, nil
	}
	object, err := stream.metadata.engine.backend.Get(ctx, info.Key)
	if err != nil {
		return false, err
	}
	var envelope storageformat.Envelope
	var head storageformat.DomainHead
	if err := storageformat.DecodeEnvelope(object.Body, info.Key, domainHeadSchema, &envelope, &head); err != nil {
		return false, err
	}
	if err := storageformat.ValidateInitialDomainHead(head); err != nil || storageformat.DomainHeadKey(head.Kind, head.DomainID) != info.Key {
		return false, nil
	}
	// A crash may leave the deliberately inert pre-registration head. It can
	// expose no values and is safe to exclude from a checkpoint; reopening a
	// mutation will either register it or lose to catalog freeze.
	return true, nil
}

func isSchema008CollectableAuthorityGarbageKey(key string) bool {
	segments := strings.Split(key, "/")
	if len(segments) == 6 && segments[0] == "endlessfs" && segments[1] == "v1" && segments[2] == "domains" && segments[3] == "catalog" && segments[4] == "pages" {
		return strings.HasSuffix(segments[5], ".json")
	}
	if len(segments) != 7 || segments[0] != "endlessfs" || segments[1] != "v1" || segments[2] != "domains" || segments[4] == "" || segments[5] != "pages" && segments[5] != "packs" || segments[5] == "pages" && !strings.HasSuffix(segments[6], ".json") || segments[5] == "packs" && !strings.HasSuffix(segments[6], ".bin") {
		return false
	}
	switch storageformat.ConsistencyDomainKind(segments[3]) {
	case storageformat.DomainNamespace, storageformat.DomainOwnerControl, storageformat.DomainAdmin, storageformat.DomainCapability, storageformat.DomainShare, storageformat.DomainIdentity, storageformat.DomainOwnerJobs:
		return true
	default:
		return false
	}
}
