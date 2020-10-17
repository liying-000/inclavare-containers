// +build linux

package libenclave // import "github.com/inclavare-containers/rune/libenclave"

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"time"

	securejoin "github.com/cyphar/filepath-securejoin"
	enclaveConfigs "github.com/inclavare-containers/rune/libenclave/configs"
	"github.com/opencontainers/runc/libcontainer"
	"github.com/opencontainers/runc/libcontainer/cgroups"
	"github.com/opencontainers/runc/libcontainer/configs"
	"github.com/opencontainers/runc/libcontainer/intelrdt"
	"github.com/opencontainers/runc/libcontainer/system"
	"github.com/opencontainers/runc/libcontainer/utils"
	"github.com/opencontainers/runtime-spec/specs-go"

	criurpc "github.com/checkpoint-restore/go-criu/rpc"
	"github.com/golang/protobuf/proto"
	"github.com/sirupsen/logrus"
	"github.com/vishvananda/netlink/nl"
	"golang.org/x/sys/unix"
)

// Add some definition in message.go

// list of known message types we want to send to bootstrap program
// The number is randomly chosen to not conflict with known netlink types
const (
	InitMsg          uint16 = 62000
	CloneFlagsAttr   uint16 = 27281
	NsPathsAttr      uint16 = 27282
	UidmapAttr       uint16 = 27283
	GidmapAttr       uint16 = 27284
	SetgroupAttr     uint16 = 27285
	OomScoreAdjAttr  uint16 = 27286
	RootlessEUIDAttr uint16 = 27287
	UidmapPathAttr   uint16 = 27288
	GidmapPathAttr   uint16 = 27289
)

const stdioFdCount = 3

type linuxContainer struct {
	id                   string
	root                 string
	config               *configs.Config
	enclaveConfig        *enclaveConfigs.EnclaveConfig
	detached             bool
	cgroupManager        cgroups.Manager
	intelRdtManager      intelrdt.Manager
	initPath             string
	initArgs             []string
	initProcess          parentProcess
	initProcessStartTime uint64
	criuPath             string
	newuidmapPath        string
	newgidmapPath        string
	m                    sync.Mutex
	criuVersion          int
	state                containerState
	created              time.Time
}

// State represents a running container's state
type State struct {
	BaseState

	// Platform specific fields below here

	// Specified if the container was started under the rootless mode.
	// Set to true if BaseState.Config.RootlessEUID && BaseState.Config.RootlessCgroups
	Rootless bool `json:"rootless"`

	// Path to all the cgroups setup for a container. Key is cgroup subsystem name
	// with the value as the path.
	CgroupPaths map[string]string `json:"cgroup_paths"`

	// NamespacePaths are filepaths to the container's namespaces. Key is the namespace type
	// with the value as the path.
	NamespacePaths map[configs.NamespaceType]string `json:"namespace_paths"`

	// Container's standard descriptors (std{in,out,err}), needed for checkpoint and restore
	ExternalDescriptors []string `json:"external_descriptors,omitempty"`

	// Intel RDT "resource control" filesystem path
	IntelRdtPath string `json:"intel_rdt_path"`
}

// Container is a libenclave container object.
//
// Each container is thread-safe within the same process. Since a container can
// be destroyed by a separate process, any function may return that the container
// was not found.
type Container interface {
	BaseContainer

	// Methods below here are platform specific

	// Checkpoint checkpoints the running container's state to disk using the criu(8) utility.
	//
	// errors:
	// Systemerror - System error.
	Checkpoint(criuOpts *libcontainer.CriuOpts) error

	// Restore restores the checkpointed container to a running state using the criu(8) utility.
	//
	// errors:
	// Systemerror - System error.
	Restore(process *Process, criuOpts *libcontainer.CriuOpts) error

	// If the Container state is RUNNING or CREATED, sets the Container state to PAUSING and pauses
	// the execution of any user processes. Asynchronously, when the container finished being paused the
	// state is changed to PAUSED.
	// If the Container state is PAUSED, do nothing.
	//
	// errors:
	// ContainerNotExists - Container no longer exists,
	// ContainerNotRunning - Container not running or created,
	// Systemerror - System error.
	Pause() error

	// If the Container state is PAUSED, resumes the execution of any user processes in the
	// Container before setting the Container state to RUNNING.
	// If the Container state is RUNNING, do nothing.
	//
	// errors:
	// ContainerNotExists - Container no longer exists,
	// ContainerNotPaused - Container is not paused,
	// Systemerror - System error.
	Resume() error

	// NotifyOOM returns a read-only channel signaling when the container receives an OOM notification.
	//
	// errors:
	// Systemerror - System error.
	NotifyOOM() (<-chan struct{}, error)

	// NotifyMemoryPressure returns a read-only channel signaling when the container reaches a given pressure level
	//
	// errors:
	// Systemerror - System error.
	NotifyMemoryPressure(level PressureLevel) (<-chan struct{}, error)
}

// ID returns the container's unique ID
func (c *linuxContainer) ID() string {
	return c.id
}

// Config returns the container's configuration
func (c *linuxContainer) Config() configs.Config {
	return *c.config
}

func (c *linuxContainer) EnclaveConfig() enclaveConfigs.EnclaveConfig {

	return *c.enclaveConfig

}
func (c *linuxContainer) Status() (Status, error) {
	c.m.Lock()
	defer c.m.Unlock()
	return c.currentStatus()
}

func (c *linuxContainer) State() (*State, error) {
	c.m.Lock()
	defer c.m.Unlock()
	return c.currentState()
}

func (c *linuxContainer) OCIState() (*specs.State, error) {
	c.m.Lock()
	defer c.m.Unlock()
	return c.currentOCIState()
}

func (c *linuxContainer) Processes() ([]int, error) {
	pids, err := c.cgroupManager.GetAllPids()
	if err != nil {
		return nil, newSystemErrorWithCause(err, "getting all container pids from cgroups")
	}
	return pids, nil
}

func (c *linuxContainer) Stats() (*libcontainer.Stats, error) {
	var (
		err   error
		stats = &libcontainer.Stats{}
	)
	if stats.CgroupStats, err = c.cgroupManager.GetStats(); err != nil {
		return stats, newSystemErrorWithCause(err, "getting container stats from cgroups")
	}
	if c.intelRdtManager != nil {
		if stats.IntelRdtStats, err = c.intelRdtManager.GetStats(); err != nil {
			return stats, newSystemErrorWithCause(err, "getting container's Intel RDT stats")
		}
	}
	for _, iface := range c.config.Networks {
		switch iface.Type {
		case "veth":
			istats, err := getNetworkInterfaceStats(iface.HostInterfaceName)
			if err != nil {
				return stats, newSystemErrorWithCausef(err, "getting network stats for interface %q", iface.HostInterfaceName)
			}
			stats.Interfaces = append(stats.Interfaces, istats)
		}
	}
	return stats, nil
}

func (c *linuxContainer) Set(config configs.Config) error {
	c.m.Lock()
	defer c.m.Unlock()
	status, err := c.currentStatus()
	if err != nil {
		return err
	}
	if status == Stopped {
		return newGenericError(fmt.Errorf("container not running"), libcontainer.ContainerNotRunning)
	}
	if err := c.cgroupManager.Set(&config); err != nil {
		// Set configs back
		if err2 := c.cgroupManager.Set(c.config); err2 != nil {
			logrus.Warnf("Setting back cgroup configs failed due to error: %v, your state.json and actual configs might be inconsistent.", err2)
		}
		return err
	}
	if c.intelRdtManager != nil {
		if err := c.intelRdtManager.Set(&config); err != nil {
			// Set configs back
			if err2 := c.intelRdtManager.Set(c.config); err2 != nil {
				logrus.Warnf("Setting back intelrdt configs failed due to error: %v, your state.json and actual configs might be inconsistent.", err2)
			}
			return err
		}
	}
	// After config setting succeed, update config and states
	c.config = &config
	_, err = c.updateState(nil)
	return err
}

func (c *linuxContainer) Start(process *Process) error {
	c.m.Lock()
	defer c.m.Unlock()
	if process.Init {
		if err := c.createExecFifo(); err != nil {
			return err
		}
	}
	if err := c.start(process); err != nil {
		if process.Init {
			c.deleteExecFifo()
		}
		return err
	}
	return nil
}

func (c *linuxContainer) Run(process *Process) error {
	if err := c.Start(process); err != nil {
		return err
	}
	if process.Init {
		return c.exec()
	}
	return nil
}

func (c *linuxContainer) Exec() error {
	c.m.Lock()
	defer c.m.Unlock()
	return c.exec()
}

func (c *linuxContainer) exec() error {
	path := filepath.Join(c.root, execFifoFilename)
	pid := c.initProcess.pid()
	blockingFifoOpenCh := awaitFifoOpen(path)
	for {
		select {
		case result := <-blockingFifoOpenCh:
			return handleFifoResult(result)

		case <-time.After(time.Millisecond * 100):
			stat, err := system.Stat(pid)
			if err != nil || stat.State == system.Zombie {
				// could be because process started, ran, and completed between our 100ms timeout and our system.Stat() check.
				// see if the fifo exists and has data (with a non-blocking open, which will succeed if the writing process is complete).
				if err := handleFifoResult(fifoOpen(path, false)); err != nil {
					return errors.New("container process is already dead")
				}
				return nil
			}
		}
	}
}

