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
	"strings"

	"github.com/asticode/go-astiav"
	"github.com/rs/zerolog"
)

var ffmpegLog zerolog.Logger

// ffmpegToZerologLevel maps ffmpeg's internal log levels to zerolog's.
// This maps is queried on every log invocation.
var ffmpegToZerologLevel = map[astiav.LogLevel]zerolog.Level{
	astiav.LogLevelQuiet:   zerolog.Disabled,
	astiav.LogLevelPanic:   zerolog.PanicLevel,
	astiav.LogLevelFatal:   zerolog.FatalLevel,
	astiav.LogLevelError:   zerolog.ErrorLevel,
	astiav.LogLevelWarning: zerolog.WarnLevel,
	astiav.LogLevelInfo:    zerolog.InfoLevel,
	astiav.LogLevelVerbose: zerolog.DebugLevel, // FFmpeg's verbose is more like zerolog's debug...
	astiav.LogLevelDebug:   zerolog.TraceLevel, // because ffmpeg's debug is more like zerolog's trace.
}

// nameToFfmpegLogLevel maps ffmpeg's log level names to their internal values.
// We have to use ffmpeg's native levels because there are more of them than
// there are zerolog levels, so the mapping isn't 1:1.
// We only use this for config translation at startup.
var nameToFfmpegLogLevel = map[string]astiav.LogLevel{
	"quiet":   astiav.LogLevelQuiet,
	"panic":   astiav.LogLevelPanic,
	"fatal":   astiav.LogLevelFatal,
	"error":   astiav.LogLevelError,
	"warning": astiav.LogLevelWarning,
	"info":    astiav.LogLevelInfo,
	"verbose": astiav.LogLevelVerbose,
	"debug":   astiav.LogLevelDebug,
}

// squelchedFfmpegLogPrefixes is a list of prefixes for ffmpeg log messages
// that we want to squelch. Sometimes ffmpeg logs the same message over and over
// for a stream, and it can do it so frequently that we become I/O blocked on logs.
var squelchedFfmpegLogPrefixes = []string{
	"PES packet size",
	"Packet corrupt",
	"Invalid level prefix",
	"error while decoding MB",
	"more samples than frame size",
	"deprecated pixel format used",
}

// How many times we saw each message. Didn't bother making this atomic
// figuring that it's not worth the overhead and miscounts are inconsequential.
var squelchedFfmpegLogCounts = make([]int, len(squelchedFfmpegLogPrefixes))

const (
	squelchedLogInterval = 1024 // log every Nth message
	lSquelch             = "squelch count"
)

func ffmpegLogCallback(l astiav.LogLevel, fmt, msg, parent string) {
	// FFmpeg sometimes logs a single "." to indicated progress. We just ignore it.
	if msg == ".\n" {
		return
	}

	var (
		squelch bool
		i       int
		prefix  string
	)

	for i, prefix = range squelchedFfmpegLogPrefixes {
		if strings.HasPrefix(msg, prefix) {
			squelch = true

			squelchedFfmpegLogCounts[i]++
			if squelchedFfmpegLogCounts[i]%squelchedLogInterval != 1 {
				return
			}

			break
		}
	}

	zl, ok := ffmpegToZerologLevel[l]
	if !ok {
		zl = zerolog.ErrorLevel // If it's not in the map, at least we'll log it.
	}

	msg = strings.TrimSuffix(msg, "\n")
	event := ffmpegLog.WithLevel(zl)

	if squelch {
		event = event.Int(lSquelch, squelchedFfmpegLogCounts[i])
	}

	event.Msg(msg)
}

func ffmpegLoggerSetup(config *Config) {
	ffmpegLog = log.With().Str("pkg", "ffmpeg").Logger()

	ffmpegLogLevel, ok := nameToFfmpegLogLevel[config.FfmpegLogLevel]
	if !ok {
		panic("invalid ffmpeg log level: " + config.FfmpegLogLevel)
	}

	// FFmpeg logs get doubly filtered. First we set the ffmpeg-specific level:
	astiav.SetLogLevel(ffmpegLogLevel)
	// The FFmpeg logs then feed through the Framer's own log level filter.
	astiav.SetLogCallback(ffmpegLogCallback)
}
