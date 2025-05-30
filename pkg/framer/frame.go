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

//nolint:wrapcheck // gRPC calls should return status.Error.
package framer

import (
	"context"
	"errors"
	"io"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/TurbineOne/ffmpeg-framer/api/proto/gen/go/fps/model"
	"github.com/TurbineOne/ffmpeg-framer/api/proto/gen/go/fps/service"
)

const grpcErrorFormat = "%s"

// rewriteError changes certain errors to be more appropriate for a gRPC client.
func rewriteError(ctx context.Context, err error) error {
	// EOF is expected for files, and returning a nil causes gRPC to return
	// a simple, unwrapped io.EOF to the client.
	if errors.Is(err, io.EOF) {
		return nil
	}

	// This is normal for a client cancellation.
	if ctx.Err() != nil {
		return nil //nolint:nilerr // Intentional.
	}

	// If it's not a gRPC status error, we upgrade it to look like one.
	if _, ok := status.FromError(err); !ok {
		return status.Errorf(codes.Aborted, grpcErrorFormat, err.Error())
	}

	return err
}

// serviceNextFrames is the steady-state loop for the Frame() gRPC call.
func (f *Framer) serviceNextFrames(grpcStream service.Framer_FrameServer, frameTap *frameTap,
) (frameCounts []int, err error) {
	frameCounts = make([]int, frameTap.maxStreamIndex+1)

	defer func() {
		err = rewriteError(grpcStream.Context(), err)
	}()

	for {
		var req *service.FrameRequest

		if req, err = grpcStream.Recv(); err != nil {
			return frameCounts, err
		}

		if req.GetNextFrames() == nil {
			err = status.Error(codes.InvalidArgument, "expected FrameRequest.NextFrames")

			return frameCounts, err
		}

		var medias []*model.Media

		if medias, err = frameTap.GetModelMedias(grpcStream.Context(), req.GetNextFrames().GetPoll()); err != nil {
			return frameCounts, err
		}

		for _, media := range medias {
			frameCounts[media.GetKey().GetSliceSpec().GetStreamIndex()]++
		}

		if err = grpcStream.Send(&service.FrameResponse{
			NextFrames: &service.FrameResponse_NextFrames{
				Medias: medias,
			},
		}); err != nil {
			return frameCounts, err
		}
	}
}

// Frame implements the gRPC Framer service.
//
//nolint:funlen // Not worth breaking up.
func (f *Framer) Frame(grpcStream service.Framer_FrameServer) error {
	var (
		rawURL      string
		frameCounts []int
		err         error
	)

	defer func() {
		log.Info().Ints(lFrameCount, frameCounts).Err(err).Str(lURL, rawURL).Msg("Frame() exiting")
	}()

	// First request MUST be a Start.
	req, err := grpcStream.Recv()
	if err != nil {
		return status.Error(codes.Canceled, err.Error())
	}

	startReq := req.GetStart()
	if startReq == nil {
		err = status.Error(codes.InvalidArgument, "expected FrameRequest.Start")

		return err
	}

	rawURL = startReq.GetUrl()

	log.Info().Str(lURL, rawURL).Msg("Frame() start")

	source, err := f.getSource(grpcStream.Context(), rawURL, startReq.GetFileFraming(),
		startReq.GetIsLive(), req.GetSeek())
	if err != nil {
		err = status.Errorf(codes.NotFound, grpcErrorFormat, err.Error())

		return err
	}

	defer source.Unref()

	setupErr := source.SetupErr()
	if setupErr != nil {
		return status.Errorf(codes.InvalidArgument, grpcErrorFormat, setupErr.Error())
	}

	startResp := &service.FrameResponse{
		Start: &service.FrameResponse_Start{
			MediaInfo: source.MediaInfo(),
		},
	}

	if err = grpcStream.Send(startResp); err != nil {
		err = status.Error(codes.Canceled, err.Error())

		return err
	}

	// Second request MUST be a Tap to describe the streams to be tapped.
	req, err = grpcStream.Recv()
	if err != nil {
		err = status.Error(codes.Canceled, err.Error())

		return err
	}

	tapReq := req.GetTap()
	if tapReq == nil {
		err = status.Error(codes.InvalidArgument, "expected FrameRequest.Tap")

		return err
	}

	if seekReq := req.GetSeek(); seekReq != nil {
		if err = source.Seek(seekReq.GetTime().AsDuration(), seekReq.GetClosestKeyframe()); err != nil {
			err = status.Errorf(codes.InvalidArgument, grpcErrorFormat, err.Error())

			return err
		}
	}

	frameTap := newFrameTap(source)
	if err = frameTap.Init(tapReq.StreamIndices, int(tapReq.GetSample().GetFrameInterval()),
		tapReq.GetSample().GetTimeInterval().AsDuration(), tapReq.OutputPath); err != nil {
		err = status.Errorf(codes.Internal, grpcErrorFormat, err.Error())

		return err
	}

	if err = source.AddTap(frameTap, tapLayerDecoder); err != nil {
		err = status.Errorf(codes.NotFound, grpcErrorFormat, err.Error())

		return err
	}

	defer source.RemoveTap(frameTap)

	log.Debug().Str(lURL, rawURL).Msg("Frame() tap ok")

	if err = grpcStream.Send(&service.FrameResponse{
		Tap: &service.FrameResponse_Tap{},
	}); err != nil {
		err = status.Error(codes.Canceled, err.Error())

		return err
	}

	// From here on out, all we expect are NextFrames requests.
	frameCounts, err = f.serviceNextFrames(grpcStream, frameTap)

	return err
}