func readFromExecFifo(execFifo io.Reader) error {
	data, err := ioutil.ReadAll(execFifo)
	if err != nil {
		return err
	}
	if len(data) <= 0 {
		return fmt.Errorf("cannot start an already running container")
	}
	return nil
}

func awaitFifoOpen(path string) <-chan openResult {
	fifoOpened := make(chan openResult)
	go func() {
		result := fifoOpen(path, true)
		fifoOpened <- result
	}()
	return fifoOpened
}

func fifoOpen(path string, block bool) openResult {
	flags := os.O_RDONLY
	if !block {
		flags |= unix.O_NONBLOCK
	}
	f, err := os.OpenFile(path, flags, 0)
	if err != nil {
		return openResult{err: newSystemErrorWithCause(err, "open exec fifo for reading")}
	}
	return openResult{file: f}
}

func handleFifoResult(result openResult) error {
	if result.err != nil {
		return result.err
	}
	f := result.file
	defer f.Close()
	if err := readFromExecFifo(f); err != nil {
		return err
	}
	return os.Remove(f.Name())
}

type openResult struct {
	file *os.File
	err  error
}

func (c *linuxContainer) start(process *Process) error {
	parent, err := c.newParentProcess(process)
	if err != nil {
		return newSystemErrorWithCause(err, "creating new parent process")
	}
	parent.forwardChildLogs()
	if err := parent.start(); err != nil {
		// terminate the process to ensure that it properly is reaped.
		if err := ignoreTerminateErrors(parent.terminate()); err != nil {
			logrus.Warn(err)
		}
		return newSystemErrorWithCause(err, "starting container process")
	}
	// generate a timestamp indicating when the container was started
	c.created = time.Now().UTC()
	if process.Init {
		c.state = &createdState{
			c: c,
		}
		state, err := c.updateState(parent)
		if err != nil {
			return err
		}
		c.initProcessStartTime = state.InitProcessStartTime

		if c.config.Hooks != nil {
			s, err := c.currentOCIState()
			if err != nil {
				return err
			}
			for i, hook := range c.config.Hooks.Poststart {
				if err := hook.Run(s); err != nil {
					if err := ignoreTerminateErrors(parent.terminate()); err != nil {
						logrus.Warn(err)
					}
					return newSystemErrorWithCausef(err, "running poststart hook %d", i)
				}
			}
		}
	}
	return nil
}

func (c *linuxContainer) Signal(s os.Signal, all bool) error {
	c.m.Lock()
	defer c.m.Unlock()
	if all {
		return signalAllProcesses(c.cgroupManager, s)
	}
	status, err := c.currentStatus()
	if err != nil {
		return err
	}
	// to avoid a PID reuse attack
	if status == Running || status == Created || status == Paused {
		if err := c.initProcess.signal(s); err != nil {
			return newSystemErrorWithCause(err, "signaling init process")
		}
		return nil
	}
	return newGenericError(fmt.Errorf("container not running"), libcontainer.ContainerNotRunning)
}

func (c *linuxContainer) createExecFifo() error {
	rootuid, err := c.Config().HostRootUID()
	if err != nil {
		return err
	}
	rootgid, err := c.Config().HostRootGID()
	if err != nil {
		return err
	}

	fifoName := filepath.Join(c.root, execFifoFilename)
	if _, err := os.Stat(fifoName); err == nil {
		return fmt.Errorf("exec fifo %s already exists", fifoName)
	}
	oldMask := unix.Umask(0000)
	if err := unix.Mkfifo(fifoName, 0622); err != nil {
		unix.Umask(oldMask)
		return err
	}
	unix.Umask(oldMask)
	return os.Chown(fifoName, rootuid, rootgid)
}

func (c *linuxContainer) deleteExecFifo() {
	fifoName := filepath.Join(c.root, execFifoFilename)
	os.Remove(fifoName)
}

// includeExecFifo opens the container's execfifo as a pathfd, so that the
// container cannot access the statedir (and the FIFO itself remains
// un-opened). It then adds the FifoFd to the given exec.Cmd as an inherited
// fd, with _LIBCONTAINER_FIFOFD set to its fd number.
func (c *linuxContainer) includeExecFifo(cmd *exec.Cmd) error {
	fifoName := filepath.Join(c.root, execFifoFilename)
	fifoFd, err := unix.Open(fifoName, unix.O_PATH|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}

	cmd.ExtraFiles = append(cmd.ExtraFiles, os.NewFile(uintptr(fifoFd), fifoName))
	cmd.Env = append(cmd.Env,
		fmt.Sprintf("_LIBCONTAINER_FIFOFD=%d", stdioFdCount+len(cmd.ExtraFiles)-1))
	return nil
}

func (c *linuxContainer) newParentProcess(p *Process) (parentProcess, error) {
	parentInitPipe, childInitPipe, err := utils.NewSockPair("init")
	if err != nil {
		return nil, newSystemErrorWithCause(err, "creating new init pipe")
	}
	messageSockPair := filePair{parentInitPipe, childInitPipe}

	if c.enclaveConfig != nil {
		if p.Init {
			uid, err := c.Config().HostRootUID()
			if err != nil {
				return nil, newSystemErrorWithCause(err, "getting host root uid")
			}
			gid, err := c.Config().HostRootGID()
			if err != nil {
				return nil, newSystemErrorWithCause(err, "getting host root gid")
			}
			p.AgentPipe, err = CreateParentAgentPipe(c.root, uid, gid)
			if err != nil {
				return nil, newSystemErrorWithCause(err, "creating parent agent pipe")
			}
		} else {
			p.AgentPipe, err = CreateChildAgentPipe(c.root)
			if err != nil {
				return nil, newSystemErrorWithCause(err, "creating child agent pipe")
			}
		}
	}

	parentLogPipe, childLogPipe, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("Unable to create the log pipe:  %s", err)
	}
	logFilePair := filePair{parentLogPipe, childLogPipe}

	cmd := c.commandTemplate(p, childInitPipe, childLogPipe, p.AgentPipe)
	if !p.Init {
		return c.newSetnsProcess(p, cmd, messageSockPair, logFilePair)
	}

	// We only set up fifoFd if we're not doing a `runc exec`. The historic
	// reason for this is that previously we would pass a dirfd that allowed
	// for container rootfs escape (and not doing it in `runc exec` avoided
	// that problem), but we no longer do that. However, there's no need to do
	// this for `runc exec` so we just keep it this way to be safe.
	if err := c.includeExecFifo(cmd); err != nil {
		return nil, newSystemErrorWithCause(err, "including execfifo in cmd.Exec setup")
	}
	return c.newInitProcess(p, cmd, messageSockPair, logFilePair)
}

func (c *linuxContainer) commandTemplate(p *Process, childInitPipe *os.File, childLogPipe *os.File, agentPipe *os.File) *exec.Cmd {
	cmd := exec.Command(c.initPath, c.initArgs[1:]...)
	cmd.Args[0] = c.initArgs[0]
	cmd.Stdin = p.Stdin
	cmd.Stdout = p.Stdout
	cmd.Stderr = p.Stderr
	cmd.Dir = c.config.Rootfs
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &unix.SysProcAttr{}
	}
	cmd.Env = append(cmd.Env, fmt.Sprintf("GOMAXPROCS=%s", os.Getenv("GOMAXPROCS")))
	cmd.ExtraFiles = append(cmd.ExtraFiles, p.ExtraFiles...)
	if p.ConsoleSocket != nil {
		cmd.ExtraFiles = append(cmd.ExtraFiles, p.ConsoleSocket)
		cmd.Env = append(cmd.Env,
			fmt.Sprintf("_LIBCONTAINER_CONSOLE=%d", stdioFdCount+len(cmd.ExtraFiles)-1),
		)
	}
	cmd.ExtraFiles = append(cmd.ExtraFiles, childInitPipe)
	cmd.Env = append(cmd.Env,
		fmt.Sprintf("_LIBCONTAINER_INITPIPE=%d", stdioFdCount+len(cmd.ExtraFiles)-1),
		fmt.Sprintf("_LIBCONTAINER_STATEDIR=%s", c.root),
	)

	if c.enclaveConfig != nil {
		if agentPipe != nil {
			cmd.ExtraFiles = append(cmd.ExtraFiles, agentPipe)
			cmd.Env = append(cmd.Env,
				fmt.Sprintf("_LIBENCLAVE_AGENTPIPE=%d", stdioFdCount+len(cmd.ExtraFiles)-1))
		}

	}

	cmd.ExtraFiles = append(cmd.ExtraFiles, childLogPipe)
	cmd.Env = append(cmd.Env,
		fmt.Sprintf("_LIBCONTAINER_LOGPIPE=%d", stdioFdCount+len(cmd.ExtraFiles)-1),
		fmt.Sprintf("_LIBCONTAINER_LOGLEVEL=%s", p.LogLevel),
	)

	// NOTE: when running a container with no PID namespace and the parent process spawning the container is
	// PID1 the pdeathsig is being delivered to the container's init process by the kernel for some reason
	// even with the parent still running.
	if c.config.ParentDeathSignal > 0 {
		cmd.SysProcAttr.Pdeathsig = unix.Signal(c.config.ParentDeathSignal)
	}
	return cmd
}

