package commands

import (
	"github.com/sirupsen/logrus"
	"github.com/urfave/cli"
	"gitlab.s.upyun.com/platform/tikv-proxy/config"
	"gitlab.s.upyun.com/platform/tikv-proxy/xerror"
	"os"
)

func init() {
	registerCommand(cli.Command{
		Name: "init",
		Flags: []cli.Flag{
			cli.StringFlag{
				Name:  "config, conf",
				Usage: "init config path",
			},
		},
		Action: func(c *cli.Context) error {
			return runInit(c)
		},
	})
}

func runInit(c *cli.Context) error {
	path := c.String("config")
	_, err := os.Stat(path)
	if err == nil {
		logrus.Errorf("file exist, %s", path)
		return xerror.ErrExists
	}
	if !os.IsNotExist(err) {
		logrus.Errorf("path %s err, %s", path, err)
		return err
	}
	return config.Save(config.DefaultConfig(), path)
}