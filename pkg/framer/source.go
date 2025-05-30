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
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/asticode/go-astiav"
	"github.com/rs/zerolog"
	"golang.org/x/exp/slices"

	"github.com/TurbineOne/ffmpeg-framer/api/proto/gen/go/fps"
	"github.com/TurbineOne/ffmpeg-framer/api/proto/gen/go/fps/model"
	"github.com/TurbineOne/ffmpeg-framer/api/proto/gen/go/fps/service"
	"github.com/TurbineOne/ffmpeg-framer/pkg/mimer"
)

type noStreamsError struct {
	url string
}

func (e *noStreamsError) Error() string {
	return fmt.Sprintf("no streams found in %q", e.url)
}

type sourceExitedError struct{}

func (e *sourceExitedError) Error() string {
	return "failed to create reference; source already exited"
}

type seekNotSupportedError struct {
	url string
}

func (e *seekNotSupportedError) Error() string {
	return fmt.Sprintf("seek not supported for %q", e.url)
}

// tap represents a tap on a source. Taps are used to either consume individual
// frames from the source, or to monitor a recording of the source.
type tap interface {
	Send(context.Context, wrapper, bool)
	DrainUntilClosed()
	Close(error) // The Close err is the disposition of the source, i.e., why it closed.
}

// source represents a source of frames, either a network stream or a file.
type source struct {
	// Set at Init():
	url             string
	scheme          string
	outFilePrefix   string
	fileThrottle    fps.SourceFileFraming_FileThrottle
	seekTime        time.Duration
	startAtKeyframe bool
	LiveStream      bool // a source that cannot be throttled and should be reused by taps
	hwDecoderEnable bool

	// Reference counting lets us share a source among multiple clients.
	refLock     sync.Mutex
	refCount    int
	refDisabled bool
	cancel      context.CancelFunc

	// State shared with the taps:
	tapsLock     sync.Mutex
	tapsDisabled bool
	demuxerTaps  []tap // Taps that want raw demuxed packets before any decoding (iow, record clients)
	decoderTaps  []tap // Taps that want decoded frames, if available (iow, frame clients)
	tapAddedC    chan struct{}

	// Internal state:
	// The MediaInfo and streams are not available until we have a chance to read
	// the source, so the writer lock is held at newSource() until setup completes.
	// This MediaInfo includes streams and is returned to callers of Start().
	// It refers to the source media we're asked to frame, not the individual frames.
	// So this mediaInfo is also returned as the ContainerMediaInfo for individual frames.
	mediaInfoLock sync.RWMutex
	setupErr      error // If we fail to get through setup, the error is preserved here.
	mediaInfo     *model.MediaInfo
	lossySend     bool // taps drop packets if not consumed fast enough
	isContainer   bool // e.g., mp4, as opposed to image formats like gif or jpg.
	inStreams     map[int]*inStream
	decStreams    map[int]*decStream

	// FFmpeg state:
	inputFormatContext *astiav.FormatContext
}

// newSource creates a new source.
func newSource() *source {
	s := &source{
		mediaInfo: &model.MediaInfo{
			Streams: make(map[int32]*model.StreamInfo),
		},

		tapAddedC: make(chan struct{}, 1),

		inStreams:  make(map[int]*inStream),
		decStreams: make(map[int]*decStream),
	}
	s.mediaInfoLock.Lock() // unlocked once the source is started

	return s
}

// Init initializes the source. It must be called before running the source.
func (s *source) Init(rawURL string, framing *fps.SourceFileFraming,
	isLive bool, seek *service.FrameRequest_Seek, hwDecoderEnable bool,
) error {
	s.url = rawURL
	s.LiveStream = isLive
	s.fileThrottle = framing.GetThrottle()
	s.seekTime = seek.GetTime().AsDuration()
	s.startAtKeyframe = seek.GetClosestKeyframe()
	s.hwDecoderEnable = hwDecoderEnable

	if parsedURL, err := url.Parse(s.url); err != nil {
		if !strings.HasPrefix(s.url, "/") {
			return fmt.Errorf("parsing url failed: %w", err)
		}

		// not all file names can be parsed as URLs because of special characters
		s.scheme = ""
	} else {
		s.scheme = parsedURL.Scheme
	}

	// If this source's frames are requested as files, this is the filename prefix.
	s.outFilePrefix = strings.Trim(s.url, string(filepath.Separator))
	for _, char := range []string{":", "?", "*", "\"", "<", ">", "|", "\\", "/"} {
		s.outFilePrefix = strings.ReplaceAll(s.outFilePrefix, char, "_")
	}

	log.Info().Str(lURL, s.url).Str(lThrottle, s.fileThrottle.String()).
		Bool(lLive, s.LiveStream).Dur(lSeekTime, s.seekTime).Msg("source init")

	return nil
}

