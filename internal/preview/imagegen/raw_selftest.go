package imagegen

import (
	"encoding/binary"
	"fmt"
	"math"
)

// rawSelfTestDNG is a deterministic, uncompressed 128x96 CFA DNG. It contains
// no identifying metadata and exists only to prove the packaged decoder can
// perform a complete RAW-to-raster operation before readiness succeeds.
func rawSelfTestDNG() []byte {
	const width, height = 128, 96
	type entry struct {
		tag, dataType uint16
		count         uint32
		value         []byte
		offset        uint32
	}
	shorts := func(values ...uint16) []byte {
		data := make([]byte, len(values)*2)
		for index, value := range values {
			binary.LittleEndian.PutUint16(data[index*2:], value)
		}
		return data
	}
	longs := func(values ...uint32) []byte {
		data := make([]byte, len(values)*4)
		for index, value := range values {
			binary.LittleEndian.PutUint32(data[index*4:], value)
		}
		return data
	}
	srationals := func(values ...uint32) []byte {
		data := make([]byte, len(values)*4)
		for index, value := range values {
			binary.LittleEndian.PutUint32(data[index*4:], value)
		}
		return data
	}
	pixels := make([]byte, width*height*2)
	for y := range height {
		for x := range width {
			value := uint16(256 + (x*137+y*193)%3500)
			binary.LittleEndian.PutUint16(pixels[(y*width+x)*2:], value)
		}
	}
	entries := []entry{
		{tag: 254, dataType: 4, count: 1, value: longs(0)},
		{tag: 256, dataType: 4, count: 1, value: longs(width)},
		{tag: 257, dataType: 4, count: 1, value: longs(height)},
		{tag: 258, dataType: 3, count: 1, value: shorts(16)},
		{tag: 259, dataType: 3, count: 1, value: shorts(1)},
		{tag: 262, dataType: 3, count: 1, value: shorts(32803)},
		{tag: 271, dataType: 2, count: 10, value: []byte("EndlessFS\x00")},
		{tag: 272, dataType: 2, count: 18, value: []byte("Deterministic RAW\x00")},
		{tag: 273, dataType: 4, count: 1, value: longs(0)},
		{tag: 274, dataType: 3, count: 1, value: shorts(1)},
		{tag: 277, dataType: 3, count: 1, value: shorts(1)},
		{tag: 278, dataType: 4, count: 1, value: longs(height)},
		{tag: 279, dataType: 4, count: 1, value: longs(rawSelfTestUint32(len(pixels)))},
		{tag: 284, dataType: 3, count: 1, value: shorts(1)},
		{tag: 33421, dataType: 3, count: 2, value: shorts(2, 2)},
		{tag: 33422, dataType: 1, count: 4, value: []byte{0, 1, 1, 2}},
		{tag: 50706, dataType: 1, count: 4, value: []byte{1, 4, 0, 0}},
		{tag: 50707, dataType: 1, count: 4, value: []byte{1, 1, 0, 0}},
		{tag: 50708, dataType: 2, count: 28, value: []byte("EndlessFS Deterministic RAW\x00")},
		{tag: 50710, dataType: 1, count: 3, value: []byte{0, 1, 2}},
		{tag: 50711, dataType: 3, count: 1, value: shorts(1)},
		{tag: 50713, dataType: 3, count: 2, value: shorts(1, 1)},
		{tag: 50714, dataType: 5, count: 1, value: longs(0, 1)},
		{tag: 50717, dataType: 4, count: 1, value: longs(4095)},
		{tag: 50718, dataType: 5, count: 2, value: longs(1, 1, 1, 1)},
		{tag: 50719, dataType: 4, count: 2, value: longs(0, 0)},
		{tag: 50720, dataType: 4, count: 2, value: longs(width, height)},
		{tag: 50721, dataType: 10, count: 9, value: srationals(1, 1, 0, 1, 0, 1, 0, 1, 1, 1, 0, 1, 0, 1, 0, 1, 1, 1)},
		{tag: 50728, dataType: 5, count: 3, value: longs(1, 1, 1, 1, 1, 1)},
		{tag: 50778, dataType: 3, count: 1, value: shorts(21)},
		{tag: 50829, dataType: 4, count: 4, value: longs(0, 0, height, width)},
	}
	ifdSize := 2 + len(entries)*12 + 4
	nextOffset := rawSelfTestUint32(8 + ifdSize)
	align := func(value uint32) uint32 { return (value + 3) &^ 3 }
	for index := range entries {
		if len(entries[index].value) > 4 {
			entries[index].offset = nextOffset
			nextOffset = align(nextOffset + rawSelfTestUint32(len(entries[index].value)))
		}
	}
	pixelOffset := nextOffset
	for index := range entries {
		if entries[index].tag == 273 {
			entries[index].value = longs(pixelOffset)
		}
	}
	output := make([]byte, int(pixelOffset)+len(pixels))
	copy(output[:2], "II")
	binary.LittleEndian.PutUint16(output[2:4], 42)
	binary.LittleEndian.PutUint32(output[4:8], 8)
	binary.LittleEndian.PutUint16(output[8:10], rawSelfTestUint16(len(entries)))
	for index, item := range entries {
		offset := 10 + index*12
		binary.LittleEndian.PutUint16(output[offset:offset+2], item.tag)
		binary.LittleEndian.PutUint16(output[offset+2:offset+4], item.dataType)
		binary.LittleEndian.PutUint32(output[offset+4:offset+8], item.count)
		if len(item.value) <= 4 {
			copy(output[offset+8:offset+12], item.value)
		} else {
			binary.LittleEndian.PutUint32(output[offset+8:offset+12], item.offset)
			copy(output[item.offset:], item.value)
		}
	}
	copy(output[pixelOffset:], pixels)
	return output
}

func rawSelfTestUint32(value int) uint32 {
	converted := int64(value)
	if converted < 0 || converted > math.MaxUint32 {
		panic(fmt.Sprintf("RAW self-test fixture value %d exceeds uint32", value))
	}
	return uint32(converted)
}

func rawSelfTestUint16(value int) uint16 {
	converted := int64(value)
	if converted < 0 || converted > math.MaxUint16 {
		panic(fmt.Sprintf("RAW self-test fixture value %d exceeds uint16", value))
	}
	return uint16(converted)
}
