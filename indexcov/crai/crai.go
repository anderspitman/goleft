package main

import (
	"os"
	"compress/gzip"
        //"net/http"
	//arg "github.com/alexflint/go-arg"
	"bufio"
	"fmt"
	"io"
	"log"
	"strconv"
	"strings"
)

// note: for index cov, just need alnStart, alnSpan and sliceLen
// will need to scale sliceLen by 16384/alnSpan and then artifically partition
// into 16KB chunks?

// Slice holds the index information for a particular cram slice
type Slice struct {
	alnStart int64
	alnSpan  int64
	// Container start byte offset in the file
	containerStart int64
	// Slice start byte offset in the container data (‘blocks’)
	sliceStart int64
	sliceLen   int32
}

func (s Slice) Start() int64 {
	return s.alnStart
}

func (s Slice) SliceBytes() int32 {
	return s.sliceLen
}

func (s Slice) Span() int64 {
	return s.alnSpan
}

type Index struct {
	Slices [][]Slice
}

const TileWidth = 16384

func (idx *Index) Sizes() [][]int64 {
	refs := make([][]int64, len(idx.Slices))
	for i, s := range idx.Slices {
		refs[i] = idx.makeSizes(s)
	}
	return refs
}

// estimate the sizes (in arbitrary units of 16KB blocks from the cram index.
// the index has arbitrary slice sizes so this function interpolates the 16KB
// blocks.
func (idx *Index) makeSizes(slices []Slice) []int64 {
	// each slice may be hundreds of KB. This function splits those into 16KB chunks to match the
	// bam index. If we have e.g. start, end, size: 10000, 30000, 100
	// then we have to back fill from 0-10000
	if len(slices) == 0 {
		return nil
	}
	last := slices[len(slices)-1]
	if last.alnSpan < 0 {
		last.alnSpan = 0
	}
	if last.alnSpan > 1000000 {
		last.alnSpan = 0
	}

	sizes := make([]int64, 0, (last.Start()+last.Span()+TileWidth)/TileWidth)
	lastStart := int64(0)
	lastVal := int64(0)
	//unusedBytes := int32(0)

	for _, sl := range slices {
		// back fill gaps
		for k := 0; lastStart < sl.Start()-TileWidth; lastStart += TileWidth {
			if k == 0 {
				sizes = append(sizes, lastVal)
				lastVal = 0
			} else {
				sizes = append(sizes, 0)
			}
			k++
		}
		overhang := (sl.Start() - lastStart)
		if overhang > TileWidth {
			panic("tilewidth logic error")
		}
		for overhang < -TileWidth {
			// can get here with long reads if a read from the previous slice
			// extended > tileWidth into the next slice.
			// could get slightly better by taking average, but should be pretty close
			// as long as the cram slices are largish.
			sl.alnStart += TileWidth
			sl.alnSpan -= TileWidth
			overhang = (sl.Start() - lastStart)
		}
		if sl.alnSpan <= 0 {
			// if we did so much correction for overlapping bins above that alnSpan
			// becomes negative, then just skip this bin.
			continue
		}
		// 100000 is an arbitrary scalar to make sure we have enough resolution.
		perBase := int64(100000 * float64(sl.SliceBytes()) / float64(int64(sl.Span())))

		nTiles := int64(float64(sl.Span()) / float64(TileWidth))
		if nTiles == 0 && sl.Start()-lastStart < TileWidth {
			lastVal = perBase
			continue
		}

		for i := 0; i < int(nTiles); i++ {
			sizes = append(sizes, perBase)
		}
		cmp := int(sl.Start()+sl.Span()) / TileWidth
		if len(sizes) > cmp+1 || cmp < len(sizes)-1 {
			log.Println(len(sizes), cmp, overhang, sl.alnSpan)
			panic("logic error")
		}

		lastStart += TileWidth * nTiles
		lastVal = perBase
	}
	return sizes
}

func ReadIndex(r io.Reader) (*Index, error) {
	b := bufio.NewReader(r)

	idx := &Index{Slices: make([][]Slice, 0, 2)}
	iline := 1

	for line, err := b.ReadString('\n'); err == nil; line, err = b.ReadString('\n') {
		parts := strings.Split(strings.TrimSpace(line), "\t")
		if len(parts) != 6 {
			return nil, fmt.Errorf("crai: expected 6 fields in index, got %d at line %d", len(parts), iline)
		}

		si, err := strconv.Atoi(parts[0])
		if err != nil {
			return nil, fmt.Errorf("crai: unable to parse seqID (%s) at line %d", parts[0], iline)
		}
		if si == -1 {
			// TODO: handle unmapped.
			continue
		}
		for i := len(idx.Slices); i <= si; i++ {
			idx.Slices = append(idx.Slices, make([]Slice, 0, 16))
		}

		sl := Slice{}
		if alnStart, err := strconv.Atoi(parts[1]); err != nil {
			return nil, fmt.Errorf("crai: unable to parse alignment start (%s) at line %d", parts[1], iline)
		} else {
			sl.alnStart = int64(alnStart)
		}

		if alnSpan, err := strconv.Atoi(parts[2]); err != nil {
			return nil, fmt.Errorf("crai: unable to parse alignment span (%s) at line %d", parts[2], iline)
		} else {
			if alnSpan < 0 {
				log.Printf("crai: negative alnSpan in line %d: %s. breaking early.", iline, line)
				break
			}
			sl.alnSpan = int64(alnSpan)
		}

		if containerStart, err := strconv.Atoi(parts[3]); err != nil {
			return nil, fmt.Errorf("crai: unable to parse alignment container start (%s) at line %d", parts[3], iline)
		} else {
			sl.containerStart = int64(containerStart)
		}

		if sliceStart, err := strconv.Atoi(parts[4]); err != nil {
			return nil, fmt.Errorf("crai: unable to parse alignment slice start (%s) at line %d", parts[4], iline)
		} else {
			sl.sliceStart = int64(sliceStart)
		}

		if sliceLen, err := strconv.Atoi(parts[5]); err != nil {
			return nil, fmt.Errorf("crai: unable to parse alignment slice length (%s) at line %d", parts[5], iline)
		} else {
			sl.sliceLen = int32(sliceLen)
		}
		idx.Slices[si] = append(idx.Slices[si], sl)

		iline++
	}
	return idx, nil
}

//var cli = &struct {
//	Url string         `arg:"-i,required,help:CRAI URL"`
//}{}

func main() {

	//arg.MustParse(cli)

	//f, err := os.Open("index.crai")
	//if err != nil {
	//	panic(err)
	//}
        //gz, err := gzip.NewReader(f)
        //resp, err := http.Get("http://localhost:9001/index.crai")
        //resp, err := http.Get(cli.Url)
        gz, err := gzip.NewReader(os.Stdin)
        //gz, err := gzip.NewReader(resp.Body)
	if err != nil {
		panic(err)
	}
        idx, err := ReadIndex(gz)

        var currentOffset int64

	for refId, refSizes := range idx.Sizes() {
                fmt.Printf("#%d\n", refId)

                currentOffset = 0

                for _, size := range refSizes {
                        fmt.Printf("%d\t%d\n", currentOffset, size)
                        currentOffset += TileWidth
                }
        }
}
