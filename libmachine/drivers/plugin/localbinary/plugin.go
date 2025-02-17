package localbinary

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/rancher/machine/libmachine/log"
)

var (
	// Timeout where we will bail if we're not able to properly contact the
	// plugin server.
	defaultTimeout               = 10 * time.Second
	CurrentBinaryIsDockerMachine = false
	CoreDrivers                  = []string{
		"amazonec2",
		"azure",
		"digitalocean",
		"exoscale",
		"generic",
		"google",
		"hyperv",
		"none",
		"openstack",
		"rackspace",
		"softlayer",
		"virtualbox",
		"vmwarefusion",
		"vmwarevcloudair",
		"vmwarevsphere",
		"pod",
		"noop",
	}
)

const (
	pluginOut           = "(%s) %s"
	pluginErr           = "(%s) DBG | %s"
	PluginEnvKey        = "MACHINE_PLUGIN_TOKEN"
	PluginEnvVal        = "42"
	PluginEnvDriverName = "MACHINE_PLUGIN_DRIVER_NAME"
	PluginUID           = "MACHINE_PLUGIN_UID"
	PluginGID           = "MACHINE_PLUGIN_GID"
)

type PluginStreamer interface {
	// Return a channel for receiving the output of the stream line by
	// line.
	//
	// It happens to be the case that we do this all inside of the main
	// plugin struct today, but that may not be the case forever.
	AttachStream(*bufio.Scanner) <-chan string
}

type PluginServer interface {
	// Get the address where the plugin server is listening.
	Address() (string, error)

	// Serve kicks off the plugin server.
	Serve() error

	// Close shuts down the initialized server.
	Close() error
}

type McnBinaryExecutor interface {
	// Execute the driver plugin.  Returns scanners for plugin binary
	// stdout and stderr.
	Start() (*bufio.Scanner, *bufio.Scanner, error)

	// Stop reading from the plugins in question.
	Close() error
}

// DriverPlugin interface wraps the underlying mechanics of starting a driver
// plugin server and then figuring out where it can be dialed.
type DriverPlugin interface {
	PluginServer
	PluginStreamer
}

type Plugin struct {
	Executor    McnBinaryExecutor
	Addr        string
	MachineName string
	addrCh      chan string
	stopCh      chan bool
	timeout     time.Duration
}

type Executor struct {
	pluginStdout, pluginStderr io.ReadCloser
	DriverName                 string
	cmd                        *exec.Cmd
	binaryPath                 string
}

type ErrPluginBinaryNotFound struct {
	driverName string
	driverPath string
}

func (e ErrPluginBinaryNotFound) Error() string {
	return fmt.Sprintf("Driver %q not found. Do you have the plugin binary %q accessible in your PATH?", e.driverName, e.driverPath)
}

// driverPath locates the path of a driver binary based on its name.
//   - For core drivers, there is no separate driver binary. The current binary is reused if it's `docker-machine`,
//     or it is assumed that `docker-machine` is available in the PATH.
//   - For non-core drivers, a separate binary must be in the PATH with the name `docker-machine-driver-driverName`.
func driverPath(driverName string) string {
	for _, coreDriver := range CoreDrivers {
		if coreDriver == driverName {
			if CurrentBinaryIsDockerMachine {
				return os.Args[0]
			}

			return "rancher-machine"
		}
	}

	return fmt.Sprintf("docker-machine-driver-%s", driverName)
}

// NewPlugin creates a Plugin for the specified driver.
//
// The `driverName` can be either a simple name or an absolute path to the driver:
//   - If `driverName` is a simple name, "docker-machine-driver-" is prepended to it,
//     and the executable is searched for in the directories listed in the PATH environment variable.
//   - If `driverName` is an absolute path, the executable is searched for at that specific location.
func NewPlugin(driverName string) (*Plugin, error) {
	var path string
	dir, name := filepath.Split(driverName)
	if dir == "" {
		path = driverPath(driverName)
	} else {
		path = driverName
	}
	binaryPath, err := exec.LookPath(path)
	if err != nil {
		return nil, ErrPluginBinaryNotFound{name, path}
	}

	log.Debugf("Found binary path at %s", binaryPath)

	return &Plugin{
		stopCh: make(chan bool),
		addrCh: make(chan string, 1),
		Executor: &Executor{
			DriverName: name,
			binaryPath: binaryPath,
		},
	}, nil
}

