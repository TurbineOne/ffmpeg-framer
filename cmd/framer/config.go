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
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/TurbineOne/ffmpeg-framer/pkg/config"
	"github.com/TurbineOne/ffmpeg-framer/pkg/framer"
	"github.com/TurbineOne/ffmpeg-framer/pkg/logger"
)

const (
	configFileName = "config.yaml"
)

//nolint:gochecknoglobals // Needed for makefile injection.
var (
	// Version is provided by the makefile.
	Version = "v0"
	// Revision is a git tag provided by the makefile.
	Revision = "0"
	// Created is a date provided by the makefile.
	Created = "0000-00-00"
)

// mainConfig is the master config for the executable.
type mainConfig struct { //nolint:govet // Don't care about alignment.
	Framer framer.Config `yaml:"framer"`
	Logger logger.Config `yaml:"logger"`
}

var currentConfig = mainConfig{ //nolint:gochecknoglobals  // Static config
	Framer: framer.ConfigDefault(),
	Logger: logger.ConfigDefault(),
}

// initConfig initializes the config by calling config.Init() and handling
// the results. May exit the program if there is an error.
func initConfig() {
	err := config.Init(configFileName, "", &currentConfig)
	if err != nil {
		// A missing config file is not fatal. Anything else is.
		ncError := &config.NoConfigError{}
		if !errors.As(err, &ncError) {
			fmt.Println(err.Error()) //nolint:forbidigo // OK to print here.
			os.Exit(-1)
		}
	}

	log = logger.New(&currentConfig.Logger)

	binName := filepath.Base(os.Args[0])
	log.Info().Msg(fmt.Sprintf("%s %s rev:%s created:%s", binName, Version, Revision, Created))
	log.Info().Interface("config", &currentConfig).Msg("effective config")

	// If there was no config file, we log it here.
	if err != nil {
		log.Info().Msg(err.Error())
	}
}
