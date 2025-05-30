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
	"container/ring"
	"context"
	"time"

	"github.com/asticode/go-astiav"
	"github.com/rs/zerolog"
)

// inStream handles wrapping packets from an input stream in packetWrappers.
type inStream struct {
	// InFFmpegStream is exported so it can be referenced when setting up
	// corresponding output streams.
	InFFmpegStream *astiav.Stream
	TimeBase       astiav.Rational

	// We use a ring buffer of packetWrappers to ingest data.
	packetWrappers *ring.Ring

	// startPacketTime lets us throttle the stream to the framerate of the input.
	startPacketTime time.Time
	startPts        int64

	PktCount int
}

// newInStream returns a new inStream instance. Must be closed with Close() when done.
func newInStream() *inStream {
	// 3 wrappers is enough that we can have taps processing one frame as
	// the stream distributes the next, and then invalidates the 3rd for the next input.
	// It seems like enough to avoid missed frames in the worst case.
	const numWrappers = 3

	return &inStream{
		packetWrappers: ring.New(numWrappers),
	}
}

func (st *inStream) MarshalZerologObject(e *zerolog.Event) {
	e.Str(lCodec, st.InFFmpegStream.CodecParameters().CodecID().Name()).
		Int(lTimeBaseNum, st.TimeBase.Num()).
		Int(lTimeBaseDen, st.TimeBase.Den()).
		Int(lFrameRateNum, st.InFFmpegStream.AvgFrameRate().Num()).
		Int(lFrameRateDen, st.InFFmpegStream.AvgFrameRate().Den()).
		Int(lIndex, st.InFFmpegStream.Index())
}

// Close frees ffmpeg resources associated with the stream.
func (st *inStream) Close() {
	st.packetWrappers.Do(func(v interface{}) {
		if v != nil {
			v.(*packetWrapper).Close() //nolint:forcetypeassert // It's a wrapper.
		}
	})
}

// Init initializes the stream.
func (st *inStream) Init(input *astiav.Stream, rawURL string) {
	st.InFFmpegStream = input
	st.TimeBase = input.TimeBase()
	sourceEncoding := input.CodecParameters().CodecID().Name()

	for i := 0; i < st.packetWrappers.Len(); i++ {
		st.packetWrappers.Value = newPacketWrapper(input.Index(), sourceEncoding, st.TimeBase, rawURL)
		st.packetWrappers = st.packetWrappers.Next()
	}
}

func (st *inStream) ThrottleWait(ctx context.Context, pkt *astiav.Packet) error {
	if st.startPacketTime.IsZero() {
		st.startPacketTime = time.Now()
		st.startPts = pkt.Pts()

		return nil
	}

	nextFrameDeadline := ptsToDuration(pkt.Pts()-st.startPts, st.TimeBase)
	now := time.Now()

	elapsed := now.Sub(st.startPacketTime)
	if elapsed < nextFrameDeadline {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(nextFrameDeadline - elapsed):
		}
	}

	return nil
}

// nextPacketWrapper returns the next wrapper in this stream's ring buffer.
func (st *inStream) nextPacketWrapper() *packetWrapper {
	st.packetWrappers = st.packetWrappers.Next()

	return st.packetWrappers.Value.(*packetWrapper) //nolint:forcetypeassert // It's a wrapper.
}

func (st *inStream) WrapPacket(pkt *astiav.Packet) *packetWrapper {
	// We're about to overwrite this wrapper's payload.
	// If there's a tap still holding this wrapper, setting it invalid ensures
	// that w.Unwrap or w.ToModelMedia() will not try to access the wrapped packet.
	pw := st.nextPacketWrapper()
	pw.SetValid(false)

	// We're not decoding, so we just copy the packet into the packetWrapper.
	pw.Packet.Unref()
	_ = pw.Packet.Ref(pkt) // Only fails for memory.

	// Now this fw has a valid frame and can be sent to taps.
	pw.SetValid(true)

	st.PktCount++

	return pw
}
