package main

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/crypto/ssh/terminal"

	"github.com/lxc/lxd/client"
	"github.com/lxc/lxd/shared"
	"github.com/lxc/lxd/shared/api"
	"github.com/lxc/lxd/shared/cmd"
	"github.com/lxc/lxd/shared/logger"
)

// CmdInitArgs holds command line arguments for the "lxd init" command.
type CmdInitArgs struct {
	Auto                bool
	StorageBackend      string
	StorageCreateDevice string
	StorageCreateLoop   int64
	StorageDataset      string
	NetworkPort         int64
	NetworkAddress      string
	TrustPassword       string
}

// CmdInit implements the "lxd init" command line.
type CmdInit struct {
	Context         *cmd.Context
	Args            *CmdInitArgs
	RunningInUserns bool
	SocketPath      string
	PasswordReader  func(int) ([]byte, error)
}

// Run triggers the execution of the init command.
func (cmd *CmdInit) Run() error {
	var defaultPrivileged int // controls whether we set security.privileged=true
	var storageSetup bool     // == supportedStoragePoolDrivers
	var storageBackend string // == supportedStoragePoolDrivers
	var storageLoopSize int64 // Size in GB
	var storageDevice string  // Path
	var storagePool string    // pool name
	var storageDataset string // existing ZFS pool name
	var networkAddress string // Address
	var networkPort int64     // Port
	var trustPassword string  // Trust password
	var imagesAutoUpdate bool // controls whether we set images.auto_update_interval to 0
	var bridgeName string     // Bridge name
	var bridgeIPv4 string     // IPv4 address
	var bridgeIPv4Nat bool    // IPv4 address
	var bridgeIPv6 string     // IPv6 address
	var bridgeIPv6Nat bool    // IPv6 address

	// Detect userns
	defaultPrivileged = -1
	runningInUserns = cmd.RunningInUserns
	imagesAutoUpdate = true

	backendsAvailable := []string{"dir"}

	// Check available backends
	for _, driver := range supportedStoragePoolDrivers {
		if driver == "dir" {
			continue
		}

		// btrfs can work in user namespaces too. (If
		// source=/some/path/on/btrfs is used.)
		if runningInUserns && driver != "btrfs" {
			continue
		}

		// Initialize a core storage interface for the given driver.
		_, err := storageCoreInit(driver)
		if err != nil {
			continue
		}

		backendsAvailable = append(backendsAvailable, driver)
	}

	// Connect to LXD
	c, err := lxd.ConnectLXDUnix(cmd.SocketPath, nil)
	if err != nil {
		return fmt.Errorf("Unable to talk to LXD: %s", err)
	}

	pools, err := c.GetStoragePoolNames()
	if err != nil {
		// We should consider this fatal since this means
		// something's wrong with the daemon.
		return err
	}

	if cmd.Args.Auto {
		if cmd.Args.StorageBackend == "" {
			cmd.Args.StorageBackend = "dir"
		}

		// Do a bunch of sanity checks
		if !shared.StringInSlice(cmd.Args.StorageBackend, supportedStoragePoolDrivers) {
			return fmt.Errorf("The requested backend '%s' isn't supported by lxd init.", cmd.Args.StorageBackend)
		}

		if !shared.StringInSlice(cmd.Args.StorageBackend, backendsAvailable) {
			return fmt.Errorf("The requested backend '%s' isn't available on your system (missing tools).", cmd.Args.StorageBackend)
		}

		if cmd.Args.StorageBackend == "dir" {
			if cmd.Args.StorageCreateLoop != -1 || cmd.Args.StorageCreateDevice != "" || cmd.Args.StorageDataset != "" {
				return fmt.Errorf("None of --storage-pool, --storage-create-device or --storage-create-loop may be used with the 'dir' backend.")
			}
		} else {
			if cmd.Args.StorageCreateLoop != -1 && cmd.Args.StorageCreateDevice != "" {
				return fmt.Errorf("Only one of --storage-create-device or --storage-create-loop can be specified.")
			}
		}

		if cmd.Args.NetworkAddress == "" {
			if cmd.Args.NetworkPort != -1 {
				return fmt.Errorf("--network-port cannot be used without --network-address.")
			}
			if cmd.Args.TrustPassword != "" {
				return fmt.Errorf("--trust-password cannot be used without --network-address.")
			}
		}

		storageBackend = cmd.Args.StorageBackend
		storageLoopSize = cmd.Args.StorageCreateLoop
		storageDevice = cmd.Args.StorageCreateDevice
		storageDataset = cmd.Args.StorageDataset
		networkAddress = cmd.Args.NetworkAddress
		networkPort = cmd.Args.NetworkPort
		trustPassword = cmd.Args.TrustPassword
		storagePool = "default"

		// FIXME: Allow to configure multiple storage pools on auto init
		// run if explicit arguments to do so are passed.
		if len(pools) == 0 {
			storageSetup = true
		}
	} else {
		if cmd.Args.StorageBackend != "" || cmd.Args.StorageCreateDevice != "" || cmd.Args.StorageCreateLoop != -1 || cmd.Args.StorageDataset != "" || cmd.Args.NetworkAddress != "" || cmd.Args.NetworkPort != -1 || cmd.Args.TrustPassword != "" {
			return fmt.Errorf("Init configuration is only valid with --auto")
		}

		defaultStorage := "dir"
		if shared.StringInSlice("zfs", backendsAvailable) {
			defaultStorage = "zfs"
		}

		// User chose an already existing storage pool name. Ask him
		// again if he still wants to create one.
	askForStorageAgain:
		storageSetup = cmd.Context.AskBool("Do you want to configure a new storage pool (yes/no) [default=yes]? ", "yes")
		if storageSetup {
			storagePool = cmd.Context.AskString("Name of the new storage pool [default=default]: ", "default", nil)
			if shared.StringInSlice(storagePool, pools) {
				fmt.Printf("The requested storage pool \"%s\" already exists. Please choose another name.\n", storagePool)
				// Ask the user again if hew wants to create a
				// storage pool.
				goto askForStorageAgain
			}

			storageBackend = cmd.Context.AskChoice(fmt.Sprintf("Name of the storage backend to use (%s) [default=%s]: ", strings.Join(backendsAvailable, ", "), defaultStorage), supportedStoragePoolDrivers, defaultStorage)

			if !shared.StringInSlice(storageBackend, supportedStoragePoolDrivers) {
				return fmt.Errorf("The requested backend '%s' isn't supported by lxd init.", storageBackend)
			}

			if !shared.StringInSlice(storageBackend, backendsAvailable) {
				return fmt.Errorf("The requested backend '%s' isn't available on your system (missing tools).", storageBackend)
			}
		}

		if storageSetup && storageBackend != "dir" {
			storageLoopSize = -1
			q := fmt.Sprintf("Create a new %s pool (yes/no) [default=yes]? ", strings.ToUpper(storageBackend))
			if cmd.Context.AskBool(q, "yes") {
				if cmd.Context.AskBool("Would you like to use an existing block device (yes/no) [default=no]? ", "no") {
					deviceExists := func(path string) error {
						if !shared.IsBlockdevPath(path) {
							return fmt.Errorf("'%s' is not a block device", path)
						}
						return nil
					}
					storageDevice = cmd.Context.AskString("Path to the existing block device: ", "", deviceExists)
				} else {
					backingFs, err := filesystemDetect(shared.VarPath())
					if err == nil && storageBackend == "btrfs" && backingFs == "btrfs" {
						if cmd.Context.AskBool("Would you like to create a new subvolume for the BTRFS storage pool (yes/no) [default=yes]: ", "yes") {
							storageDataset = shared.VarPath("storage-pools", storagePool)
						}
					} else {

						st := syscall.Statfs_t{}
						err := syscall.Statfs(shared.VarPath(), &st)
						if err != nil {
							return fmt.Errorf("couldn't statfs %s: %s", shared.VarPath(), err)
						}

						/* choose 15 GB < x < 100GB, where x is 20% of the disk size */
						def := uint64(st.Frsize) * st.Blocks / (1024 * 1024 * 1024) / 5
						if def > 100 {
							def = 100
						}
						if def < 15 {
							def = 15
						}

						q := fmt.Sprintf("Size in GB of the new loop device (1GB minimum) [default=%dGB]: ", def)
						storageLoopSize = cmd.Context.AskInt(q, 1, -1, fmt.Sprintf("%d", def))
					}
				}
			} else {
				q := fmt.Sprintf("Name of the existing %s pool or dataset: ", strings.ToUpper(storageBackend))
				storageDataset = cmd.Context.AskString(q, "", nil)
			}
		}

		// Detect lack of uid/gid
		needPrivileged := false
		idmapset, err := shared.DefaultIdmapSet()
		if err != nil || len(idmapset.Idmap) == 0 || idmapset.Usable() != nil {
			needPrivileged = true
		}

		if runningInUserns && needPrivileged {
			fmt.Printf(`
We detected that you are running inside an unprivileged container.
This means that unless you manually configured your host otherwise,
you will not have enough uid and gid to allocate to your containers.

LXD can re-use your container's own allocation to avoid the problem.
Doing so makes your nested containers slightly less safe as they could
in theory attack their parent container and gain more privileges than
they otherwise would.

`)
			if cmd.Context.AskBool("Would you like to have your containers share their parent's allocation (yes/no) [default=yes]? ", "yes") {
				defaultPrivileged = 1
			} else {
				defaultPrivileged = 0
			}
		}

		if cmd.Context.AskBool("Would you like LXD to be available over the network (yes/no) [default=no]? ", "no") {
			isIPAddress := func(s string) error {
				if s != "all" && net.ParseIP(s) == nil {
					return fmt.Errorf("'%s' is not an IP address", s)
				}
				return nil
			}

			networkAddress = cmd.Context.AskString("Address to bind LXD to (not including port) [default=all]: ", "all", isIPAddress)
			if networkAddress == "all" {
				networkAddress = "::"
			}

			if net.ParseIP(networkAddress).To4() == nil {
				networkAddress = fmt.Sprintf("[%s]", networkAddress)
			}
			networkPort = cmd.Context.AskInt("Port to bind LXD to [default=8443]: ", 1, 65535, "8443")
			trustPassword = cmd.Context.AskPassword("Trust password for new clients: ", cmd.PasswordReader)
		}

		if !cmd.Context.AskBool("Would you like stale cached images to be updated automatically (yes/no) [default=yes]? ", "yes") {
			imagesAutoUpdate = false
		}

	askForNetworkAgain:
		bridgeName = ""
		if cmd.Context.AskBool("Would you like to create a new network bridge (yes/no) [default=yes]? ", "yes") {
			bridgeName = cmd.Context.AskString("What should the new bridge be called [default=lxdbr0]? ", "lxdbr0", networkValidName)
			_, _, err := c.GetNetwork(bridgeName)
			if err == nil {
				fmt.Printf("The requested network bridge \"%s\" already exists. Please choose another name.\n", bridgeName)
				// Ask the user again if hew wants to create a
				// storage pool.
				goto askForNetworkAgain
			}

			bridgeIPv4 = cmd.Context.AskString("What IPv4 address should be used (CIDR subnet notation, “auto” or “none”) [default=auto]? ", "auto", func(value string) error {
				if shared.StringInSlice(value, []string{"auto", "none"}) {
					return nil
				}
				return networkValidAddressCIDRV4(value)
			})

			if !shared.StringInSlice(bridgeIPv4, []string{"auto", "none"}) {
				bridgeIPv4Nat = cmd.Context.AskBool("Would you like LXD to NAT IPv4 traffic on your bridge? [default=yes]? ", "yes")
			}

			bridgeIPv6 = cmd.Context.AskString("What IPv6 address should be used (CIDR subnet notation, “auto” or “none”) [default=auto]? ", "auto", func(value string) error {
				if shared.StringInSlice(value, []string{"auto", "none"}) {
					return nil
				}
				return networkValidAddressCIDRV6(value)
			})

			if !shared.StringInSlice(bridgeIPv6, []string{"auto", "none"}) {
				bridgeIPv6Nat = cmd.Context.AskBool("Would you like LXD to NAT IPv6 traffic on your bridge? [default=yes]? ", "yes")
			}
		}
	}

	if storageSetup {
		// Unset core.https_address and core.trust_password
		for _, key := range []string{"core.https_address", "core.trust_password"} {
			err = cmd.setServerConfig(c, key, "")
			if err != nil {
				return err
			}
		}

		// Pool configuration
		storagePoolConfig := map[string]string{}

		if storageDevice != "" {
			storagePoolConfig["source"] = storageDevice
			if storageDataset != "" {
				storagePool = storageDataset
			}
		} else if storageLoopSize != -1 {
			if storageDataset != "" {
				storagePool = storageDataset
			}
		} else {
			storagePoolConfig["source"] = storageDataset
		}

		if storageLoopSize > 0 {
			storagePoolConfig["size"] = strconv.FormatInt(storageLoopSize, 10) + "GB"
		}

		// Create the requested storage pool.
		storageStruct := api.StoragePoolsPost{
			Name:   storagePool,
			Driver: storageBackend,
		}
		storageStruct.Config = storagePoolConfig

		err := c.CreateStoragePool(storageStruct)
		if err != nil {
			return err
		}

		// When lxd init is rerun and there are already storage pools
		// configured, do not try to set a root disk device in the
		// default profile again. Let the user figure this out.
		if len(pools) == 0 {
			// Check if there even is a default profile.
			profiles, err := c.GetProfiles()
			if err != nil {
				return err
			}

			defaultProfileExists := false
			for _, p := range profiles {
				if p.Name != "default" {
					continue
				}

				defaultProfileExists = true

				foundRootDiskDevice := false
				for k, v := range p.Devices {
					if v["path"] == "/" && v["source"] == "" {
						foundRootDiskDevice = true

						// Unconditionally overwrite because if the user ends up
						// with a clean LXD but with a pool property key existing in
						// the default profile it must be empty otherwise it would
						// not have been possible to delete the storage pool in
						// the first place.
						update := p.Writable()
						update.Devices[k]["pool"] = storagePool

						// Update profile devices.
						err := c.UpdateProfile("default", update, "")
						if err != nil {
							return err
						}
						logger.Debugf("Set pool property of existing root disk device \"%s\" in profile \"default\" to \"%s\".", storagePool)

						break
					}
				}

				if foundRootDiskDevice {
					break
				}

				props := map[string]string{
					"type": "disk",
					"path": "/",
					"pool": storagePool,
				}

				err = cmd.profileDeviceAdd(c, "default", "root", props)
				if err != nil {
					return err
				}

				break
			}

			if !defaultProfileExists {
				logger.Warnf("Did not find profile \"default\" so no default storage pool will be set. Manual intervention needed.")
			}
		}
	}

	if defaultPrivileged == 0 {
		err = cmd.setProfileConfigItem(c, "default", "security.privileged", "")
		if err != nil {
			return err
		}
	} else if defaultPrivileged == 1 {
		err = cmd.setProfileConfigItem(c, "default", "security.privileged", "true")
		if err != nil {
		}
	}

	if imagesAutoUpdate {
		ss, _, err := c.GetServer()
		if err != nil {
			return err
		}

		if val, ok := ss.Config["images.auto_update_interval"]; ok && val == "0" {
			err = cmd.setServerConfig(c, "images.auto_update_interval", "")
			if err != nil {
				return err
			}
		}
	} else {
		err = cmd.setServerConfig(c, "images.auto_update_interval", "0")
		if err != nil {
			return err
		}
	}

	if networkAddress != "" {
		err = cmd.setServerConfig(c, "core.https_address", fmt.Sprintf("%s:%d", networkAddress, networkPort))
		if err != nil {
			return err
		}

		if trustPassword != "" {
			err = cmd.setServerConfig(c, "core.trust_password", trustPassword)
			if err != nil {
				return err
			}
		}
	}

	if bridgeName != "" {
		bridgeConfig := map[string]string{}
		bridgeConfig["ipv4.address"] = bridgeIPv4
		bridgeConfig["ipv6.address"] = bridgeIPv6

		if bridgeIPv4Nat {
			bridgeConfig["ipv4.nat"] = "true"
		}

		if bridgeIPv6Nat {
			bridgeConfig["ipv6.nat"] = "true"
		}

		network := api.NetworksPost{
			Name: bridgeName}
		network.Config = bridgeConfig

		err = c.CreateNetwork(network)
		if err != nil {
			return err
		}

		props := map[string]string{
			"type":    "nic",
			"nictype": "bridged",
			"parent":  bridgeName,
		}

		err = cmd.profileDeviceAdd(c, "default", "eth0", props)
		if err != nil {
			return err
		}
	}

	fmt.Printf("LXD has been successfully configured.\n")
	return nil
}

