// Command cdcstat measures how much of a chain of actions/cache entries a
// content-defined chunk store would upload: each entry's tar is split into
// chunks whose edges follow the content (a gear hash, as FastCDC does),
// and only the chunks no earlier entry had count as new.
//
//	cdcstat [-avg 64] <entry.tzst>...
//
// An entry can be a named pipe, which another process fills from the
// bucket, so a chain larger than the disk can be measured.
//
// The entries are given in the order they were saved. For each: its size
// as saved, its tar's size, how much of the tar is new, and what the new
// chunks take compressed one zstd frame each, as a store that serves an
// entry by joining its chunks' frames would keep them. The total line
// sums them, against uploading every entry whole.
package main

import (
	"crypto/sha256"
	"flag"
	"fmt"
	"io"
	"math/rand/v2"
	"os"

	"github.com/klauspost/compress/zstd"
)

func main() {
	avgKiB := flag.Int("avg", 64, "the chunks' average size, KiB; the least is a quarter of it and the most four times")
	flag.Parse()
	avg := *avgKiB << 10
	c := newChunker(avg/4, avg, avg*4)
	enc, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault), zstd.WithEncoderConcurrency(1))
	if err != nil {
		fail(err)
	}
	seen := map[[32]byte]bool{}
	var tSaved, tTar, tNew, tNewZ int64
	fmt.Printf("%-40s %9s %9s %9s %6s %9s\n", "entry", "saved", "tar", "new", "new%", "upload")
	for _, path := range flag.Args() {
		saved, tar, fresh, freshZ, err := measure(path, c, enc, seen)
		if err != nil {
			fail(fmt.Errorf("%s: %w", path, err))
		}
		tSaved, tTar, tNew, tNewZ = tSaved+saved, tTar+tar, tNew+fresh, tNewZ+freshZ
		fmt.Printf("%-40s %9s %9s %9s %5.0f%% %9s\n", base(path), mb(saved), mb(tar), mb(fresh), pct(fresh, tar), mb(freshZ))
	}
	fmt.Printf("%-40s %9s %9s %9s %5.0f%% %9s  (%.0f%% of uploading each entry whole; %d chunks)\n",
		"total", mb(tSaved), mb(tTar), mb(tNew), pct(tNew, tTar), mb(tNewZ), pct(tNewZ, tSaved), len(seen))
}

// measure chunks one entry, adding its chunks to seen.
func measure(path string, c *chunker, enc *zstd.Encoder, seen map[[32]byte]bool) (saved, tar, fresh, freshZ int64, err error) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	// Counted as read: an entry can come through a pipe, straight from the
	// bucket.
	cr := &counter{r: f}
	defer func() { saved = cr.n }()
	dec, err := zstd.NewReader(cr, zstd.WithDecoderConcurrency(1))
	if err != nil {
		return
	}
	defer dec.Close()
	var buf []byte
	err = c.split(dec, func(chunk []byte) {
		tar += int64(len(chunk))
		h := sha256.Sum256(chunk)
		if seen[h] {
			return
		}
		seen[h] = true
		fresh += int64(len(chunk))
		buf = enc.EncodeAll(chunk, buf[:0])
		freshZ += int64(len(buf))
	})
	return
}

// chunker splits a stream where a rolling gear hash of its last bytes
// matches a mask, between min and max bytes apart. The mask has more bits
// before avg than after it, so the sizes gather around avg (FastCDC's
// normalized chunking).
type chunker struct {
	min, avg, max int
	maskS, maskL  uint64
	gear          [256]uint64
}

func newChunker(min, avg, max int) *chunker {
	c := &chunker{min: min, avg: avg, max: max}
	bits := 0
	for 1<<bits < avg {
		bits++
	}
	c.maskS = spread(bits + 2)
	c.maskL = spread(bits - 2)
	// A fixed table, so every run cuts the same chunks.
	r := rand.New(rand.NewPCG(1, 2))
	for i := range c.gear {
		c.gear[i] = r.Uint64()
	}
	return c
}

// spread is a mask of n bits spread over the hash's top bits, which a gear
// hash mixes best.
func spread(n int) uint64 {
	var m uint64
	for i := 0; i < n; i++ {
		m |= 1 << (63 - 2*i%64)
	}
	return m
}

func (c *chunker) split(r io.Reader, emit func([]byte)) error {
	buf := make([]byte, 0, 4*c.max)
	eof := false
	for {
		for !eof && len(buf) < c.max {
			n, err := r.Read(buf[len(buf):cap(buf)])
			buf = buf[:len(buf)+n]
			if err == io.EOF {
				eof = true
			} else if err != nil {
				return err
			}
		}
		if len(buf) == 0 {
			return nil
		}
		n := c.cut(buf)
		emit(buf[:n])
		buf = buf[:copy(buf, buf[n:])]
	}
}

// cut is where the first chunk of b ends.
func (c *chunker) cut(b []byte) int {
	if len(b) <= c.min {
		return len(b)
	}
	end := min(len(b), c.max)
	var h uint64
	i := c.min
	for ; i < min(end, c.avg); i++ {
		h = h<<1 + c.gear[b[i]]
		if h&c.maskS == 0 {
			return i + 1
		}
	}
	for ; i < end; i++ {
		h = h<<1 + c.gear[b[i]]
		if h&c.maskL == 0 {
			return i + 1
		}
	}
	return end
}

type counter struct {
	r io.Reader
	n int64
}

func (c *counter) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

func base(p string) string {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' {
			return p[i+1:]
		}
	}
	return p
}

func mb(n int64) string { return fmt.Sprintf("%.1fM", float64(n)/(1<<20)) }

func pct(a, b int64) float64 {
	if b == 0 {
		return 0
	}
	return 100 * float64(a) / float64(b)
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "cdcstat:", err)
	os.Exit(1)
}