func (c *linuxContainer) newInitProcess(p *Process, cmd *exec.Cmd, messageSockPair, logFilePair filePair) (*initProcess, error) {
	cmd.Env = append(cmd.Env, "_LIBCONTAINER_INITTYPE="+string(initStandard))
	nsMaps := make(map[configs.NamespaceType]string)
	for _, ns := range c.config.Namespaces {
		if ns.Path != "" {
			nsMaps[ns.Type] = ns.Path
		}
	}
	_, sharePidns := nsMaps[configs.NEWPID]
	data, err := c.bootstrapData(c.config.Namespaces.CloneFlags(), nsMaps)
	if err != nil {
		return nil, err
	}
	init := &initProcess{
		cmd:             cmd,
		messageSockPair: messageSockPair,
		logFilePair:     logFilePair,
		manager:         c.cgroupManager,
		intelRdtManager: c.intelRdtManager,
		config:          c.newInitConfig(p),
		container:       c,
		process:         p,
		bootstrapData:   data,
		sharePidns:      sharePidns,
	}
	c.initProcess = init
	return init, nil
}

func (c *linuxContainer) newSetnsProcess(p *Process, cmd *exec.Cmd, messageSockPair, logFilePair filePair) (*setnsProcess, error) {
	cmd.Env = append(cmd.Env, "_LIBCONTAINER_INITTYPE="+string(initSetns))
	state, err := c.currentState()
	if err != nil {
		return nil, newSystemErrorWithCause(err, "getting container's current state")
	}
	// for setns process, we don't have to set cloneflags as the process namespaces
	// will only be set via setns syscall
	data, err := c.bootstrapData(0, state.NamespacePaths)
	if err != nil {
		return nil, err
	}
	return &setnsProcess{
		cmd:             cmd,
		cgroupPaths:     c.cgroupManager.GetPaths(),
		rootlessCgroups: c.config.RootlessCgroups,
		intelRdtPath:    state.IntelRdtPath,
		messageSockPair: messageSockPair,
		logFilePair:     logFilePair,
		config:          c.newInitConfig(p),
		process:         p,
		bootstrapData:   data,
	}, nil
}

func (c *linuxContainer) newInitConfig(process *Process) *initConfig {
	cfg := &initConfig{
		Config:           c.config,
		Args:             process.Args,
		Env:              process.Env,
		User:             process.User,
		AdditionalGroups: process.AdditionalGroups,
		Cwd:              process.Cwd,
		Capabilities:     process.Capabilities,
		PassedFilesCount: len(process.ExtraFiles),
		ContainerId:      c.ID(),
		NoNewPrivileges:  c.config.NoNewPrivileges,
		RootlessEUID:     c.config.RootlessEUID,
		RootlessCgroups:  c.config.RootlessCgroups,
		AppArmorProfile:  c.config.AppArmorProfile,
		ProcessLabel:     c.config.ProcessLabel,
		Rlimits:          c.config.Rlimits,
		EnclaveConfig:    c.enclaveConfig,
		Detached:         c.detached,
	}
	if process.NoNewPrivileges != nil {
		cfg.NoNewPrivileges = *process.NoNewPrivileges
	}
	if process.AppArmorProfile != "" {
		cfg.AppArmorProfile = process.AppArmorProfile
	}
	if process.Label != "" {
		cfg.ProcessLabel = process.Label
	}
	if len(process.Rlimits) > 0 {
		cfg.Rlimits = process.Rlimits
	}
	cfg.CreateConsole = process.ConsoleSocket != nil
	cfg.ConsoleWidth = process.ConsoleWidth
	cfg.ConsoleHeight = process.ConsoleHeight
	return cfg
}

func (c *linuxContainer) Destroy() error {
	c.m.Lock()
	defer c.m.Unlock()
	return c.state.destroy()
}

func (c *linuxContainer) Pause() error {
	c.m.Lock()
	defer c.m.Unlock()
	status, err := c.currentStatus()
	if err != nil {
		return err
	}
	switch status {
	case Running, Created:
		if err := c.cgroupManager.Freeze(configs.Frozen); err != nil {
			return err
		}
		return c.state.transition(&pausedState{
			c: c,
		})
	}
	return newGenericError(fmt.Errorf("container not running or created: %s", status), libcontainer.ContainerNotRunning)
}

func (c *linuxContainer) Resume() error {
	c.m.Lock()
	defer c.m.Unlock()
	status, err := c.currentStatus()
	if err != nil {
		return err
	}
	if status != Paused {
		return newGenericError(fmt.Errorf("container not paused"), libcontainer.ContainerNotPaused)
	}
	if err := c.cgroupManager.Freeze(configs.Thawed); err != nil {
		return err
	}
	return c.state.transition(&runningState{
		c: c,
	})
}

func (c *linuxContainer) NotifyOOM() (<-chan struct{}, error) {
	// XXX(cyphar): This requires cgroups.
	if c.config.RootlessCgroups {
		logrus.Warn("getting OOM notifications may fail if you don't have the full access to cgroups")
	}
	return notifyOnOOM(c.cgroupManager.GetPaths())
}

func (c *linuxContainer) NotifyMemoryPressure(level PressureLevel) (<-chan struct{}, error) {
	// XXX(cyphar): This requires cgroups.
	if c.config.RootlessCgroups {
		logrus.Warn("getting memory pressure notifications may fail if you don't have the full access to cgroups")
	}
	return notifyMemoryPressure(c.cgroupManager.GetPaths(), level)
}

var criuFeatures *criurpc.CriuFeatures

func (c *linuxContainer) checkCriuFeatures(criuOpts *libcontainer.CriuOpts, rpcOpts *criurpc.CriuOpts, criuFeat *criurpc.CriuFeatures) error {

	var t criurpc.CriuReqType
	t = criurpc.CriuReqType_FEATURE_CHECK

	// criu 1.8 => 10800
	if err := c.checkCriuVersion(10800); err != nil {
		// Feature checking was introduced with CRIU 1.8.
		// Ignore the feature check if an older CRIU version is used
		// and just act as before.
		// As all automated PR testing is done using CRIU 1.7 this
		// code will not be tested by automated PR testing.
		return nil
	}

	// make sure the features we are looking for are really not from
	// some previous check
	criuFeatures = nil

	req := &criurpc.CriuReq{
		Type: &t,
		// Theoretically this should not be necessary but CRIU
		// segfaults if Opts is empty.
		// Fixed in CRIU  2.12
		Opts:     rpcOpts,
		Features: criuFeat,
	}

	err := c.criuSwrk(nil, req, criuOpts, false, nil)
	if err != nil {
		logrus.Debugf("%s", err)
		return fmt.Errorf("CRIU feature check failed")
	}

	logrus.Debugf("Feature check says: %s", criuFeatures)
	missingFeatures := false

	// The outer if checks if the fields actually exist
	if (criuFeat.MemTrack != nil) &&
		(criuFeatures.MemTrack != nil) {
		// The inner if checks if they are set to true
		if *criuFeat.MemTrack && !*criuFeatures.MemTrack {
			missingFeatures = true
			logrus.Debugf("CRIU does not support MemTrack")
		}
	}

	// This needs to be repeated for every new feature check.
	// Is there a way to put this in a function. Reflection?
	if (criuFeat.LazyPages != nil) &&
		(criuFeatures.LazyPages != nil) {
		if *criuFeat.LazyPages && !*criuFeatures.LazyPages {
			missingFeatures = true
			logrus.Debugf("CRIU does not support LazyPages")
		}
	}

	if missingFeatures {
		return fmt.Errorf("CRIU is missing features")
	}

	return nil
}

func parseCriuVersion(path string) (int, error) {
	var x, y, z int

	out, err := exec.Command(path, "-V").Output()
	if err != nil {
		return 0, fmt.Errorf("Unable to execute CRIU command: %s", path)
	}

	x = 0
	y = 0
	z = 0
	if ep := strings.Index(string(out), "-"); ep >= 0 {
		// criu Git version format
		var version string
		if sp := strings.Index(string(out), "GitID"); sp > 0 {
			version = string(out)[sp:ep]
		} else {
			return 0, fmt.Errorf("Unable to parse the CRIU version: %s", path)
		}

		n, err := fmt.Sscanf(version, "GitID: v%d.%d.%d", &x, &y, &z) // 1.5.2
		if err != nil {
			n, err = fmt.Sscanf(version, "GitID: v%d.%d", &x, &y) // 1.6
			y++
		} else {
			z++
		}
		if n < 2 || err != nil {
			return 0, fmt.Errorf("Unable to parse the CRIU version: %s %d %s", version, n, err)
		}
	} else {
		// criu release version format
		n, err := fmt.Sscanf(string(out), "Version: %d.%d.%d\n", &x, &y, &z) // 1.5.2
		if err != nil {
			n, err = fmt.Sscanf(string(out), "Version: %d.%d\n", &x, &y) // 1.6
		}
		if n < 2 || err != nil {
			return 0, fmt.Errorf("Unable to parse the CRIU version: %s %d %s", out, n, err)
		}
	}

	return x*10000 + y*100 + z, nil
}

