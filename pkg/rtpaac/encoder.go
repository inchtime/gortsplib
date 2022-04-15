package rtpaac

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"time"

	"github.com/icza/bitio"
	"github.com/pion/rtp"
)

func randUint32() uint32 {
	var b [4]byte
	rand.Read(b[:])
	return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
}

// Encoder is a RTP/AAC encoder.
type Encoder struct {
	// payload type of packets.
	PayloadType uint8

	// SSRC of packets (optional).
	// It defaults to a random value.
	SSRC *uint32

	// initial sequence number of packets (optional).
	// It defaults to a random value.
	InitialSequenceNumber *uint16

	// initial timestamp of packets (optional).
	// It defaults to a random value.
	InitialTimestamp *uint32

	// maximum size of packet payloads (optional).
	// It defaults to 1460.
	PayloadMaxSize int

	// sample rate of packets.
	SampleRate int

	// The number of bits on which the AU-size field is encoded in the AU-header (optional).
	// It defaults to 13.
	SizeLength *int

	// The number of bits on which the AU-Index is encoded in the first AU-header (optional).
	// It defaults to 3.
	IndexLength *int

	// The number of bits on which the AU-Index-delta field is encoded in any non-first AU-header (optional).
	// It defaults to 3.
	IndexDeltaLength *int

	sequenceNumber uint16
}

// Init initializes the encoder.
func (e *Encoder) Init() {
	if e.SSRC == nil {
		v := randUint32()
		e.SSRC = &v
	}
	if e.InitialSequenceNumber == nil {
		v := uint16(randUint32())
		e.InitialSequenceNumber = &v
	}
	if e.InitialTimestamp == nil {
		v := randUint32()
		e.InitialTimestamp = &v
	}
	if e.PayloadMaxSize == 0 {
		e.PayloadMaxSize = 1460 // 1500 (UDP MTU) - 20 (IP header) - 8 (UDP header) - 12 (RTP header)
	}
	if e.SizeLength == nil {
		v := 13
		e.SizeLength = &v
	}
	if e.IndexLength == nil {
		v := 3
		e.IndexLength = &v
	}
	if e.IndexDeltaLength == nil {
		v := 3
		e.IndexDeltaLength = &v
	}

	e.sequenceNumber = *e.InitialSequenceNumber
}

func (e *Encoder) encodeTimestamp(ts time.Duration) uint32 {
	return *e.InitialTimestamp + uint32(ts.Seconds()*float64(e.SampleRate))
}

// Encode encodes AUs into RTP/AAC packets.
func (e *Encoder) Encode(aus [][]byte, firstPTS time.Duration) ([]*rtp.Packet, error) {
	var rets []*rtp.Packet
	var batch [][]byte

	pts := firstPTS

	// split AUs into batches
	for _, au := range aus {
		if e.lenAggregated(batch, au) <= e.PayloadMaxSize {
			// add to existing batch
			batch = append(batch, au)
		} else {
			// write last batch
			if batch != nil {
				pkts, err := e.writeBatch(batch, pts)
				if err != nil {
					return nil, err
				}
				rets = append(rets, pkts...)
				pts += time.Duration(len(batch)) * 1000 * time.Second / time.Duration(e.SampleRate)
			}

			// initialize new batch
			batch = [][]byte{au}
		}
	}

	// write last batch
	pkts, err := e.writeBatch(batch, pts)
	if err != nil {
		return nil, err
	}
	rets = append(rets, pkts...)

	return rets, nil
}

func (e *Encoder) writeBatch(aus [][]byte, firstPTS time.Duration) ([]*rtp.Packet, error) {
	if len(aus) == 1 {
		// the AU fits into a single RTP packet
		if len(aus[0]) < e.PayloadMaxSize {
			return e.writeAggregated(aus, firstPTS)
		}

		// split the AU into multiple fragmentation packet
		return e.writeFragmented(aus[0], firstPTS)
	}

	return e.writeAggregated(aus, firstPTS)
}

