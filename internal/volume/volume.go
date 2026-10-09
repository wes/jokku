// Package volume copies a disk image to another node, sending only the
// blocks the receiving copy lacks. A local volume follows its instance this
// way: the new node copies the disk while the app keeps running, then, once
// the app has stopped, a final pass sends just the blocks written since.
//
//	receiver --POST summary of its copy (block -> SHA-256)--> owner
//	receiver <-- header, differing and zeroed blocks, end --- owner
//
// Holes and all-zero blocks are never sent, so sparse disks stay sparse.
// Every block carries its hash, which the receiver checks before writing it.
package volume

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
)

// BlockSize is the unit of comparison and transfer.
const BlockSize = 1 << 20

// ErrBusy is the owner's answer to a final pass while the disk is still
// attached to a running VM.
var ErrBusy = errors.New("the volume is still attached to a running instance")

const (
	summaryMagic = "JKSUM1"
	streamMagic  = "JKVOL1"

	recData = 'D'
	recZero = 'Z'
	recEnd  = 'E'
)

var zeros = make([]byte, BlockSize)

// Summary maps each block holding data to the SHA-256 of its contents.
// Blocks not listed are holes or all zeros.
type Summary map[int64][32]byte

// Stats counts what one pass transferred.
type Stats struct {
	Blocks int   // blocks sent
	Bytes  int64 // their size
	Zeroed int   // blocks the receiver had to clear
}

// Summarize reads the blocks of a disk image that hold data.
func Summarize(f *os.File) (Summary, error) {
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	size := st.Size()
	sum := Summary{}
	buf := make([]byte, BlockSize)
	err = eachDataBlock(f, size, func(i int64) error {
		b, err := readBlock(f, buf, i, size)
		if err != nil {
			return err
		}
		if !isZero(b) {
			sum[i] = sha256.Sum256(b)
		}
		return nil
	})
	return sum, err
}

// EachBlock calls fn, in order, with each block of f that holds data; holes
// and all-zero blocks are skipped. b is reused between calls. It returns f's
// size.
func EachBlock(f *os.File, fn func(i int64, b []byte) error) (int64, error) {
	st, err := f.Stat()
	if err != nil {
		return 0, err
	}
	size := st.Size()
	buf := make([]byte, BlockSize)
	return size, eachDataBlock(f, size, func(i int64) error {
		b, err := readBlock(f, buf, i, size)
		if err != nil || isZero(b) {
			return err
		}
		return fn(i, b)
	})
}

// WriteSummary encodes a summary for the owner.
func WriteSummary(w io.Writer, sum Summary) error {
	idx := make([]int64, 0, len(sum))
	for i := range sum {
		idx = append(idx, i)
	}
	sort.Slice(idx, func(a, b int) bool { return idx[a] < idx[b] })
	bw := bufio.NewWriter(w)
	bw.WriteString(summaryMagic)
	binary.Write(bw, binary.BigEndian, uint64(len(idx)))
	for _, i := range idx {
		h := sum[i]
		binary.Write(bw, binary.BigEndian, uint64(i))
		bw.Write(h[:])
	}
	return bw.Flush()
}

// ReadSummary decodes what a receiver sent.
func ReadSummary(r io.Reader) (Summary, error) {
	br := bufio.NewReader(r)
	magic := make([]byte, len(summaryMagic))
	if _, err := io.ReadFull(br, magic); err != nil || string(magic) != summaryMagic {
		return nil, errors.New("not a volume summary")
	}
	var n uint64
	if err := binary.Read(br, binary.BigEndian, &n); err != nil {
		return nil, err
	}
	sum := make(Summary, min(n, 1<<20))
	for range n {
		var i uint64
		var h [32]byte
		if err := binary.Read(br, binary.BigEndian, &i); err != nil {
			return nil, err
		}
		if _, err := io.ReadFull(br, h[:]); err != nil {
			return nil, err
		}
		sum[int64(i)] = h
	}
	return sum, nil
}