// Ref adds a reference to s. If s has already exited, returns an error.
func (s *source) Ref() error {
	s.refLock.Lock()
	defer s.refLock.Unlock()

	if s.refDisabled {
		return &sourceExitedError{}
	}

	s.refCount++

	return nil
}

// Unref removes a reference from s. If s has no more references, cancels its context.
func (s *source) Unref() {
	s.refLock.Lock()
	defer s.refLock.Unlock()

	s.refCount--

	if s.refCount == 0 {
		s.refDisabled = true
		if s.cancel != nil {
			s.cancel()
		}
	}
}

// ffmpegStreamToStreamInfo converts an FFmpeg stream to a StreamInfo.
func ffmpegStreamToStreamInfo(ffmpegStream *astiav.Stream) *model.StreamInfo {
	streamInfo := &model.StreamInfo{
		Index: int32(ffmpegStream.Index()),
		// TODO(casey): This version of astiav doesn't expose IANA media types
		// for streams, so we're just passing the MediaType string,
		// e.g., "video". We could try to handle this better.
		Type:              ffmpegStream.CodecParameters().MediaType().String(),
		Codec:             ffmpegStream.CodecParameters().CodecID().Name(),
		Duration:          ptsToDurationPB(ffmpegStream.Duration(), ffmpegStream.TimeBase()),
		StartOffset:       ptsToDurationPB(ffmpegStream.StartTime(), ffmpegStream.TimeBase()),
		AvgFrameRate:      rationalToPB(ffmpegStream.AvgFrameRate()),
		RealBaseFrameRate: rationalToPB(ffmpegStream.RFrameRate()),
		TimeBase:          rationalToPB(ffmpegStream.TimeBase()),
	}

	switch ffmpegStream.CodecParameters().MediaType() {
	case astiav.MediaTypeVideo:
		streamInfo.Stream = &model.StreamInfo_Video{Video: &model.VideoStreamInfo{
			FrameCount: ffmpegStream.NbFrames(),
			Width:      int32(ffmpegStream.CodecParameters().Width()),
			Height:     int32(ffmpegStream.CodecParameters().Height()),
			Fps:        ffmpegStream.RFrameRate().ToDouble(),
		}}
	case astiav.MediaTypeAudio:
		streamInfo.Stream = &model.StreamInfo_Audio{Audio: &model.AudioStreamInfo{
			SampleCount:      ffmpegStream.NbFrames(),
			SamplesPerSecond: float64(ffmpegStream.CodecParameters().SampleRate()),
			Channels:         int32(ffmpegStream.CodecParameters().Channels()),
		}}
	case astiav.MediaTypeSubtitle:
		streamInfo.Stream = &model.StreamInfo_Subtitle{Subtitle: &model.SubtitleStreamInfo{}}
	case astiav.MediaTypeData:
		streamInfo.Stream = &model.StreamInfo_Data{Data: &model.DataStreamInfo{}}
	case astiav.MediaTypeAttachment:
		streamInfo.Stream = &model.StreamInfo_Attachment{Attachment: &model.AttachmentStreamInfo{}}
	case astiav.MediaTypeUnknown:
	default:
		streamInfo.Stream = &model.StreamInfo_Unknown{Unknown: &model.UnknownStreamInfo{}}
	}

	if metaDict := ffmpegStream.Metadata(); metaDict != nil {
		streamInfo.Metadata = dictToMap(metaDict)
	}

	return streamInfo
}

