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

// mimer is a helper package to determine the mime type of a file.
package mimer

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/aofei/mimesniffer"
)

// MediaTypeNITF is the media type for NITF files.
const (
	MediaTypeFuser    = "application/vnd.turbineone.fuser"
	MediaTypeMISBJSON = "application/vnd.misb0601+json"
	MediaTypeNITF     = "application/vnd.nitf"
	MediaTypeJPEG     = "image/jpeg"
	MediaTypeJPEG2000 = "image/jp2"
	MediaTypeM3U      = "application/x-mpegurl"

	UnknownMediaType = "application/octet-stream"
)

// isVideoTsSignature returns true if the given buffer is a video.ts file.
// According to https://en.wikipedia.org/wiki/List_of_file_signatures,
// the hex value 0x47 should be the first byte of a video.ts file and
// repeated every 188 bytes.
func isVideoTsSignature(buffer []byte) bool {
	const (
		tsSignature         = 0x47
		tsSignatureInterval = 188
	)

	if len(buffer) < tsSignatureInterval {
		return false
	}

	for i := 0; i < len(buffer); i += tsSignatureInterval {
		if buffer[i] != tsSignature {
			return false
		}
	}

	return true
}

// isNITFSignature returns true if the buffer has a NITF file signature.
// Signature based on the NITF spec here, section 5.11.1:
// https://nsgreg.nga.mil/doc/view?i=5533&month=8&day=16&year=2024
func isNITFSignature(buffer []byte) bool {
	const nitfSignature = "NITF02.10"

	if len(buffer) < len(nitfSignature) {
		return false
	}

	return strings.HasPrefix(string(buffer), nitfSignature)
}

func isM3USignature(buffer []byte) bool {
	const m3uSignature = "#EXTM3U"

	if len(buffer) < len(m3uSignature) {
		return false
	}

	return strings.HasPrefix(string(buffer), m3uSignature)
}

// init initializes the mimer package.
func init() {
	mimesniffer.Register("video/mp2t", isVideoTsSignature)
	mimesniffer.Register(MediaTypeNITF, isNITFSignature)
	mimesniffer.Register(MediaTypeM3U, isM3USignature)
}

// GetContentType returns the content type of the given resource at the given path.
func GetContentTypeFromReader(reader io.Reader) (string, error) {
	const fingerprintSize = 512

	// Only the first 512 bytes are used to sniff the content type.
	buffer := make([]byte, fingerprintSize)

	_, err := reader.Read(buffer)
	if err != nil {
		return UnknownMediaType, fmt.Errorf("mime check failed read: %w", err)
	}

	mimeType := mimesniffer.Sniff(buffer)

	return mimeType, nil
}

// GetContentType returns the content type of the given resource at the given path.
func GetContentType(sourcePath string) string {
	// Get the content type of the file.
	f, err := os.Open(sourcePath)
	if err != nil {
		return UnknownMediaType
	}

	defer func() {
		_ = f.Close()
	}()

	mimeType, _ := GetContentTypeFromReader(f)

	return mimeType
}
