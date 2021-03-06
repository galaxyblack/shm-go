// Copyright 2016 Tom Thorogood. All rights reserved.
// Use of this source code is governed by a
// Modified BSD License license that can be found in
// the LICENSE file.

package shm

import (
	"golang.org/x/sys/unix"
	"io"
	"sync/atomic"
	"unsafe"

	"github.com/tmthrgd/go-sem"
)

const (
	eofFlagIndex = 0
	eofFlagMask  = 0x01
)

type Buffer struct {
	block *sharedBlock
	write bool

	Data  []byte
	Flags *[blockFlagsSize]byte
}

type ReadWriteCloser struct {
	name string

	data          []byte
	readShared    *sharedMem
	writeShared   *sharedMem
	size          uint64
	fullBlockSize uint64

	// Must be accessed using atomic operations
	Flags *[sharedFlagsSize]uint32

	closed uint32
}

func (rw *ReadWriteCloser) Close() error {
	if !atomic.CompareAndSwapUint32(&rw.closed, 0, 1) {
		return nil
	}

	// finish all sends before close!

	return unix.Munmap(rw.data)
}

// Name returns the name of the shared memory.
func (rw *ReadWriteCloser) Name() string {
	return rw.name
}

// Unlink removes the shared memory.
//
// It is the equivalent to calling Unlink(string) with
// the same name as Create* or Open*.
//
// Taken from shm_unlink(3):
// 	The  operation  of shm_unlink() is analogous to unlink(2): it removes a
// 	shared memory object name, and, once all processes  have  unmapped  the
// 	object, de-allocates and destroys the contents of the associated memory
// 	region.  After a successful shm_unlink(),  attempts  to  shm_open()  an
// 	object  with  the same name will fail (unless O_CREAT was specified, in
// 	which case a new, distinct object is created).
func (rw *ReadWriteCloser) Unlink() error {
	return Unlink(rw.name)
}

// Read

func (rw *ReadWriteCloser) Read(p []byte) (n int, err error) {
	buf, err := rw.GetReadBuffer()
	if err != nil {
		return 0, err
	}

	n = copy(p, buf.Data)
	isEOF := buf.Flags[eofFlagIndex]&eofFlagMask != 0

	if err = rw.SendReadBuffer(buf); err != nil {
		return n, err
	}

	if isEOF {
		return n, io.EOF
	}

	return n, nil
}

func (rw *ReadWriteCloser) WriteTo(w io.Writer) (n int64, err error) {
	for {
		buf, err := rw.GetReadBuffer()
		if err != nil {
			return n, err
		}

		nn, err := w.Write(buf.Data)
		n += int64(nn)

		isEOF := buf.Flags[eofFlagIndex]&eofFlagMask != 0

		if putErr := rw.SendReadBuffer(buf); putErr != nil {
			return n, putErr
		}

		if err != nil || isEOF {
			return n, err
		}
	}
}

func (rw *ReadWriteCloser) GetReadBuffer() (Buffer, error) {
	if atomic.LoadUint32(&rw.closed) != 0 {
		return Buffer{}, io.ErrClosedPipe
	}

	var block *sharedBlock

	blocks := uintptr(unsafe.Pointer(rw.readShared)) + sharedHeaderSize

	for {
		blockIndex := atomic.LoadUint32((*uint32)(&rw.readShared.ReadStart))
		if blockIndex > uint32(rw.readShared.BlockCount) {
			return Buffer{}, ErrInvalidSharedMemory
		}

		block = (*sharedBlock)(unsafe.Pointer(blocks + uintptr(uint64(blockIndex)*rw.fullBlockSize)))

		if blockIndex == atomic.LoadUint32((*uint32)(&rw.readShared.WriteEnd)) {
			if err := ((*sem.Semaphore)(&rw.readShared.SemSignal)).Wait(); err != nil {
				return Buffer{}, err
			}

			continue
		}

		if atomic.CompareAndSwapUint32((*uint32)(&rw.readShared.ReadStart), blockIndex, uint32(block.Next)) {
			break
		}
	}

	data := (*[1 << 30]byte)(unsafe.Pointer(uintptr(unsafe.Pointer(block)) + blockHeaderSize))
	flags := (*[len(block.Flags)]byte)(unsafe.Pointer(&block.Flags[0]))
	return Buffer{
		block: block,

		Data:  data[:block.Size:rw.readShared.BlockSize],
		Flags: flags,
	}, nil
}

