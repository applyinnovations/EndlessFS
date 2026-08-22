package portable

import (
	"container/heap"
	"context"
	"errors"
	"sort"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

const (
	fileOperationPreparationPageSchema = "file-operation-preparation-page-v1"
	maxOperationPreparationPageItems   = 64
	operationPreparationMergeFanIn     = 16
)

type operationPreparationRunCollector struct {
	store       *FileStore
	operation   storageformat.FileOperation
	generation  uint64
	runCount    uint64
	buffer      []storageformat.FileOperationPreparationItem
	maxBuffered int
}

func newOperationPreparationRunCollector(store *FileStore, operation storageformat.FileOperation, generation uint64) (*operationPreparationRunCollector, error) {
	if store == nil || store.engine == nil || store.engine.backend == nil || operation.UserID == "" || operation.OperationID == "" || operation.Preparation == nil || operation.Preparation.SchemaVersion != 1 || operation.Preparation.RunSetID == "" {
		return nil, domain.NewError(domain.ErrorInvalid, "invalid operation preparation collector")
	}
	return &operationPreparationRunCollector{store: store, operation: operation, generation: generation, buffer: make([]storageformat.FileOperationPreparationItem, 0, maxOperationPreparationPageItems)}, nil
}

func (collector *operationPreparationRunCollector) Add(ctx context.Context, item storageformat.FileOperationPreparationItem) error {
	if err := validateOperationPreparationItem(item); err != nil {
		return err
	}
	collector.buffer = append(collector.buffer, item)
	collector.maxBuffered = max(collector.maxBuffered, len(collector.buffer))
	if len(collector.buffer) < maxOperationPreparationPageItems {
		return nil
	}
	return collector.flush(ctx)
}

func (collector *operationPreparationRunCollector) Close(ctx context.Context) (uint64, error) {
	if err := collector.flush(ctx); err != nil {
		return 0, err
	}
	return collector.runCount, nil
}

func (collector *operationPreparationRunCollector) flush(ctx context.Context) error {
	if len(collector.buffer) == 0 {
		return nil
	}
	sort.Slice(collector.buffer, func(left, right int) bool {
		return operationPreparationItemLess(collector.buffer[left], collector.buffer[right])
	})
	page := storageformat.FileOperationPreparationPage{
		SchemaVersion: 1, UserID: collector.operation.UserID, OperationID: collector.operation.OperationID,
		RunSetID: collector.operation.Preparation.RunSetID, Generation: collector.generation,
		Run: collector.runCount, Page: 0, Final: true,
		Items: append([]storageformat.FileOperationPreparationItem(nil), collector.buffer...),
	}
	if _, err := collector.store.writeOperationPreparationPage(ctx, collector.operation, page); err != nil {
		return err
	}
	collector.runCount++
	collector.buffer = collector.buffer[:0]
	return nil
}

func operationPreparationItemLess(left, right storageformat.FileOperationPreparationItem) bool {
	if left.SortKey != right.SortKey {
		return left.SortKey < right.SortKey
	}
	leftBody, _ := storageformat.EncodeCanonical(left)
	rightBody, _ := storageformat.EncodeCanonical(right)
	return string(leftBody) < string(rightBody)
}

func validateOperationPreparationItem(item storageformat.FileOperationPreparationItem) error {
	values := 0
	if item.Root != nil {
		values++
	}
	if item.Prerequisite != nil {
		values++
	}
	if item.Copy != nil {
		values++
	}
	if item.SortKey == "" || values != 1 || item.Kind == storageformat.FileOperationPreparationRoot && item.Root == nil || item.Kind == storageformat.FileOperationPreparationPrerequisite && item.Prerequisite == nil || item.Kind == storageformat.FileOperationPreparationCopy && item.Copy == nil {
		return domain.NewError(domain.ErrorInvalid, "invalid operation preparation item")
	}
	switch item.Kind {
	case storageformat.FileOperationPreparationRoot, storageformat.FileOperationPreparationPrerequisite, storageformat.FileOperationPreparationCopy:
		return nil
	default:
		return domain.NewError(domain.ErrorInvalid, "invalid operation preparation item kind")
	}
}

func (s *FileStore) writeOperationPreparationPage(ctx context.Context, operation storageformat.FileOperation, page storageformat.FileOperationPreparationPage) (string, error) {
	if operation.Preparation == nil || page.SchemaVersion != 1 || page.UserID != operation.UserID || page.OperationID != operation.OperationID || page.RunSetID != operation.Preparation.RunSetID || len(page.Items) == 0 || len(page.Items) > maxOperationPreparationPageItems {
		return "", domain.NewError(domain.ErrorInvalid, "invalid operation preparation page")
	}
	previous := ""
	for index, item := range page.Items {
		if err := validateOperationPreparationItem(item); err != nil || index > 0 && operationPreparationItemLess(item, page.Items[index-1]) {
			return "", domain.NewError(domain.ErrorInvalid, "invalid operation preparation page items")
		}
		if previous != "" && item.SortKey < previous {
			return "", domain.NewError(domain.ErrorInvalid, "operation preparation page is not sorted")
		}
		previous = item.SortKey
	}
	key := storageformat.FileOperationPreparationPageKey(page.UserID, page.OperationID, page.RunSetID, page.Generation, page.Run, page.Page)
	body, err := storageformat.EncodeEnvelope(fileOperationPreparationPageSchema, key, 1, page)
	if err != nil {
		return "", err
	}
	if err := s.ensureImmutableOperationObject(ctx, key, body); err != nil {
		return "", err
	}
	return storageformat.Digest(body), nil
}

type operationPreparationRunReader struct {
	store      *FileStore
	operation  storageformat.FileOperation
	generation uint64
	run        uint64
	page       uint64
	previous   string
	items      []storageformat.FileOperationPreparationItem
	index      int
	final      bool
}

func (reader *operationPreparationRunReader) next(ctx context.Context) (storageformat.FileOperationPreparationItem, bool, error) {
	for reader.index == len(reader.items) {
		if reader.final {
			return storageformat.FileOperationPreparationItem{}, false, nil
		}
		key := storageformat.FileOperationPreparationPageKey(reader.operation.UserID, reader.operation.OperationID, reader.operation.Preparation.RunSetID, reader.generation, reader.run, reader.page)
		object, err := reader.store.engine.backend.Get(ctx, key)
		if err != nil {
			return storageformat.FileOperationPreparationItem{}, false, err
		}
		var envelope storageformat.Envelope
		var page storageformat.FileOperationPreparationPage
		if storageformat.DecodeEnvelope(object.Body, key, fileOperationPreparationPageSchema, &envelope, &page) != nil || page.SchemaVersion != 1 || page.UserID != reader.operation.UserID || page.OperationID != reader.operation.OperationID || page.RunSetID != reader.operation.Preparation.RunSetID || page.Generation != reader.generation || page.Run != reader.run || page.Page != reader.page || page.PreviousDigest != reader.previous || len(page.Items) == 0 || len(page.Items) > maxOperationPreparationPageItems {
			return storageformat.FileOperationPreparationItem{}, false, domain.NewError(domain.ErrorInvalid, "invalid operation preparation run page")
		}
		for index, item := range page.Items {
			if err := validateOperationPreparationItem(item); err != nil || index > 0 && operationPreparationItemLess(item, page.Items[index-1]) {
				return storageformat.FileOperationPreparationItem{}, false, domain.NewError(domain.ErrorInvalid, "invalid operation preparation run ordering")
			}
		}
		reader.items, reader.index, reader.final = page.Items, 0, page.Final
		reader.previous = storageformat.Digest(object.Body)
		reader.page++
	}
	item := reader.items[reader.index]
	reader.index++
	return item, true, nil
}

type operationPreparationHeapItem struct {
	item   storageformat.FileOperationPreparationItem
	reader int
}

type operationPreparationHeap []operationPreparationHeapItem

func (values operationPreparationHeap) Len() int { return len(values) }
func (values operationPreparationHeap) Less(left, right int) bool {
	if operationPreparationItemLess(values[left].item, values[right].item) {
		return true
	}
	if operationPreparationItemLess(values[right].item, values[left].item) {
		return false
	}
	return values[left].reader < values[right].reader
}
func (values operationPreparationHeap) Swap(left, right int) {
	values[left], values[right] = values[right], values[left]
}
func (values *operationPreparationHeap) Push(value any) {
	*values = append(*values, value.(operationPreparationHeapItem))
}
func (values *operationPreparationHeap) Pop() any {
	old := *values
	value := old[len(old)-1]
	*values = old[:len(old)-1]
	return value
}

type operationPreparationSortedRunWriter struct {
	store      *FileStore
	operation  storageformat.FileOperation
	generation uint64
	run        uint64
	page       uint64
	previous   string
	buffer     []storageformat.FileOperationPreparationItem
}

func (writer *operationPreparationSortedRunWriter) add(ctx context.Context, item storageformat.FileOperationPreparationItem) error {
	if len(writer.buffer) == maxOperationPreparationPageItems {
		if err := writer.flush(ctx, false); err != nil {
			return err
		}
	}
	writer.buffer = append(writer.buffer, item)
	return nil
}

func (writer *operationPreparationSortedRunWriter) close(ctx context.Context) error {
	if len(writer.buffer) == 0 {
		return domain.NewError(domain.ErrorInvalid, "empty merged operation preparation run")
	}
	return writer.flush(ctx, true)
}

func (writer *operationPreparationSortedRunWriter) flush(ctx context.Context, final bool) error {
	page := storageformat.FileOperationPreparationPage{
		SchemaVersion: 1, UserID: writer.operation.UserID, OperationID: writer.operation.OperationID,
		RunSetID: writer.operation.Preparation.RunSetID, Generation: writer.generation, Run: writer.run,
		Page: writer.page, PreviousDigest: writer.previous, Final: final,
		Items: append([]storageformat.FileOperationPreparationItem(nil), writer.buffer...),
	}
	digest, err := writer.store.writeOperationPreparationPage(ctx, writer.operation, page)
	if err != nil {
		return err
	}
	writer.previous = digest
	writer.page++
	writer.buffer = writer.buffer[:0]
	return nil
}

func (s *FileStore) mergeOperationPreparationRuns(ctx context.Context, operation storageformat.FileOperation, generation, runCount uint64) (uint64, error) {
	if operation.Preparation == nil || runCount == 0 {
		return 0, domain.NewError(domain.ErrorInvalid, "invalid operation preparation merge")
	}
	for runCount > 1 {
		nextCount := uint64(0)
		for start := uint64(0); start < runCount; start += operationPreparationMergeFanIn {
			end := min(start+operationPreparationMergeFanIn, runCount)
			readers := make([]*operationPreparationRunReader, 0, end-start)
			values := operationPreparationHeap{}
			for run := start; run < end; run++ {
				reader := &operationPreparationRunReader{store: s, operation: operation, generation: generation, run: run}
				item, ok, err := reader.next(ctx)
				if err != nil {
					return 0, err
				}
				if !ok {
					return 0, domain.NewError(domain.ErrorInvalid, "empty operation preparation input run")
				}
				readers = append(readers, reader)
				heap.Push(&values, operationPreparationHeapItem{item: item, reader: len(readers) - 1})
			}
			writer := operationPreparationSortedRunWriter{store: s, operation: operation, generation: generation + 1, run: nextCount, buffer: make([]storageformat.FileOperationPreparationItem, 0, maxOperationPreparationPageItems)}
			for values.Len() != 0 {
				value := heap.Pop(&values).(operationPreparationHeapItem)
				if err := writer.add(ctx, value.item); err != nil {
					return 0, err
				}
				next, ok, err := readers[value.reader].next(ctx)
				if err != nil {
					return 0, err
				}
				if ok {
					heap.Push(&values, operationPreparationHeapItem{item: next, reader: value.reader})
				}
			}
			if err := writer.close(ctx); err != nil {
				return 0, err
			}
			nextCount++
		}
		generation++
		runCount = nextCount
	}
	return generation, nil
}

func (s *FileStore) forEachOperationPreparationRunItem(ctx context.Context, operation storageformat.FileOperation, generation, run uint64, visit func(storageformat.FileOperationPreparationItem) error) error {
	if visit == nil || operation.Preparation == nil {
		return domain.NewError(domain.ErrorInvalid, "invalid operation preparation visitor")
	}
	reader := operationPreparationRunReader{store: s, operation: operation, generation: generation, run: run}
	for {
		item, ok, err := reader.next(ctx)
		if err != nil || !ok {
			return err
		}
		if err := visit(item); err != nil {
			return err
		}
	}
}

func (s *FileStore) sealFileOperationPreparation(ctx context.Context, object objectstore.Object, envelope storageformat.Envelope, operation storageformat.FileOperation) error {
	if operation.SchemaVersion != 2 || operation.State != storageformat.FileOperationPreparing || operation.Preparation == nil || operation.Preparation.Phase != "seal" || operation.Preparation.RunCount == 0 {
		return domain.NewError(domain.ErrorInvalid, "invalid file operation preparation seal")
	}
	generation, err := s.mergeOperationPreparationRuns(ctx, operation, operation.Preparation.Generation, operation.Preparation.RunCount)
	if err != nil {
		return err
	}
	operation.Preparation.Generation = generation
	operation.Preparation.RunCount = 1
	if err := s.persistPreparedFileOperationSteps(ctx, &operation); err != nil {
		return err
	}
	operation.State = storageformat.FileOperationRunning
	operation.Preparation = nil
	operation.UpdatedAt = s.engine.clock.Now().UTC()
	body, err := storageformat.EncodeEnvelope(fileOperationSchema, object.Key, envelope.Revision+1, operation)
	if err != nil {
		return err
	}
	if _, err := s.engine.backend.Put(ctx, object.Key, body, objectstore.PutCondition{Mode: objectstore.PutMatch, Version: object.Version}); err != nil {
		if errors.Is(err, domain.ErrPreconditionFailed) || errors.Is(err, domain.ErrConflict) || errors.Is(err, domain.ErrNotFound) {
			return domain.NewError(domain.ErrorUnavailable, "file operation preparation ownership changed")
		}
		return err
	}
	return nil
}

func (s *FileStore) persistPreparedFileOperationSteps(ctx context.Context, operation *storageformat.FileOperation) error {
	if operation == nil || operation.Preparation == nil || operation.Preparation.RunCount != 1 || operation.StepSetID != "" || operation.StepPageCount != 0 || operation.StepDigest != "" {
		return domain.NewError(domain.ErrorInvalid, "invalid prepared operation step input")
	}
	operation.StepSetID = operation.Preparation.RunSetID
	operation.StepsStaged = true
	previous := ""
	pageCount := uint64(0)
	writePage := func(page storageformat.FileOperationStepPage) error {
		page.SchemaVersion = 1
		page.UserID = operation.UserID
		page.OperationID = operation.OperationID
		page.StepSetID = operation.StepSetID
		page.Index = pageCount
		page.PreviousDigest = previous
		key := stagedFileOperationStepPageKey(*operation, pageCount)
		body, err := storageformat.EncodeEnvelope(fileOperationStepPageSchema, key, 1, page)
		if err != nil {
			return domain.WrapError(domain.ErrorInvalid, "prepared operation step page exceeds the bounded record limit", err)
		}
		if err := s.ensureImmutableOperationObject(ctx, key, body); err != nil {
			return err
		}
		previous = storageformat.Digest(body)
		pageCount++
		return nil
	}
	var roots []storageformat.FileOperationRoot
	var prerequisites []storageformat.MutationObjectReference
	var copies []storageformat.MutationCopy
	rootCount := uint64(0)
	lastRoot, lastPrerequisite, lastCopy := "", "", ""
	flushRoots := func() error {
		if len(roots) == 0 {
			return nil
		}
		err := writePage(storageformat.FileOperationStepPage{Roots: roots})
		roots = roots[:0]
		return err
	}
	flushPrerequisites := func() error {
		if len(prerequisites) == 0 {
			return nil
		}
		err := writePage(storageformat.FileOperationStepPage{Prerequisites: prerequisites})
		prerequisites = prerequisites[:0]
		return err
	}
	flushCopies := func() error {
		if len(copies) == 0 {
			return nil
		}
		err := writePage(storageformat.FileOperationStepPage{Copies: copies})
		copies = copies[:0]
		return err
	}
	err := s.forEachOperationPreparationRunItem(ctx, *operation, operation.Preparation.Generation, 0, func(item storageformat.FileOperationPreparationItem) error {
		switch item.Kind {
		case storageformat.FileOperationPreparationRoot:
			if item.Root.Key == "" || item.Root.Key <= lastRoot {
				return domain.NewError(domain.ErrorInvalid, "prepared operation roots are not uniquely ordered")
			}
			lastRoot = item.Root.Key
			roots = append(roots, *item.Root)
			rootCount++
			if len(roots) == maxOperationPageRoots {
				return flushRoots()
			}
		case storageformat.FileOperationPreparationPrerequisite:
			if item.Prerequisite.Key == "" || item.Prerequisite.BodyDigest == "" || item.Prerequisite.Key <= lastPrerequisite {
				return domain.NewError(domain.ErrorInvalid, "prepared operation prerequisites are not uniquely ordered")
			}
			lastPrerequisite = item.Prerequisite.Key
			prerequisites = append(prerequisites, *item.Prerequisite)
			if len(prerequisites) == maxOperationPagePrerequisites {
				return flushPrerequisites()
			}
		case storageformat.FileOperationPreparationCopy:
			if item.Copy.DestinationKey == "" || item.Copy.DestinationKey <= lastCopy {
				return domain.NewError(domain.ErrorInvalid, "prepared operation copies are not uniquely ordered")
			}
			lastCopy = item.Copy.DestinationKey
			copies = append(copies, *item.Copy)
			if len(copies) == maxOperationPageCopies {
				return flushCopies()
			}
		default:
			return domain.NewError(domain.ErrorInvalid, "unreduced operation preparation item")
		}
		return nil
	})
	if err != nil {
		return err
	}
	if err := flushRoots(); err != nil {
		return err
	}
	if err := flushPrerequisites(); err != nil {
		return err
	}
	if err := flushCopies(); err != nil {
		return err
	}
	if rootCount == 0 || pageCount == 0 {
		return domain.NewError(domain.ErrorInvalid, "prepared file operation has no visibility roots")
	}
	operation.StepPageCount = pageCount
	operation.StepDigest = previous
	return nil
}
