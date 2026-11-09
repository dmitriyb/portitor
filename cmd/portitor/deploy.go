package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"

	"github.com/dmitriyb/portitor/internal/deploy"
)

// gateBinaryBeside is the portitor-gate install.sh puts next to this binary.
func gateBinaryBeside() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(exe), "portitor-gate"), nil
}

func deployRun(configPath string, stdout, stderr io.Writer) int {
	c, err := deploy.LoadConfig(configPath)
	if err != nil {
		fmt.Fprintf(stderr, "deploy: %v\n", err)
		return 1
	}
	own, err := deploy.OwnBuildID()
	if err != nil {
		fmt.Fprintf(stderr, "deploy: %v\n", err)
		return 1
	}
	gate, err := gateBinaryBeside()
	if err != nil {
		fmt.Fprintf(stderr, "deploy: locate portitor-gate: %v\n", err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	d := &deploy.Deployer{
		Run:     deploy.ExecRunner{},
		Token:   deploy.TokenCommand,
		Log:     stderr,
		Arch:    runtime.GOARCH,
		Own:     own,
		GateBin: gate,
	}
	res, err := d.Deploy(ctx, c)
	if err != nil {
		fmt.Fprintf(stderr, "deploy: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "%s %s\n", res.ContainerID, res.Image)
	return 0
}