// resetStreams resets all streams and stream info.
func (s *source) resetStreams(ffmpegInStreams []*astiav.Stream) map[int32]*model.StreamInfo {
	for _, inStream := range s.inStreams {
		inStream.Close()
	}

	for _, decStream := range s.decStreams {
		decStream.Close()
	}

	s.inStreams = make(map[int]*inStream)
	s.decStreams = make(map[int]*decStream)

	modelStreamInfo := make(map[int32]*model.StreamInfo, len(ffmpegInStreams))

	for _, ffmpegStream := range ffmpegInStreams {
		index := ffmpegStream.Index()
		// We create a map of stream info to return to callers of Start().
		modelStreamInfo[int32(index)] = ffmpegStreamToStreamInfo(ffmpegStream)

		s.inStreams[index] = newInStream()
		s.inStreams[index].Init(ffmpegStream, s.url)
		s.decStreams[index] = newDecStream()
		s.decStreams[index].Init(s.inputFormatContext, ffmpegStream, s.url, s.hwDecoderEnable)
	}

	// Set the start time for all streams.
	if s.seekTime > 0 && !s.LiveStream {
		// convert seconds to timebase
		streamTime := int64(s.seekTime.Seconds() * float64(astiav.TimeBase))
		flags := astiav.NewSeekFlags(astiav.SeekFlagBackward, astiav.SeekFlagFrame)

		log.Debug().Dur(lSeekTime, s.seekTime).Int64(lStreamTime, streamTime).
			Msg("seeking")

		err := s.inputFormatContext.SeekFrame(-1, streamTime, flags)
		if err != nil {
			log.Info().Dur(lSeekTime, s.seekTime).Int64(lStreamTime, streamTime).
				Err(err).Msg("error seeking")
		}
	}

	// If we have a simple image file that does not require streaming,
	// we return media info with isContainer=false and disable streaming.
	// Callers should just grab the image directly.
	//
	// NOTE(casey): I've looked a bunch of different ways of detecting this, but
	// none of the fields you'd want to use are consistent, and I'm not really
	// sure what we want to do for animated gifs and jpegs.
	// - media_type is video(0) for all image formats, regardless
	// - nb_frames is zero on some video streams that have many frames
	// - duration is often 1 for images, but sometimes not, even for non-animated gifs.
	// So I opted for just checking specific codecs, and we can expand this list
	// or refine the logic as needed.
	s.isContainer = true
	if !s.LiveStream && len(ffmpegInStreams) == 1 {
		codecID := ffmpegInStreams[0].CodecParameters().CodecID()
		switch codecID {
		case astiav.CodecIDAnsi,
			astiav.CodecIDBmp,
			astiav.CodecIDGif,
			astiav.CodecIDJpeg2000,
			astiav.CodecIDJpegls,
			astiav.CodecIDMjpeg,
			astiav.CodecIDPam,
			astiav.CodecIDPbm,
			astiav.CodecIDPgm,
			astiav.CodecIDPgmyuv,
			astiav.CodecIDPng,
			astiav.CodecIDPpm,
			astiav.CodecIDTiff,
			astiav.CodecIDWebp:
			log.Info().Str(lURL, s.url).Str(lCodec, codecID.String()).Msg("non-container image format")

			s.isContainer = false
		}
	}

	return modelStreamInfo
}

