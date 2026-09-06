package storageformat

import (
	"testing"
	"time"
)

func TestPortableUploadBatchAbortValidation(t *testing.T) {
	valid := PortableUploadBatchAbort{
		SchemaVersion: 1,
		OwnerID:       "owner",
		BatchID:       Digest([]byte("batch")),
		Count:         10,
		Aborted:       []byte{0b00000001, 0b00000010},
		ModifiedAt:    time.Date(2068, 1, 2, 3, 4, 5, 0, time.UTC),
	}
	if err := ValidatePortableUploadBatchAbort(valid); err != nil {
		t.Fatal(err)
	}
	if !valid.Aborts(0) || !valid.Aborts(9) || valid.Aborts(8) || valid.Aborts(10) {
		t.Fatal("abort bitmap membership is incorrect")
	}
	for name, mutate := range map[string]func(*PortableUploadBatchAbort){
		"empty":        func(value *PortableUploadBatchAbort) { value.Aborted = []byte{0, 0} },
		"short":        func(value *PortableUploadBatchAbort) { value.Aborted = []byte{1} },
		"out-of-range": func(value *PortableUploadBatchAbort) { value.Aborted[1] = 0b10000000 },
	} {
		t.Run(name, func(t *testing.T) {
			candidate := valid
			candidate.Aborted = append([]byte(nil), valid.Aborted...)
			mutate(&candidate)
			if err := ValidatePortableUploadBatchAbort(candidate); err == nil {
				t.Fatal("invalid abort bitmap was accepted")
			}
		})
	}
}
