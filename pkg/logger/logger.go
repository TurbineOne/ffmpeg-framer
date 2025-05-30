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

package logger

import (
	"io"
	"os"
	"time"

	"github.com/mattn/go-isatty"
	"github.com/rs/zerolog"
)

func init() {
	// Users of our logging will always adhere to these global settings:
	zerolog.TimeFieldFormat = time.RFC3339Nano
	zerolog.DurationFieldInteger = false
	zerolog.DurationFieldUnit = time.Second
}

// Config configures the logger.
type Config struct { //nolint:govet // Don't care about alignment.
	Level   string `yaml:"level" json:"level" doc:"Log level. One of: trace, debug, info, warn, error, fatal, panic"`
	Console bool   `yaml:"console" json:"console" doc:"Logging includes terminal colors"`
}

// ConfigDefault returns the default values for a Config.
func ConfigDefault() Config {
	return Config{
		Level:   zerolog.InfoLevel.String(),
		Console: false,
	}
}

// termOut returns a ConsoleWriter if we detect a tty or console config,
// otherwise returns os.Stdout since we're assuming we're running under docker.
func termOut(c *Config) io.Writer {
	if c.Console || isatty.IsTerminal(os.Stdout.Fd()) {
		return zerolog.ConsoleWriter{
			Out:        os.Stdout,
			TimeFormat: "2006-01-02T15:04:05.000000", // Omitting timezone on console.
		}
	}

	return os.Stdout
}

// New returns a new logger as described by the config. If logging to a file
// is enabled, also returns the file name.
// Panics in case of an invalid configuration.
func New(c *Config) (log zerolog.Logger) {
	zLevel, err := zerolog.ParseLevel(c.Level)
	if err != nil {
		panic(err.Error())
	}

	log = zerolog.New(termOut(c)).
		Level(zLevel).
		With().Timestamp().Caller().
		Logger()

	return log
}