// setup is internal and called by the source's main run loop.
func (s *source) setup() error {
	// Regardless of how we exit, we need to allow callers to read whatever
	// stream metadata we manage to collect.
	defer s.mediaInfoLock.Unlock()

	s.inputFormatContext = astiav.AllocFormatContext()

	optsDict := astiav.NewDictionary()
	defer optsDict.Free()

	// This flag should only be needed for hls streams, but it doesn't hurt to set it.
	// Start at the last segment (most recent) for live streams.
	if err := optsDict.Set("live_start_index", "-1", astiav.DictionaryFlags(0)); err != nil {
		s.setupErr = fmt.Errorf("setting live_start_index failed: %w", err)

		return s.setupErr
	}

	if s.LiveStream {
		if err := optsDict.Set("use_wallclock_as_timestamps", "1", astiav.DictionaryFlags(0)); err != nil {
			s.setupErr = fmt.Errorf("setting use_wallclock_as_timestamps failed: %w", err)

			return s.setupErr
		}
	}

	if err := s.openInput(nil, optsDict); err != nil {
		s.setupErr = err

		return s.setupErr
	}

	// We let the taps drop frames if it's a live stream, or if lock-step throttle is disabled.
	s.lossySend = s.LiveStream || (s.fileThrottle != fps.SourceFileFraming_FILE_THROTTLE_LOCK_STEP)

	ffmpegInStreams := s.inputFormatContext.Streams()
	if len(ffmpegInStreams) == 0 {
		s.setupErr = &noStreamsError{s.url}

		return s.setupErr
	}

	streams := s.resetStreams(ffmpegInStreams)

	// Technically, avlib can handle this, but the API to get this info is not
	// exposed by the current version of astiav, so we do it here.
	mimeType := mimer.UnknownMediaType
	if s.scheme == "" {
		mimeType = mimer.GetContentType(s.url)
	}

	s.mediaInfo = &model.MediaInfo{
		Type:     mimeType,
		Duration: ptsToDurationPB(s.inputFormatContext.Duration(), astiav.TimeBaseQ),
		Streams:  streams,

		IsContainer: s.isContainer,
		IsSeekable:  !s.LiveStream,
	}

	logInStreams := zerolog.Arr()
	for _, st := range s.inStreams {
		logInStreams.Object(st)
	}

	logDecStreams := zerolog.Arr()
	for _, st := range s.decStreams {
		logDecStreams.Object(st)
	}

	log.Info().Str(lURL, s.url).Bool(lLossy, s.lossySend).
		Str(lInFormatFlags, ioFormatFlagsToString(s.inputFormatContext.InputFormat().Flags())).
		Array(lInStreams, logInStreams).Array(lDecStreams, logDecStreams).
		Msg("source setup")

	return nil
}

// sendDemuxerTaps sends w to each demuxer tap.
func (s *source) sendDemuxerTaps(ctx context.Context, w wrapper) {
	// We have to hold the lock for the entire duration of this function.
	// This prevents a tap from being asynchronously Stop()'d and its channels
	// closed while we're writing to them.
	s.tapsLock.Lock()
	defer s.tapsLock.Unlock()

	for _, t := range s.demuxerTaps {
		t.Send(ctx, w, s.lossySend)
	}
}

// sendDecoderTaps sends w to each decoder tap.
func (s *source) sendDecoderTaps(ctx context.Context, w wrapper) {
	// Demuxing starts at a keyframe, so we can't send frames until we've
	// demuxed to the start time.
	if !s.LiveStream && !s.startAtKeyframe && s.seekTime > 0 && ptsToDuration(w.PTS(), w.TimeBase()) < s.seekTime {
		log.Trace().Int(lIndex, w.StreamIndex()).Dur(lSeekTime, s.seekTime).
			Dur(lPTS, ptsToDuration(w.PTS(), w.TimeBase())).Msg("skipping frame prior to startTime")

		return
	}

	// We have to hold the lock for the entire duration of this function.
	// This prevents a tap from being asynchronously Stop()'d and its channels
	// closed while we're writing to them.
	s.tapsLock.Lock()
	defer s.tapsLock.Unlock()

	for _, t := range s.decoderTaps {
		t.Send(ctx, w, s.lossySend)
	}
}

func (s *source) SetupErr() error {
	s.mediaInfoLock.RLock()
	defer s.mediaInfoLock.RUnlock()

	return s.setupErr
}

func (s *source) MediaInfo() *model.MediaInfo {
	s.mediaInfoLock.RLock()
	defer s.mediaInfoLock.RUnlock()

	return s.mediaInfo
}

// InStreams returns the map of input streams. This accessor is just here so
// a destination can align its output streams with the source's input streams.
func (s *source) InStreams() map[int]*inStream {
	s.mediaInfoLock.RLock()
	defer s.mediaInfoLock.RUnlock()

	return s.inStreams
}

// DecStreams returns the map of decoder streams. This accessor is just here so
// a frame tap can align its streams with ours.
func (s *source) DecStreams() map[int]*decStream {
	s.mediaInfoLock.RLock()
	defer s.mediaInfoLock.RUnlock()

	return s.decStreams
}

type tapLayer int

const (
	tapLayerDemuxer tapLayer = iota
	tapLayerDecoder
)

