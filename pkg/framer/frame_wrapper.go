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
	"errors"
	"fmt"
	"strconv"
	"sync"

	"github.com/asticode/go-astiav"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/TurbineOne/ffmpeg-framer/api/proto/gen/go/fps/model"
)

const codecIDForUnknown = astiav.CodecIDNone

var mediaTypeToCodecID = map[astiav.MediaType]astiav.CodecID{
	astiav.MediaTypeVideo: astiav.CodecIDMjpeg,
	// TODO: We should use aac for audio, but that encoder buffers the input and
	// may cause interleaving across frame wrappers. We'd need to switch to a single
	// encoder context across all frame wrappers to fix this.
	astiav.MediaTypeAudio:      astiav.CodecIDWavpack,
	astiav.MediaTypeData:       codecIDForUnknown,
	astiav.MediaTypeSubtitle:   astiav.CodecIDText,
	astiav.MediaTypeAttachment: codecIDForUnknown,
	astiav.MediaTypeUnknown:    codecIDForUnknown,
}

const mimeTypeForUnknown = "application/octet-stream"

var codecIDToMimeType = map[astiav.CodecID]string{
	astiav.CodecIDMjpeg:   "image/jpeg",
	astiav.CodecIDTiff:    "image/tiff",
	astiav.CodecIDAac:     "audio/aac",
	astiav.CodecIDWavpack: "audio/wav",
	astiav.CodecIDText:    "text/plain",
	astiav.CodecIDNone:    mimeTypeForUnknown,
}

// mimeTypeToExtension isn't used here, but it's tightly coupled to the
// codecIDToMimeType map, so we define it here.
var mimeTypeToExtension = map[string]string{
	"image/jpeg": "jpg",
	"image/tiff": "tiff",
	"audio/aac":  "aac",
	"audio/wav":  "wav",
	"text/plain": "txt",
}

var (
	buffersrcFlags  = astiav.NewBuffersrcFlags(astiav.BuffersrcFlagKeepRef)
	buffersinkFlags = astiav.NewBuffersinkFlags()
)

type wrapperInvalidError struct{}

func (e *wrapperInvalidError) Error() string {
	return "frameWrapper is no longer valid"
}

type filterFindError struct {
	filter string
}

func (e *filterFindError) Error() string {
	return fmt.Sprintf("could not find filter %q", e.filter)
}

// frameWrapper wraps an astiav.Frame and provides just-in-time
// re-encoding. The Frame field is exported to designate that they
// are directly set by the owner.
//
// Methods are thread-safe after Init(). The Frame may be directly
// written after calling SetValid(false). Then call SetValid(true) to reset
// the frame that will be encoded by ToModelMedia().
type frameWrapper struct {
	// Frame is the frame to be encoded. It is exported so the owner can
	// set the frame's data. While the frame is being written, it should
	// be declared invalid by calling SetValid(false).
	Frame *astiav.Frame

	// Internals:
	// The lock is used by the methods to protect the internals.
	lock              sync.Mutex
	encCodecID        astiav.CodecID
	encCodec          *astiav.Codec
	encCodecContext   *astiav.CodecContext
	filterGraph       *astiav.FilterGraph
	buffersinkContext *astiav.FilterContext
	buffersrcContext  *astiav.FilterContext
	filterFrame       *astiav.Frame
	encPkt            *astiav.Packet
	inputValid        bool
	modelMedia        *model.Media
	rawURL            string

	// Set at allocation:
	mimeType       string
	streamIndex    int
	sourceEncoding string
}

// newFrameWrapper returns a new frameWrapper using the given mediaType.
// The returned frameWrapper is not ready for use until Init()'ed.
func newFrameWrapper(mediaType astiav.MediaType, sourceStreamIndex int,
	sourceEncoding string,
) *frameWrapper {
	encCodecID, ok := mediaTypeToCodecID[mediaType]
	if !ok {
		encCodecID = codecIDForUnknown

		log.Info().Int(lIndex, sourceStreamIndex).Uint(lMediaType, uint(mediaType)).
			Msg("no CodecID for MediaType")
	}

	encCodec := astiav.FindEncoder(encCodecID)
	if encCodec == nil {
		// This is ok, and just means we won't re-encode the data.
		log.Debug().Int(lIndex, sourceStreamIndex).Uint(lCodec, uint(encCodecID)).
			Msg("no encoder for CodecID")
	}

	fw := &frameWrapper{
		Frame: astiav.AllocFrame(),

		encCodecID:  encCodecID,
		encCodec:    encCodec, // May be nil.
		filterFrame: astiav.AllocFrame(),
		encPkt:      astiav.AllocPacket(),

		// We restrict the range of encCodecID so this map lookup always succeeds:
		mimeType:       codecIDToMimeType[encCodecID],
		streamIndex:    sourceStreamIndex,
		sourceEncoding: sourceEncoding,
	}

	return fw
}

