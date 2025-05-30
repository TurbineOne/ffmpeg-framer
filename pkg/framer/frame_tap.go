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
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"time"

	"github.com/asticode/go-astiav"
	"github.com/rs/zerolog"
	"golang.org/x/exp/slices"
	"google.golang.org/protobuf/proto"

	"github.com/TurbineOne/ffmpeg-framer/api/proto/gen/go/fps/model"
)

type noTappedStreamsError struct{}

func (*noTappedStreamsError) Error() string {
	return "no tapped streams"
}

type unsupportedMediaFormatError struct {
	format interface{}
}

func (e *unsupportedMediaFormatError) Error() string {
	return fmt.Sprintf("unsupported media format: %T", e.format)
}

type duplicateOutputFileError struct {
	stat os.FileInfo
}

func (e *duplicateOutputFileError) Error() string {
	return fmt.Sprintf("media would overwrite existing output file: %+v", e.stat)
}

// frameTapStream represents a single stream within the frame tap.
type frameTapStream struct {
	frameC chan wrapper

	// State for sub-sampling frames on primary streams:
	timeBase    astiav.Rational // can be different per-stream, hence ptsInterval is per-stream
	ptsInterval int64           // minimum presentation time stamp (PTS) between sending frames
	prevPTS     int64           // previous sent frame's PTS
	frameSkips  int             // When this reaches frameInterval, we send a frame on this stream.

	// State for detecting reused frames on receive side:
	prevRxOffset time.Duration // previously received frame offset
}

func (fts *frameTapStream) MarshalZerologObject(e *zerolog.Event) {
	e.Bool(lRequested, fts.frameC != nil).
		Int64(lPTSInterval, fts.ptsInterval)
}

// frameTap represents a model.Media output for a source. It is the result
// of a Start() request. Subsequent requests for frames consume from the
// slice of channels embedded in the frameTap, one channel per stream.
// When the client is requesting frames faster than they're coming in,
// the client is blocked on this set of channels. When the source is generating
// frames faster than the client, the tap drops older frames in favor of newer
// ones, preventing stale frames being seen by the client. So each channel
// acts like a 1-deep, head-drop buffer.
type frameTap struct {
	source           *source
	streams          map[int]*frameTapStream
	outputPath       string
	outputFilePrefix string // unique to this tap
	maxStreamIndex   int
	framesIn         []int32
	framesDropped    []int32

	// Init() sets these:
	frameInterval int // how many frames read in per sampled frame sent out
	// selectCases is all stream.frameC channels, plus a spare for a ctx, added at runtime.
	// Case index may not match stream index because requested streams is sparse,
	// and this slice must be dense for reflect.Select().
	selectCases []reflect.SelectCase

	closeOnce sync.Once
	closeErrC chan error
}

// newFrameTap creates a new frame tap for the given source.
// The resulting tap must be initialized before adding to the source.
func newFrameTap(s *source) *frameTap {
	mediaInfo := s.MediaInfo()
	t := &frameTap{
		source:  s,
		streams: make(map[int]*frameTapStream),

		closeErrC: make(chan error, 1),
	}

	decStreams := s.DecStreams()

	for i32 := range mediaInfo.Streams {
		i := int(i32)

		var tb astiav.Rational
		if decStreams[i] != nil {
			tb = decStreams[i].TimeBase
		}

		t.streams[i] = &frameTapStream{
			timeBase: tb,
		}

		if i > t.maxStreamIndex {
			t.maxStreamIndex = i
		}
	}

	t.framesIn = make([]int32, t.maxStreamIndex+1)
	t.framesDropped = make([]int32, t.maxStreamIndex+1)

	return t
}

// randString generates a random string of length n.
// Characters are compatible with file names.
func randString(n int) string {
	const letterBytes = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

	b := make([]byte, n)

	for i := range b {
		b[i] = letterBytes[rand.Intn(len(letterBytes))]
	}

	return string(b)
}

