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

package config

import (
	"fmt"
	"io"
	"os"

	"github.com/caarlos0/env/v6"
	"gopkg.in/yaml.v3"
)

// NoConfigError indicates that we couldn't find a config file.
// This is usually OK and should be treated as a warning.
type NoConfigError struct {
	Path string
}

func (e *NoConfigError) Error() string {
	return "cannot find config file [" + e.Path + "], continuing with defaults"
}

// parseFile parses the config file at 'path' and overwrites defaults in 'out'.
func parseFile(path string, out interface{}) error {
	f, err := os.Open(path)
	if err != nil {
		ncErr := &NoConfigError{path}

		return ncErr
	}
	defer f.Close() //nolint:errcheck // Don't care about error

	b, err := io.ReadAll(f)
	if err != nil {
		return fmt.Errorf("failed to read config file [%s]: %w", path, err)
	}

	err = yaml.Unmarshal(b, out)
	if err != nil {
		return fmt.Errorf("failed to parse config file [%s]: %w", path, err)
	}

	return nil
}

// parseEnv parses the environment and overwrites defaults in 'out'.
func parseEnv(envPrefix string, out interface{}) error {
	envErr := env.Parse(out, env.Options{Prefix: envPrefix})
	if envErr != nil {
		return fmt.Errorf("config failed to parse environment: %w", envErr)
	}

	return nil
}

// Init initializes 'out' based on a config file and the environment.
// First it parses the environment variables. Then the YAML config file,
// overriding anything from the environment.
//
// The 'envPrefix' is prefixed to the names of any environment variables
// that we look for, so e.g., if 'envPrefix' is "APP_" and there's a struct
// tag saying $HTTP_PORT, the result will come from $APP_HTTP_PORT.
//
// If the returned error is QuietExitError, the caller should exit with the
// specified exit code.
func Init(path string, envPrefix string, out interface{}) error {
	// First, we parse the environment variables.
	if err := parseEnv(envPrefix, out); err != nil {
		return err
	}

	// Now we open, read, and parse the contents of the config file.
	return parseFile(path, out)
}