// Send writes everything a receiver holding have needs to match src: the
// header, each block that differs, each block it must clear, and the end
// marker. src must not change during a final pass.
func Send(w io.Writer, have Summary, src *os.File) (Stats, error) {
	var stats Stats
	st, err := src.Stat()
	if err != nil {
		return stats, err
	}
	size := st.Size()
	bw := bufio.NewWriterSize(w, 256<<10)
	bw.WriteString(streamMagic)
	binary.Write(bw, binary.BigEndian, uint64(size))
	binary.Write(bw, binary.BigEndian, uint32(BlockSize))

	stale := make(map[int64]bool, len(have)) // blocks the receiver has that src may not
	for i := range have {
		stale[i] = true
	}
	records := uint64(0)
	buf := make([]byte, BlockSize)
	err = eachDataBlock(src, size, func(i int64) error {
		b, err := readBlock(src, buf, i, size)
		if err != nil {
			return err
		}
		if isZero(b) {
			return nil // cleared below if the receiver has data there
		}
		delete(stale, i)
		h := sha256.Sum256(b)
		if have[i] == h {
			return nil
		}
		bw.WriteByte(recData)
		binary.Write(bw, binary.BigEndian, uint64(i))
		binary.Write(bw, binary.BigEndian, uint32(len(b)))
		bw.Write(h[:])
		if _, err := bw.Write(b); err != nil {
			return err
		}
		records++
		stats.Blocks++
		stats.Bytes += int64(len(b))
		return nil
	})
	if err != nil {
		return stats, err
	}
	for i := range stale {
		if i*BlockSize >= size {
			continue // cut off by the final truncate
		}
		bw.WriteByte(recZero)
		binary.Write(bw, binary.BigEndian, uint64(i))
		records++
		stats.Zeroed++
	}
	bw.WriteByte(recEnd)
	binary.Write(bw, binary.BigEndian, records)
	return stats, bw.Flush()
}

// Receive applies a Send stream to dst and keeps sum, dst's summary, up to
// date. dst matches the owner's disk only if it returns nil.
func Receive(r io.Reader, dst *os.File, sum Summary, progress func(int64)) (Stats, error) {
	var stats Stats
	br := bufio.NewReaderSize(r, 256<<10)
	magic := make([]byte, len(streamMagic))
	if _, err := io.ReadFull(br, magic); err != nil || string(magic) != streamMagic {
		return stats, errors.New("the owner did not send a volume")
	}
	var size uint64
	var block uint32
	if err := binary.Read(br, binary.BigEndian, &size); err != nil {
		return stats, err
	}
	if err := binary.Read(br, binary.BigEndian, &block); err != nil {
		return stats, err
	}
	if block != BlockSize {
		return stats, fmt.Errorf("the owner uses %d-byte blocks, this node %d", block, BlockSize)
	}
	if st, err := dst.Stat(); err != nil {
		return stats, err
	} else if st.Size() < int64(size) {
		if err := dst.Truncate(int64(size)); err != nil {
			return stats, err
		}
	}
	buf := make([]byte, BlockSize)
	records := uint64(0)
	for {
		kind, err := br.ReadByte()
		if err != nil {
			return stats, cutOff(err)
		}
		switch kind {
		case recData:
			var i uint64
			var n uint32
			var h [32]byte
			if err := binary.Read(br, binary.BigEndian, &i); err != nil {
				return stats, cutOff(err)
			}
			if err := binary.Read(br, binary.BigEndian, &n); err != nil {
				return stats, cutOff(err)
			}
			if n > BlockSize {
				return stats, fmt.Errorf("block %d is %d bytes, more than a block", i, n)
			}
			if _, err := io.ReadFull(br, h[:]); err != nil {
				return stats, cutOff(err)
			}
			if _, err := io.ReadFull(br, buf[:n]); err != nil {
				return stats, cutOff(err)
			}
			if sha256.Sum256(buf[:n]) != h {
				return stats, fmt.Errorf("block %d arrived corrupted", i)
			}
			if _, err := dst.WriteAt(buf[:n], int64(i)*BlockSize); err != nil {
				return stats, err
			}
			sum[int64(i)] = h
			stats.Blocks++
			stats.Bytes += int64(n)
			if progress != nil {
				progress(stats.Bytes)
			}
		case recZero:
			var i uint64
			if err := binary.Read(br, binary.BigEndian, &i); err != nil {
				return stats, cutOff(err)
			}
			off := int64(i) * BlockSize
			if err := zeroRange(dst, off, min(BlockSize, int64(size)-off)); err != nil {
				return stats, err
			}
			delete(sum, int64(i))
			stats.Zeroed++
		case recEnd:
			var n uint64
			if err := binary.Read(br, binary.BigEndian, &n); err != nil {
				return stats, cutOff(err)
			}
			if n != records {
				return stats, fmt.Errorf("received %d blocks, the owner sent %d", records, n)
			}
			if err := dst.Truncate(int64(size)); err != nil {
				return stats, err
			}
			for i := range sum {
				if i*BlockSize >= int64(size) {
					delete(sum, i)
				}
			}
			return stats, dst.Sync()
		default:
			return stats, fmt.Errorf("unexpected record %q in the volume stream", kind)
		}
		records++
	}
}