// Init initializes the frameTap with the given parameters.
func (t *frameTap) Init(tappedStreams []int32, frameInterval int,
	timeInterval time.Duration, outputPath string,
) error {
	t.frameInterval = frameInterval
	t.outputPath = outputPath

	const randomizerLength = 6

	// Any truncation will start cutting at the front of the string, so we add
	// our unique random string toward the end.
	t.outputFilePrefix = t.source.outFilePrefix + "-" + randString(randomizerLength)

	logTapStreams := zerolog.Arr()
	tappedSomething := false

	for i, stream := range t.streams {
		stream.ptsInterval = durationToPts(timeInterval, stream.timeBase)
		// We init prevPTS and frameSkips such that the first frame is always sent.
		stream.prevPTS = -stream.ptsInterval - 1 // -1 prevents a 0 PTS from looking like a duplicate
		stream.frameSkips = frameInterval

		if slices.Contains(tappedStreams, int32(i)) {
			tappedSomething = true
			stream.frameC = make(chan wrapper, 1)
			t.selectCases = append(t.selectCases,
				reflect.SelectCase{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(stream.frameC)})
		}

		logTapStreams.Object(stream)
	}

	if !tappedSomething {
		return &noTappedStreamsError{}
	}

	// We add a spare case to hold a ctx channel, which must be added at runtime.
	t.selectCases = append(t.selectCases, reflect.SelectCase{})

	log.Info().Str(lURL, t.source.url).Array(lTapStreams, logTapStreams).
		Int(lFrameInterval, t.frameInterval).Dur(lTimeInterval, timeInterval).
		Msg("frame tap init")

	return nil
}

// DrainUntilClosed consumes and discards tap frames until the tap is closed.
// This is called in the context of the consumer when it's done with the tap
// and requesting its removal (though normally called by the source's RemoveTap()
// method). If the source is currently blocked on this tap, draining releases it,
// allowing the source to finish a pending Send() and unlock the taps mutex so
// removal can complete.
func (t *frameTap) DrainUntilClosed() {
	for _, s := range t.streams {
		if s.frameC != nil {
			go func(c <-chan wrapper) {
				for range c {
					// Drain.
				}
			}(s.frameC)
		}
	}
}

// Close is called by the source and closes the channels, alerting the consumer
// that the tap is done.
func (t *frameTap) Close(err error) {
	t.closeOnce.Do(func() {
		t.closeErrC <- err
		close(t.closeErrC)

		for _, s := range t.streams {
			if s.frameC != nil {
				close(s.frameC)
			}
		}

		log.Info().Err(err).Str(lURL, t.source.url).
			Ints32(lFrameCount, t.framesIn).Ints32(lFrameDropCount, t.framesDropped).
			Msg("frame tap closed")
	})
}

// subSample returns true if the frame should be sent to the client.
func (t *frameTap) subSample(stream *frameTapStream, w wrapper) bool {
	pts := w.PTS()

	// We require timestamps to be unique, so we always drop frames that have the
	// same PTS as the previous frame emitted on this stream.
	if stream.prevPTS == pts {
		log.Debug().Int(lIndex, w.StreamIndex()).Int64(lPTS, pts).
			Int(lTimeBaseNum, stream.timeBase.Num()).Int(lTimeBaseDen, stream.timeBase.Den()).
			Str(lURL, t.source.url).Msg("dropping frame with duplicate PTS")

		return false
	}

	// If we're time sampling, we drop frames that are too close to the previous frame in time.
	if stream.ptsInterval != 0 && pts-stream.prevPTS < stream.ptsInterval {
		return false
	}

	// If we're frame sampling, we drop all but every n'th frame.
	if t.frameInterval != 0 && stream.frameSkips < t.frameInterval {
		stream.frameSkips++

		return false
	}

	stream.frameSkips = 0
	stream.prevPTS = pts

	return true
}