// AddTap adds a tap to 's'.
func (s *source) AddTap(t tap, layer tapLayer) error {
	s.tapsLock.Lock()
	defer s.tapsLock.Unlock()

	if s.tapsDisabled {
		return &sourceExitedError{}
	}

	switch layer {
	case tapLayerDemuxer:
		s.demuxerTaps = append(s.demuxerTaps, t)
	case tapLayerDecoder:
		s.decoderTaps = append(s.decoderTaps, t)
	}

	// Only the first tap is critical, so we don't block on it.
	select {
	case s.tapAddedC <- struct{}{}:
	default:
	}

	return nil
}

// RemoveTap removes a tap from 's'. This is an interface call for the consumer,
// symmetrical to AddTap(). The source itself should not use this call, as it
// drains the tap before closing, which could steal desirable frames from the
// consumer.
func (s *source) RemoveTap(t tap) {
	t.DrainUntilClosed()

	s.tapsLock.Lock()
	defer s.tapsLock.Unlock()

	t.Close(nil)

	if i := slices.Index(s.decoderTaps, t); i >= 0 {
		s.decoderTaps = slices.Delete(s.decoderTaps, i, i+1)

		return
	}

	if i := slices.Index(s.demuxerTaps, t); i >= 0 {
		s.demuxerTaps = slices.Delete(s.demuxerTaps, i, i+1)
	}
}

// exit cleans up the source, closing all taps and freeing ffmpeg resources.
func (s *source) exit(err error) {
	s.refLock.Lock()
	s.cancel() // Only affects the source Run loop.
	s.refDisabled = true
	s.refLock.Unlock()

	s.tapsLock.Lock()
	s.tapsDisabled = true
	decoderTaps := make([]tap, len(s.decoderTaps))
	copy(decoderTaps, s.decoderTaps)
	s.decoderTaps = nil
	demuxerTaps := make([]tap, len(s.demuxerTaps))
	copy(demuxerTaps, s.demuxerTaps)
	s.demuxerTaps = nil
	s.tapsLock.Unlock()

	for _, t := range decoderTaps {
		t.Close(err)
	}

	for _, t := range demuxerTaps {
		t.Close(err)
	}

	pktCounts := make([]int, len(s.inStreams))
	for i, st := range s.inStreams {
		pktCounts[i] = st.PktCount
		st.Close()
	}

	frameCounts := make([]int, len(s.decStreams))
	for i, st := range s.decStreams {
		frameCounts[i] = st.FrameCount
		st.Close()
	}

	log.Debug().Ints(lFrameCount, frameCounts).Ints(lPacketCount, pktCounts).
		Int(lDecoderTaps, len(decoderTaps)).Int(lDemuxerTaps, len(demuxerTaps)).
		Str(lURL, s.url).Msg("source exiting, taps closed")

	s.inputFormatContext.CloseInput()
	s.inputFormatContext.Free()
}

// flushDecoderToTaps flushes the decoder and sends any remaining frames to taps.
func (s *source) flushDecoderToTaps(ctx context.Context) {
	pendingTaps := false

	for _, decStream := range s.decStreams {
		if decStream == nil {
			continue
		}

		wrappers, err := decStream.DecodeFlush()
		if err != nil {
			continue
		}

		for _, w := range wrappers {
			s.sendDecoderTaps(ctx, w)

			pendingTaps = true
		}
	}

	// Give the taps a chance to drain before we exit.
	if pendingTaps {
		time.Sleep(1 * time.Second)
	}
}

