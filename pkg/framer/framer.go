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

// Package framer implements the Framer gRPC service.
// It can be invoked directly or by wrapping in a gRPC service endpoint.
package framer

import (
	"context"
	"sync"

	"github.com/asticode/go-astiav"
	"github.com/rs/zerolog"

	"github.com/TurbineOne/ffmpeg-framer/api/proto/gen/go/fps"
	"github.com/TurbineOne/ffmpeg-framer/api/proto/gen/go/fps/model"
	"github.com/TurbineOne/ffmpeg-framer/api/proto/gen/go/fps/service"
)

const (
	lCodec           = "codecID"
	lDecoder         = "decoder"
	lDecoderTaps     = "decoderTaps"
	lDecStreams      = "decStreams"
	lDemuxerTaps     = "demuxerTaps"
	lFile            = "file"
	lFFMpeg          = "ffmpeg"
	lFrameCount      = "frameCount"
	lFrameDropCount  = "frameDropCount"
	lFrameInterval   = "frameInterval"
	lFrameRateNum    = "frameRateNum"
	lFrameRateDen    = "frameRateDen"
	lIndex           = "streamIndex"
	lInFormatFlags   = "inFormatFlags"
	lInStreams       = "inStreams"
	lLive            = "live"
	lLoop            = "loop"
	lLossy           = "lossy"
	lMediaType       = "mediaType"
	lOutStreams      = "outStreams"
	lPacketCount     = "packetCount"
	lPacketDropCount = "packetDropCount"
	lPTS             = "pts"
	lPTSInterval     = "ptsInterval"
	lPrime           = "prime"
	lRecode          = "recode"
	lRequested       = "requested"
	lSeekTime        = "seekTime"
	lSourceCount     = "sourceCount"
	lSplit           = "split"
	lStreamInfo      = "streamInfo"
	lStreamTime      = "streamTime"
	lTapStreams      = "tapStreams"
	lTimeBaseNum     = "timeBaseNum"
	lTimeBaseDen     = "timeBaseDen"
	lTimeInterval    = "timeInterval"
	lThrottle        = "throttle"
	lURL             = "url"
	lWatch           = "watch"
)

//nolint:gochecknoglobals // allows logging from non-method funcs
var log zerolog.Logger

// wrapper represents a wrapped blob of ffmpeg data, either Frame or Packet.
type wrapper interface {
	ToModelMedia() *model.Media // Returned media may not have media.Info set.
	StreamIndex() int
	PTS() int64
	TimeBase() astiav.Rational
}

// Framer is the top-level implementation of the gRPC Framer service.
type Framer struct {
	config *Config

	// sourcesLock protects access to the sources lists.
	sourcesLock     sync.Mutex
	sources         map[*source]struct{}
	reusableSources map[string]*source // Mapped by raw URL from the user, not normalized.

	runSourceC chan *source

	service.UnimplementedFramerServer
}

// New returns a new Framer instance.
func New(config *Config, logger *zerolog.Logger) *Framer {
	log = logger.With().Str("pkg", "framer").Logger()

	if config.LogLevel != ConfigDefault().LogLevel {
		level, err := zerolog.ParseLevel(config.LogLevel)
		if err != nil {
			panic(err.Error())
		}

		log = log.Level(level)
	}

	ffmpegLoggerSetup(config)

	return &Framer{
		config: config,

		sources:         make(map[*source]struct{}),
		reusableSources: make(map[string]*source),

		runSourceC: make(chan *source, 1),
	}
}

// Init initializes the Framer.
func (f *Framer) Init() error {
	return nil
}

// getSource returns a reference-counted source. It might be an existing reusable
// source, or a newly initialized one. The caller is responsible
// for calling source.Unref() when finished with the source.
func (f *Framer) getSource(ctx context.Context, rawURL string,
	framing *fps.SourceFileFraming, isLive bool, seek *service.FrameRequest_Seek,
) (*source, error) {
	f.sourcesLock.Lock()
	defer f.sourcesLock.Unlock()

	// First we try to find an existing source and add a reference.
	s, ok := f.reusableSources[rawURL]
	if ok {
		if err := s.Ref(); err == nil {
			return s, nil
		}
	}

	// Failing that, we allocate, initialize, and run a new source.
	s = newSource()
	if err := s.Init(rawURL, framing, isLive, seek, f.config.HwDecoderEnable); err != nil {
		return nil, err
	}

	_ = s.Ref() // Can't fail, since we haven't run it yet.

	select {
	case f.runSourceC <- s:
	case <-ctx.Done():
		s.Unref()

		return nil, ctx.Err()
	}

	f.sources[s] = struct{}{}
	// We only reuse live-streamed sources; file sources are per-client.
	if s.LiveStream {
		f.reusableSources[s.url] = s
	}

	return s, nil
}

// runSource is meant to run async in a goroutine. It calls the source's
// Run() method and removes it from f.sources when it returns.
func (f *Framer) runSource(ctx context.Context, s *source) {
	log.Info().Str(lURL, s.url).Msg("source starting")
	err := s.Run(ctx)

	// This source has exited, but it's possible a new one has already
	// taken its place in the map from someone else calling StartFrames.
	f.sourcesLock.Lock()
	delete(f.sources, s)

	if f.reusableSources[s.url] == s {
		delete(f.reusableSources, s.url)
	}

	numSources := len(f.sources)
	f.sourcesLock.Unlock()

	log.Info().Err(err).Str(lURL, s.url).Int(lSourceCount, numSources).
		Msg("source exited")
}

// Run is the main Framer loop.
// We defer the running of sources to this run loop just so they can have the
// long-lived ctx, not the short-lived gRPC service call ctx.
func (f *Framer) Run(ctx context.Context) error {
	for {
		select {
		case s := <-f.runSourceC:
			go f.runSource(ctx, s)

		case <-ctx.Done():
			// This context finishing should also exit any sources.
			return nil
		}
	}
}
