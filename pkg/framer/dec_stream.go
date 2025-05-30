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
	"errors"
	"fmt"

	"github.com/asticode/go-astiav"
	"github.com/rs/zerolog"
)

var codecIDToHwDecoder = map[astiav.CodecID]string{
	astiav.CodecIDH264:       "h264_cuvid",
	astiav.CodecIDHevc:       "hevc_cuvid",
	astiav.CodecIDMpeg2Video: "mpeg2_cuvid",
	astiav.CodecIDMpeg4:      "mpeg4_cuvid",
	astiav.CodecIDVc1:        "vc1_cuvid",
	astiav.CodecIDVp8:        "vp8_cuvid",
	astiav.CodecIDVp9:        "vp9_cuvid",
}

type SkippedCodecError struct {
	CodecID astiav.CodecID
}

func (e SkippedCodecError) Error() string {
	return fmt.Sprintf("skipping decoder, returning raw data for: %s", e.CodecID.Name())
}

// decStream wraps a single input stream with the state for decoding and
// distributing frames.
type decStream struct {
	// InFFmpegStream is exported so it can be referenced when setting up
	// corresponding output streams.
	InFFmpegStream *astiav.Stream

	// TimeBase is the time base of the decoder.
	TimeBase astiav.Rational

	// If decCodecContext is nil, this stream is a no-op for an unsupported
	// encoding and just passes packetWrappers through. If not nil, it's a
	// decoding stream returning frameWrappers.
	decCodecContext *astiav.CodecContext

	// inPkt is the packet we read from the input stream to decode.
	inPkt *astiav.Packet

	// outFW is the frameWrapper we're currently decoding into.
	outFW *frameWrapper

	// We use a ring buffer of frameWrappers to distribute data.
	frameWrappers *ring.Ring

	FrameCount int
}

// newDecStream returns a new decStream instance. Must be closed with Close() when done.
func newDecStream() *decStream {
	// 3 wrappers seems like enough that we can have taps processing one frame as
	// the stream distributes the next, and then invalidates the 3rd for the next input.
	// It seems like enough to avoid missed frames in the worst case for lock-step.
	// However, aac seems to come in multi-frame packets, and 4 seems to help
	// prevent drops.
	const numWrappers = 4

	return &decStream{
		inPkt:         astiav.AllocPacket(),
		frameWrappers: ring.New(numWrappers),
	}
}

func (st *decStream) MarshalZerologObject(e *zerolog.Event) {
	if st.decCodecContext == nil {
		return
	}

	e.Str(lCodec, st.decCodecContext.CodecID().Name()).
		Int(lTimeBaseNum, st.TimeBase.Num()).
		Int(lTimeBaseDen, st.TimeBase.Den()).
		Int(lFrameRateNum, st.decCodecContext.Framerate().Num()).
		Int(lFrameRateDen, st.decCodecContext.Framerate().Den()).
		Int(lIndex, st.InFFmpegStream.Index())
}

// Close frees ffmpeg resources associated with the stream.
func (st *decStream) Close() {
	if st.decCodecContext != nil {
		// Best practice is to send a nil packet to flush the decoder.
		_ = st.decCodecContext.SendPacket(nil)

		fw := st.nextFrameWrapper() // need a dummy frame into which we can receive.
		fw.SetValid(false)

		var err error
		for err == nil {
			err = st.decCodecContext.ReceiveFrame(fw.Frame)
		}

		st.decCodecContext.Free()
	}

	st.inPkt.Free()
	st.frameWrappers.Do(func(v interface{}) {
		if v != nil {
			v.(*frameWrapper).Close() //nolint:forcetypeassert // It's a frameWrapper.
		}
	})
}

// initFrameDecoder initializes the stream to decode frames from the input
// stream and wrap them in frameWrappers.
func (st *decStream) initFrameDecoder(inputFormatContext *astiav.FormatContext,
	input *astiav.Stream, decCodec *astiav.Codec, rawURL string,
) {
	var err error

	defer func() {
		if err != nil {
			log.Info().Int(lIndex, input.Index()).Str(lCodec, decCodec.Name()).Err(err).Msg("")
			st.decCodecContext.Free()
			st.decCodecContext = nil
		}
	}()

	codecParams := input.CodecParameters()
	mediaType := codecParams.MediaType()
	decCodecID := codecParams.CodecID()

	st.decCodecContext = astiav.AllocCodecContext(decCodec)

	_ = codecParams.ToCodecContext(st.decCodecContext)

	switch mediaType {
	case astiav.MediaTypeAudio:
		if st.decCodecContext.SampleRate() == 0 {
			const guessSampleRate = 48000

			log.Info().Int(lIndex, input.Index()).Str(lCodec, decCodec.Name()).
				Msg("guessing sample rate for audio stream")
			st.decCodecContext.SetSampleRate(guessSampleRate)
		}

		if st.decCodecContext.ChannelLayout() == 0 {
			log.Info().Int(lIndex, input.Index()).Str(lCodec, decCodec.Name()).
				Msg("guessing channel layout for audio stream")
			st.decCodecContext.SetChannelLayout(astiav.ChannelLayoutStereo)
		}

	case astiav.MediaTypeVideo:
		st.decCodecContext.SetFramerate(inputFormatContext.GuessFrameRate(input, nil))

	case astiav.MediaTypeSubtitle:
		// This decoder crashes during decCodecContext.SendPacket() with an internal
		// assertion failure that seems like it could be an ffmpeg bug. Bypassing for now.
		if decCodecID == astiav.CodecIDDvbSubtitle {
			err = &SkippedCodecError{CodecID: decCodecID}

			return
		}
	}

	if err = st.decCodecContext.Open(decCodec, nil); err != nil {
		err = fmt.Errorf("opening decoder context failed: %w", err)

		return
	}

	st.TimeBase = st.decCodecContext.TimeBase()

	for i := 0; i < st.frameWrappers.Len(); i++ {
		st.frameWrappers.Value = newFrameWrapper(mediaType, input.Index(), decCodecID.Name())
		st.frameWrappers = st.frameWrappers.Next()
	}

	st.frameWrappers.Do(func(v interface{}) {
		//nolint:forcetypeassert // We know.
		fwErr := v.(*frameWrapper).Init(st.decCodecContext, rawURL)

		// We just remember the first error for the outer layer to return.
		if fwErr != nil && err == nil {
			err = fmt.Errorf("initializing frame wrapper(s) failed: %w", fwErr)
		}
	})

	st.outFW = st.nextFrameWrapper()
}

