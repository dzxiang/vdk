package ts

import (
	"fmt"
	"io"
	"time"

	"github.com/dzxiang/vdk/codec/h265parser"

	"github.com/dzxiang/vdk/av"
	"github.com/dzxiang/vdk/codec/aacparser"
	"github.com/dzxiang/vdk/codec/h264parser"
	"github.com/dzxiang/vdk/format/ts/tsio"
)

var CodecTypes = []av.CodecType{av.H264, av.H265, av.AAC}

type Muxer struct {
	w       io.Writer
	streams map[int]*Stream

	PaddingToMakeCounterCont bool

	psidata []byte
	peshdr  []byte
	tshdr   []byte
	adtshdr []byte
	datav   [][]byte
	nalus   [][]byte

	tswpat, tswpmt *tsio.TSWriter
}

func NewMuxer(w io.Writer) *Muxer {
	return &Muxer{
		w:       w,
		psidata: make([]byte, 188),
		peshdr:  make([]byte, tsio.MaxPESHeaderLength),
		tshdr:   make([]byte, tsio.MaxTSHeaderLength),
		adtshdr: make([]byte, aacparser.ADTSHeaderLength),
		nalus:   make([][]byte, 16),
		datav:   make([][]byte, 16),
		tswpmt:  tsio.NewTSWriter(tsio.PMT_PID),
		tswpat:  tsio.NewTSWriter(tsio.PAT_PID),
	}
}

func (self *Muxer) newStream(idx int, codec av.CodecData) (err error) {
	ok := false
	for _, c := range CodecTypes {
		if codec.Type() == c {
			ok = true
			break
		}
	}
	if !ok {
		err = fmt.Errorf("ts: codec type=%s is not supported", codec.Type())
		return
	}

	pid := uint16(idx + 0x100)
	stream := &Stream{
		muxer:     self,
		CodecData: codec,
		pid:       pid,
		tsw:       tsio.NewTSWriter(pid),
	}
	self.streams[idx] = stream
	return
}

func (self *Muxer) writePaddingTSPackets(streamW *Stream) (err error) {
	for streamW.tsw.ContinuityCounter&0xf != 0x0 {
		header := tsio.TSHeader{
			PID:               uint(streamW.pid),
			ContinuityCounter: streamW.tsw.ContinuityCounter,
		}
		if _, err = tsio.WriteTSHeader(self.w, header, 0); err != nil {
			return
		}
		streamW.tsw.ContinuityCounter++
	}
	return
}

func (self *Muxer) WriteTrailer() (err error) {
	if self.PaddingToMakeCounterCont {
		for _, stream := range self.streams {
			if err = self.writePaddingTSPackets(stream); err != nil {
				return
			}
		}
	}
	return
}

func (self *Muxer) SetWriter(w io.Writer) {
	self.w = w
	return
}

func (self *Muxer) WritePATPMT() (err error) {
	pat := tsio.PAT{
		Entries: []tsio.PATEntry{
			{ProgramNumber: 1, ProgramMapPID: tsio.PMT_PID},
		},
	}
	patlen := pat.Marshal(self.psidata[tsio.PSIHeaderLength:])
	n := tsio.FillPSI(self.psidata, tsio.TableIdPAT, tsio.TableExtPAT, patlen)
	self.datav[0] = self.psidata[:n]
	if err = self.tswpat.WritePackets(self.w, self.datav[:1], 0, false, true); err != nil {
		return
	}

	var elemStreams []tsio.ElementaryStreamInfo
	for _, stream := range self.streams {
		switch stream.Type() {
		case av.AAC:
			elemStreams = append(elemStreams, tsio.ElementaryStreamInfo{
				StreamType:    tsio.ElementaryStreamTypeAdtsAAC,
				ElementaryPID: stream.pid,
			})
		case av.H264:
			elemStreams = append(elemStreams, tsio.ElementaryStreamInfo{
				StreamType:    tsio.ElementaryStreamTypeH264,
				ElementaryPID: stream.pid,
			})
		case av.H265:
			elemStreams = append(elemStreams, tsio.ElementaryStreamInfo{
				StreamType:    tsio.ElementaryStreamTypeH265,
				ElementaryPID: stream.pid,
			})
		}
	}

	pmt := tsio.PMT{
		PCRPID:                0x100,
		ElementaryStreamInfos: elemStreams,
	}
	pmtlen := pmt.Len()
	if pmtlen+tsio.PSIHeaderLength > len(self.psidata) {
		err = fmt.Errorf("ts: pmt too large")
		return
	}
	pmt.Marshal(self.psidata[tsio.PSIHeaderLength:])
	n = tsio.FillPSI(self.psidata, tsio.TableIdPMT, tsio.TableExtPMT, pmtlen)
	self.datav[0] = self.psidata[:n]
	if err = self.tswpmt.WritePackets(self.w, self.datav[:1], 0, false, true); err != nil {
		return
	}

	return
}

