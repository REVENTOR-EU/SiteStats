//go:build !linux

package collect

import (
	"runtime"
	"time"

	"github.com/REVENTOR-EU/SiteStats/internal/sites"
)

type procState struct {
	first bool
}

// ProcState is the exported handle for the scanner state.
type ProcState = procState

func NewProcState() *ProcState { return newProcState() }

// ScanProc is a no-op outside Linux (used for local builds/tests only).
func ScanProc(st *ProcState, reg *sites.Registry) Snapshot {
	return scanProc(st, reg)
}

func newProcState() *procState { return &procState{first: true} }

func scanProc(st *procState, reg *sites.Registry) Snapshot {
	return Snapshot{Ts: time.Now().Unix(), Sites: map[string]*SiteLive{}}
}

func runtimeNumCPU() int { return runtime.NumCPU() }
