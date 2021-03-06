// +build linux

package oci

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/containerd/cgroups"
	"github.com/cri-o/cri-o/utils"
	"github.com/opencontainers/runc/libcontainer"
	rspec "github.com/opencontainers/runtime-spec/specs-go"
	"github.com/sirupsen/logrus"
	"golang.org/x/sys/unix"
)

func createUnitName(prefix, name string) string {
	return fmt.Sprintf("%s-%s.scope", prefix, name)
}

func createConmonUnitName(name string) string {
	return createUnitName("crio-conmon", name)
}

func (r *runtimeOCI) createContainerPlatform(c *Container, cgroupParent string, pid int) {
	// Move conmon to specified cgroup
	if r.conmonCgroup == "pod" || r.conmonCgroup == "" {
		if r.cgroupManager == SystemdCgroupsManager {
			logrus.Debugf("Running conmon under slice %s and unitName %s", cgroupParent, createConmonUnitName(c.id))
			if err := utils.RunUnderSystemdScope(pid, cgroupParent, createConmonUnitName(c.id)); err != nil {
				logrus.Warnf("Failed to add conmon to systemd sandbox cgroup: %v", err)
			}
		}

		control, err := cgroups.New(cgroups.V1, cgroups.StaticPath(filepath.Join(cgroupParent, "/crio-conmon-"+c.id)), &rspec.LinuxResources{})
		if err != nil {
			logrus.Warnf("Failed to add conmon to cgroupfs sandbox cgroup: %v", err)
		}

		if control != nil {
			// Here we should defer a crio-connmon- cgroup hierarchy deletion, but it will
			// always fail as conmon's pid is still there.
			// Fortunately, kubelet takes care of deleting this for us, so the leak will
			// only happens in corner case where one does a manual deletion of the container
			// through e.g. runc. This should be handled by implementing a conmon monitoring
			// routine that does the cgroup cleanup once conmon is terminated.
			if err := control.Add(cgroups.Process{Pid: pid}); err != nil {
				logrus.Warnf("Failed to add conmon to cgroupfs sandbox cgroup: %v", err)
			}
		}
	} else if strings.HasSuffix(r.conmonCgroup, ".slice") {
		logrus.Debugf("Running conmon under custom slice %s and unitName %s", r.conmonCgroup, createConmonUnitName(c.id))
		if err := utils.RunUnderSystemdScope(pid, r.conmonCgroup, createConmonUnitName(c.id)); err != nil {
			logrus.Warnf("Failed to add conmon to custom systemd sandbox cgroup: %v", err)
		}
	}
}

func sysProcAttrPlatform() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{
		Setpgid: true,
	}
}

// newPipe creates a unix socket pair for communication
func newPipe() (parent, child *os.File, err error) {
	fds, err := unix.Socketpair(unix.AF_LOCAL, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, nil, err
	}
	return os.NewFile(uintptr(fds[1]), "parent"), os.NewFile(uintptr(fds[0]), "child"), nil
}

func loadFactory(root string) (libcontainer.Factory, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	cgroupManager := libcontainer.Cgroupfs
	return libcontainer.New(abs, cgroupManager, libcontainer.CriuPath(""))
}

// libcontainerStats gets the stats for the container with the given id from runc/libcontainer
func (r *runtimeOCI) libcontainerStats(ctr *Container) (*libcontainer.Stats, error) {
	factory, err := loadFactory(r.root)
	if err != nil {
		return nil, err
	}
	container, err := factory.Load(ctr.ID())
	if err != nil {
		return nil, err
	}
	return container.Stats()
}

func (r *runtimeOCI) containerStats(ctr *Container) (*ContainerStats, error) {
	libcontainerStats, err := r.libcontainerStats(ctr)
	if err != nil {
		return nil, err
	}
	cgroupStats := libcontainerStats.CgroupStats
	stats := new(ContainerStats)
	stats.Container = ctr.ID()
	stats.CPUNano = cgroupStats.CpuStats.CpuUsage.TotalUsage
	stats.SystemNano = time.Now().UnixNano()
	stats.CPU = calculateCPUPercent(libcontainerStats)
	stats.MemUsage = cgroupStats.MemoryStats.Usage.Usage
	stats.MemLimit = getMemLimit(cgroupStats.MemoryStats.Usage.Limit)
	stats.MemPerc = float64(stats.MemUsage) / float64(stats.MemLimit)
	stats.PIDs = cgroupStats.PidsStats.Current
	stats.BlockInput, stats.BlockOutput = calculateBlockIO(libcontainerStats)
	stats.NetInput, stats.NetOutput = getContainerNetIO(libcontainerStats)

	return stats, nil
}

func metricsToCtrStats(c *Container, m *cgroups.Metrics) *ContainerStats {
	var (
		cpu         float64
		cpuNano     uint64
		memUsage    uint64
		memLimit    uint64
		memPerc     float64
		netInput    uint64
		netOutput   uint64
		blockInput  uint64
		blockOutput uint64
		pids        uint64
	)

	if m != nil {
		if m.Pids != nil {
			pids = m.Pids.Current
		}

		if m.CPU != nil {
			if m.CPU.Usage != nil {
				cpuNano = m.CPU.Usage.Total
				cpu = genericCalculateCPUPercent(cpuNano, m.CPU.Usage.PerCPU)
			}
		}

		if m.Memory != nil {
			if m.Memory.Usage != nil {
				memUsage = m.Memory.Usage.Usage
				memLimit = getMemLimit(m.Memory.Usage.Limit)
				memPerc = float64(memUsage) / float64(memLimit)
			}
		}

		if m.Blkio != nil {
			for _, entry := range m.Blkio.IoServiceBytesRecursive {
				if entry == nil {
					continue
				}
				switch strings.ToLower(entry.Op) {
				case "read":
					blockInput += entry.Value
				case "write":
					blockOutput += entry.Value
				}
			}
		}
	}

	return &ContainerStats{
		Container:   c.ID(),
		CPU:         cpu,
		CPUNano:     cpuNano,
		SystemNano:  time.Now().UnixNano(),
		MemUsage:    memUsage,
		MemLimit:    memLimit,
		MemPerc:     memPerc,
		NetInput:    netInput,
		NetOutput:   netOutput,
		BlockInput:  blockInput,
		BlockOutput: blockOutput,
		PIDs:        pids,
	}
}
