// Frontline Perception System
// Copyright (C) 2020-2025 TurbineOne LLC
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU General Public License for more details.
//
// You should have received a copy of the GNU General Public License
// along with this program.  If not, see <https://www.gnu.org/licenses/>.

package framer

import (
	"sync"

	"github.com/asticode/go-astiav"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/TurbineOne/ffmpeg-framer/api/proto/gen/go/fps/model"
)

// packetWrapper is a wrapper for astiav.Packet. It implements the wrapper
// interface, and can provide the packet data as a model.Media.
// It's different from a frameWrapper in that it does no encoding (e.g., jpeg).
// This is used for data where ffmpeg lacks an encoder/decoder and can
// only pass raw packets through to the taps.
type packetWrapper struct {
	// Packet is the packet to be copied directly to the model.Media.
	// It is exported so the owner can set the packet data directly.
	Packet *astiav.Packet

	// Set at allocation:
	mimeType       string
	streamIndex    int
	sourceEncoding string
	timeBase       astiav.Rational
	rawURL         string

	// Internals:
	// The lock is used by the methods to protect the internals.
	lock       sync.Mutex
	inputValid bool
	modelMedia *model.Media
}

func newPacketWrapper(sourceStreamIndex int, sourceEncoding string,
	timeBase astiav.Rational, rawURL string,
) *packetWrapper {
	pw := &packetWrapper{
		Packet: astiav.AllocPacket(),

		// This is a raw packet with no encoder, so whatever the payload was,
		// we just return it as an octet-stream mime type.
		mimeType:       mimeTypeForUnknown,
		streamIndex:    sourceStreamIndex,
		sourceEncoding: sourceEncoding,
		timeBase:       timeBase,
		rawURL:         rawURL,
	}

	return pw
}

func (pw *packetWrapper) Close() {
	pw.lock.Lock()
	defer pw.lock.Unlock()

	pw.Packet.Free()
	pw.Packet = nil
}

func (pw *packetWrapper) StreamIndex() int {
	return pw.streamIndex
}

// SetValid sets the validity of the embedded ffmpeg Packet. If invalid,
// the ToModelMedia() method will return not attempt to access the Packet
// and will only return a model.Media if it had been previously encoded.
func (pw *packetWrapper) SetValid(valid bool) {
	pw.lock.Lock()
	defer pw.lock.Unlock()

	pw.inputValid = valid
	if valid {
		// The old model.Media was still accessible before, even while the ffmpeg
		// frame was being written. Now that the ffmpeg frame is valid, the first
		// call of ToModelMedia() will generate a new model.Media based on the new
		// ffmpeg frame.
		pw.modelMedia = nil
	}
}

func (pw *packetWrapper) PTS() int64 {
	pw.lock.Lock()

	pts := int64(-1)
	if pw.inputValid {
		pts = pw.Packet.Pts()
	}
	pw.lock.Unlock()

	return pts
}

func (pw *packetWrapper) TimeBase() astiav.Rational {
	return pw.timeBase
}

// ToModelMedia encodes the packetWrapper's ffmpeg Packet into a model.Media.
// The first call to ToModelMedia will encode the Packet, and subsequent
// calls will return the same model.Media until SetValid(true) is called.
// At that point, the next call to ToModelMedia will encode the new Packet.
// It can return nil if the embedded Packet is declared
// invalid before this method is called at least once.
func (pw *packetWrapper) ToModelMedia() *model.Media {
	pw.lock.Lock()
	defer pw.lock.Unlock()

	// If the Packet was already turned into a model.Media, we can use it as-is.
	// Note, this can still work even while the underlying ffmpeg Packet
	// is invalid and being written. Only once the Packet is declared valid will
	// this cached modelMedia be reset.
	if pw.modelMedia != nil {
		return pw.modelMedia
	}

	// If the underlying ffmpeg frame has been invalidated, it's being reused
	// so we can no longer try to encode it.
	// This generally shouldn't happen, but it could in some unlucky situations.
	// (It probably means there's already a new frame in the tap.)
	if !pw.inputValid {
		log.Info().Msg("bad timing: frame invalidated before encoding")

		return nil
	}

	if pw.Packet == nil {
		log.Info().Msg("Missing Packet: frame invalidated before encoding")

		return nil
	}

	pw.modelMedia = &model.Media{
		Key: &model.MediaKey{
			Url: pw.rawURL,
			SliceSpec: &model.MediaSliceSpec{
				StreamIndex: int32(pw.streamIndex),
			},
		},
		Info: &model.MediaInfo{
			Type:  pw.mimeType,
			Codec: pw.sourceEncoding,
		},
		Format: &model.Media_MediaBytes{
			MediaBytes: &model.MediaBytes{
				Data: pw.Packet.Data(),
			},
		},
	}

	offset := ptsToDuration(pw.Packet.Pts(), pw.timeBase)
	if offset != noPTS {
		pw.modelMedia.Key.SliceSpec.Offset = durationpb.New(offset)
	}

	return pw.modelMedia
}

func (pw *packetWrapper) Unwrap(packet *astiav.Packet) error {
	pw.lock.Lock()
	defer pw.lock.Unlock()

	if !pw.inputValid {
		return &wrapperInvalidError{}
	}

	packet.Unref()
	_ = packet.Ref(pw.Packet)

	return nil
}
