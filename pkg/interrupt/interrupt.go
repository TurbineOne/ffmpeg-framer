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

// Package interrupt handles interrupt signals from the OS.
package interrupt

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"syscall"
)

// ErrInterrupt indicates that an interrupt was received from the OS.
var ErrInterrupt = errors.New("interrupt received")

// Run runs until ctx is done or an interrupt is received.
func Run(ctx context.Context) error {
	signalChan := make(chan os.Signal, 1)
	signal.Notify(signalChan, syscall.SIGTERM, syscall.SIGINT)

	select {
	case <-signalChan: // first signal, cancel context
		return ErrInterrupt
	case <-ctx.Done():
		return nil
	}
}
