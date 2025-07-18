package mkv

import (
	"time"

	"github.com/dzxiang/vdk/av"
)

type Stream struct {
	av.CodecData

	demuxer *Demuxer

	pid        uint16
	streamId   uint8
	streamType uint8

	idx int

	iskeyframe bool
	pts, dts   time.Duration
	data       []byte
	datalen    int
}