func compareCriuVersion(criuVersion int, minVersion int) error {
	// simple function to perform the actual version compare
	if criuVersion < minVersion {
		return fmt.Errorf("CRIU version %d must be %d or higher", criuVersion, minVersion)
	}

	return nil
}

// This is used to store the result of criu version RPC
var criuVersionRPC *criurpc.CriuVersion

// checkCriuVersion checks Criu version greater than or equal to minVersion
func (c *linuxContainer) checkCriuVersion(minVersion int) error {

	// If the version of criu has already been determined there is no need
	// to ask criu for the version again. Use the value from c.criuVersion.
	if c.criuVersion != 0 {
		return compareCriuVersion(c.criuVersion, minVersion)
	}

	// First try if this version of CRIU support the version RPC.
	// The CRIU version RPC was introduced with CRIU 3.0.

	// First, reset the variable for the RPC answer to nil
	criuVersionRPC = nil

	var t criurpc.CriuReqType
	t = criurpc.CriuReqType_VERSION
	req := &criurpc.CriuReq{
		Type: &t,
	}

	err := c.criuSwrk(nil, req, nil, false, nil)
	if err != nil {
		return fmt.Errorf("CRIU version check failed: %s", err)
	}

	if criuVersionRPC != nil {
		logrus.Debugf("CRIU version: %s", criuVersionRPC)
		// major and minor are always set
		c.criuVersion = int(*criuVersionRPC.Major) * 10000
		c.criuVersion += int(*criuVersionRPC.Minor) * 100
		if criuVersionRPC.Sublevel != nil {
			c.criuVersion += int(*criuVersionRPC.Sublevel)
		}
		if criuVersionRPC.Gitid != nil {
			// runc's convention is that a CRIU git release is
			// always the same as increasing the minor by 1
			c.criuVersion -= (c.criuVersion % 100)
			c.criuVersion += 100
		}
		return compareCriuVersion(c.criuVersion, minVersion)
	}

	// This is CRIU without the version RPC and therefore
	// older than 3.0. Parsing the output is required.

	// This can be remove once runc does not work with criu older than 3.0

	c.criuVersion, err = parseCriuVersion(c.criuPath)
	if err != nil {
		return err
	}

	return compareCriuVersion(c.criuVersion, minVersion)
}

const descriptorsFilename = "descriptors.json"

func (c *linuxContainer) addCriuDumpMount(req *criurpc.CriuReq, m *configs.Mount) {
	mountDest := m.Destination
	if strings.HasPrefix(mountDest, c.config.Rootfs) {
		mountDest = mountDest[len(c.config.Rootfs):]
	}

	extMnt := &criurpc.ExtMountMap{
		Key: proto.String(mountDest),
		Val: proto.String(mountDest),
	}
	req.Opts.ExtMnt = append(req.Opts.ExtMnt, extMnt)
}

func (c *linuxContainer) addMaskPaths(req *criurpc.CriuReq) error {
	for _, path := range c.config.MaskPaths {
		fi, err := os.Stat(fmt.Sprintf("/proc/%d/root/%s", c.initProcess.pid(), path))
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return err
		}
		if fi.IsDir() {
			continue
		}

		extMnt := &criurpc.ExtMountMap{
			Key: proto.String(path),
			Val: proto.String("/dev/null"),
		}
		req.Opts.ExtMnt = append(req.Opts.ExtMnt, extMnt)
	}
	return nil
}

func waitForCriuLazyServer(r *os.File, status string) error {

	data := make([]byte, 1)
	_, err := r.Read(data)
	if err != nil {
		return err
	}
	fd, err := os.OpenFile(status, os.O_TRUNC|os.O_WRONLY, os.ModeAppend)
	if err != nil {
		return err
	}
	_, err = fd.Write(data)
	if err != nil {
		return err
	}
	fd.Close()

	return nil
}

func (c *linuxContainer) handleCriuConfigurationFile(rpcOpts *criurpc.CriuOpts) {
	// CRIU will evaluate a configuration starting with release 3.11.
	// Settings in the configuration file will overwrite RPC settings.
	// Look for annotations. The annotation 'org.criu.config'
	// specifies if CRIU should use a different, container specific
	// configuration file.
	_, annotations := utils.Annotations(c.config.Labels)
	configFile, exists := annotations["org.criu.config"]
	if exists {
		// If the annotation 'org.criu.config' exists and is set
		// to a non-empty string, tell CRIU to use that as a
		// configuration file. If the file does not exist, CRIU
		// will just ignore it.
		if configFile != "" {
			rpcOpts.ConfigFile = proto.String(configFile)
		}
		// If 'org.criu.config' exists and is set to an empty
		// string, a runc specific CRIU configuration file will
		// be not set at all.
	} else {
		// If the mentioned annotation has not been found, specify
		// a default CRIU configuration file.
		rpcOpts.ConfigFile = proto.String("/etc/criu/runc.conf")
	}
}