// initFilter initializes the filter graph based on the given decCodecContext.
//
//nolint:funlen // Long but linear.
func (fw *frameWrapper) initFilter(decCodecContext *astiav.CodecContext) error {
	var args astiav.FilterArgs

	var buffersrc, buffersink *astiav.Filter

	// content is the *actual filter* we're doing on the data.
	// Almost everything else is just boilerplate.
	var content string

	switch decCodecContext.MediaType() {
	case astiav.MediaTypeVideo:
		args = astiav.FilterArgs{
			"pix_fmt":      strconv.Itoa(int(decCodecContext.PixelFormat())),
			"pixel_aspect": decCodecContext.SampleAspectRatio().String(),
			"time_base":    decCodecContext.TimeBase().String(),
			"video_size":   strconv.Itoa(decCodecContext.Width()) + "x" + strconv.Itoa(decCodecContext.Height()),
		}
		buffersrc = astiav.FindFilterByName("buffer")
		buffersink = astiav.FindFilterByName("buffersink")
		content = fmt.Sprintf("format=pix_fmts=%s", fw.encCodecContext.PixelFormat().Name())

	case astiav.MediaTypeAudio:
		args = astiav.FilterArgs{
			"channel_layout": decCodecContext.ChannelLayout().String(),
			"sample_fmt":     decCodecContext.SampleFormat().Name(),
			"sample_rate":    strconv.Itoa(decCodecContext.SampleRate()),
			"time_base":      decCodecContext.TimeBase().String(),
		}
		buffersrc = astiav.FindFilterByName("abuffer")
		buffersink = astiav.FindFilterByName("abuffersink")
		content = fmt.Sprintf("aformat=sample_fmts=%s:channel_layouts=%s",
			fw.encCodecContext.SampleFormat().Name(), fw.encCodecContext.ChannelLayout().String())

	default:
		// No filtering needed.
		return nil
	}

	if buffersrc == nil {
		return &filterFindError{"buffersrc"}
	}

	if buffersink == nil {
		return &filterFindError{"buffersink"}
	}

	// Create filter contexts
	fw.filterGraph = astiav.AllocFilterGraph()

	var err error
	if fw.buffersrcContext, err = fw.filterGraph.NewFilterContext(buffersrc, "in", args); err != nil {
		return fmt.Errorf("creating buffersrc context failed: %w", err)
	}

	if fw.buffersinkContext, err = fw.filterGraph.NewFilterContext(buffersink, "in", nil); err != nil {
		return fmt.Errorf("creating buffersink context failed: %w", err)
	}

	// The Filter I/O's express the pad they want to connect to, so we tell
	// the Outputs I/O that it's wired to the "in" pad of the buffersrc context
	// and vice-versa.
	inputs := astiav.AllocFilterInOut()
	defer inputs.Free()

	inputs.SetName("out")
	inputs.SetFilterContext(fw.buffersinkContext)
	inputs.SetPadIdx(0)
	inputs.SetNext(nil)

	outputs := astiav.AllocFilterInOut()
	defer outputs.Free()

	outputs.SetName("in")
	outputs.SetFilterContext(fw.buffersrcContext)
	outputs.SetPadIdx(0)
	outputs.SetNext(nil)

	// Parse
	if err = fw.filterGraph.Parse(content, inputs, outputs); err != nil {
		return fmt.Errorf("parsing filter failed: %w", err)
	}

	// Configure
	if err = fw.filterGraph.Configure(); err != nil {
		return fmt.Errorf("configuring filter failed: %w", err)
	}

	return nil
}

