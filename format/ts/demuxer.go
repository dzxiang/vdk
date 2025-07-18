package ts

import (
	"bufio"
	"fmt"
	"io"
	"time"

	"github.com/dzxiang/vdk/av"
	"github.com/dzxiang/vdk/codec/aacparser"
	"github.com/dzxiang/vdk/codec/h264parser"
	"github.com/dzxiang/vdk/codec/mjpeg"
	"github.com/dzxiang/vdk/format/ts/tsio"
	"github.com/dzxiang/vdk/utils/bits/pio"
)

type Demuxer struct {
	r *bufio.Reader

	pkts []av.Packet

	pat     *tsio.PAT
	pmt     *tsio.PMT
	streams []*Stream
	tshdr   []byte
	AnnexB  bool
	stage   int
}

func NewDemuxer(r io.Reader) *Demuxer {
	return &Demuxer{
		tshdr: make([]byte, 188),
		r:     bufio.NewReaderSize(r, pio.RecommendBufioSize),
	}
}

func (self *Demuxer) Streams() (streams []av.CodecData, err error) {
	if err = self.probe(); err != nil {
		return
	}
	for _, stream := range self.streams {
		streams = append(streams, stream.CodecData)
	}
	return
}

func (self *Demuxer) probe() (err error) {
	if self.stage == 0 {
		for {
			if self.pmt != nil {
				n := 0
				for _, stream := range self.streams {
					if stream.CodecData != nil {
						n++
					}
				}
				if n == len(self.streams) {
					break
				}
			}
			if err = self.poll(); err != nil {
				return
			}
		}
		self.stage++
	}
	return
}

func (self *Demuxer) ReadPacket() (pkt av.Packet, err error) {
	if err = self.probe(); err != nil {
		return
	}

	for len(self.pkts) == 0 {
		if err = self.poll(); err != nil {
			return
		}
	}

	pkt = self.pkts[0]
	self.pkts = self.pkts[1:]
	return
}

func (self *Demuxer) poll() (err error) {
	if err = self.readTSPacket(); err == io.EOF {
		var n int
		if n, err = self.payloadEnd(); err != nil {
			return
		}
		if n == 0 {
			err = io.EOF
		}
	}
	return
}

func (self *Demuxer) initPMT(payload []byte) (err error) {
	var psihdrlen int
	var datalen int
	if _, _, psihdrlen, datalen, err = tsio.ParsePSI(payload); err != nil {
		return
	}
	self.pmt = &tsio.PMT{}
	if _, err = self.pmt.Unmarshal(payload[psihdrlen : psihdrlen+datalen]); err != nil {
		return
	}

	self.streams = []*Stream{}
	for i, info := range self.pmt.ElementaryStreamInfos {
		stream := &Stream{}
		stream.idx = i
		stream.demuxer = self
		stream.pid = info.ElementaryPID
		stream.streamType = info.StreamType
		switch info.StreamType {
		case tsio.ElementaryStreamTypeH264:
			self.streams = append(self.streams, stream)
		case tsio.ElementaryStreamTypeAdtsAAC:
			self.streams = append(self.streams, stream)
		case tsio.ElementaryStreamTypeAlignmentDescriptor:
			self.streams = append(self.streams, stream)
		}
	}
	return
}

func (self *Demuxer) payloadEnd() (n int, err error) {
	for _, stream := range self.streams {
		var i int
		if i, err = stream.payloadEnd(); err != nil {
			return
		}
		n += i
	}
	return
}

func (self *Demuxer) readTSPacket() (err error) {
	var hdrlen int
	var pid uint16
	var start bool
	var iskeyframe bool

	if _, err = io.ReadFull(self.r, self.tshdr); err != nil {
		return
	}

	if pid, start, iskeyframe, hdrlen, err = tsio.ParseTSHeader(self.tshdr); err != nil {
		return
	}
	payload := self.tshdr[hdrlen:]

	if self.pat == nil {
		if pid == 0 {
			var psihdrlen int
			var datalen int
			if _, _, psihdrlen, datalen, err = tsio.ParsePSI(payload); err != nil {
				return
			}
			self.pat = &tsio.PAT{}
			if _, err = self.pat.Unmarshal(payload[psihdrlen : psihdrlen+datalen]); err != nil {
				return
			}
		}
	} else if self.pmt == nil {
		for _, entry := range self.pat.Entries {
			if entry.ProgramMapPID == pid {
				if err = self.initPMT(payload); err != nil {
					return
				}
				break
			}
		}
	} else {
		for _, stream := range self.streams {
			if pid == stream.pid {
				if stream.streamType == tsio.ElementaryStreamTypeAdtsAAC {
					iskeyframe = false
				}
				if err = stream.handleTSPacket(start, iskeyframe, payload); err != nil {
					return
				}
				break
			}
		}
	}

	return
}