func (e *Encoder) writeFragmented(au []byte, pts time.Duration) ([]*rtp.Packet, error) {
	auHeaderLen := *e.SizeLength + *e.IndexLength
	auMaxSize := e.PayloadMaxSize - 2 - auHeaderLen/8
	packetCount := len(au) / auMaxSize
	lastPacketSize := len(au) % auMaxSize
	if lastPacketSize > 0 {
		packetCount++
	}

	ret := make([]*rtp.Packet, packetCount)
	encPTS := e.encodeTimestamp(pts)

	for i := range ret {
		var le int
		if i != (packetCount - 1) {
			le = auMaxSize
		} else {
			le = lastPacketSize
		}

		byts := make([]byte, 2+auHeaderLen/8+le)

		// AU-headers-length
		binary.BigEndian.PutUint16(byts, uint16(auHeaderLen))

		// AU-headers
		bw := bitio.NewWriter(bytes.NewBuffer(byts[2:2]))
		bw.WriteBits(uint64(le), uint8(*e.SizeLength))
		bw.WriteBits(0, uint8(*e.IndexLength))
		bw.Close()

		// AU
		copy(byts[2+auHeaderLen/8:], au[:le])
		au = au[le:]

		ret[i] = &rtp.Packet{
			Header: rtp.Header{
				Version:        rtpVersion,
				PayloadType:    e.PayloadType,
				SequenceNumber: e.sequenceNumber,
				Timestamp:      encPTS,
				SSRC:           *e.SSRC,
				Marker:         (i == (packetCount - 1)),
			},
			Payload: byts,
		}

		e.sequenceNumber++
	}

	return ret, nil
}

func (e *Encoder) lenAggregated(aus [][]byte, addAU []byte) int {
	ret := 2 // AU-headers-length

	i := 0
	for _, au := range aus {
		// AU-header
		if i == 0 {
			ret += (*e.SizeLength + *e.IndexLength) / 8
		} else {
			ret += (*e.SizeLength + *e.IndexDeltaLength) / 8
		}
		ret += len(au) // AU
		i++
	}

	if addAU != nil {
		// AU-header
		if i == 0 {
			ret += (*e.SizeLength + *e.IndexLength) / 8
		} else {
			ret += (*e.SizeLength + *e.IndexDeltaLength) / 8
		}
		ret += len(addAU) // AU
	}

	return ret
}

func (e *Encoder) writeAggregated(aus [][]byte, firstPTS time.Duration) ([]*rtp.Packet, error) {
	payload := make([]byte, e.lenAggregated(aus, nil))

	// AU-headers
	written := 0
	bw := bitio.NewWriter(bytes.NewBuffer(payload[2:2]))
	for i, au := range aus {
		bw.WriteBits(uint64(len(au)), uint8(*e.SizeLength))
		written += *e.SizeLength
		if i == 0 {
			bw.WriteBits(0, uint8(*e.IndexLength))
			written += *e.IndexLength
		} else {
			bw.WriteBits(0, uint8(*e.IndexDeltaLength))
			written += *e.IndexDeltaLength
		}
	}
	bw.Close()
	pos := 2 + (written / 8)

	// AU-headers-length
	binary.BigEndian.PutUint16(payload, uint16(written))

	// AUs
	for _, au := range aus {
		auLen := copy(payload[pos:], au)
		pos += auLen
	}

	pkt := &rtp.Packet{
		Header: rtp.Header{
			Version:        rtpVersion,
			PayloadType:    e.PayloadType,
			SequenceNumber: e.sequenceNumber,
			Timestamp:      e.encodeTimestamp(firstPTS),
			SSRC:           *e.SSRC,
			Marker:         true,
		},
		Payload: payload,
	}

	e.sequenceNumber++

	return []*rtp.Packet{pkt}, nil
}