// Init initializes the stream. It uses the inputFormatContext to guess the
// framerate of the stream, if it's a video stream. If there are initialization
// errors, falls back to being a passthrough stream. (Always returns nil.)
func (st *decStream) Init(inputFormatContext *astiav.FormatContext, input *astiav.Stream, rawURL string, hwAccel bool) {
	st.InFFmpegStream = input
	st.TimeBase = input.TimeBase() // Default, but if we open the decoder, this will be updated.

	var decCodec *astiav.Codec

	if decName, ok := codecIDToHwDecoder[input.CodecParameters().CodecID()]; ok && hwAccel {
		log.Debug().Int(lIndex, input.Index()).Str(lCodec, input.CodecParameters().CodecID().Name()).
			Str(lDecoder, decName).Msg("using hardware decoder")

		decCodec = astiav.FindDecoderByName(decName)
	}

	if decCodec == nil {
		decCodec = astiav.FindDecoder(input.CodecParameters().CodecID())
	}

	if decCodec == nil {
		// This descStream will just pass through raw packets from the input stream.
		log.Debug().Int(lIndex, input.Index()).Str(lCodec, input.CodecParameters().CodecID().Name()).
			Msg("no decoder found, falling back to passthrough")

		return
	}

	st.initFrameDecoder(inputFormatContext, input, decCodec, rawURL)
}

// nextFrameWrapper returns the next wrapper in this stream's ring buffer.
func (st *decStream) nextFrameWrapper() *frameWrapper {
	st.frameWrappers = st.frameWrappers.Next()

	return st.frameWrappers.Value.(*frameWrapper) //nolint:forcetypeassert // It's a wrapper.
}

// Decode converts the a single packet into a set of wrappers, decoding if needed.
func (st *decStream) Decode(pw *packetWrapper) ([]wrapper, error) {
	// If no decoder, this stream is a no-op.
	if st.decCodecContext == nil {
		wrappers := []wrapper{pw}

		return wrappers, nil
	}

	if err := pw.Unwrap(st.inPkt); err != nil {
		return nil, err
	}

	st.inPkt.RescaleTs(st.InFFmpegStream.TimeBase(), st.decCodecContext.TimeBase())

	if err := st.decCodecContext.SendPacket(st.inPkt); err != nil {
		return nil, fmt.Errorf("sending packet to decoder failed: %w", err)
	}

	return st.receiveFrames()
}

// receiveFrames receives all frames from the decoder and returns them.
func (st *decStream) receiveFrames() ([]wrapper, error) {
	wrappers := make([]wrapper, 0, 1)
	// Technically, one packet could expand into multiple frames, so we query
	// the decoder in a loop until it returns an error.
	// If multiple frames actually happens with any regularity, we should increase
	// the size of the ring buffer.
	for {
		// We're about to overwrite this wrappers's ffmpeg buffer.
		// If there's a tap still holding this wrapper, setting it invalid
		// ensures that fw.ToModelMedia() will not try to access the wrapped frame.
		st.outFW.SetValid(false)

		// ReceiveFrame() will release any previous buffers in fw.Frame.
		if err := st.decCodecContext.ReceiveFrame(st.outFW.Frame); err != nil {
			if errors.Is(err, astiav.ErrEof) || errors.Is(err, astiav.ErrEagain) {
				st.FrameCount += len(wrappers)
				if len(wrappers) > st.frameWrappers.Len() {
					log.Info().Int(lFrameCount, len(wrappers)).
						Int(lIndex, st.InFFmpegStream.Index()).
						Msg("frame decode count exceeded ring buffer size")
				}

				return wrappers, nil
			}

			return nil, fmt.Errorf("receiving frame from decoder failed: %w", err)
		}

		// Now this fw has a valid frame and can be sent to taps.
		st.outFW.SetValid(true)
		wrappers = append(wrappers, st.outFW)
		st.outFW = st.nextFrameWrapper()
	}
}

// DecodeFlush flushes the decoder and returns any remaining frames.
func (st *decStream) DecodeFlush() ([]wrapper, error) {
	if st.decCodecContext == nil {
		return nil, nil
	}

	// Best practice is to send a nil packet to flush the decoder.
	_ = st.decCodecContext.SendPacket(nil)

	return st.receiveFrames()
}