func (lbe *Executor) Start() (*bufio.Scanner, *bufio.Scanner, error) {
	var err error

	log.Debugf("Launching plugin server for driver %s", lbe.DriverName)

	// The child process that gets executed when we run this subcommand will already inherit all this process' envvars,
	// but we still need to pass all command-line arguments to it manually.
	cmd := exec.Command(lbe.binaryPath, os.Args...)
	gid := os.Getenv(PluginGID)
	uid := os.Getenv(PluginUID)
	if uid != "" && gid != "" {
		uid, err := strconv.Atoi(uid)
		if err != nil {
			return nil, nil, fmt.Errorf("error parsing user ID: %w", err)
		}
		gid, err := strconv.Atoi(gid)
		if err != nil {
			return nil, nil, fmt.Errorf("error parsing group ID: %w", err)
		}
		cmd.SysProcAttr = &syscall.SysProcAttr{
			Credential: &syscall.Credential{
				Uid: uint32(uid),
				Gid: uint32(gid),
			},
		}
	}
	lbe.cmd = cmd
	lbe.pluginStdout, err = lbe.cmd.StdoutPipe()
	if err != nil {
		return nil, nil, fmt.Errorf("Error getting cmd stdout pipe: %s", err)
	}

	lbe.pluginStderr, err = lbe.cmd.StderrPipe()
	if err != nil {
		return nil, nil, fmt.Errorf("Error getting cmd stderr pipe: %s", err)
	}

	outScanner := bufio.NewScanner(lbe.pluginStdout)
	errScanner := bufio.NewScanner(lbe.pluginStderr)

	os.Setenv(PluginEnvKey, PluginEnvVal)
	os.Setenv(PluginEnvDriverName, lbe.DriverName)

	if err := lbe.cmd.Start(); err != nil {
		return nil, nil, fmt.Errorf("Error starting plugin binary: %s", err)
	}

	return outScanner, errScanner, nil
}

func (lbe *Executor) Close() error {
	if err := lbe.cmd.Wait(); err != nil {
		return fmt.Errorf("Error waiting for binary close: %s", err)
	}

	return nil
}

func stream(scanner *bufio.Scanner, streamOutCh chan<- string) {
	for scanner.Scan() {
		line := scanner.Text()
		if err := scanner.Err(); err != nil {
			log.Warnf("Scanning stream: %s", err)
		}
		streamOutCh <- strings.Trim(line, "\n")
	}
}

func (lbp *Plugin) AttachStream(scanner *bufio.Scanner) <-chan string {
	streamOutCh := make(chan string)
	go stream(scanner, streamOutCh)
	return streamOutCh
}

func (lbp *Plugin) execServer() error {
	outScanner, errScanner, err := lbp.Executor.Start()
	if err != nil {
		return err
	}

	// Scan just one line to get the address, then send it to the relevant
	// channel.
	outScanner.Scan()
	addr := outScanner.Text()
	if err := outScanner.Err(); err != nil {
		return fmt.Errorf("Reading plugin address failed: %s", err)
	}

	lbp.addrCh <- strings.TrimSpace(addr)

	stdOutCh := lbp.AttachStream(outScanner)
	stdErrCh := lbp.AttachStream(errScanner)

	for {
		select {
		case out := <-stdOutCh:
			log.Infof(pluginOut, lbp.MachineName, out)
		case err := <-stdErrCh:
			log.Debugf(pluginErr, lbp.MachineName, err)
		case <-lbp.stopCh:
			if err := lbp.Executor.Close(); err != nil {
				return fmt.Errorf("Error closing local plugin binary: %s", err)
			}
			return nil
		}
	}
}

func (lbp *Plugin) Serve() error {
	return lbp.execServer()
}

func (lbp *Plugin) Address() (string, error) {
	if lbp.Addr == "" {
		if lbp.timeout == 0 {
			lbp.timeout = defaultTimeout
		}

		select {
		case lbp.Addr = <-lbp.addrCh:
			log.Debugf("Plugin server listening at address %s", lbp.Addr)
			close(lbp.addrCh)
			return lbp.Addr, nil
		case <-time.After(lbp.timeout):
			return "", fmt.Errorf("Failed to dial the plugin server in %s", lbp.timeout)
		}
	}
	return lbp.Addr, nil
}

func (lbp *Plugin) Close() error {
	lbp.stopCh <- true
	return nil
}