func (c *linuxContainer) Checkpoint(criuOpts *libcontainer.CriuOpts) error {
	c.m.Lock()
	defer c.m.Unlock()

	// Checkpoint is unlikely to work if os.Geteuid() != 0 || system.RunningInUserNS().
	// (CLI prints a warning)
	// TODO(avagin): Figure out how to make this work nicely. CRIU 2.0 has
	//               support for doing unprivileged dumps, but the setup of
	//               rootless containers might make this complicated.

	// criu 1.5.2 => 10502
	if err := c.checkCriuVersion(10502); err != nil {
		return err
	}

	if criuOpts.ImagesDirectory == "" {
		return fmt.Errorf("invalid directory to save checkpoint")
	}

	// Since a container can be C/R'ed multiple times,
	// the checkpoint directory may already exist.
	if err := os.Mkdir(criuOpts.ImagesDirectory, 0700); err != nil && !os.IsExist(err) {
		return err
	}

	if criuOpts.WorkDirectory == "" {
		criuOpts.WorkDirectory = filepath.Join(c.root, "criu.work")
	}

	if err := os.Mkdir(criuOpts.WorkDirectory, 0700); err != nil && !os.IsExist(err) {
		return err
	}

	workDir, err := os.Open(criuOpts.WorkDirectory)
	if err != nil {
		return err
	}
	defer workDir.Close()

	imageDir, err := os.Open(criuOpts.ImagesDirectory)
	if err != nil {
		return err
	}
	defer imageDir.Close()

	rpcOpts := criurpc.CriuOpts{
		ImagesDirFd:     proto.Int32(int32(imageDir.Fd())),
		WorkDirFd:       proto.Int32(int32(workDir.Fd())),
		LogLevel:        proto.Int32(4),
		LogFile:         proto.String("dump.log"),
		Root:            proto.String(c.config.Rootfs),
		ManageCgroups:   proto.Bool(true),
		NotifyScripts:   proto.Bool(true),
		Pid:             proto.Int32(int32(c.initProcess.pid())),
		ShellJob:        proto.Bool(criuOpts.ShellJob),
		LeaveRunning:    proto.Bool(criuOpts.LeaveRunning),
		TcpEstablished:  proto.Bool(criuOpts.TcpEstablished),
		ExtUnixSk:       proto.Bool(criuOpts.ExternalUnixConnections),
		FileLocks:       proto.Bool(criuOpts.FileLocks),
		EmptyNs:         proto.Uint32(criuOpts.EmptyNs),
		OrphanPtsMaster: proto.Bool(true),
		AutoDedup:       proto.Bool(criuOpts.AutoDedup),
		LazyPages:       proto.Bool(criuOpts.LazyPages),
	}

	c.handleCriuConfigurationFile(&rpcOpts)

	// If the container is running in a network namespace and has
	// a path to the network namespace configured, we will dump
	// that network namespace as an external namespace and we
	// will expect that the namespace exists during restore.
	// This basically means that CRIU will ignore the namespace
	// and expect to be setup correctly.
	nsPath := c.config.Namespaces.PathOf(configs.NEWNET)
	if nsPath != "" {
		// For this to work we need at least criu 3.11.0 => 31100.
		// As there was already a successful version check we will
		// not error out if it fails. runc will just behave as it used
		// to do and ignore external network namespaces.
		err := c.checkCriuVersion(31100)
		if err == nil {
			// CRIU expects the information about an external namespace
			// like this: --external net[<inode>]:<key>
			// This <key> is always 'extRootNetNS'.
			var netns unix.Stat_t
			err = unix.Stat(nsPath, &netns)
			if err != nil {
				return err
			}
			criuExternal := fmt.Sprintf("net[%d]:extRootNetNS", netns.Ino)
			rpcOpts.External = append(rpcOpts.External, criuExternal)
		}
	}

	// CRIU can use cgroup freezer; when rpcOpts.FreezeCgroup
	// is not set, CRIU uses ptrace() to pause the processes.
	var fcg string
	switch cgroups.IsCgroup2UnifiedMode() {
	case false:
		fcg = c.cgroupManager.GetPaths()["freezer"]
	case true:
		// cgroup v2 freezer is only supported since CRIU release 3.14
		if c.checkCriuVersion(31400) == nil {
			fcg, err = c.cgroupManager.GetUnifiedPath()
			if err != nil {
				// should not happen
				return err
			}
		}
	}
	if fcg != "" {
		rpcOpts.FreezeCgroup = proto.String(fcg)
	}

	// append optional criu opts, e.g., page-server and port
	if criuOpts.PageServer.Address != "" && criuOpts.PageServer.Port != 0 {
		rpcOpts.Ps = &criurpc.CriuPageServerInfo{
			Address: proto.String(criuOpts.PageServer.Address),
			Port:    proto.Int32(criuOpts.PageServer.Port),
		}
	}

	//pre-dump may need parentImage param to complete iterative migration
	if criuOpts.ParentImage != "" {
		rpcOpts.ParentImg = proto.String(criuOpts.ParentImage)
		rpcOpts.TrackMem = proto.Bool(true)
	}

	// append optional manage cgroups mode
	if criuOpts.ManageCgroupsMode != 0 {
		// criu 1.7 => 10700
		if err := c.checkCriuVersion(10700); err != nil {
			return err
		}
		mode := criurpc.CriuCgMode(criuOpts.ManageCgroupsMode)
		rpcOpts.ManageCgroupsMode = &mode
	}

	var t criurpc.CriuReqType
	if criuOpts.PreDump {
		feat := criurpc.CriuFeatures{
			MemTrack: proto.Bool(true),
		}

		if err := c.checkCriuFeatures(criuOpts, &rpcOpts, &feat); err != nil {
			return err
		}

		t = criurpc.CriuReqType_PRE_DUMP
	} else {
		t = criurpc.CriuReqType_DUMP
	}
	req := &criurpc.CriuReq{
		Type: &t,
		Opts: &rpcOpts,
	}

	if criuOpts.LazyPages {
		// lazy migration requested; check if criu supports it
		feat := criurpc.CriuFeatures{
			LazyPages: proto.Bool(true),
		}

		if err := c.checkCriuFeatures(criuOpts, &rpcOpts, &feat); err != nil {
			return err
		}

		statusRead, statusWrite, err := os.Pipe()
		if err != nil {
			return err
		}
		rpcOpts.StatusFd = proto.Int32(int32(statusWrite.Fd()))
		go waitForCriuLazyServer(statusRead, criuOpts.StatusFd)
	}

	// no need to dump all this in pre-dump
	if !criuOpts.PreDump {
		hasCgroupns := c.config.Namespaces.Contains(configs.NEWCGROUP)
		for _, m := range c.config.Mounts {
			switch m.Device {
			case "bind":
				c.addCriuDumpMount(req, m)
			case "cgroup":
				if cgroups.IsCgroup2UnifiedMode() || hasCgroupns {
					// real mount(s)
					continue
				}
				// a set of "external" bind mounts
				binds, err := getCgroupMounts(m)
				if err != nil {
					return err
				}
				for _, b := range binds {
					c.addCriuDumpMount(req, b)
				}
			}
		}

		if err := c.addMaskPaths(req); err != nil {
			return err
		}

		for _, node := range c.config.Devices {
			m := &configs.Mount{Destination: node.Path, Source: node.Path}
			c.addCriuDumpMount(req, m)
		}

		// Write the FD info to a file in the image directory
		fdsJSON, err := json.Marshal(c.initProcess.externalDescriptors())
		if err != nil {
			return err
		}

		err = ioutil.WriteFile(filepath.Join(criuOpts.ImagesDirectory, descriptorsFilename), fdsJSON, 0600)
		if err != nil {
			return err
		}
	}

	err = c.criuSwrk(nil, req, criuOpts, false, nil)
	if err != nil {
		return err
	}
	return nil
}

func (c *linuxContainer) addCriuRestoreMount(req *criurpc.CriuReq, m *configs.Mount) {
	mountDest := m.Destination
	if strings.HasPrefix(mountDest, c.config.Rootfs) {
		mountDest = mountDest[len(c.config.Rootfs):]
	}

	extMnt := &criurpc.ExtMountMap{
		Key: proto.String(mountDest),
		Val: proto.String(m.Source),
	}
	req.Opts.ExtMnt = append(req.Opts.ExtMnt, extMnt)
}

func (c *linuxContainer) restoreNetwork(req *criurpc.CriuReq, criuOpts *libcontainer.CriuOpts) {
	for _, iface := range c.config.Networks {
		switch iface.Type {
		case "veth":
			veth := new(criurpc.CriuVethPair)
			veth.IfOut = proto.String(iface.HostInterfaceName)
			veth.IfIn = proto.String(iface.Name)
			req.Opts.Veths = append(req.Opts.Veths, veth)
		case "loopback":
			// Do nothing
		}
	}
	for _, i := range criuOpts.VethPairs {
		veth := new(criurpc.CriuVethPair)
		veth.IfOut = proto.String(i.HostInterfaceName)
		veth.IfIn = proto.String(i.ContainerInterfaceName)
		req.Opts.Veths = append(req.Opts.Veths, veth)
	}
}

// makeCriuRestoreMountpoints makes the actual mountpoints for the
// restore using CRIU. This function is inspired from the code in
// rootfs_linux.go
func (c *linuxContainer) makeCriuRestoreMountpoints(m *configs.Mount) error {
	switch m.Device {
	case "cgroup":
		// No mount point(s) need to be created:
		//
		// * for v1, mount points are saved by CRIU because
		//   /sys/fs/cgroup is a tmpfs mount
		//
		// * for v2, /sys/fs/cgroup is a real mount, but
		//   the mountpoint appears as soon as /sys is mounted
		return nil
	case "bind":
		// The prepareBindMount() function checks if source
		// exists. So it cannot be used for other filesystem types.
		if err := prepareBindMount(m, c.config.Rootfs); err != nil {
			return err
		}
	default:
		// for all other filesystems just create the mountpoints
		dest, err := securejoin.SecureJoin(c.config.Rootfs, m.Destination)
		if err != nil {
			return err
		}
		if err := checkProcMount(c.config.Rootfs, dest, ""); err != nil {
			return err
		}
		m.Destination = dest
		if err := os.MkdirAll(dest, 0755); err != nil {
			return err
		}
	}
	return nil
}

// isPathInPrefixList is a small function for CRIU restore to make sure
// mountpoints, which are on a tmpfs, are not created in the roofs
func isPathInPrefixList(path string, prefix []string) bool {
	for _, p := range prefix {
		if strings.HasPrefix(path, p+"/") {
			return true
		}
	}
	return false
}

// prepareCriuRestoreMounts tries to set up the rootfs of the
// container to be restored in the same way runc does it for
// initial container creation. Even for a read-only rootfs container
// runc modifies the rootfs to add mountpoints which do not exist.
// This function also creates missing mountpoints as long as they
// are not on top of a tmpfs, as CRIU will restore tmpfs content anyway.
func (c *linuxContainer) prepareCriuRestoreMounts(mounts []*configs.Mount) error {
	// First get a list of a all tmpfs mounts
	tmpfs := []string{}
	for _, m := range mounts {
		switch m.Device {
		case "tmpfs":
			tmpfs = append(tmpfs, m.Destination)
		}
	}
	// Now go through all mounts and create the mountpoints
	// if the mountpoints are not on a tmpfs, as CRIU will
	// restore the complete tmpfs content from its checkpoint.
	for _, m := range mounts {
		if !isPathInPrefixList(m.Destination, tmpfs) {
			if err := c.makeCriuRestoreMountpoints(m); err != nil {
				return err
			}
		}
	}
	return nil
}

