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
	"math"
	"math/big"
	"strings"
	"time"

	"github.com/asticode/go-astiav"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/TurbineOne/ffmpeg-framer/api/proto/gen/go/fps/model"
)

const noPTS = time.Duration(math.MinInt64)

var pictureTypeToPB = map[astiav.PictureType]model.MediaInfo_PictureType{
	astiav.PictureTypeI:  model.MediaInfo_PICTURE_TYPE_I,
	astiav.PictureTypeP:  model.MediaInfo_PICTURE_TYPE_P,
	astiav.PictureTypeB:  model.MediaInfo_PICTURE_TYPE_B,
	astiav.PictureTypeS:  model.MediaInfo_PICTURE_TYPE_S,
	astiav.PictureTypeSi: model.MediaInfo_PICTURE_TYPE_SI,
	astiav.PictureTypeSp: model.MediaInfo_PICTURE_TYPE_SP,
	astiav.PictureTypeBi: model.MediaInfo_PICTURE_TYPE_BI,
}

var ioFormatFlagStrings = map[astiav.IOFormatFlag]string{
	astiav.IOFormatFlagNofile:       "IOFormatFlagNofile",
	astiav.IOFormatFlagNeednumber:   "IOFormatFlagNeednumber",
	astiav.IOFormatFlagShowIds:      "IOFormatFlagShowIds",
	astiav.IOFormatFlagGlobalheader: "IOFormatFlagGlobalheader",
	astiav.IOFormatFlagNotimestamps: "IOFormatFlagNotimestamps",
	astiav.IOFormatFlagGenericIndex: "IOFormatFlagGenericIndex",
	astiav.IOFormatFlagTsDiscont:    "IOFormatFlagTsDiscont",
	astiav.IOFormatFlagVariableFps:  "IOFormatFlagVariableFps",
	astiav.IOFormatFlagNodimensions: "IOFormatFlagNodimensions",
	astiav.IOFormatFlagNostreams:    "IOFormatFlagNostreams",
	astiav.IOFormatFlagNobinsearch:  "IOFormatFlagNobinsearch",
	astiav.IOFormatFlagNogensearch:  "IOFormatFlagNogensearch",
	astiav.IOFormatFlagNoByteSeek:   "IOFormatFlagNoByteSeek",
	astiav.IOFormatFlagAllowFlush:   "IOFormatFlagAllowFlush",
	astiav.IOFormatFlagTsNonstrict:  "IOFormatFlagTsNonstrict",
	astiav.IOFormatFlagTsNegative:   "IOFormatFlagTsNegative",
	astiav.IOFormatFlagSeekToPts:    "IOFormatFlagSeekToPts",
}

// ioFormatFlagsToString returns a string representation of astiav.IOFormatFlags.
func ioFormatFlagsToString(flags astiav.IOFormatFlags) string {
	var setFlags []string

	for bit, name := range ioFormatFlagStrings {
		if flags&astiav.IOFormatFlags(bit) != 0 {
			setFlags = append(setFlags, name)
		}
	}

	if len(setFlags) == 0 {
		return ""
	}

	return strings.Join(setFlags, " | ")
}

// dictToMap converts an astiav.Dictionary to a map[string]string.
func dictToMap(d *astiav.Dictionary) map[string]string {
	result := make(map[string]string)

	var entry *astiav.DictionaryEntry

	for {
		entry = d.Get("", entry, astiav.DictionaryFlags(astiav.DictionaryFlagIgnoreSuffix))
		if entry == nil {
			break
		}

		result[entry.Key()] = entry.Value()
	}

	return result
}

// ptsToDuration converts pts to a time.Duration.
func ptsToDuration(pts int64, timeBase astiav.Rational) time.Duration {
	if pts == astiav.NoPtsValue {
		return noPTS
	}

	if timeBase.Den() == 0 {
		return 0
	}

	ptsBig := big.NewInt(pts)
	secBig := big.NewInt(int64(time.Second))
	tbNumBig := big.NewInt(int64(timeBase.Num()))
	tbDenBig := big.NewInt(int64(timeBase.Den()))

	durBig := new(big.Int).Mul(ptsBig, secBig)
	durBig.Mul(durBig, tbNumBig).Div(durBig, tbDenBig)

	return time.Duration(durBig.Int64())
}

// durationToPts converts a time.Duration to pts.
func durationToPts(duration time.Duration, timeBase astiav.Rational) int64 {
	if timeBase.Num() == 0 {
		return 0
	}

	// Using float math here in order to round to the nearest integer.
	// e.g. pts 23.99 should round to 24, not 23.
	return int64(duration.Seconds()*float64(timeBase.Den())/
		float64(timeBase.Num()) + 0.5)
}

// ptsToDurationPB converts pts to a durationpb.Duration given a timebase.
func ptsToDurationPB(pts int64, timeBase astiav.Rational) *durationpb.Duration {
	dur := ptsToDuration(pts, timeBase)

	if dur == noPTS {
		return nil
	}

	return durationpb.New(dur)
}

// rationalToPB converts an astiav.Rational to a model.Rational.
func rationalToPB(r astiav.Rational) *model.Rational {
	return &model.Rational{
		Num: int64(r.Num()),
		Den: int64(r.Den()),
	}
}