// Init initializes the frameWrapper based on the given decCodecContext.
// It prepares the frameWrapper's encoder and filter graph for use.
func (fw *frameWrapper) Init(decCodecContext *astiav.CodecContext, rawURL string) error {
	fw.rawURL = rawURL
	mediaType := fw.encCodecID.MediaType()

	switch mediaType {
	case astiav.MediaTypeVideo:
		fw.encCodecContext = astiav.AllocCodecContext(fw.encCodec)

		if v := fw.encCodec.PixelFormats(); len(v) > 0 {
			fw.encCodecContext.SetPixelFormat(v[0])
		} else {
			fw.encCodecContext.SetPixelFormat(decCodecContext.PixelFormat())
		}

		fw.encCodecContext.SetSampleAspectRatio(decCodecContext.SampleAspectRatio())
		fw.encCodecContext.SetHeight(decCodecContext.Height())
		fw.encCodecContext.SetWidth(decCodecContext.Width())
		// We manually set quantization to require high quality from the encoder.
		fw.encCodecContext.SetFlags(fw.encCodecContext.Flags().Add(astiav.CodecContextFlagQscale))
		fw.encCodecContext.SetQmin(1)

	case astiav.MediaTypeAudio:
		fw.encCodecContext = astiav.AllocCodecContext(fw.encCodec)

		if v := fw.encCodec.ChannelLayouts(); len(v) > 0 {
			fw.encCodecContext.SetChannelLayout(v[0])
		} else {
			fw.encCodecContext.SetChannelLayout(decCodecContext.ChannelLayout())
		}

		if v := fw.encCodec.SampleFormats(); len(v) > 0 {
			fw.encCodecContext.SetSampleFormat(v[0])
		} else {
			fw.encCodecContext.SetSampleFormat(decCodecContext.SampleFormat())
		}

		fw.encCodecContext.SetChannels(decCodecContext.Channels())
		fw.encCodecContext.SetSampleRate(decCodecContext.SampleRate())

	default:
		// For other media types, we use no encoder or filter.
		return nil
	}

	fw.encCodecContext.SetTimeBase(decCodecContext.TimeBase())

	if decCodecContext.Flags().Has(astiav.CodecContextFlagGlobalHeader) {
		fw.encCodecContext.SetFlags(fw.encCodecContext.Flags().Add(astiav.CodecContextFlagGlobalHeader))
	}

	if err := fw.encCodecContext.Open(fw.encCodec, nil); err != nil {
		return fmt.Errorf("opening encoder context failed: %w", err)
	}

	return fw.initFilter(decCodecContext)
}

// Close frees the frameWrapper's resources. This must be called to prevent
// leaks of the underlying FFmpeg objects. Only called at teardown, not between
// reuse.
func (fw *frameWrapper) Close() {
	fw.lock.Lock()
	defer fw.lock.Unlock()

	fw.Frame.Free()

	fw.inputValid = false
	fw.modelMedia = nil

	fw.encPkt.Free()
	fw.filterFrame.Free()

	// Freeing the FilterGraph frees the src and sink contexts.
	if fw.filterGraph != nil {
		fw.filterGraph.Free()
	}

	if fw.encCodecContext != nil {
		fw.encCodecContext.Free()
	}
}

func (fw *frameWrapper) StreamIndex() int {
	return fw.streamIndex
}

// SetValid sets the validity of the embedded ffmpeg Frame. If invalid,
// the ToModelMedia() method will return not attempt to access the Frame
// and will only return a model.Media if it had been previously encoded.
func (fw *frameWrapper) SetValid(valid bool) {
	fw.lock.Lock()
	defer fw.lock.Unlock()

	fw.inputValid = valid
	if valid {
		// The old model.Media was still accessible before, even while the ffmpeg
		// frame was being written. Now that the ffmpeg frame is valid, the first
		// call of ToModelMedia() will generate a new model.Media based on the new
		// ffmpeg frame.
		fw.modelMedia = nil
	}
}

func (fw *frameWrapper) PTS() int64 {
	fw.lock.Lock()

	pts := int64(-1)
	if fw.inputValid {
		pts = fw.Frame.Pts()
	}
	fw.lock.Unlock()

	return pts
}

func (fw *frameWrapper) TimeBase() astiav.Rational {
	return fw.encCodecContext.TimeBase()
}

