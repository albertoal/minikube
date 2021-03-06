/*
Copyright 2016 The Kubernetes Authors All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package cluster

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"html/template"
	"net"
	"os/exec"
	"regexp"
	"time"

	"github.com/docker/machine/libmachine"
	"github.com/docker/machine/libmachine/engine"
	"github.com/docker/machine/libmachine/host"
	"github.com/docker/machine/libmachine/mcnerror"
	"github.com/docker/machine/libmachine/provision"
	"github.com/docker/machine/libmachine/ssh"
	"github.com/docker/machine/libmachine/state"
	"github.com/golang/glog"
	"github.com/pkg/errors"
	"github.com/spf13/viper"
	cfg "k8s.io/minikube/pkg/minikube/config"
	"k8s.io/minikube/pkg/minikube/console"
	"k8s.io/minikube/pkg/minikube/constants"
	"k8s.io/minikube/pkg/minikube/exit"
	"k8s.io/minikube/pkg/minikube/registry"
	"k8s.io/minikube/pkg/util"
	pkgutil "k8s.io/minikube/pkg/util"
)

const (
	defaultVirtualboxNicType = "virtio"
)

//This init function is used to set the logtostderr variable to false so that INFO level log info does not clutter the CLI
//INFO lvl logging is displayed due to the kubernetes api calling flag.Set("logtostderr", "true") in its init()
//see: https://github.com/kubernetes/kubernetes/blob/master/pkg/kubectl/util/logs/logs.go#L32-L34
func init() {
	flag.Set("logtostderr", "false")

	// Setting the default client to native gives much better performance.
	ssh.SetDefaultClient(ssh.Native)
}

// StartHost starts a host VM.
func StartHost(api libmachine.API, config cfg.MachineConfig) (*host.Host, error) {
	exists, err := api.Exists(cfg.GetMachineName())
	if err != nil {
		return nil, errors.Wrapf(err, "machine name: %s", cfg.GetMachineName())
	}
	if !exists {
		glog.Infoln("Machine does not exist... provisioning new machine")
		glog.Infof("Provisioning machine with config: %+v", config)
		return createHost(api, config)
	}

	glog.Infoln("Skipping create...Using existing machine configuration")

	h, err := api.Load(cfg.GetMachineName())
	if err != nil {
		return nil, errors.Wrap(err, "Error loading existing host. Please try running [minikube delete], then run [minikube start] again.")
	}

	if h.Driver.DriverName() != config.VMDriver {
		console.Out("\n")
		console.Warning("Ignoring --vm-driver=%s, as the existing %q VM was created using the %s driver.",
			config.VMDriver, cfg.GetMachineName(), h.Driver.DriverName())
		console.Warning("To switch drivers, you may create a new VM using `minikube start -p <name> --vm-driver=%s`", config.VMDriver)
		console.Warning("Alternatively, you may delete the existing VM using `minikube delete -p %s`", cfg.GetMachineName())
		console.Out("\n")
	} else if exists && cfg.GetMachineName() == constants.DefaultMachineName {
		console.OutStyle("tip", "Tip: To create a new cluster, use 'minikube start -p <new name>' or use 'minikube delete' to delete this one.")
	}

	s, err := h.Driver.GetState()
	glog.Infoln("Machine state: ", s)
	if err != nil {
		return nil, errors.Wrap(err, "Error getting state for host")
	}

	if s == state.Running {
		console.OutStyle("running", "Re-using the currently running %s VM for %q ...", h.Driver.DriverName(), cfg.GetMachineName())
	} else {
		console.OutStyle("restarting", "Restarting existing %s VM for %q ...", h.Driver.DriverName(), cfg.GetMachineName())
		if err := h.Driver.Start(); err != nil {
			return nil, errors.Wrap(err, "start")
		}
		if err := api.Save(h); err != nil {
			return nil, errors.Wrap(err, "save")
		}
	}

	e := engineOptions(config)
	glog.Infof("engine options: %+v", e)

	// Slightly counter-intuitive, but this is what DetectProvisioner & ConfigureAuth block on.
	console.OutStyle("waiting", "Waiting for SSH access ...")

	if len(e.Env) > 0 {
		h.HostOptions.EngineOptions.Env = e.Env
		provisioner, err := provision.DetectProvisioner(h.Driver)
		if err != nil {
			return nil, errors.Wrap(err, "detecting provisioner")
		}
		if err := provisioner.Provision(*h.HostOptions.SwarmOptions, *h.HostOptions.AuthOptions, *h.HostOptions.EngineOptions); err != nil {
			return nil, errors.Wrap(err, "provision")
		}
	}

	if h.Driver.DriverName() != "none" {
		if err := h.ConfigureAuth(); err != nil {
			return nil, &util.RetriableError{Err: errors.Wrap(err, "Error configuring auth on host")}
		}
	}
	return h, nil
}

// tryPowerOff runs the poweroff command on the guest VM to speed up deletion
func tryPowerOff(h *host.Host) {
	if h.Driver.DriverName() == "none" {
		return
	}
	s, err := h.Driver.GetState()
	if err != nil {
		glog.Warningf("unable to get state: %v", err)
		return
	}
	if s != state.Running {
		glog.Infof("host is in state %s", s)
		return
	}

	console.OutStyle("shutdown", "Powering off %q via SSH ...", cfg.GetMachineName())
	out, err := h.RunSSHCommand("sudo poweroff")
	// poweroff always results in an error, since the host disconnects.
	glog.Infof("poweroff result: out=%s, err=%v", out, err)
}

// StopHost stops the host VM, saving state to disk.
func StopHost(api libmachine.API) error {
	host, err := api.Load(cfg.GetMachineName())
	if err != nil {
		return errors.Wrapf(err, "load")
	}
	console.OutStyle("stopping", "Stopping %q in %s ...", cfg.GetMachineName(), host.DriverName)
	if err := host.Stop(); err != nil {
		alreadyInStateError, ok := err.(mcnerror.ErrHostAlreadyInState)
		if ok && alreadyInStateError.State == state.Stopped {
			return nil
		}
		return &util.RetriableError{Err: errors.Wrapf(err, "Stop: %s", cfg.GetMachineName())}
	}
	return nil
}

// DeleteHost deletes the host VM.
func DeleteHost(api libmachine.API) error {
	host, err := api.Load(cfg.GetMachineName())
	if err != nil {
		return errors.Wrap(err, "load")
	}
	tryPowerOff(host)
	console.OutStyle("deleting-host", "Deleting %q from %s ...", cfg.GetMachineName(), host.DriverName)
	if err := host.Driver.Remove(); err != nil {
		return errors.Wrap(err, "host remove")
	}
	if err := api.Remove(cfg.GetMachineName()); err != nil {
		return errors.Wrap(err, "api remove")
	}
	return nil
}

// GetHostStatus gets the status of the host VM.
func GetHostStatus(api libmachine.API) (string, error) {
	exists, err := api.Exists(cfg.GetMachineName())
	if err != nil {
		return "", errors.Wrapf(err, "%s exists", cfg.GetMachineName())
	}
	if !exists {
		return state.None.String(), nil
	}

	host, err := api.Load(cfg.GetMachineName())
	if err != nil {
		return "", errors.Wrapf(err, "load")
	}

	s, err := host.Driver.GetState()
	if err != nil {
		return "", errors.Wrap(err, "state")
	}
	return s.String(), nil
}

// GetHostDriverIP gets the ip address of the current minikube cluster
func GetHostDriverIP(api libmachine.API, machineName string) (net.IP, error) {
	host, err := CheckIfHostExistsAndLoad(api, machineName)
	if err != nil {
		return nil, err
	}

	ipStr, err := host.Driver.GetIP()
	if err != nil {
		return nil, errors.Wrap(err, "getting IP")
	}
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return nil, fmt.Errorf("parsing IP: %s", ipStr)
	}
	return ip, nil
}

func engineOptions(config cfg.MachineConfig) *engine.Options {
	o := engine.Options{
		Env:              config.DockerEnv,
		InsecureRegistry: append([]string{pkgutil.DefaultServiceCIDR}, config.InsecureRegistry...),
		RegistryMirror:   config.RegistryMirror,
		ArbitraryFlags:   config.DockerOpt,
	}
	return &o
}

func preCreateHost(config *cfg.MachineConfig) error {
	switch config.VMDriver {
	case "kvm":
		if viper.GetBool(cfg.ShowDriverDeprecationNotification) {
			console.Warning(`The kvm driver is deprecated and support for it will be removed in a future release.
				Please consider switching to the kvm2 driver, which is intended to replace the kvm driver.
				See https://github.com/kubernetes/minikube/blob/master/docs/drivers.md#kvm2-driver for more information.
				To disable this message, run [minikube config set WantShowDriverDeprecationNotification false]`)
		}
	case "xhyve":
		if viper.GetBool(cfg.ShowDriverDeprecationNotification) {
			console.Warning(`The xhyve driver is deprecated and support for it will be removed in a future release.
Please consider switching to the hyperkit driver, which is intended to replace the xhyve driver.
See https://github.com/kubernetes/minikube/blob/master/docs/drivers.md#hyperkit-driver for more information.
To disable this message, run [minikube config set WantShowDriverDeprecationNotification false]`)
		}
	case "vmwarefusion":
		if viper.GetBool(cfg.ShowDriverDeprecationNotification) {
			console.Warning(`The vmwarefusion driver is deprecated and support for it will be removed in a future release.
				Please consider switching to the new vmware unified driver, which is intended to replace the vmwarefusion driver.
				See https://github.com/kubernetes/minikube/blob/master/docs/drivers.md#vmware-unified-driver for more information.
				To disable this message, run [minikube config set WantShowDriverDeprecationNotification false]`)
		}
	}

	return nil
}

func createHost(api libmachine.API, config cfg.MachineConfig) (*host.Host, error) {
	err := preCreateHost(&config)
	if err != nil {
		return nil, err
	}

	console.OutStyle("starting-vm", "Creating %s VM (CPUs=%d, Memory=%dMB, Disk=%dMB) ...", config.VMDriver, config.CPUs, config.Memory, config.DiskSize)
	def, err := registry.Driver(config.VMDriver)
	if err != nil {
		if err == registry.ErrDriverNotFound {
			exit.Usage("unsupported driver: %s", config.VMDriver)
		}
		exit.WithError("error getting driver", err)
	}

	if config.VMDriver != "none" {
		if err := config.Downloader.CacheMinikubeISOFromURL(config.MinikubeISO); err != nil {
			return nil, errors.Wrap(err, "unable to cache ISO")
		}
	}

	driver := def.ConfigCreator(config)

	data, err := json.Marshal(driver)
	if err != nil {
		return nil, errors.Wrap(err, "marshal")
	}

	h, err := api.NewHost(config.VMDriver, data)
	if err != nil {
		return nil, errors.Wrap(err, "new host")
	}

	h.HostOptions.AuthOptions.CertDir = constants.GetMinipath()
	h.HostOptions.AuthOptions.StorePath = constants.GetMinipath()
	h.HostOptions.EngineOptions = engineOptions(config)

	if err := api.Create(h); err != nil {
		// Wait for all the logs to reach the client
		time.Sleep(2 * time.Second)
		return nil, errors.Wrap(err, "create")
	}

	if err := api.Save(h); err != nil {
		return nil, errors.Wrap(err, "save")
	}
	return h, nil
}

// GetHostDockerEnv gets the necessary docker env variables to allow the use of docker through minikube's vm
func GetHostDockerEnv(api libmachine.API) (map[string]string, error) {
	host, err := CheckIfHostExistsAndLoad(api, cfg.GetMachineName())
	if err != nil {
		return nil, errors.Wrap(err, "Error checking that api exists and loading it")
	}
	ip, err := host.Driver.GetIP()
	if err != nil {
		return nil, errors.Wrap(err, "Error getting ip from host")
	}

	tcpPrefix := "tcp://"
	port := "2376"

	envMap := map[string]string{
		"DOCKER_TLS_VERIFY": "1",
		"DOCKER_HOST":       tcpPrefix + net.JoinHostPort(ip, port),
		"DOCKER_CERT_PATH":  constants.MakeMiniPath("certs"),
	}
	return envMap, nil
}

// MountHost runs the mount command from the 9p client on the VM to the 9p server on the host
func MountHost(api libmachine.API, ip net.IP, path, port, mountVersion string, uid, gid, msize int) error {
	host, err := CheckIfHostExistsAndLoad(api, cfg.GetMachineName())
	if err != nil {
		return errors.Wrap(err, "Error checking that api exists and loading it")
	}
	if ip == nil {
		ip, err = GetVMHostIP(host)
		if err != nil {
			return errors.Wrap(err, "Error getting the host IP address to use from within the VM")
		}
	}
	host.RunSSHCommand(GetMountCleanupCommand(path))
	mountCmd, err := GetMountCommand(ip, path, port, mountVersion, uid, gid, msize)
	if err != nil {
		return errors.Wrap(err, "mount command")
	}
	_, err = host.RunSSHCommand(mountCmd)
	if err != nil {
		return errors.Wrap(err, "running mount")
	}
	return nil
}

// GetVMHostIP gets the ip address to be used for mapping host -> VM and VM -> host
func GetVMHostIP(host *host.Host) (net.IP, error) {
	switch host.DriverName {
	case "kvm":
		return net.ParseIP("192.168.42.1"), nil
	case "kvm2":
		return net.ParseIP("192.168.39.1"), nil
	case "hyperv":
		re := regexp.MustCompile(`"VSwitch": "(.*?)",`)
		// TODO(aprindle) Change this to deserialize the driver instead
		hypervVirtualSwitch := re.FindStringSubmatch(string(host.RawDriver))[1]
		ip, err := getIPForInterface(fmt.Sprintf("vEthernet (%s)", hypervVirtualSwitch))
		if err != nil {
			return []byte{}, errors.Wrap(err, fmt.Sprintf("ip for interface (%s)", hypervVirtualSwitch))
		}
		return ip, nil
	case "virtualbox":
		out, err := exec.Command(detectVBoxManageCmd(), "showvminfo", host.Name, "--machinereadable").Output()
		if err != nil {
			return []byte{}, errors.Wrap(err, "vboxmanage")
		}
		re := regexp.MustCompile(`hostonlyadapter2="(.*?)"`)
		iface := re.FindStringSubmatch(string(out))[1]
		ip, err := getIPForInterface(iface)
		if err != nil {
			return []byte{}, errors.Wrap(err, "Error getting VM/Host IP address")
		}
		return ip, nil
	case "xhyve", "hyperkit":
		return net.ParseIP("192.168.64.1"), nil
	default:
		return []byte{}, errors.New("Error, attempted to get host ip address for unsupported driver")
	}
}

// Based on code from http://stackoverflow.com/questions/23529663/how-to-get-all-addresses-and-masks-from-local-interfaces-in-go
func getIPForInterface(name string) (net.IP, error) {
	i, _ := net.InterfaceByName(name)
	addrs, _ := i.Addrs()
	for _, a := range addrs {
		if ipnet, ok := a.(*net.IPNet); ok {
			if ip := ipnet.IP.To4(); ip != nil {
				return ip, nil
			}
		}
	}
	return nil, errors.Errorf("Error finding IPV4 address for %s", name)
}

func CheckIfHostExistsAndLoad(api libmachine.API, machineName string) (*host.Host, error) {
	exists, err := api.Exists(machineName)
	if err != nil {
		return nil, errors.Wrapf(err, "Error checking that machine exists: %s", machineName)
	}
	if !exists {
		return nil, errors.Errorf("Machine does not exist for api.Exists(%s)", machineName)
	}

	host, err := api.Load(machineName)
	if err != nil {
		return nil, errors.Wrapf(err, "Error loading store for: %s", machineName)
	}
	return host, nil
}

func CreateSSHShell(api libmachine.API, args []string) error {
	machineName := cfg.GetMachineName()
	host, err := CheckIfHostExistsAndLoad(api, machineName)
	if err != nil {
		return errors.Wrap(err, "host exists and load")
	}

	currentState, err := host.Driver.GetState()
	if err != nil {
		return errors.Wrap(err, "state")
	}

	if currentState != state.Running {
		return errors.Errorf("%q is not running", machineName)
	}

	client, err := host.CreateSSHClient()
	if err != nil {
		return errors.Wrap(err, "Creating ssh client")
	}
	return client.Shell(args...)
}

// EnsureMinikubeRunningOrExit checks that minikube has a status available and that
// the status is `Running`, otherwise it will exit
func EnsureMinikubeRunningOrExit(api libmachine.API, exitStatus int) {
	s, err := GetHostStatus(api)
	if err != nil {
		exit.WithError("Error getting machine status", err)
	}
	if s != state.Running.String() {
		exit.WithCode(exit.Unavailable, "minikube is not running, so the service cannot be accessed")
	}
}

func GetMountCleanupCommand(path string) string {
	return fmt.Sprintf("sudo umount %s;", path)
}

var mountTemplate = `
sudo mkdir -p {{.Path}} || true;
sudo mount -t 9p -o trans=tcp,port={{.Port}},dfltuid={{.UID}},dfltgid={{.GID}},version={{.Version}},msize={{.Msize}} {{.IP}} {{.Path}};
sudo chmod 775 {{.Path}} || true;`

func GetMountCommand(ip net.IP, path, port, mountVersion string, uid, gid, msize int) (string, error) {
	t := template.Must(template.New("mountCommand").Parse(mountTemplate))
	buf := bytes.Buffer{}
	data := struct {
		IP      string
		Path    string
		Port    string
		Version string
		UID     int
		GID     int
		Msize   int
	}{
		IP:      ip.String(),
		Path:    path,
		Port:    port,
		Version: mountVersion,
		UID:     uid,
		GID:     gid,
		Msize:   msize,
	}
	if err := t.Execute(&buf, data); err != nil {
		return "", err
	}
	return buf.String(), nil
}