func (c *linuxContainer) Restore(process *Process, criuOpts *libcontainer.CriuOpts) error {
	c.m.Lock()
	defer c.m.Unlock()

	var extraFiles []*os.File

	// Restore is unlikely to work if os.Geteuid() != 0 || system.RunningInUserNS().
	// (CLI prints a warning)
	// TODO(avagin): Figure out how to make this work nicely. CRIU doesn't have
	//               support for unprivileged restore at the moment.

	// criu 1.5.2 => 10502
	if err := c.checkCriuVersion(10502); err != nil {
		return err
	}
	if criuOpts.WorkDirectory == "" {
		criuOpts.WorkDirectory = filepath.Join(c.root, "criu.work")
	}
	// Since a container can be C/R'ed multiple times,
	// the work directory may already exist.
	if err := os.Mkdir(criuOpts.WorkDirectory, 0700); err != nil && !os.IsExist(err) {
		return err
	}
	workDir, err := os.Open(criuOpts.WorkDirectory)
	if err != nil {
		return err
	}
	defer workDir.Close()
	if criuOpts.ImagesDirectory == "" {
		return fmt.Errorf("invalid directory to restore checkpoint")
	}
	imageDir, err := os.Open(criuOpts.ImagesDirectory)
	if err != nil {
		return err
	}
	defer imageDir.Close()
	// CRIU has a few requirements for a root directory:
	// * it must be a mount point
	// * its parent must not be overmounted
	// c.config.Rootfs is bind-mounted to a temporary directory
	// to satisfy these requirements.
	root := filepath.Join(c.root, "criu-root")
	if err := os.Mkdir(root, 0755); err != nil {
		return err
	}
	defer os.Remove(root)
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return err
	}
	err = unix.Mount(c.config.Rootfs, root, "", unix.MS_BIND|unix.MS_REC, "")
	if err != nil {
		return err
	}
	defer unix.Unmount(root, unix.MNT_DETACH)
	t := criurpc.CriuReqType_RESTORE
	req := &criurpc.CriuReq{
		Type: &t,
		Opts: &criurpc.CriuOpts{
			ImagesDirFd:     proto.Int32(int32(imageDir.Fd())),
			WorkDirFd:       proto.Int32(int32(workDir.Fd())),
			EvasiveDevices:  proto.Bool(true),
			LogLevel:        proto.Int32(4),
			LogFile:         proto.String("restore.log"),
			RstSibling:      proto.Bool(true),
			Root:            proto.String(root),
			ManageCgroups:   proto.Bool(true),
			NotifyScripts:   proto.Bool(true),
			ShellJob:        proto.Bool(criuOpts.ShellJob),
			ExtUnixSk:       proto.Bool(criuOpts.ExternalUnixConnections),
			TcpEstablished:  proto.Bool(criuOpts.TcpEstablished),
			FileLocks:       proto.Bool(criuOpts.FileLocks),
			EmptyNs:         proto.Uint32(criuOpts.EmptyNs),
			OrphanPtsMaster: proto.Bool(true),
			AutoDedup:       proto.Bool(criuOpts.AutoDedup),
			LazyPages:       proto.Bool(criuOpts.LazyPages),
		},
	}

	c.handleCriuConfigurationFile(req.Opts)

	// Same as during checkpointing. If the container has a specific network namespace
	// assigned to it, this now expects that the checkpoint will be restored in a
	// already created network namespace.
	nsPath := c.config.Namespaces.PathOf(configs.NEWNET)
	if nsPath != "" {
		// For this to work we need at least criu 3.11.0 => 31100.
		// As there was already a successful version check we will
		// not error out if it fails. runc will just behave as it used
		// to do and ignore external network namespaces.
		err := c.checkCriuVersion(31100)
		if err == nil {
			// CRIU wants the information about an existing network namespace
			// like this: --inherit-fd fd[<fd>]:<key>
			// The <key> needs to be the same as during checkpointing.
			// We are always using 'extRootNetNS' as the key in this.
			netns, err := os.Open(nsPath)
			if err != nil {
				logrus.Errorf("If a specific network namespace is defined it must exist: %s", err)
				return fmt.Errorf("Requested network namespace %v does not exist", nsPath)
			}
			defer netns.Close()
			inheritFd := new(criurpc.InheritFd)
			inheritFd.Key = proto.String("extRootNetNS")
			// The offset of four is necessary because 0, 1, 2 and 3 is already
			// used by stdin, stdout, stderr, 'criu swrk' socket.
			inheritFd.Fd = proto.Int32(int32(4 + len(extraFiles)))
			req.Opts.InheritFd = append(req.Opts.InheritFd, inheritFd)
			// All open FDs need to be transferred to CRIU via extraFiles
			extraFiles = append(extraFiles, netns)
		}
	}

	// This will modify the rootfs of the container in the same way runc
	// modifies the container during initial creation.
	if err := c.prepareCriuRestoreMounts(c.config.Mounts); err != nil {
		return err
	}

	hasCgroupns := c.config.Namespaces.Contains(configs.NEWCGROUP)
	for _, m := range c.config.Mounts {
		switch m.Device {
		case "bind":
			c.addCriuRestoreMount(req, m)
		case "cgroup":
			if cgroups.IsCgroup2UnifiedMode() || hasCgroupns {
				continue
			}
			// cgroup v1 is a set of bind mounts, unless cgroupns is used
			binds, err := getCgroupMounts(m)
			if err != nil {
				return err
			}
			for _, b := range binds {
				c.addCriuRestoreMount(req, b)
			}
		}
	}

	if len(c.config.MaskPaths) > 0 {
		m := &configs.Mount{Destination: "/dev/null", Source: "/dev/null"}
		c.addCriuRestoreMount(req, m)
	}

	for _, node := range c.config.Devices {
		m := &configs.Mount{Destination: node.Path, Source: node.Path}
		c.addCriuRestoreMount(req, m)
	}

	if criuOpts.EmptyNs&unix.CLONE_NEWNET == 0 {
		c.restoreNetwork(req, criuOpts)
	}

	// append optional manage cgroups mode
	if criuOpts.ManageCgroupsMode != 0 {
		// criu 1.7 => 10700
		if err := c.checkCriuVersion(10700); err != nil {
			return err
		}
		mode := criurpc.CriuCgMode(criuOpts.ManageCgroupsMode)
		req.Opts.ManageCgroupsMode = &mode
	}

	var (
		fds    []string
		fdJSON []byte
	)
	if fdJSON, err = ioutil.ReadFile(filepath.Join(criuOpts.ImagesDirectory, descriptorsFilename)); err != nil {
		return err
	}

	if err := json.Unmarshal(fdJSON, &fds); err != nil {
		return err
	}
	for i := range fds {
		if s := fds[i]; strings.Contains(s, "pipe:") {
			inheritFd := new(criurpc.InheritFd)
			inheritFd.Key = proto.String(s)
			inheritFd.Fd = proto.Int32(int32(i))
			req.Opts.InheritFd = append(req.Opts.InheritFd, inheritFd)
		}
	}
	return c.criuSwrk(process, req, criuOpts, true, extraFiles)
}

func (c *linuxContainer) criuApplyCgroups(pid int, req *criurpc.CriuReq) error {
	// XXX: Do we need to deal with this case? AFAIK criu still requires root.
	if err := c.cgroupManager.Apply(pid); err != nil {
		return err
	}

	if err := c.cgroupManager.Set(c.config); err != nil {
		return newSystemError(err)
	}

	path := fmt.Sprintf("/proc/%d/cgroup", pid)
	cgroupsPaths, err := cgroups.ParseCgroupFile(path)
	if err != nil {
		return err
	}

	for c, p := range cgroupsPaths {
		cgroupRoot := &criurpc.CgroupRoot{
			Ctrl: proto.String(c),
			Path: proto.String(p),
		}
		req.Opts.CgRoot = append(req.Opts.CgRoot, cgroupRoot)
	}

	return nil
}