func (rw *ReadWriteCloser) SendReadBuffer(buf Buffer) error {
	if atomic.LoadUint32(&rw.closed) != 0 {
		return io.ErrClosedPipe
	}

	if buf.write {
		return ErrInvalidBuffer
	}

	block := buf.block

	atomic.StoreUint32((*uint32)(&block.DoneRead), 1)

	blocks := uintptr(unsafe.Pointer(rw.readShared)) + sharedHeaderSize

	for {
		blockIndex := atomic.LoadUint32((*uint32)(&rw.readShared.ReadEnd))
		if blockIndex > uint32(rw.readShared.BlockCount) {
			return ErrInvalidSharedMemory
		}

		block = (*sharedBlock)(unsafe.Pointer(blocks + uintptr(uint64(blockIndex)*rw.fullBlockSize)))

		if !atomic.CompareAndSwapUint32((*uint32)(&block.DoneRead), 1, 0) {
			return nil
		}

		atomic.CompareAndSwapUint32((*uint32)(&rw.readShared.ReadEnd), blockIndex, uint32(block.Next))

		if uint32(block.Prev) == atomic.LoadUint32((*uint32)(&rw.readShared.WriteStart)) {
			if err := ((*sem.Semaphore)(&rw.readShared.SemAvail)).Post(); err != nil {
				return err
			}
		}
	}
}

// Write

func (rw *ReadWriteCloser) Write(p []byte) (n int, err error) {
	buf, err := rw.GetWriteBuffer()
	if err != nil {
		return 0, err
	}

	n = copy(buf.Data[:cap(buf.Data)], p)
	buf.Data = buf.Data[:n]

	buf.Flags[eofFlagIndex] |= eofFlagMask

	_, err = rw.SendWriteBuffer(buf)
	return n, err
}

func (rw *ReadWriteCloser) ReadFrom(r io.Reader) (n int64, err error) {
	for {
		buf, err := rw.GetWriteBuffer()
		if err != nil {
			return n, err
		}

		nn, err := r.Read(buf.Data[:cap(buf.Data)])
		buf.Data = buf.Data[:nn]
		n += int64(nn)

		if err == io.EOF {
			buf.Flags[eofFlagIndex] |= eofFlagMask
		} else {
			buf.Flags[eofFlagIndex] &^= eofFlagMask
		}

		if _, putErr := rw.SendWriteBuffer(buf); putErr != nil {
			return n, err
		}

		if err == io.EOF {
			return n, nil
		} else if err != nil {
			return n, err
		}
	}
}

func (rw *ReadWriteCloser) GetWriteBuffer() (Buffer, error) {
	if atomic.LoadUint32(&rw.closed) != 0 {
		return Buffer{}, io.ErrClosedPipe
	}

	var block *sharedBlock

	blocks := uintptr(unsafe.Pointer(rw.writeShared)) + sharedHeaderSize

	for {
		blockIndex := atomic.LoadUint32((*uint32)(&rw.writeShared.WriteStart))
		if blockIndex > uint32(rw.writeShared.BlockCount) {
			return Buffer{}, ErrInvalidSharedMemory
		}

		block = (*sharedBlock)(unsafe.Pointer(blocks + uintptr(uint64(blockIndex)*rw.fullBlockSize)))

		if uint32(block.Next) == atomic.LoadUint32((*uint32)(&rw.writeShared.ReadEnd)) {
			if err := ((*sem.Semaphore)(&rw.writeShared.SemAvail)).Wait(); err != nil {
				return Buffer{}, err
			}

			continue
		}

		if atomic.CompareAndSwapUint32((*uint32)(&rw.writeShared.WriteStart), blockIndex, uint32(block.Next)) {
			break
		}
	}

	data := (*[1 << 30]byte)(unsafe.Pointer(uintptr(unsafe.Pointer(block)) + blockHeaderSize))
	flags := (*[len(block.Flags)]byte)(unsafe.Pointer(&block.Flags[0]))
	return Buffer{
		block: block,
		write: true,

		Data:  data[:0:rw.writeShared.BlockSize],
		Flags: flags,
	}, nil
}

func (rw *ReadWriteCloser) SendWriteBuffer(buf Buffer) (n int, err error) {
	if atomic.LoadUint32(&rw.closed) != 0 {
		return 0, io.ErrClosedPipe
	}

	if !buf.write {
		return 0, ErrInvalidBuffer
	}

	block := buf.block

	*(*uint64)(&block.Size) = uint64(len(buf.Data))

	atomic.StoreUint32((*uint32)(&block.DoneWrite), 1)

	blocks := uintptr(unsafe.Pointer(rw.writeShared)) + sharedHeaderSize

	for {
		blockIndex := atomic.LoadUint32((*uint32)(&rw.writeShared.WriteEnd))
		if blockIndex > uint32(rw.writeShared.BlockCount) {
			return len(buf.Data), ErrInvalidSharedMemory
		}

		block = (*sharedBlock)(unsafe.Pointer(blocks + uintptr(uint64(blockIndex)*rw.fullBlockSize)))

		if !atomic.CompareAndSwapUint32((*uint32)(&block.DoneWrite), 1, 0) {
			return len(buf.Data), nil
		}

		atomic.CompareAndSwapUint32((*uint32)(&rw.writeShared.WriteEnd), blockIndex, uint32(block.Next))

		if blockIndex == atomic.LoadUint32((*uint32)(&rw.writeShared.ReadStart)) {
			if err := ((*sem.Semaphore)(&rw.writeShared.SemSignal)).Post(); err != nil {
				return len(buf.Data), err
			}
		}
	}
}