// Send is called by the source to send data to the tap.
// This may only be called by the source until the tap is removed from the source.
// This is not thread-safe because it assumes we can empty and re-fill the
// channel without blocking. But it's only called by the source, which is
// single-threaded.
func (t *frameTap) Send(ctx context.Context, w wrapper, lossy bool) {
	streamIndex := w.StreamIndex()

	stream, ok := t.streams[streamIndex]
	if !ok {
		log.Info().Int(lIndex, streamIndex).Msg("stream index not in tap channels")

		return
	}

	t.framesIn[streamIndex]++

	if !t.subSample(stream, w) {
		// Intentionally sub-sampling isn't counted as a "drop" in the stats.
		return
	}

	if stream.frameC != nil {
		// If this is lossy, we clear any stale data out of the channel,
		// if still there. That makes sure the channel send won't block, and the
		// channel receive will only see the very latest frame.
		if lossy {
			select {
			case <-stream.frameC:
				t.framesDropped[streamIndex]++
			default:
			}
		}
		// Now we buffer the data into the channel, potentially blocking if !lossy.
		stream.frameC <- w
	}
}

// wrappersToModelMedias converts a slice of wrappers to a slice of model.Medias
// by calling ToModelMedia() on each. It skips any nil wrappers in the slice,
// as these streams were either not present or not requested by the client.
// It returns nil if nothing was successfully encoded. This can happen if
// one of the wrappers fails encoding, or if we were late encoding it and the
// internal media was already reclaimed. It's also possible to return a slice
// containing valid medias, but not one from a "primary" stream.
func (t *frameTap) wrappersToModelMedias(wrappers []wrapper) []*model.Media {
	mms := make([]*model.Media, 0, len(wrappers))
	gotValidMedia := false

	for i, w := range wrappers {
		if w == nil {
			continue
		}

		mm := w.ToModelMedia()
		if mm == nil {
			// A nil mm means we were late encoding the frame and it was already
			// reclaimed, or there was some other error encoding it.
			log.Debug().Int(lIndex, i).Msg("lost frame")
			// TODO: Probably should t.framesDropped++ here, but we're async so
			// that would need a mutex.

			continue
		}

		// Race condition: It's possible that we receive a frame/packet wrapper from
		// the stream.frameC, but the wrapper gets reused before we can encode it here.
		// Then the same wrapper gets sent back to frameC, resulting in us seeing
		// the same wrapper here twice in a row. We can detect this by checking the
		// offset of the slice spec, which should be unique. This only happens on
		// streams where the sender is running lossy and we get a burst of ~3
		// frames for the same stream back-to-back before the next request.
		// If it happens, we just drop the second copy.
		offset := mm.GetKey().GetSliceSpec().GetOffset().AsDuration()
		si := w.StreamIndex()

		if t.streams[si].prevRxOffset != 0 && offset == t.streams[si].prevRxOffset {
			log.Debug().Interface("key", mm.GetKey()).Int64(lPTS, w.PTS()).Str("prefix", t.outputFilePrefix).
				Msg("tap rx duplicate offset, likely reused wrapper")

			continue
		}

		t.streams[si].prevRxOffset = offset

		// We're reaching into t.source to grab mediaInfo, but this is safe because
		// it's only touched during setup, so by the time we're grabbing frames,
		// we don't need to check the lock.
		mm.ContainerInfo = t.source.mediaInfo
		mms = append(mms, mm)
		gotValidMedia = true
	}

	if !gotValidMedia {
		return nil
	}

	return mms
}

// nonblockingRx is a non-blocking poll of the tap's channels. It modifies the
// backing store of wrappers in place, adding any frames that are available.
// Returns the number of frames received.
func (t *frameTap) nonblockingRx(wrappers []wrapper) (count int) {
	for i, s := range t.streams {
		if s.frameC == nil {
			continue
		}

		select {
		case w, ok := <-s.frameC:
			if !ok {
				// This stream is now closed, but we may still need to drain others.
				break
			}

			wrappers[i] = w
			count++
		default:
		}
	}

	return count
}

