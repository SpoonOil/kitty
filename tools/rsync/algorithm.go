// Algorithm found at: https://rsync.samba.org/tech_report/tech_report.html
// Code in this file is inspired by: https://github.com/jbreiding/rsync-go
//
// Definitions
//
//	Source: The final content.
//	Target: The content to be made into final content.
//	Signature: The sequence of hashes used to identify the content.
package rsync

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"hash"
	"io"
	"os"

	"github.com/zeebo/xxh3"
)

// If no BlockSize is specified in the rsync instance, this value is used.
const DefaultBlockSize = 1024 * 6

// Internal constant used in rolling checksum.
const _M = 1 << 16

// Operation Types.
type OpType byte // enum

const (
	OpBlock OpType = iota
	OpData
	OpHash
	OpBlockRange
)

type xxh3_128 struct {
	xxh3.Hasher
}

func (self *xxh3_128) Sum(b []byte) []byte {
	s := self.Sum128()
	pos := len(b)
	limit := pos + 16
	if limit > cap(b) {
		var x [16]byte
		b = append(b, x[:]...)
	} else {
		b = b[:limit]
	}
	binary.BigEndian.PutUint64(b[pos:], s.Hi)
	binary.BigEndian.PutUint64(b[pos+8:], s.Lo)
	return b
}

func new_xxh3_64() hash.Hash64 {
	ans := xxh3.New()
	ans.Reset()
	return ans
}

func new_xxh3_128() hash.Hash {
	ans := new(xxh3_128)
	ans.Reset()
	return ans
}

// Instruction to mutate target to align to source.
type Operation struct {
	Type          OpType
	BlockIndex    uint64
	BlockIndexEnd uint64
	Data          []byte

	serialized_repr []byte
}

var bin = binary.LittleEndian

func (self Operation) SerializeSize() int {
	switch self.Type {
	case OpBlock:
		return 9
	case OpBlockRange:
		return 13
	case OpHash:
		return 3 + len(self.Data)
	case OpData:
		return 5 + len(self.Data)
	}
	return -1
}

func (self Operation) Serialize(ans []byte) {
	if self.serialized_repr != nil {
		copy(ans, self.serialized_repr)
	}
	switch self.Type {
	case OpBlock:
		bin.PutUint64(ans[1:], self.BlockIndex)
	case OpBlockRange:
		bin.PutUint64(ans[1:], self.BlockIndex)
		bin.PutUint32(ans[9:], uint32(self.BlockIndexEnd-self.BlockIndex))
	case OpHash:
		bin.PutUint16(ans[1:], uint16(len(self.Data)))
		copy(ans[3:], self.Data)
	case OpData:
		bin.PutUint32(ans[1:], uint32(len(self.Data)))
		copy(ans[5:], self.Data)
	}
	ans[0] = byte(self.Type)
}

func (self *Operation) Unserialize(data []byte) (n int, err error) {
	if len(data) < 1 {
		return -1, io.ErrShortBuffer
	}
	switch OpType(data[0]) {
	case OpBlock:
		n = 9
		if len(data) < n {
			return -1, io.ErrShortBuffer
		}
		self.BlockIndex = bin.Uint64(data[1:])
		self.Data = nil
	case OpBlockRange:
		n = 13
		if len(data) < n {
			return -1, io.ErrShortBuffer
		}
		self.BlockIndex = bin.Uint64(data[1:])
		self.BlockIndexEnd = self.BlockIndex + uint64(bin.Uint32(data[9:]))
		self.Data = nil
	case OpHash:
		n = 3
		if len(data) < n {
			return -1, io.ErrShortBuffer
		}
		sz := int(bin.Uint16(data[1:]))
		n += sz
		if len(data) < n {
			return -1, io.ErrShortBuffer
		}
		self.Data = data[3:n]
	case OpData:
		n = 5
		if len(data) < n {
			return -1, io.ErrShortBuffer
		}
		sz := int(bin.Uint32(data[1:]))
		n += sz
		if len(data) < n {
			return -1, io.ErrShortBuffer
		}
		self.Data = data[5:n]
	default:
		return 0, fmt.Errorf("record has unknown operation type: %d", data[0])
	}
	self.Type = OpType(data[0])
	return
}

// Signature hash item generated from target.
type BlockHash struct {
	Index      uint64
	WeakHash   uint32
	StrongHash uint64
}

const BlockHashSize = 20

// Put the serialization of this BlockHash to output
func (self BlockHash) Serialize(output []byte) {
	bin.PutUint64(output, self.Index)
	bin.PutUint32(output[8:], self.WeakHash)
	bin.PutUint64(output[12:], self.StrongHash)
}

