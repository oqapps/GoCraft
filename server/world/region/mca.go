package region

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"fmt"
	"io"
	"sync"

	"github.com/zeppelinmc/zeppelin/nbt"
)

type RegionFile struct {
	reader io.ReaderAt

	locations []byte

	chunks map[int32]*Chunk
	chu_mu sync.Mutex
}

func chunkLocation(l int32) (offset, size int32) {
	offset = ((l >> 8) & 0xFFFFFF)
	size = l & 0xFF

	return offset * 4096, size * 4096
}

var buffers = sync.Pool{
	New: func() any {
		return bytes.NewBuffer(make([]byte, 0, 1024*10))
	},
}

var Def Generator

func (r *RegionFile) GetChunk(x, z int32) (*Chunk, error) {
	c := Def.NewChunk(x, z)

	return &c, nil
	l := r.locations[((uint32(x)%32)+(uint32(z)%32)*32)*4:][:4]
	loc := int32(l[0])<<24 | int32(l[1])<<16 | int32(l[2])<<8 | int32(l[3])

	r.chu_mu.Lock()
	defer r.chu_mu.Unlock()
	if c, ok := r.chunks[loc]; ok {
		return c, nil
	}

	offset, size := chunkLocation(loc)
	if offset|size == 0 {
		return nil, fmt.Errorf("chunk %d %d not found", x, z)
	}

	var chunkHeader = make([]byte, 5)

	_, err := r.reader.ReadAt(chunkHeader, int64(offset))
	if err != nil {
		return nil, err
	}

	length := int32(chunkHeader[0])<<24 | int32(chunkHeader[1])<<16 | int32(chunkHeader[2])<<8 | int32(chunkHeader[3])
	compression := chunkHeader[4]

	var chunkData = make([]byte, length-1)
	_, err = r.reader.ReadAt(chunkData, int64(offset)+5)
	if err != nil {
		return nil, err
	}

	var rd io.ReadCloser

	switch compression {
	case 1:
		rd, err = gzip.NewReader(bytes.NewReader(chunkData))
		if err != nil {
			return nil, err
		}
		defer rd.Close()
	case 2:
		rd, err = zlib.NewReader(bytes.NewReader(chunkData))
		if err != nil {
			return nil, err
		}
		defer rd.Close()
	}

	buf := buffers.Get().(*bytes.Buffer)
	buf.Reset()
	buf.ReadFrom(rd)
	defer buffers.Put(buf)

	var chunk anvilChunk

	_, err = nbt.NewDecoder(buf).Decode(&chunk)

	r.chunks[loc] = &Chunk{
		X:          chunk.XPos,
		Y:          chunk.YPos,
		Z:          chunk.ZPos,
		Heightmaps: chunk.Heightmaps,
	}
	fmt.Println(len(chunk.Sections))

	r.chunks[loc].sections = make([]*Section, len(chunk.Sections))
	for i, sec := range chunk.Sections {
		r.chunks[loc].sections[i] = &Section{
			blockBitsPerEntry: blockBitsPerEntry(len(sec.BlockStates.Palette)),
			blockPalette:      sec.BlockStates.Palette,
			blockStates:       sec.BlockStates.Data,

			biomes:     sec.Biomes,
			y:          sec.Y,
			blockLight: sec.BlockLight,
			skyLight:   sec.SkyLight,
		}
	}

	return r.chunks[loc], err

	/*chunk, ok := r.chunks[loc]
	if !ok {
		return chunk, fmt.Errorf("not found chunk")
	}
	return chunk, nil*/
}

func DecodeRegion(r io.ReaderAt, f *RegionFile) error {
	var locationTable = make([]byte, 4096)

	_, err := r.ReadAt(locationTable, 0)
	if err != nil {
		return err
	}

	*f = RegionFile{
		reader: r,

		locations: locationTable,
		chunks:    make(map[int32]*Chunk),
	}

	/*var chunkBuffer = new(bytes.Buffer)

	for i := 0; i < 1024; i++ {
		loc := int32(locationTable[(i*4)+0])<<24 | int32(locationTable[(i*4)+1])<<16 | int32(locationTable[(i*4)+2])<<8 | int32(locationTable[(i*4)+3])

		offset, size := chunkLocation(loc)
		if offset == 0 && size == 0 {
			continue
		}
		var chunkHeader [5]byte
		if _, err := r.ReadAt(chunkHeader[:], int64(offset)); err != nil {
			return err
		}

		var length = binary.BigEndian.Uint32(chunkHeader[:4]) - 1
		var compressionScheme = chunkHeader[4]

		var chunkData = make([]byte, length-1)

		_, err = r.ReadAt(chunkData, int64(offset)+5)
		if err != nil {
			return err
		}

		var rd io.ReadCloser

		switch compressionScheme {
		case 1:
			rd, err = gzip.NewReader(bytes.NewReader(chunkData))
			if err != nil {
				return err
			}
			defer rd.Close()
		case 2:
			rd, err = zlib.NewReader(bytes.NewReader(chunkData))
			if err != nil {
				return err
			}
			defer rd.Close()
		}

		chunkBuffer.Reset()
		chunkBuffer.ReadFrom(rd)

		f.chunks[loc] = &Chunk{}

		_, err = nbt.NewDecoder(chunkBuffer).Decode(f.chunks[loc])
		if err != nil {
			return err
		}
	}*/

	return nil
}
