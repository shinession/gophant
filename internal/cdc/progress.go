package cdc

import (
	"sync/atomic"

	"github.com/jackc/pglogrepl"
)

// Progress is the position state shared by the stream reader, the heartbeat and the applier.
//
//   - Written is the highest WAL position received from s1.
//   - Flushed is the highest position whose transactions are committed on s3 together with
//     applied_lsn. Only this position is ever reported to s1 as flushed: once s1 has it, s1 may
//     delete the WAL before it.
//   - InFlight counts the transactions handed to the applier but not yet committed on s3.
type Progress struct {
	written  atomic.Uint64
	flushed  atomic.Uint64
	inflight atomic.Int64
	fast     atomic.Bool
}

// Written returns the highest received WAL position.
func (p *Progress) Written() pglogrepl.LSN { return pglogrepl.LSN(p.written.Load()) }

// AdvanceWritten moves Written forward; a smaller value is ignored.
func (p *Progress) AdvanceWritten(lsn pglogrepl.LSN) { advance(&p.written, uint64(lsn)) }

// Flushed returns the highest position that is durably applied on s3.
func (p *Progress) Flushed() pglogrepl.LSN { return pglogrepl.LSN(p.flushed.Load()) }

// AdvanceFlushed moves Flushed forward; a smaller value is ignored.
func (p *Progress) AdvanceFlushed(lsn pglogrepl.LSN) { advance(&p.flushed, uint64(lsn)) }

// InFlight returns the number of transactions that are not yet committed on s3.
func (p *Progress) InFlight() int64 { return p.inflight.Load() }

// SetFast makes the heartbeat report every second instead of every standby_status_interval. The cutover
// uses it, so that the position of an idle stream reaches the target quickly.
func (p *Progress) SetFast(fast bool) { p.fast.Store(fast) }

// Fast reports whether the heartbeat runs at the fast rate.
func (p *Progress) Fast() bool { return p.fast.Load() }

func advance(v *atomic.Uint64, to uint64) {
	for {
		cur := v.Load()
		if to <= cur || v.CompareAndSwap(cur, to) {
			return
		}
	}
}
