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

package main

import (
	"context"
	"net"
	"os"
	"path/filepath"

	"github.com/rs/zerolog"
	"google.golang.org/grpc"

	"github.com/TurbineOne/ffmpeg-framer/api/proto/gen/go/fps/service"
	"github.com/TurbineOne/ffmpeg-framer/pkg/framer"
	"github.com/TurbineOne/ffmpeg-framer/pkg/interrupt"
)

var log zerolog.Logger //nolint:gochecknoglobals // Don't care.

func main() {
	initConfig() // May early exit if config init fails.

	serviceSocket := filepath.Join(currentConfig.Framer.ServiceSocketRoot,
		framer.SocketName)
	if err := os.RemoveAll(serviceSocket); err != nil {
		log.Error().Err(err).Msg("failed to remove existing socket")
	}

	l, err := net.Listen("unix", serviceSocket)
	if err != nil {
		log.Error().Err(err).Msg("failed to listen on socket")

		return
	}

	defer func() {
		_ = l.Close()
	}()

	ctx, cancel := context.WithCancel(context.Background())

	go func() {
		_ = interrupt.Run(ctx)

		cancel()
	}()

	framer := framer.New(&currentConfig.Framer, &log)
	if err := framer.Init(); err != nil {
		log.Error().Err(err).Msg("failed to initialize framer")

		return
	}

	opts := make([]grpc.ServerOption, 0)
	server := grpc.NewServer(opts...)
	service.RegisterFramerServer(server, framer)

	go func() {
		if err := framer.Run(ctx); err != nil {
			log.Error().Err(err).Msg("framer error")
		}

		server.Stop()
	}()

	log.Info().Str("socket", serviceSocket).Msg("starting server")

	err = server.Serve(l)
	if err != nil {
		log.Error().Err(err).Msg("gRPC server failed")
	}

	log.Info().Msg("server stopped")
	cancel()
}
