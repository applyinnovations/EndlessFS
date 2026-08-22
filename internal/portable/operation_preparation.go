package portable

import (
	"container/heap"
	"context"
	"sort"

	"github.com/applyinnovations/endlessfs/internal/domain"
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
