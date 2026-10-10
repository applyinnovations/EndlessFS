package telemetry

import (
	"fmt"
	"io"
)

var migrationStages = [...]string{"started", "gate-closed", "directories-verified", "checkpoint-inventory", "checkpoint-created", "complete"}

type migrationProgress struct {
	Completed, Total, Resumed int
	Bytes, TotalBytes         int64
	Seen                      uint64
}

// MigrationProgress accepts only the declared stage vocabulary. Epoch IDs,
// provider identifiers, keys, and paths are deliberately absent.
func (observer *Observer) MigrationProgress(stage string, role Role, completed, total, resumed int, bytes, totalBytes int64) {
	if observer == nil || role >= roleCount || completed < 0 || total < 0 || resumed < 0 || bytes < 0 || totalBytes < 0 {
		return
	}
	for index, name := range migrationStages {
		if name == stage {
			observer.mu.Lock()
			progress := &observer.migration[index][role]
			progress.Completed = completed
			progress.Total = total
			progress.Resumed = resumed
			progress.Bytes = bytes
			progress.TotalBytes = totalBytes
			progress.Seen++
			observer.mu.Unlock()
			return
		}
	}
}
func writeMigration(output io.Writer, values [len(migrationStages)][roleCount]migrationProgress) {
	for index, roles := range values {
		for role, progress := range roles {
			if progress.Seen > 0 {
				labels := fmt.Sprintf("stage=%q,role=%q", migrationStages[index], roleNames[role])
				fmt.Fprintf(output, "endlessfs_migration_progress_events_total{%s} %d\nendlessfs_migration_completed_objects{%s} %d\nendlessfs_migration_total_objects{%s} %d\nendlessfs_migration_resumed_objects{%s} %d\nendlessfs_migration_completed_bytes{%s} %d\nendlessfs_migration_total_bytes{%s} %d\n", labels, progress.Seen, labels, progress.Completed, labels, progress.Total, labels, progress.Resumed, labels, progress.Bytes, labels, progress.TotalBytes)
			}
		}
	}
}