// encodeMedia encodes f into a model.Media and returns it, or nil if encoding
// fails. Does not touch the fw.modelMedia.
func (fw *frameWrapper) encodeMedia(f *astiav.Frame) *model.Media {
	if err := fw.encCodecContext.SendFrame(f); err != nil {
		log.Debug().Int(lIndex, fw.streamIndex).Err(err).Msg("error sending to encoder")

		return nil
	}

	fw.encPkt.Unref()

	err := fw.encCodecContext.ReceivePacket(fw.encPkt)
	if err != nil {
		if errors.Is(err, astiav.ErrEagain) {
			// Eagain happens when the encoder wants more data to make a packet, e.g., audio.
			// However, incoming frames are muxed across multiple frame wrappers, each
			// with its own encoding context.
			log.Info().Int(lIndex, fw.streamIndex).Msg("warning: encoded frames may be interleaved")
		} else {
			log.Info().Int(lIndex, fw.streamIndex).Err(err).Msg("error receiving from encoder")
		}

		return nil
	}

	if fw.encPkt.Size() == 0 {
		log.Info().Int(lIndex, fw.streamIndex).Msg("empty packet encoded from frame")

		return nil
	}

	modelMedia := &model.Media{
		Key: &model.MediaKey{
			Url: fw.rawURL,
			SliceSpec: &model.MediaSliceSpec{
				StreamIndex: int32(fw.streamIndex),
			},
		},
		Info: &model.MediaInfo{
			Type:        fw.mimeType,
			Codec:       fw.sourceEncoding,
			PictureType: pictureTypeToPB[f.PictureType()],
			IsKeyFrame:  f.KeyFrame(),
		},
		Format: &model.Media_MediaBytes{
			MediaBytes: &model.MediaBytes{
				Data: fw.encPkt.Data(),
			},
		},
	}

	offset := ptsToDuration(f.Pts(), fw.encCodecContext.TimeBase())
	if offset != noPTS {
		modelMedia.Key.SliceSpec.Offset = durationpb.New(offset)
	}

	// Technically, the encoder can return multiple packets for a single frame
	// being fed in. We don't support that, so we'll just drain any remainders.
	for err == nil {
		fw.encPkt.Unref()
		err = fw.encCodecContext.ReceivePacket(fw.encPkt)
	}

	return modelMedia
}

// ToModelMedia encodes the frameWrapper's ffmpeg Frame into a model.Media.
// The first call to ToModelMedia will encode the frame, and subsequent
// calls will return the same model.Media until SetValid(true) is called.
// At that point, the next call to ToModelMedia will encode the new Frame.
// It can return nil if encoding fails or if the embedded Frame is declared
// invalid before this method is called at least once.
func (fw *frameWrapper) ToModelMedia() *model.Media {
	fw.lock.Lock()
	defer fw.lock.Unlock()

	// If the frame was already turned into a model.Media, we can use it as-is.
	// Note, this can still work even while the underlying ffmpeg frame
	// is invalid and being written. Only once the Frame is declared valid will
	// this cached modelMedia be reset.
	if fw.modelMedia != nil {
		return fw.modelMedia
	}

	// If the underlying ffmpeg frame has been invalidated, it's being reused
	// so we can no longer try to encode it.
	// This generally shouldn't happen, but it could in some unlucky situations.
	// (It probably means there's already a new frame in the tap.)
	if !fw.inputValid {
		return nil
	}

	// Else, we have an encoder.
	// If there's no filter, we can just encode fw.Frame.
	if fw.filterGraph == nil {
		fw.modelMedia = fw.encodeMedia(fw.Frame)

		return fw.modelMedia
	}

	// Else, we have a filter.
	if err := fw.buffersrcContext.BuffersrcAddFrame(fw.Frame, buffersrcFlags); err != nil {
		log.Info().Int(lIndex, fw.streamIndex).Err(err).Msg("buffersrc add frame error")

		return nil
	}

	fw.filterFrame.Unref()

	if err := fw.buffersinkContext.BuffersinkGetFrame(fw.filterFrame, buffersinkFlags); err != nil {
		if !errors.Is(err, astiav.ErrEof) && !errors.Is(err, astiav.ErrEagain) {
			log.Info().Int(lIndex, fw.streamIndex).Err(err).Msg("buffersink get frame error")
		}

		return nil
	}

	fw.filterFrame.SetPictureType(astiav.PictureTypeNone)

	fw.modelMedia = fw.encodeMedia(fw.filterFrame)

	// Technically, the buffersink can return multiple frames for a single frame
	// being fed in. We don't support that, so we'll just drain any remainders.
	var err error
	for err == nil {
		fw.filterFrame.Unref()
		err = fw.buffersinkContext.BuffersinkGetFrame(fw.filterFrame, buffersinkFlags)
	}

	return fw.modelMedia
}

// Unwrap returns the underlying ffmpeg Frame by referencing its
// contents into the caller's frame.
func (fw *frameWrapper) Unwrap(frame *astiav.Frame) error {
	fw.lock.Lock()
	defer fw.lock.Unlock()

	if !fw.inputValid {
		return &wrapperInvalidError{}
	}

	frame.Unref()
	_ = frame.Ref(fw.Frame)

	return nil
}
