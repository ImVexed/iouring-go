package iouring

import (
	"os"
	"sync"
	"sync/atomic"
	"syscall"

	"github.com/pkg/errors"
	"golang.org/x/sys/unix"
)

// Ring contains an io_uring submit and completion ring.
type Ring struct {
	fd      int
	p       *Params
	Cq      *CompletionQueue
	CqMu    sync.RWMutex
	Sq      *SubmitQueue
	SqMu    sync.RWMutex
	sqPool  sync.Pool
	idx     *uint64
	debug   bool
	fileReg FileRegistry
}

func (r *Ring) Fd() int {
	return r.fd
}

// New is used to create an iouring.Ring.
func New(size uint, p *Params) (*Ring, error) {
	if p == nil {
		p = &Params{}
	}
	fd, err := Setup(size, p)
	if err != nil {
		return nil, err
	}
	var (
		cq       CompletionQueue
		sq       SubmitQueue
		sqWrites uint32
	)
	if err := MmapRing(fd, p, &sq, &cq); err != nil {
		return nil, err
	}
	idx := uint64(0)
	sqState := RingStateEmpty
	sq.state = &sqState
	sq.writes = &sqWrites
	return &Ring{
		p:       p,
		fd:      fd,
		Cq:      &cq,
		Sq:      &sq,
		idx:     &idx,
		fileReg: NewFileRegistry(fd),
	}, nil
}

// Enter is used to enter the ring.
func (r *Ring) Enter(toSubmit uint, minComplete uint, flags uint, sigset *unix.Sigset_t) error {
	// Acquire the submit barrier so that the ring can safely be entered.
	r.Sq.submitBarrier()
	if r.Sq.NeedWakeup() {
		flags |= EnterSqWakeup
	}
	completed, err := Enter(r.fd, toSubmit, minComplete, flags, sigset)
	if err != nil {
		// TODO(hodgesds): are certain errors able to empty the ring?
		r.Sq.fill()
		return err
	}
	if uint(completed) < toSubmit {
		r.Sq.fill()
		return nil
	}
	r.Sq.empty()
	return nil
}

func (r *Ring) canEnter() bool {
	return atomic.LoadUint32(r.Sq.Head) != atomic.LoadUint32(r.Sq.Tail)
}

// Close is used to close the ring.
func (r *Ring) Close() error {
	if err := r.closeSq(); err != nil {
		return err
	}
	if r.p.Flags&FeatSingleMmap == 0 {
		if err := r.closeCq(); err != nil {
			return err
		}
	}
	return syscall.Close(r.fd)
}

func (r *Ring) closeCq() error {
	r.CqMu.Lock()
	defer r.CqMu.Unlock()
	if r.Cq == nil {
		return nil
	}

	_, _, errno := syscall.Syscall6(
		syscall.SYS_MUNMAP,
		r.Cq.ptr,
		uintptr(r.Cq.Size),
		uintptr(0),
		uintptr(0),
		uintptr(0),
		uintptr(0),
	)
	if errno != 0 {
		err := errno
		return errors.Wrap(err, "failed to munmap cq ring")
	}
	r.Cq = nil
	return nil
}

func (r *Ring) closeSq() error {
	r.SqMu.Lock()
	defer r.SqMu.Unlock()
	if r.Sq == nil {
		return nil
	}

	_, _, errno := syscall.Syscall6(
		syscall.SYS_MUNMAP,
		r.Sq.ptr,
		uintptr(r.Sq.Size),
		uintptr(0),
		uintptr(0),
		uintptr(0),
		uintptr(0),
	)
	if errno != 0 {
		err := errno
		return errors.Wrap(err, "failed to munmap sq ring")
	}
	r.Sq = nil
	return nil
}

// SubmitHead returns the position of the head of the submit queue. This method
// is safe for calling concurrently.
func (r *Ring) SubmitHead() int {
	return int(atomic.LoadUint32(r.Sq.Head) & atomic.LoadUint32(r.Sq.Mask))
}

// SubmitTail returns the position of the tail of the submit queue. This method
// is safe for calling concurrently.
func (r *Ring) SubmitTail() int {
	return int(atomic.LoadUint32(r.Sq.Tail) & atomic.LoadUint32(r.Sq.Mask))
}

// CompleteHead returns the position of the head of the completion queue. This
// method is safe for calling concurrently.
func (r *Ring) CompleteHead() int {
	return int(atomic.LoadUint32(r.Cq.Head) & atomic.LoadUint32(r.Cq.Mask))
}

// CompleteTail returns the position of the tail of the submit queue. This method
// is safe for calling concurrently.
func (r *Ring) CompleteTail() int {
	return int(atomic.LoadUint32(r.Cq.Tail) & atomic.LoadUint32(r.Cq.Mask))
}

// SubmitEntry returns the next available SubmitEntry or nil if the ring is
// busy. The returned function should be called after SubmitEntry is ready to
// enter the ring.
func (r *Ring) SubmitEntry() (*SubmitEntry, func()) {
	// This function roughly follows this:
	// https://github.com/axboe/liburing/blob/master/src/queue.c#L258

	if r.p != nil && (uint(r.p.Flags)&SetupIOPoll == 0) {
	getNext:
		tail := atomic.LoadUint32(r.Sq.Tail)
		head := atomic.LoadUint32(r.Sq.Head)
		mask := atomic.LoadUint32(r.Sq.Mask)
		next := tail&mask + 1
		if next-head <= uint32(len(r.Sq.Entries)) {
			// Make sure the ring is safe for updating by acquring the
			// update barrier.
			r.Sq.updateBarrier()
			if !atomic.CompareAndSwapUint32(r.Sq.Tail, tail, next) {
				goto getNext
			}
			// Increase the write counter as the caller will be
			// updating the returned SubmitEntry.
			r.Sq.startWrite()

			// The callback that is returned is used to update the
			// state of the ring and decrement the active writes
			// counter.
			return &r.Sq.Entries[tail&mask], func() {
				r.Sq.completeWrite()
				r.Sq.fill()
				r.Sq.Array[next-1] = head & mask
			}
		}
		goto getNext
	}
	// TODO handle pool based
	return nil, func() {}
}

// ID returns an id for a SQEs, it is a monotonically increasing value (until
// uint64 wrapping).
func (r *Ring) ID() uint64 {
	return atomic.AddUint64(r.idx, 1)
}

// FileReadWriter returns an io.ReadWriter from an os.File that uses the ring.
// Note that is is not valid to use other operations on the file (Seek/Close)
// in combination with the reader.
func (r *Ring) FileReadWriter(f *os.File) (ReadWriteSeekerCloser, error) {
	var offset int64
	if o, err := f.Seek(0, 0); err == nil {
		offset = int64(o)
	}
	return &ringFIO{
		r:       r,
		f:       f,
		fOffset: &offset,
	}, r.fileReg.Register(int(f.Fd()))
}