// readAndDecode reads a packet from the source, decodes it if necessary, and
// distributes it to both packet and decoder taps. This is a blocking call.
func (s *source) readAndDecode(ctx context.Context, pkt *astiav.Packet) error {
	s.tapsLock.Lock()
	shouldDecode := len(s.decoderTaps) > 0
	s.tapsLock.Unlock()

	// If this source is a non-container file (e.g., gif, jpeg) we bail.
	if !s.isContainer {
		return io.EOF
	}

	pkt.Unref()

	// This read is blocking:
	if err := s.inputFormatContext.ReadFrame(pkt); err != nil {
		if errors.Is(err, astiav.ErrEof) {
			s.flushDecoderToTaps(ctx)

			err = io.EOF
		}

		return fmt.Errorf("source: input read failed: %w", err)
	}

	index := pkt.StreamIndex()

	// If we don't have a matching stream object, we skip this packet.
	inStream := s.inStreams[index]
	if inStream == nil {
		return nil
	}

	// If this is a file and we're throttling to the presentation rate, we
	// wait until the PTS. This is obviously blocking.
	if !s.LiveStream && s.fileThrottle == fps.SourceFileFraming_FILE_THROTTLE_PRESENTATION_RATE {
		if err := inStream.ThrottleWait(ctx, pkt); err != nil {
			return nil //nolint:nilerr // Likely a context cancellation, let caller figure it out.
		}
	}

	pw := inStream.WrapPacket(pkt)
	s.sendDemuxerTaps(ctx, pw) // This can also block, but probably not for long.

	if !shouldDecode {
		return nil
	}

	decStream := s.decStreams[index]
	if decStream == nil {
		return nil
	}

	wrappers, err := decStream.Decode(pw)
	if err != nil {
		log.Info().Err(err).Str(lURL, s.url).Int(lIndex, index).
			Msg("decode failed, dropping packet")

		return nil
	}

	for _, w := range wrappers {
		s.sendDecoderTaps(ctx, w)
	}

	return nil
}

// openInput opens the inputFormatContext for the source.
func (s *source) openInput(format *astiav.InputFormat, d *astiav.Dictionary) error {
	if err := s.inputFormatContext.OpenInput(s.url, format, d); err != nil {
		return fmt.Errorf("opening input failed: %w", err)
	}

	if err := s.inputFormatContext.FindStreamInfo(nil); err != nil {
		s.inputFormatContext.CloseInput()

		return fmt.Errorf("finding stream info failed: %w", err)
	}

	return nil
}

// reopenInput closes and reopens the inputFormatContext at the specified start time.
func (s *source) reopenInput(seekTime time.Duration, startAtKeyframe bool) error {
	s.inputFormatContext.CloseInput()
	log.Info().Str(lURL, s.url).Msg("reopening input file")

	optsDict := astiav.NewDictionary()
	defer optsDict.Free()

	// This flag should only be needed for hls streams, but it doesn't hurt to set it.
	// Start at the last segment (most recent) for live streams.
	if err := optsDict.Set("live_start_index", "-1", astiav.DictionaryFlags(0)); err != nil {
		return fmt.Errorf("setting live_start_index failed: %w", err)
	}

	if err := s.openInput(nil, optsDict); err != nil {
		return err
	}

	ffmpegStreams := s.inputFormatContext.Streams()
	s.mediaInfoLock.Lock()
	s.seekTime = seekTime
	s.startAtKeyframe = startAtKeyframe
	s.resetStreams(ffmpegStreams)
	s.mediaInfoLock.Unlock()

	return nil
}

// Seek seeks to the given timestamp in the input stream.
// At the moment this is only supported for single file non-live streams.
// And even then, only if you call seek before the first frame is read.
// TODO: Support more scenarios. This will require a rethink on
// mutexes and how we handle the inputFormatContext.
func (s *source) Seek(seekTime time.Duration, startAtKeyframe bool) error {
	if s.LiveStream {
		return &seekNotSupportedError{url: s.url}
	}

	return s.reopenInput(seekTime, startAtKeyframe)
}

// Run manages the source, reading packets from the input URL and distributing
// frames to taps.
func (s *source) Run(ctx context.Context) error {
	s.refLock.Lock()
	if s.refDisabled {
		s.refLock.Unlock()

		return nil
	}

	ctx, s.cancel = context.WithCancel(ctx)
	s.refLock.Unlock()

	var err error

	defer func() {
		s.exit(err)
	}()

	if err = s.setup(); err != nil {
		return err
	}

	// If this isn't live, we wait for the first tap to be added before reading.
	if !s.LiveStream {
		select {
		case <-s.tapAddedC:
		case <-ctx.Done():
			err = ctx.Err()

			return err
		}
	}

	pkt := astiav.AllocPacket()
	defer pkt.Free()

	for {
		// This loop blocks mostly on I/O  so we can only poll for ctx cancellation.
		if ctx.Err() != nil {
			err = ctx.Err()

			return err
		}

		if err = s.readAndDecode(ctx, pkt); err != nil {
			return err
		}
	}
}