// blockingRx blocks on all the channels in the tap until one of them has data
// or all of them have closed. It modifies the backing store of wrappers in place,
// adding any frames that are available.
func (t *frameTap) blockingRx(ctx context.Context, wrappers []wrapper) error {
	// The rest of t.selectCases are already set to the channels we care about.
	// We overwrite the last case in the slice to be current ctx channel.
	t.selectCases[len(t.selectCases)-1] = reflect.SelectCase{
		Dir:  reflect.SelectRecv,
		Chan: reflect.ValueOf(ctx.Done()),
	}

	for {
		if len(t.selectCases) == 1 {
			// All the channels have closed, so there should be an error waiting for us.
			err := <-t.closeErrC
			if err == nil {
				err = io.EOF // Caller expects a non-nil error to denote closure.
			}

			return err
		}

		// We don't yet have a frame. Block on all channels:
		i, value, ok := reflect.Select(t.selectCases)

		// The only value of i that tells us anything is the last one, which is the ctx.
		// This i may not be the same as the stream index.
		if i == len(t.selectCases)-1 {
			// The selected channel was the ctx.
			return ctx.Err()
		}

		if !ok {
			// Any other channel closure signals that a stream has closed.
			// However, others may still have data for us, so we pull this one out
			// of the list and try again.
			t.selectCases = slices.Delete(t.selectCases, i, i+1)

			continue
		}

		w := value.Interface().(wrapper) //nolint:forcetypeassert // We know.
		wrappers[w.StreamIndex()] = w

		return nil
	}
}

// formatDurationSortable formats a time.Duration as a fixed-width sortable string
// up to double-digit hours and nanosecond precision in a way that is compatible
// with filenames.
func formatDurationSortable(d time.Duration) string {
	sign := ""
	if d < 0 {
		sign = "-"
		d = -d
	}

	hours := d / time.Hour
	d %= time.Hour
	minutes := d / time.Minute
	d %= time.Minute
	seconds := d / time.Second
	d %= time.Second
	nanos := d

	return fmt.Sprintf("%s%02dh%02dm%02d.%09ds", sign, hours, minutes, seconds, nanos)
}

// mediaOutputFileName generates a filename for a media output file based on the
// media's source stream index and time offset.
func mediaOutputFileName(media *model.Media, prefix string) string {
	// We guess at a file extension. If mime doesn't work, we try the encoding.
	ext, ok := mimeTypeToExtension[media.GetInfo().GetType()]
	if !ok {
		ext = media.GetInfo().GetCodec()
		if ext == "none" || ext == "" {
			ext = "bin"
		}
	}

	sliceSpec := media.GetKey().GetSliceSpec()
	name := fmt.Sprintf("%s-%d-%s.%s", prefix, sliceSpec.GetStreamIndex(),
		formatDurationSortable(sliceSpec.GetOffset().AsDuration()), ext)

	// If we exceed the max filename length, we truncate toward the end, which
	// is more likely to be unique and has the right extension.
	const maxNameLen = 255
	if len(name) > maxNameLen {
		name = name[len(name)-maxNameLen:]
	}

	return name
}

// protoShallowCopy makes a shallow copy of a proto.Message.
// It only copies the first layer of the message, unlike proto.Clone().
func protoShallowCopy(original proto.Message) proto.Message {
	if original == nil {
		return nil
	}

	// We first make a new instance of the same type as the original.
	originalValue := reflect.ValueOf(original).Elem()
	copyValue := reflect.New(originalValue.Type()).Elem()

	// Then copy all the fields that are settable.
	for i := 0; i < originalValue.NumField(); i++ {
		field := originalValue.Field(i)
		if field.CanSet() {
			copyValue.Field(i).Set(field)
		}
	}

	return copyValue.Addr().Interface().(proto.Message) //nolint:forcetypeassert // See declaration.
}

