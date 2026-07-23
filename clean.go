package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
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
// By default, clean also removes well-known OS state via ec2-macos-utils to
// ready the system for imaging. The -init-state-only flag limits cleanup to
// ec2-macos-init's own state.
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
	var osStateErr error
	if *initStateOnly {
		c.Log.Info("Skipping OS state cleanup (-init-state-only)")
	} else {
		osStateErr = cleanOSState(c)
		if osStateErr != nil {
			c.Log.Errorf("Unable to remove OS state: %s", osStateErr)
		}
	}

	// Fail loud if any part of cleanup did not complete so a partially
	// cleaned system is never mistaken for one ready for imaging.
	if historyErr != nil {
		c.Log.Fatalf(1, "Clean incomplete: unable to remove instance history: %s", historyErr)
	}
	if osStateErr != nil {
		c.Log.Fatalf(1, "Clean incomplete: unable to remove OS state: %s", osStateErr)
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

// cleanOSState removes well-known OS state via ec2-macos-utils. For
// backwards compatibility it warns and returns nil when utils is missing or
// too old to support 'system cleanup-state' (this may become an error in a
// future release); a genuine command failure is returned so the caller fails
// loudly.
func cleanOSState(c *ec2macosinit.InitConfig) error {
	utilsPath, err := resolveUtilsCommand()
	if err != nil {
		c.Log.Warnf("Skipping OS state cleanup: %s", err)
		c.Log.Warnf("Cached OS state (e.g. NetworkInterfaces.plist) was not removed and may affect "+
			"instances launched from an image of this system; install %s to enable this cleanup", utilsCommandName)
		return nil
	}

	if !utilsSupportsCleanupState(utilsPath) {
		c.Log.Warnf("Skipping OS state cleanup: installed %s does not support 'system cleanup-state'", utilsCommandName)
		c.Log.Warnf("Cached OS state (e.g. NetworkInterfaces.plist) was not removed and may affect "+
			"instances launched from an image of this system; update %s to enable this cleanup", utilsCommandName)
		return nil
	}

	c.Log.Infof("Removing OS state with %s", utilsCommandName)
	if err := cleanSystemState(utilsPath); err != nil {
		return fmt.Errorf("%s system cleanup-state failed: %w", utilsCommandName, err)
	}
	c.Log.Info("OS state removal complete")
	return nil
}

// utilsSupportsCleanupState reports whether ec2-macos-utils supports the
// 'system cleanup-state' subcommand. Probing --help exits zero when the
// command exists and non-zero on versions predating it, without running the
// command or its root check.
func utilsSupportsCleanupState(utilsPath string) bool {
	cmd := exec.Command(utilsPath, "system", "cleanup-state", "--help")
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	return cmd.Run() == nil
}

// cleanSystemState invokes ec2-macos-utils to remove OS state, passing its
// STDIO through to the calling shell so output is visible to the user.
func cleanSystemState(utilsPath string) error {
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