func (c *linuxContainer) criuSwrk(process *Process, req *criurpc.CriuReq, opts *libcontainer.CriuOpts, applyCgroups bool, extraFiles []*os.File) error {
	fds, err := unix.Socketpair(unix.AF_LOCAL, unix.SOCK_SEQPACKET|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return err
	}

	var logPath string
	if opts != nil {
		logPath = filepath.Join(opts.WorkDirectory, req.GetOpts().GetLogFile())
	} else {
		// For the VERSION RPC 'opts' is set to 'nil' and therefore
		// opts.WorkDirectory does not exist. Set logPath to "".
		logPath = ""
	}
	criuClient := os.NewFile(uintptr(fds[0]), "criu-transport-client")
	criuClientFileCon, err := net.FileConn(criuClient)
	criuClient.Close()
	if err != nil {
		return err
	}

	criuClientCon := criuClientFileCon.(*net.UnixConn)
	defer criuClientCon.Close()

	criuServer := os.NewFile(uintptr(fds[1]), "criu-transport-server")
	defer criuServer.Close()

	args := []string{"swrk", "3"}
	if c.criuVersion != 0 {
		// If the CRIU Version is still '0' then this is probably
		// the initial CRIU run to detect the version. Skip it.
		logrus.Debugf("Using CRIU %d at: %s", c.criuVersion, c.criuPath)
	}
	logrus.Debugf("Using CRIU with following args: %s", args)
	cmd := exec.Command(c.criuPath, args...)
	if process != nil {
		cmd.Stdin = process.Stdin
		cmd.Stdout = process.Stdout
		cmd.Stderr = process.Stderr
	}
	cmd.ExtraFiles = append(cmd.ExtraFiles, criuServer)
	if extraFiles != nil {
		cmd.ExtraFiles = append(cmd.ExtraFiles, extraFiles...)
	}

	if err := cmd.Start(); err != nil {
		return err
	}
	// we close criuServer so that even if CRIU crashes or unexpectedly exits, runc will not hang.
	criuServer.Close()
	// cmd.Process will be replaced by a restored init.
	criuProcess := cmd.Process

	defer func() {
		criuClientCon.Close()
		_, err := criuProcess.Wait()
		if err != nil {
			return
		}
	}()

	if applyCgroups {
		err := c.criuApplyCgroups(criuProcess.Pid, req)
		if err != nil {
			return err
		}
	}

	var extFds []string
	if process != nil {
		extFds, err = getPipeFds(criuProcess.Pid)
		if err != nil {
			return err
		}
	}

	logrus.Debugf("Using CRIU in %s mode", req.GetType().String())
	// In the case of criurpc.CriuReqType_FEATURE_CHECK req.GetOpts()
	// should be empty. For older CRIU versions it still will be
	// available but empty. criurpc.CriuReqType_VERSION actually
	// has no req.GetOpts().
	if !(req.GetType() == criurpc.CriuReqType_FEATURE_CHECK ||
		req.GetType() == criurpc.CriuReqType_VERSION) {

		val := reflect.ValueOf(req.GetOpts())
		v := reflect.Indirect(val)
		for i := 0; i < v.NumField(); i++ {
			st := v.Type()
			name := st.Field(i).Name
			if strings.HasPrefix(name, "XXX_") {
				continue
			}
			value := val.MethodByName("Get" + name).Call([]reflect.Value{})
			logrus.Debugf("CRIU option %s with value %v", name, value[0])
		}
	}
	data, err := proto.Marshal(req)
	if err != nil {
		return err
	}
	_, err = criuClientCon.Write(data)
	if err != nil {
		return err
	}

	buf := make([]byte, 10*4096)
	oob := make([]byte, 4096)
	for true {
		n, oobn, _, _, err := criuClientCon.ReadMsgUnix(buf, oob)
		if err != nil {
			return err
		}
		if n == 0 {
			return fmt.Errorf("unexpected EOF")
		}
		if n == len(buf) {
			return fmt.Errorf("buffer is too small")
		}

		resp := new(criurpc.CriuResp)
		err = proto.Unmarshal(buf[:n], resp)
		if err != nil {
			return err
		}
		if !resp.GetSuccess() {
			typeString := req.GetType().String()
			if typeString == "VERSION" {
				// If the VERSION RPC fails this probably means that the CRIU
				// version is too old for this RPC. Just return 'nil'.
				return nil
			}
			return fmt.Errorf("criu failed: type %s errno %d\nlog file: %s", typeString, resp.GetCrErrno(), logPath)
		}

		t := resp.GetType()
		switch {
		case t == criurpc.CriuReqType_VERSION:
			logrus.Debugf("CRIU version: %s", resp)
			criuVersionRPC = resp.GetVersion()
			break
		case t == criurpc.CriuReqType_FEATURE_CHECK:
			logrus.Debugf("Feature check says: %s", resp)
			criuFeatures = resp.GetFeatures()
		case t == criurpc.CriuReqType_NOTIFY:
			if err := c.criuNotifications(resp, process, cmd, opts, extFds, oob[:oobn]); err != nil {
				return err
			}
			t = criurpc.CriuReqType_NOTIFY
			req = &criurpc.CriuReq{
				Type:          &t,
				NotifySuccess: proto.Bool(true),
			}
			data, err = proto.Marshal(req)
			if err != nil {
				return err
			}
			_, err = criuClientCon.Write(data)
			if err != nil {
				return err
			}
			continue
		case t == criurpc.CriuReqType_RESTORE:
		case t == criurpc.CriuReqType_DUMP:
		case t == criurpc.CriuReqType_PRE_DUMP:
		default:
			return fmt.Errorf("unable to parse the response %s", resp.String())
		}

		break
	}

	criuClientCon.CloseWrite()
	// cmd.Wait() waits cmd.goroutines which are used for proxying file descriptors.
	// Here we want to wait only the CRIU process.
	st, err := criuProcess.Wait()
	if err != nil {
		return err
	}

	// In pre-dump mode CRIU is in a loop and waits for
	// the final DUMP command.
	// The current runc pre-dump approach, however, is
	// start criu in PRE_DUMP once for a single pre-dump
	// and not the whole series of pre-dump, pre-dump, ...m, dump
	// If we got the message CriuReqType_PRE_DUMP it means
	// CRIU was successful and we need to forcefully stop CRIU
	if !st.Success() && *req.Type != criurpc.CriuReqType_PRE_DUMP {
		return fmt.Errorf("criu failed: %s\nlog file: %s", st.String(), logPath)
	}
	return nil
}

// block any external network activity
func lockNetwork(config *configs.Config) error {
	for _, config := range config.Networks {
		strategy, err := getStrategy(config.Type)
		if err != nil {
			return err
		}

		if err := strategy.detach(config); err != nil {
			return err
		}
	}
	return nil
}

func unlockNetwork(config *configs.Config) error {
	for _, config := range config.Networks {
		strategy, err := getStrategy(config.Type)
		if err != nil {
			return err
		}
		if err = strategy.attach(config); err != nil {
			return err
		}
	}
	return nil
}

func (c *linuxContainer) criuNotifications(resp *criurpc.CriuResp, process *Process, cmd *exec.Cmd, opts *libcontainer.CriuOpts, fds []string, oob []byte) error {
	notify := resp.GetNotify()
	if notify == nil {
		return fmt.Errorf("invalid response: %s", resp.String())
	}
	logrus.Debugf("notify: %s\n", notify.GetScript())
	switch {
	case notify.GetScript() == "post-dump":
		f, err := os.Create(filepath.Join(c.root, "checkpoint"))
		if err != nil {
			return err
		}
		f.Close()
	case notify.GetScript() == "network-unlock":
		if err := unlockNetwork(c.config); err != nil {
			return err
		}
	case notify.GetScript() == "network-lock":
		if err := lockNetwork(c.config); err != nil {
			return err
		}
	case notify.GetScript() == "setup-namespaces":
		if c.config.Hooks != nil {
			s, err := c.currentOCIState()
			if err != nil {
				return nil
			}
			s.Pid = int(notify.GetPid())
			for i, hook := range c.config.Hooks.Prestart {
				if err := hook.Run(s); err != nil {
					return newSystemErrorWithCausef(err, "running prestart hook %d", i)
				}
			}
		}
	case notify.GetScript() == "post-restore":
		pid := notify.GetPid()

		p, err := os.FindProcess(int(pid))
		if err != nil {
			return err
		}
		cmd.Process = p

		r, err := newRestoredProcess(cmd, fds)
		if err != nil {
			return err
		}
		process.ops = r
		if err := c.state.transition(&restoredState{
			imageDir: opts.ImagesDirectory,
			c:        c,
		}); err != nil {
			return err
		}
		// create a timestamp indicating when the restored checkpoint was started
		c.created = time.Now().UTC()
		if _, err := c.updateState(r); err != nil {
			return err
		}
		if err := os.Remove(filepath.Join(c.root, "checkpoint")); err != nil {
			if !os.IsNotExist(err) {
				logrus.Error(err)
			}
		}
	case notify.GetScript() == "orphan-pts-master":
		scm, err := unix.ParseSocketControlMessage(oob)
		if err != nil {
			return err
		}
		fds, err := unix.ParseUnixRights(&scm[0])
		if err != nil {
			return err
		}

		master := os.NewFile(uintptr(fds[0]), "orphan-pts-master")
		defer master.Close()

		// While we can access console.master, using the API is a good idea.
		if err := utils.SendFd(process.ConsoleSocket, master.Name(), master.Fd()); err != nil {
			return err
		}
	}
	return nil
}

func (c *linuxContainer) updateState(process parentProcess) (*State, error) {
	if process != nil {
		c.initProcess = process
	}
	state, err := c.currentState()
	if err != nil {
		return nil, err
	}
	err = c.saveState(state)
	if err != nil {
		return nil, err
	}
	return state, nil
}

func (c *linuxContainer) saveState(s *State) error {
	f, err := os.Create(filepath.Join(c.root, stateFilename))
	if err != nil {
		return err
	}
	defer f.Close()
	return utils.WriteJSON(f, s)
}

func (c *linuxContainer) deleteState() error {
	return os.Remove(filepath.Join(c.root, stateFilename))
}

func (c *linuxContainer) currentStatus() (Status, error) {
	if err := c.refreshState(); err != nil {
		return -1, err
	}
	return c.state.status(), nil
}

// refreshState needs to be called to verify that the current state on the
// container is what is true.  Because consumers of libenclave can use it
// out of process we need to verify the container's status based on runtime
// information and not rely on our in process info.
func (c *linuxContainer) refreshState() error {
	paused, err := c.isPaused()
	if err != nil {
		return err
	}
	if paused {
		return c.state.transition(&pausedState{c: c})
	}
	t := c.runType()
	switch t {
	case Created:
		return c.state.transition(&createdState{c: c})
	case Running:
		return c.state.transition(&runningState{c: c})
	}
	return c.state.transition(&stoppedState{c: c})
}