func (self *BlockHash) Unserialize(data []byte) (err error) {
	if len(data) < 20 {
		return fmt.Errorf("record too small to be a BlockHash: %d < %d", len(data), 20)
	}
	self.Index = bin.Uint64(data)
	self.WeakHash = bin.Uint32(data[8:])
	self.StrongHash = bin.Uint64(data[12:])
	return
}

// Write signatures as they are generated.
type OperationWriter func(op Operation) error

// Properties to use while working with the rsync algorithm.
// A single rsync should not be used concurrently as it may contain
// internal buffers and hash sums.
type rsync struct {
	BlockSize int

	// This must be non-nil before using any functions
	hasher                  hash.Hash64
	hasher_constructor      func() hash.Hash64
	checksummer_constructor func() hash.Hash
	checksummer             hash.Hash
	buffer                  []byte
}

func (r *rsync) SetHasher(c func() hash.Hash64) {
	r.hasher_constructor = c
	r.hasher = c()
}

func (r *rsync) SetChecksummer(c func() hash.Hash) {
	r.checksummer_constructor = c
	r.checksummer = c()
}

// If the target length is known the number of hashes in the
// signature can be determined.
func (r *rsync) BlockHashCount(targetLength int64) (count int64) {
	bs := int64(r.BlockSize)
	count = targetLength / bs
	if targetLength%bs != 0 {
		count++
	}
	return
}

type signature_iterator struct {
	hasher hash.Hash64
	buffer []byte
	src    io.Reader
	rc     rolling_checksum
	index  uint64
}

// ans is valid only iff err == nil
func (self *signature_iterator) next() (ans BlockHash, err error) {
	n, err := io.ReadAtLeast(self.src, self.buffer, cap(self.buffer))
	switch err {
	case io.ErrUnexpectedEOF, io.EOF, nil:
		err = nil
	default:
		return
	}
	if n == 0 {
		return ans, io.EOF
	}
	b := self.buffer[:n]
	self.hasher.Reset()
	self.hasher.Write(b)
	ans = BlockHash{Index: self.index, WeakHash: self.rc.full(b), StrongHash: self.hasher.Sum64()}
	self.index++
	return

}

// Calculate the signature of target.
func (r *rsync) CreateSignatureIterator(target io.Reader) func() (BlockHash, error) {
	return (&signature_iterator{
		hasher: r.hasher_constructor(), buffer: make([]byte, r.BlockSize), src: target,
	}).next
}

// Apply the difference to the target.
func (r *rsync) ApplyDelta(alignedTarget io.Writer, target io.ReadSeeker, op Operation) error {
	var err error
	var n int
	var block []byte

	r.set_buffer_to_size(r.BlockSize)
	buffer := r.buffer
	if r.checksummer == nil {
		r.checksummer = r.checksummer_constructor()
	}

	write_block := func(op Operation) error {
		if _, err = target.Seek(int64(r.BlockSize*int(op.BlockIndex)), os.SEEK_SET); err != nil {
			return err
		}
		n, err = io.ReadAtLeast(target, buffer, r.BlockSize)
		if err != nil {
			if err != io.ErrUnexpectedEOF {
				return err
			}
		}
		block = buffer[:n]
		r.checksummer.Write(block)
		_, err = alignedTarget.Write(block)
		if err != nil {
			return err
		}
		return nil
	}

	switch op.Type {
	case OpBlockRange:
		for i := op.BlockIndex; i <= op.BlockIndexEnd; i++ {
			err = write_block(Operation{
				Type:       OpBlock,
				BlockIndex: i,
			})
			if err != nil {
				if err == io.EOF {
					break
				}
				return err
			}
		}
	case OpBlock:
		err = write_block(op)
		if err != nil {
			if err == io.EOF {
				break
			}
			return err
		}
	case OpData:
		r.checksummer.Write(op.Data)
		_, err = alignedTarget.Write(op.Data)
		if err != nil {
			return err
		}
	case OpHash:
		expected := r.checksummer.Sum(nil)
		if !bytes.Equal(expected, op.Data) {
			return fmt.Errorf("Failed to verify overall file checksum. This usually happens if some data was corrupted in transit or one of the involved files was altered while the transfer was in progress.")
		}
	}
	return nil
}

func (r *rsync) set_buffer_to_size(sz int) {
	if cap(r.buffer) < sz {
		r.buffer = make([]byte, sz)
	} else {
		r.buffer = r.buffer[:sz]
	}
}

