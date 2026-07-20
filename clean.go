package main

import (
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/aws/ec2-macos-init/internal/paths"
	"github.com/aws/ec2-macos-init/lib/ec2macosinit"
)

// utilsCommandName is the name of the EC2 macOS utilities executable used to
// perform OS level state cleanup.
const utilsCommandName = "ec2-macos-utils"

// utilsCommandDefaultPath is the path at which the EC2 macOS utilities
// package installs its executable. This is used when the executable cannot
// be resolved using PATH, e.g. when running under sudo where PATH does not
// include /usr/local/bin.
const utilsCommandDefaultPath = "/usr/local/bin/ec2-macos-utils"

// errUtilsNotFound indicates the EC2 macOS utilities executable could not be
// located at its install path or in PATH.
var errUtilsNotFound = errors.New(utilsCommandName + " not found")

// clean removes old instance history. It has two options:
// current - This is the option when -all isn't provided. It only removes the current instance's history.
// all - When -all is provided, all instance history is removed.
//
// By default, clean also removes well-known OS state (e.g. the macOS network
// interface configuration cache) by calling out to ec2-macos-utils. This is
// required for instances that are cleaned in preparation for imaging (AMI
// creation). The -init-state-only flag limits cleanup to ec2-macos-init's
// own state.
func clean(baseDir string, c *ec2macosinit.InitConfig) {
	// Define flags
	cleanFlags := flag.NewFlagSet("clean", flag.ExitOnError)
	cleanAll := cleanFlags.Bool("all", false, "Optional; Remove all instance history.  Default is false.")
	initStateOnly := cleanFlags.Bool("init-state-only", false,
		"Optional; Only remove ec2-macos-init state, skipping OS state cleanup performed with ec2-macos-utils.  Default is false.")

	// Parse flags
	err := cleanFlags.Parse(os.Args[2:])
	if err != nil {
		c.Log.Fatalf(64, "Unable to parse arguments: %s", err)
	}

	// Remove instance history. A failure here is logged but does not stop
	// cleanup: the OS state removal below must still run so the system is
	// left as ready for imaging as possible.
	historyErr := cleanInstanceHistory(baseDir, c, *cleanAll)
	if historyErr != nil {
		c.Log.Errorf("Unable to remove instance history: %s", historyErr)
	}

	// Remove OS level state unless limited to init's own state. This must
	// happen regardless of whether any instance history was present so that
	// the system is always left ready for imaging.
	if *initStateOnly {
		c.Log.Info("Skipping OS state cleanup (-init-state-only)")
	} else {
		c.Log.Infof("Removing OS state with %s", utilsCommandName)
		err := cleanSystemState()
		if err != nil {
			c.Log.Fatalf(1, "Unable to remove OS state (re-run with -init-state-only to skip): %s", err)
		}
		c.Log.Info("OS state removal complete")
	}

	// Fail loud if any part of cleanup did not complete so a partially
	// cleaned system is never mistaken for one ready for imaging.
	if historyErr != nil {
		c.Log.Fatalf(1, "Clean incomplete: unable to remove instance history: %s", historyErr)
	}

	c.Log.Info("Clean complete")
}

// cleanInstanceHistory removes instance history beneath the given base
// directory. When all is true, every instance's history is removed;
// otherwise only the current instance's history (resolved via IMDS) is
// removed. Absent history is not an error.
func cleanInstanceHistory(baseDir string, c *ec2macosinit.InitConfig, all bool) error {
	historyPath := paths.AllInstancesHistory(baseDir)
	if all {
		c.Log.Info("Removing all instance history")
		dir, err := os.ReadDir(historyPath)
		if errors.Is(err, fs.ErrNotExist) {
			c.Log.Info("No instance history present, nothing to remove")
			return nil
		}
		if err != nil {
			return fmt.Errorf("unable to read instance history located at %s: %w", historyPath, err)
		}
		for _, d := range dir {
			if err := os.RemoveAll(filepath.Join(historyPath, d.Name())); err != nil {
				return fmt.Errorf("unable to remove instance history: %w", err)
			}
		}
		return nil
	}

	c.Log.Infof("Getting current instance ID from IMDS")
	// Instance ID is needed, run setup
	if err := SetupInstanceID(c); err != nil {
		return fmt.Errorf("unable to get instance ID: %w", err)
	}
	c.Log.Infof("Removing history for the current instance [%s]", c.IMDS.InstanceID)

	if err := os.RemoveAll(paths.InstanceHistory(baseDir, c.IMDS.InstanceID)); err != nil {
		return fmt.Errorf("unable to remove instance history: %w", err)
	}
	return nil
}

// cleanSystemState invokes ec2-macos-utils to remove well-known OS state
// (e.g. the macOS network interface configuration cache) from the running
// system. The subprocess's STDIO is passed through to the calling shell so
// its output is directly visible to the user.
func cleanSystemState() error {
	utilsPath, err := resolveUtilsCommand()
	if err != nil {
		return err
	}

	cmd := exec.Command(utilsPath, "system", "cleanup-state")
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	return cmd.Run()
}

// resolveUtilsCommand locates the ec2-macos-utils executable.
func resolveUtilsCommand() (string, error) {
	return resolveCommand(utilsCommandName, utilsCommandDefaultPath)
}

// resolveCommand locates an executable, preferring the given default install
// path and falling back to PATH resolution. The install path is preferred so
// the packaged executable is used even when PATH omits /usr/local/bin, e.g.
// when running under sudo.
func resolveCommand(name string, defaultPath string) (string, error) {
	if _, err := os.Stat(defaultPath); err == nil {
		return defaultPath, nil
	}

	path, err := exec.LookPath(name)
	if err != nil {
		return "", fmt.Errorf("%w at %s or in PATH: %v", errUtilsNotFound, defaultPath, err)
	}
	return path, nil
}
