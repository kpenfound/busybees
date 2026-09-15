package scheduler

import "github.com/kpenfound/busybees/core/ops"

// SharedPool is the fair core slot pool used across machine projects.
type SharedPool = ops.SharedPool

// NewSharedPool constructs the machine-wide developer slot pool.
func NewSharedPool(size int) *SharedPool { return ops.NewSharedPool(size) }