func cutOff(err error) error {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return errors.New("the copy was cut off")
	}
	return err
}

// Puller keeps a local copy of a disk another node serves up to date. It
// remembers what the copy holds between passes, so later passes only cost
// the owner's read.
type Puller struct {
	Client *http.Client
	URL    string // the owner's endpoint
	Token  string
	Path   string // the local copy, created if needed
	// Progress is told the bytes received so far in the current pass.
	Progress func(int64)

	sum Summary
}

// Pull runs one pass. A final pass asks for a consistent copy, which the
// owner refuses with ErrBusy while the disk is attached to a running VM.
func (p *Puller) Pull(ctx context.Context, final bool) (Stats, error) {
	f, err := os.OpenFile(p.Path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return Stats{}, err
	}
	defer f.Close()
	if p.sum == nil {
		if p.sum, err = Summarize(f); err != nil {
			return Stats{}, err
		}
	}
	var body bytes.Buffer
	if err := WriteSummary(&body, p.sum); err != nil {
		return Stats{}, err
	}
	url := p.URL
	if final {
		url += "?final=1"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, &body)
	if err != nil {
		return Stats{}, err
	}
	req.Header.Set("Authorization", "Bearer "+p.Token)
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := p.Client.Do(req)
	if err != nil {
		return Stats{}, err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusConflict:
		return Stats{}, ErrBusy
	case resp.StatusCode != http.StatusOK:
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return Stats{}, fmt.Errorf("the owner answered %s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	stats, err := Receive(resp.Body, f, p.sum, p.Progress)
	if err != nil {
		p.sum = nil // the copy is partly updated; summarize it again next time
	}
	return stats, err
}

func readBlock(f *os.File, buf []byte, i, size int64) ([]byte, error) {
	off := i * BlockSize
	n := min(BlockSize, size-off)
	got, err := f.ReadAt(buf[:n], off)
	if int64(got) == n {
		return buf[:n], nil
	}
	if err == nil || errors.Is(err, io.EOF) {
		// The file shrank while being read; what is gone reads as zeros.
		clear(buf[got:n])
		return buf[:n], nil
	}
	return nil, err
}

func isZero(b []byte) bool { return bytes.Equal(b, zeros[:len(b)]) }

func writeZeros(f *os.File, off, n int64) error {
	_, err := f.WriteAt(zeros[:n], off)
	return err
}

// allBlocks visits every block from first on.
func allBlocks(size, first int64, fn func(int64) error) error {
	for i := first; i*BlockSize < size; i++ {
		if err := fn(i); err != nil {
			return err
		}
	}
	return nil
}
