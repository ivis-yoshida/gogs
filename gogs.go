// +build go1.14

// Copyright 2014 The Gogs Authors. All rights reserved.
// Use of this source code is governed by a MIT-style
// license that can be found in the LICENSE file.

// Gogs is a painless self-hosted Git Service.
package main

import (
	"os"

	"github.com/urfave/cli"
	log "unknwon.dev/clog/v2"

	"github.com/NII-DG/gogs/internal/cmd"
	"github.com/NII-DG/gogs/internal/conf"
)

func init() {
	conf.App.Version = "0.12.3"
}

func main() {
	app := cli.NewApp()
	app.Name = "GIN"
	app.Usage = "Modern Research Data Management for Neuroscience"
	app.Version = conf.App.Version
	app.Commands = []cli.Command{
		cmd.Web,
		cmd.Serv,
		cmd.Hook,
		cmd.Cert,
		cmd.Admin,
		cmd.Import,
		cmd.Backup,
		cmd.Restore,
	}
	if err := app.Run(os.Args); err != nil {
		log.Fatal("Failed to start application: %v", err)
	}
}