type node struct {
	op   *Operation
	next *node
}

type list struct {
	head *node
}

func (self *list) push_back(op *Operation) {
	n := &node{op: op}
	n.next = self.head
	self.head = n
}

func (self *list) is_empty() bool { return self.head == nil }

func (self *list) front() *Operation {
	for c := self.head; c != nil; c = c.next {
		if c.next == nil {
			return c.op
		}
	}
	return nil
}

func (self *list) pop_front() *Operation {
	c := self.head
	var prev *node
	for c != nil {
		if c.next == nil {
			if prev == nil {
				self.head = nil
			} else {
				prev.next = nil
			}
			return c.op
		}
		prev = c
		c = c.next
	}
	return nil
}

// see https://rsync.samba.org/tech_report/node3.html
type rolling_checksum struct {
	alpha, beta, val, l           uint32
	first_byte_of_previous_window uint32
}

func (self *rolling_checksum) full(data []byte) uint32 {
	var alpha, beta uint32
	self.l = uint32(len(data)) // actually should be len(data) - 1 but the equations always use l+1
	for i, b := range data {
		alpha += uint32(b)
		beta += (self.l - uint32(i)) * uint32(b)
	}
	self.first_byte_of_previous_window = uint32(data[0])
	self.alpha = alpha % _M
	self.beta = beta % _M
	self.val = self.alpha + _M*self.beta
	return self.val
}

func (self *rolling_checksum) add_one_byte(first_byte, last_byte byte) {
	self.alpha = (self.alpha - self.first_byte_of_previous_window + uint32(last_byte)) % _M
	self.beta = (self.beta - (self.l)*self.first_byte_of_previous_window + self.alpha) % _M
	self.val = self.alpha + _M*self.beta
	self.first_byte_of_previous_window = uint32(first_byte)
}

type diff struct {
	buffer []byte
	// A single β hash may correlate with many unique hashes.
	hash_lookup map[uint32][]BlockHash
	source      io.Reader
	hasher      hash.Hash64
	checksummer hash.Hash

	window, data struct{ pos, sz int }
	block_size   int
	finished     bool
	rc           rolling_checksum

	pending_op *Operation
	ready_ops  list
}

func (self *diff) Next() (op *Operation, err error) {
	if self.ready_ops.is_empty() {
		if err = self.pump_till_op_available(); err != nil {
			return
		}
	}
	return self.ready_ops.pop_front(), nil
}

func (self *diff) hash(b []byte) uint64 {
	self.hasher.Reset()
	self.hasher.Write(b)
	return self.hasher.Sum64()
}

// Combine OpBlock into OpBlockRange. To do this store the previous
// non-data operation and determine if it can be extended.
func (self *diff) enqueue(op Operation) {
	switch op.Type {
	case OpBlock:
		if self.pending_op != nil {
			switch self.pending_op.Type {
			case OpBlock:
				if self.pending_op.BlockIndex+1 == op.BlockIndex {
					self.pending_op = &Operation{
						Type:          OpBlockRange,
						BlockIndex:    self.pending_op.BlockIndex,
						BlockIndexEnd: op.BlockIndex,
					}
					return
				}
			case OpBlockRange:
				if self.pending_op.BlockIndexEnd+1 == op.BlockIndex {
					self.pending_op.BlockIndexEnd = op.BlockIndex
					return
				}
			}
			self.ready_ops.push_back(self.pending_op)
			self.pending_op = nil
		}
		self.pending_op = &op
	case OpData, OpHash:
		if self.pending_op != nil {
			self.ready_ops.push_back(self.pending_op)
			self.pending_op = nil
		}
		self.ready_ops.push_back(&op)
	}
	return

}

func (self *diff) send_data() {
	if self.data.sz > 0 {
		data := self.buffer[self.data.pos : self.data.pos+self.data.sz]
		srepr := make([]byte, len(data)+5)
		copy(srepr[5:], data)
		bin.PutUint32(srepr[1:], uint32(len(data)))
		srepr[0] = byte(OpData)
		op := Operation{Type: OpData, Data: srepr[5:], serialized_repr: srepr}
		self.enqueue(op)
		self.data.pos += self.data.sz
		self.data.sz = 0
	}
}

func (self *diff) pump_till_op_available() error {
	for self.ready_ops.is_empty() && !self.finished {
		if err := self.read_at_least_one_operation(); err != nil {
			return err
		}
	}
	if self.finished && self.pending_op != nil {
		self.ready_ops.push_back(self.pending_op)
		self.pending_op = nil
	}
	return nil
}