func (self *Stream) addPacket(payload []byte, timedelta time.Duration, fixed time.Duration) {
	dts := self.dts
	pts := self.pts

	if dts == 0 {
		dts = pts
	}

	dur := time.Duration(0)

	if self.pt > 0 {
		dur = dts + timedelta - self.pt
	} else {
		dur = fixed
	}

	self.pt = dts + timedelta

	demuxer := self.demuxer
	pkt := av.Packet{
		Idx:        int8(self.idx),
		IsKeyFrame: self.iskeyframe,
		Time:       dts + timedelta,
		Data:       payload,
		Duration:   dur,
	}
	if pts != dts {
		pkt.CompositionTime = pts - dts
	}
	demuxer.pkts = append(demuxer.pkts, pkt)
}

func (self *Stream) payloadEnd() (n int, err error) {
	payload := self.data
	if payload == nil {
		return
	}
	if self.datalen != 0 && len(payload) != self.datalen {
		err = fmt.Errorf("ts: packet size mismatch size=%d correct=%d", len(payload), self.datalen)
		return
	}
	self.data = nil
	switch self.streamType {
	case tsio.ElementaryStreamTypeAlignmentDescriptor:
		if self.CodecData == nil {
			self.CodecData = mjpeg.CodecData{}
		}
		b := make([]byte, 4+len(payload))
		pio.PutU32BE(b[0:4], uint32(len(payload)))
		copy(b[4:], payload)
		self.addPacket(b, time.Duration(0), 0)
		n++
	case tsio.ElementaryStreamTypeAdtsAAC:
		var config aacparser.MPEG4AudioConfig

		delta := time.Duration(0)
		for len(payload) > 0 {
			var hdrlen, framelen, samples int
			if config, hdrlen, framelen, samples, err = aacparser.ParseADTSHeader(payload); err != nil {
				return
			}
			if self.CodecData == nil {
				if self.CodecData, err = aacparser.NewCodecDataFromMPEG4AudioConfig(config); err != nil {
					return
				}
			}
			self.addPacket(payload[hdrlen:framelen], delta, time.Duration(samples)*time.Second/time.Duration(config.SampleRate))
			n++
			delta += time.Duration(samples) * time.Second / time.Duration(config.SampleRate)
			payload = payload[framelen:]
		}

	case tsio.ElementaryStreamTypeH264:
		nalus, _ := h264parser.SplitNALUs(payload)
		var sps, pps []byte

		for _, nalu := range nalus {
			if len(nalu) > 0 {
				naltype := nalu[0] & 0x1f
				switch {
				case naltype == 7:
					sps = nalu
					info, err := h264parser.ParseSPS(sps)
					if err == nil {
						self.fps = info.FPS
					}
				case naltype == 8:
					pps = nalu
				case h264parser.IsDataNALU(nalu):
					// raw nalu to avcc
					if !self.demuxer.AnnexB {
						b := make([]byte, 4+len(nalu))
						pio.PutU32BE(b[0:4], uint32(len(nalu)))
						copy(b[4:], nalu)
						fps := self.fps
						if self.fps == 0 {
							fps = 25
						}
						self.addPacket(b, time.Duration(0), (1000*time.Millisecond)/time.Duration(fps))
						n++
					}
				}
			}
		}

		if self.demuxer.AnnexB {
			b := make([]byte, 4+len(payload))
			pio.PutU32BE(b[0:4], uint32(len(payload)))
			copy(b[4:], payload)
			self.addPacket(b, time.Duration(0), 0)
			n++
		}

		if self.CodecData == nil && len(sps) > 0 && len(pps) > 0 {
			if self.CodecData, err = h264parser.NewCodecDataFromSPSAndPPS(sps, pps); err != nil {
				return
			}
		}
	}

	return
}

func (self *Stream) handleTSPacket(start bool, iskeyframe bool, payload []byte) (err error) {
	if start {
		if _, err = self.payloadEnd(); err != nil {
			return
		}
		var hdrlen int
		if hdrlen, _, self.datalen, self.pts, self.dts, err = tsio.ParsePESHeader(payload); err != nil {
			return
		}
		self.iskeyframe = iskeyframe
		if self.datalen == 0 {
			self.data = make([]byte, 0, 4096)
		} else {
			self.data = make([]byte, 0, self.datalen)
		}
		self.data = append(self.data, payload[hdrlen:]...)
	} else {
		self.data = append(self.data, payload...)
	}
	return
}