func (cmd *CmdInit) setServerConfig(c lxd.ContainerServer, key string, value string) error {
	server, etag, err := c.GetServer()
	if err != nil {
		return err
	}

	if server.Config == nil {
		server.Config = map[string]interface{}{}
	}

	server.Config[key] = value

	err = c.UpdateServer(server.Writable(), etag)
	if err != nil {
		return err
	}

	return nil
}

func (cmd *CmdInit) profileDeviceAdd(c lxd.ContainerServer, profileName string, deviceName string, deviceConfig map[string]string) error {
	profile, etag, err := c.GetProfile(profileName)
	if err != nil {
		return err
	}

	if profile.Devices == nil {
		profile.Devices = map[string]map[string]string{}
	}

	_, ok := profile.Devices[deviceName]
	if ok {
		return fmt.Errorf("Device already exists: %s", deviceName)
	}

	profile.Devices[deviceName] = deviceConfig

	err = c.UpdateProfile(profileName, profile.Writable(), etag)
	if err != nil {
		return err
	}

	return nil
}

func (cmd *CmdInit) setProfileConfigItem(c lxd.ContainerServer, profileName string, key string, value string) error {
	profile, etag, err := c.GetProfile(profileName)
	if err != nil {
		return err
	}

	if profile.Config == nil {
		profile.Config = map[string]string{}
	}

	profile.Config[key] = value

	err = c.UpdateProfile(profileName, profile.Writable(), etag)
	if err != nil {
		return err
	}

	return nil
}

func cmdInit() error {
	context := cmd.NewContext(os.Stdin, os.Stdout, os.Stderr)
	args := &CmdInitArgs{
		Auto:                *argAuto,
		StorageBackend:      *argStorageBackend,
		StorageCreateDevice: *argStorageCreateDevice,
		StorageCreateLoop:   *argStorageCreateLoop,
		StorageDataset:      *argStorageDataset,
		NetworkPort:         *argNetworkPort,
		NetworkAddress:      *argNetworkAddress,
		TrustPassword:       *argTrustPassword,
	}
	command := &CmdInit{
		Context:         context,
		Args:            args,
		RunningInUserns: shared.RunningInUserNS(),
		SocketPath:      "",
		PasswordReader:  terminal.ReadPassword,
	}
	return command.Run()
}