func (self *Muxer) WriteHeader(streams []av.CodecData) (err error) {
	self.streams = map[int]*Stream{}

	for idx, stream := range streams {
		if err = self.newStream(idx, stream); err != nil {
			fmt.Println(err)
		}
	}

	if err = self.WritePATPMT(); err != nil {
		return
	}
	return
}

func (self *Muxer) WritePacket(pkt av.Packet) (err error) {
	var stream *Stream = nil

	stream, ok := self.streams[int(pkt.Idx)]
	if !ok {
		fmt.Printf("Warning, unsupported stream index: %d\n", pkt.Idx)
		return
	}

	pkt.Time += time.Second

	switch stream.Type() {
	case av.AAC:
		codec := stream.CodecData.(aacparser.CodecData)
		n := tsio.FillPESHeader(self.peshdr, tsio.StreamIdAAC, len(self.adtshdr)+len(pkt.Data), pkt.Time, 0)
		self.datav[0] = self.peshdr[:n]
		aacparser.FillADTSHeader(self.adtshdr, codec.Config, 1024, len(pkt.Data))
		self.datav[1] = self.adtshdr
		self.datav[2] = pkt.Data

		if err = stream.tsw.WritePackets(self.w, self.datav[:3], pkt.Time, true, false); err != nil {
			return
		}

	case av.H264:
		codec := stream.CodecData.(h264parser.CodecData)

		nalus := self.nalus[:0]
		if pkt.IsKeyFrame {
			nalus = append(nalus, codec.SPS())
			nalus = append(nalus, codec.PPS())
		}
		pktnalus, _ := h264parser.SplitNALUs(pkt.Data)
		for _, nalu := range pktnalus {
			nalus = append(nalus, nalu)
		}

		datav := self.datav[:1]
		for i, nalu := range nalus {
			if i == 0 {
				datav = append(datav, h264parser.AUDBytes)
			} else {
				datav = append(datav, h264parser.StartCodeBytes)
			}
			datav = append(datav, nalu)
		}

		n := tsio.FillPESHeader(self.peshdr, tsio.StreamIdH264, -1, pkt.Time+pkt.CompositionTime, pkt.Time)
		datav[0] = self.peshdr[:n]

		if err = stream.tsw.WritePackets(self.w, datav, pkt.Time, pkt.IsKeyFrame, false); err != nil {
			return
		}
	case av.H265:
		codec := stream.CodecData.(h265parser.CodecData)

		nalus := self.nalus[:0]
		if pkt.IsKeyFrame {
			nalus = append(nalus, codec.SPS())
			nalus = append(nalus, codec.PPS())
			nalus = append(nalus, codec.VPS())
		}
		pktnalus, _ := h265parser.SplitNALUs(pkt.Data)
		for _, nalu := range pktnalus {
			nalus = append(nalus, nalu)
		}

		datav := self.datav[:1]
		for i, nalu := range nalus {
			if i == 0 {
				datav = append(datav, h265parser.AUDBytes)
			} else {
				datav = append(datav, h265parser.StartCodeBytes)
			}
			datav = append(datav, nalu)
		}

		n := tsio.FillPESHeader(self.peshdr, tsio.StreamIdH264, -1, pkt.Time+pkt.CompositionTime, pkt.Time)
		datav[0] = self.peshdr[:n]

		if err = stream.tsw.WritePackets(self.w, datav, pkt.Time, pkt.IsKeyFrame, false); err != nil {
			return
		}
	}

	return
}