// mediaToFile writes 'media's data to a file in 'outDir' with a name
// based on the media's source stream index and time offset.
// Returns an updated copy of the media object.
//
// TODO(casey): This approach is just to bootstrap the file output feature.
// We could consider moving this closer to the `encodeMedia` method,
// where we might be able to avoid some data copying.
func mediaToFile(media *model.Media, outDir, prefix string) (*model.Media, error) {
	// If this isn't an inline bytes frame, we leave it as-is.
	// TODO(casey): When the datalake is ready to handle raw bytes, we could
	// be smarter and hand back inline bytes for small or zero-byte frames.
	// Right now, everything must come back as a file, even if it's empty.
	formatMB, ok := media.GetFormat().(*model.Media_MediaBytes)
	if !ok {
		return nil, &unsupportedMediaFormatError{media.GetFormat()}
	}

	name := filepath.Join(outDir, mediaOutputFileName(media, prefix))

	stat, err := os.Stat(name)
	if err == nil {
		return nil, &duplicateOutputFileError{stat}
	}

	f, err := os.Create(name)
	if err != nil {
		// This seems to be happening in the wild, so here's some
		// extra debug info to help us figure out why. We can remove this later.
		dirEntries, dirErr := os.ReadDir(outDir)
		if dirErr != nil {
			return nil, fmt.Errorf("failed to create media file, dir failed: %w", dirErr)
		}

		return nil, fmt.Errorf("failed to create media file, dir entries: %d error: %w", len(dirEntries), err)
	}

	defer func() { _ = f.Close() }()

	_, err = f.Write(formatMB.MediaBytes.GetData())
	if err != nil {
		return nil, fmt.Errorf("failed to write media output file: %w", err)
	}

	// The Wrapper manages a single instance of the media proto, and modifying it
	// directly would break other taps on the same live stream. So we make a copy.
	// We only need a shallow copy since we're only modifying the Format field.
	//
	// TODO(casey): I don't like this, but it works for now. If we moved
	// file conversion up to the wrapper, we *might* be able to avoid this, but
	// even then, the contract is that the requester has to delete the resulting files,
	// so we'd have to make copies of files for each. Maybe we should have the
	// datalake front the multiple async streamers case, and simplify framer
	// by removing the whole shared taps feature?
	mediaCopy := protoShallowCopy(media).(*model.Media) //nolint:forcetypeassert // We know.
	mediaCopy.Format = &model.Media_MediaPath{
		MediaPath: &model.MediaPath{
			Path: f.Name(),
		},
	}

	return mediaCopy, nil
}

// GetModelMedias returns a slice of model.Medias from t. If `poll` is true,
// it returns immediately, even if no medias are available. Otherwise, it
// blocks until at least one media is available.
// The returned error is either a context err or the disposition of the source.
func (t *frameTap) GetModelMedias(ctx context.Context, poll bool) ([]*model.Media, error) {
	var medias []*model.Media

	fws := make([]wrapper, t.maxStreamIndex+1)
	// For efficiency, we first try to pull once from each channel, non-blocking.
	if count := t.nonblockingRx(fws); count > 0 {
		medias = t.wrappersToModelMedias(fws)
	}

	if medias == nil && poll {
		// Nothing yet, return an empty slice.
		return []*model.Media{}, nil
	} else if medias == nil {
		for medias == nil { // Loop until encoding succeeds for at least one media.
			// fws holds results from each channel. We swap new frames into here
			// until we get one that we care about.
			// Block until we get a frame or something breaks.
			if err := t.blockingRx(ctx, fws); err != nil {
				return nil, err
			}

			medias = t.wrappersToModelMedias(fws)
		}
	}

	// If outputPath is set, the caller wants big frames returned as files, not inline.
	if t.outputPath != "" {
		fileMedias := medias[:0] // reuses the underlying array of medias

		for _, media := range medias {
			fileMedia, err := mediaToFile(media, t.outputPath, t.outputFilePrefix)
			if err != nil {
				log.Info().Err(err).Str(lURL, t.source.url).Msg("failed to convert frame to file")

				continue
			}

			fileMedias = append(fileMedias, fileMedia)
		}

		medias = fileMedias
	}

	return medias, nil
}