func (c *linuxContainer) runType() Status {
	if c.initProcess == nil {
		return Stopped
	}
	pid := c.initProcess.pid()
	stat, err := system.Stat(pid)
	if err != nil {
		return Stopped
	}
	if stat.StartTime != c.initProcessStartTime || stat.State == system.Zombie || stat.State == system.Dead {
		return Stopped
	}
	// We'll create exec fifo and blocking on it after container is created,
	// and delete it after start container.
	if _, err := os.Stat(filepath.Join(c.root, execFifoFilename)); err == nil {
		return Created
	}
	return Running
}

func (c *linuxContainer) isPaused() (bool, error) {
	var fcg, filename, pausedState string

	if !cgroups.IsCgroup2UnifiedMode() {
		fcg = c.cgroupManager.GetPaths()["freezer"]
		if fcg == "" {
			// A container doesn't have a freezer cgroup
			return false, nil
		}
		filename = "freezer.state"
		pausedState = "FROZEN"
	} else {
		var err error
		fcg, err = c.cgroupManager.GetUnifiedPath()
		if err != nil {
			// should not happen
			return false, err
		}
		filename = "cgroup.freeze"
		pausedState = "1"
	}

	data, err := ioutil.ReadFile(filepath.Join(fcg, filename))
	if err != nil {
		// If freezer cgroup is not mounted, the container would just be not paused.
		if os.IsNotExist(err) || errors.Is(err, unix.ENODEV) {
			return false, nil
		}
		return false, newSystemErrorWithCause(err, "checking if container is paused")
	}
	return bytes.Equal(bytes.TrimSpace(data), []byte(pausedState)), nil
}

func (c *linuxContainer) currentState() (*State, error) {
	var (
		startTime           uint64
		externalDescriptors []string
		pid                 = -1
	)
	if c.initProcess != nil {
		pid = c.initProcess.pid()
		startTime, _ = c.initProcess.startTime()
		externalDescriptors = c.initProcess.externalDescriptors()
	}
	intelRdtPath, err := intelrdt.GetIntelRdtPath(c.ID())
	if err != nil {
		intelRdtPath = ""
	}
	state := &State{
		BaseState: BaseState{
			ID:                   c.ID(),
			Config:               *c.config,
			InitProcessPid:       pid,
			InitProcessStartTime: startTime,
			Created:              c.created,
		},
		Rootless:            c.config.RootlessEUID && c.config.RootlessCgroups,
		CgroupPaths:         c.cgroupManager.GetPaths(),
		IntelRdtPath:        intelRdtPath,
		NamespacePaths:      make(map[configs.NamespaceType]string),
		ExternalDescriptors: externalDescriptors,
	}
	if c.enclaveConfig != nil {
		state.BaseState.EnclaveConfig = *c.enclaveConfig
	}

	if pid > 0 {
		for _, ns := range c.config.Namespaces {
			state.NamespacePaths[ns.Type] = ns.GetPath(pid)
		}
		for _, nsType := range configs.NamespaceTypes() {
			if !configs.IsNamespaceSupported(nsType) {
				continue
			}
			if _, ok := state.NamespacePaths[nsType]; !ok {
				ns := configs.Namespace{Type: nsType}
				state.NamespacePaths[ns.Type] = ns.GetPath(pid)
			}
		}
	}
	return state, nil
}

func (c *linuxContainer) currentOCIState() (*specs.State, error) {
	bundle, annotations := utils.Annotations(c.config.Labels)
	state := &specs.State{
		Version:     specs.Version,
		ID:          c.ID(),
		Bundle:      bundle,
		Annotations: annotations,
	}
	status, err := c.currentStatus()
	if err != nil {
		return nil, err
	}
	state.Status = status.String()
	if status != Stopped {
		if c.initProcess != nil {
			state.Pid = c.initProcess.pid()
		}
	}
	return state, nil
}

// orderNamespacePaths sorts namespace paths into a list of paths that we
// can setns in order.
func (c *linuxContainer) orderNamespacePaths(namespaces map[configs.NamespaceType]string) ([]string, error) {
	paths := []string{}
	for _, ns := range configs.NamespaceTypes() {

		// Remove namespaces that we don't need to join.
		if !c.config.Namespaces.Contains(ns) {
			continue
		}

		if p, ok := namespaces[ns]; ok && p != "" {
			// check if the requested namespace is supported
			if !configs.IsNamespaceSupported(ns) {
				return nil, newSystemError(fmt.Errorf("namespace %s is not supported", ns))
			}
			// only set to join this namespace if it exists
			if _, err := os.Lstat(p); err != nil {
				return nil, newSystemErrorWithCausef(err, "running lstat on namespace path %q", p)
			}
			// do not allow namespace path with comma as we use it to separate
			// the namespace paths
			if strings.ContainsRune(p, ',') {
				return nil, newSystemError(fmt.Errorf("invalid path %s", p))
			}
			paths = append(paths, fmt.Sprintf("%s:%s", configs.NsName(ns), p))
		}

	}

	return paths, nil
}

func encodeIDMapping(idMap []configs.IDMap) ([]byte, error) {
	data := bytes.NewBuffer(nil)
	for _, im := range idMap {
		line := fmt.Sprintf("%d %d %d\n", im.ContainerID, im.HostID, im.Size)
		if _, err := data.WriteString(line); err != nil {
			return nil, err
		}
	}
	return data.Bytes(), nil
}

// bootstrapData encodes the necessary data in netlink binary format
// as a io.Reader.
// Consumer can write the data to a bootstrap program
// such as one that uses nsenter package to bootstrap the container's
// init process correctly, i.e. with correct namespaces, uid/gid
// mapping etc.
func (c *linuxContainer) bootstrapData(cloneFlags uintptr, nsMaps map[configs.NamespaceType]string) (io.Reader, error) {
	// create the netlink message
	r := nl.NewNetlinkRequest(int(InitMsg), 0)

	// write cloneFlags
	r.AddData(&libcontainer.Int32msg{
		Type:  CloneFlagsAttr,
		Value: uint32(cloneFlags),
	})

	// write custom namespace paths
	if len(nsMaps) > 0 {
		nsPaths, err := c.orderNamespacePaths(nsMaps)
		if err != nil {
			return nil, err
		}
		r.AddData(&libcontainer.Bytemsg{
			Type:  NsPathsAttr,
			Value: []byte(strings.Join(nsPaths, ",")),
		})
	}

	// write namespace paths only when we are not joining an existing user ns
	_, joinExistingUser := nsMaps[configs.NEWUSER]
	if !joinExistingUser {
		// write uid mappings
		if len(c.config.UidMappings) > 0 {
			if c.config.RootlessEUID && c.newuidmapPath != "" {
				r.AddData(&libcontainer.Bytemsg{
					Type:  UidmapPathAttr,
					Value: []byte(c.newuidmapPath),
				})
			}
			b, err := encodeIDMapping(c.config.UidMappings)
			if err != nil {
				return nil, err
			}
			r.AddData(&libcontainer.Bytemsg{
				Type:  UidmapAttr,
				Value: b,
			})
		}

		// write gid mappings
		if len(c.config.GidMappings) > 0 {
			b, err := encodeIDMapping(c.config.GidMappings)
			if err != nil {
				return nil, err
			}
			r.AddData(&libcontainer.Bytemsg{
				Type:  GidmapAttr,
				Value: b,
			})
			if c.config.RootlessEUID && c.newgidmapPath != "" {
				r.AddData(&libcontainer.Bytemsg{
					Type:  GidmapPathAttr,
					Value: []byte(c.newgidmapPath),
				})
			}
			if requiresRootOrMappingTool(c.config) {
				r.AddData(&libcontainer.Boolmsg{
					Type:  SetgroupAttr,
					Value: true,
				})
			}
		}
	}

	if c.config.OomScoreAdj != nil {
		// write oom_score_adj
		r.AddData(&libcontainer.Bytemsg{
			Type:  OomScoreAdjAttr,
			Value: []byte(fmt.Sprintf("%d", *c.config.OomScoreAdj)),
		})
	}

	// write rootless
	r.AddData(&libcontainer.Boolmsg{
		Type:  RootlessEUIDAttr,
		Value: c.config.RootlessEUID,
	})

	return bytes.NewReader(r.Serialize()), nil
}

// ignoreTerminateErrors returns nil if the given err matches an error known
// to indicate that the terminate occurred successfully or err was nil, otherwise
// err is returned unaltered.
func ignoreTerminateErrors(err error) error {
	if err == nil {
		return nil
	}
	s := err.Error()
	switch {
	case strings.Contains(s, "process already finished"), strings.Contains(s, "Wait was already called"):
		return nil
	}
	return err
}

func requiresRootOrMappingTool(c *configs.Config) bool {
	gidMap := []configs.IDMap{
		{ContainerID: 0, HostID: os.Getegid(), Size: 1},
	}
	return !reflect.DeepEqual(c.GidMappings, gidMap)
}