func (self *diff) ensure_idx_valid(idx int) (ok bool, err error) {
	if idx < len(self.buffer) {
		return true, nil
	}
	if idx >= cap(self.buffer) {
		// need to wrap the buffer, so send off any data present behind the window
		self.send_data()
		// copy the window and any data present after it to the start of the buffer
		distance_from_window_pos := idx - self.window.pos
		amt_to_copy := len(self.buffer) - self.window.pos
		copy(self.buffer, self.buffer[self.window.pos:self.window.pos+amt_to_copy])
		self.buffer = self.buffer[:amt_to_copy]
		self.window.pos = 0
		self.data.pos = 0
		return self.ensure_idx_valid(distance_from_window_pos)
	}
	extra := idx - len(self.buffer) + 1
	var n int
	n, err = io.ReadAtLeast(self.source, self.buffer[len(self.buffer):cap(self.buffer)], extra)
	block := self.buffer[len(self.buffer):][:n]
	switch err {
	case nil:
		ok = true
		self.buffer = self.buffer[:len(self.buffer)+n]
		self.checksummer.Write(block)
	case io.ErrUnexpectedEOF, io.EOF:
		err = nil
		self.buffer = self.buffer[:len(self.buffer)+n]
		self.checksummer.Write(block)
	}
	return
}

func (self *diff) finish_up() {
	self.send_data()
	self.data.pos = self.window.pos
	self.data.sz = len(self.buffer) - self.window.pos
	self.send_data()
	self.enqueue(Operation{Type: OpHash, Data: self.checksummer.Sum(nil)})
	self.finished = true
}

// See https://rsync.samba.org/tech_report/node4.html for the design of this algorithm
func (self *diff) read_at_least_one_operation() (err error) {
	if self.window.sz > 0 {
		if ok, err := self.ensure_idx_valid(self.window.pos + self.window.sz); !ok {
			if err != nil {
				return err
			}
			self.finish_up()
			return nil
		}
		self.window.pos++
		self.data.sz++
		self.rc.add_one_byte(self.buffer[self.window.pos], self.buffer[self.window.pos+self.window.sz-1])
	} else {
		if ok, err := self.ensure_idx_valid(self.window.pos + self.block_size - 1); !ok {
			if err != nil {
				return err
			}
			self.finish_up()
			return nil
		}
		self.window.sz = self.block_size
		self.rc.full(self.buffer[self.window.pos : self.window.pos+self.window.sz])
	}
	found_hash := false
	var block_index uint64
	if hh, ok := self.hash_lookup[self.rc.val]; ok {
		block_index, found_hash = find_hash(hh, self.hash(self.buffer[self.window.pos:self.window.pos+self.window.sz]))
	}
	if found_hash {
		self.send_data()
		self.enqueue(Operation{Type: OpBlock, BlockIndex: block_index})
		self.window.pos += self.window.sz
		self.data.pos = self.window.pos
		self.window.sz = 0
	}
	return nil
}

func (r *rsync) CreateDiff(source io.Reader, signature []BlockHash) func() (*Operation, error) {
	ans := &diff{
		block_size: r.BlockSize, buffer: make([]byte, 0, (r.BlockSize * 8)),
		hash_lookup: make(map[uint32][]BlockHash, len(signature)),
		source:      source, hasher: r.hasher_constructor(),
		checksummer: r.checksummer_constructor(),
	}
	for _, h := range signature {
		key := h.WeakHash
		ans.hash_lookup[key] = append(ans.hash_lookup[key], h)
	}

	return ans.Next
}

func (r *rsync) CreateDelta(source io.Reader, signature []BlockHash, ops OperationWriter) (err error) {
	diff := r.CreateDiff(source, signature)
	var op *Operation
	for {
		op, err = diff()
		if op == nil {
			return
		}
		if err = ops(*op); err != nil {
			return err
		}
	}
}

// Use a more unique way to identify a set of bytes.
func (r *rsync) hash(v []byte) uint64 {
	r.hasher.Reset()
	r.hasher.Write(v)
	return r.hasher.Sum64()
}

func (r *rsync) HashSize() int      { return r.hasher.Size() }
func (r *rsync) HashBlockSize() int { return r.hasher.BlockSize() }
func (r *rsync) HasHasher() bool    { return r.hasher != nil }

// Searches for a given strong hash among all strong hashes in this bucket.
func find_hash(hh []BlockHash, hv uint64) (uint64, bool) {
	for _, block := range hh {
		if block.StrongHash == hv {
			return block.Index, true
		}
	}
	return 0, false
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
