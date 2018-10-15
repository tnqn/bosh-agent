package net

import (
	"net"
	"strconv"

	boshsettings "github.com/cloudfoundry/bosh-agent/settings"
	bosherr "github.com/cloudfoundry/bosh-utils/errors"
	boshlog "github.com/cloudfoundry/bosh-utils/logger"
	boshsys "github.com/cloudfoundry/bosh-utils/system"
)

type StaticInterfaceConfiguration struct {
	Name                string
	Address             string
	Netmask             string
	Network             string
	Broadcast           string
	IsDefaultForGateway bool
	Mac                 string
	Gateway             string
	PostUpRoutes        boshsettings.Routes
}

func (c StaticInterfaceConfiguration) Version6() string {
	if c.IsVersion6() {
		return "6"
	}
	return ""
}

func (c StaticInterfaceConfiguration) IsVersion6() bool {
	return len(c.Network) == 0 && len(c.Broadcast) == 0
}

func (c StaticInterfaceConfiguration) NetmaskOrLen() string {
	if c.IsVersion6() {
		ones, _ := net.IPMask(net.ParseIP(c.Netmask)).Size()
		return strconv.Itoa(ones)
	}
	return c.Netmask
}

type StaticInterfaceConfigurations []StaticInterfaceConfiguration

func (configs StaticInterfaceConfigurations) Len() int {
	return len(configs)
}

func (configs StaticInterfaceConfigurations) Less(i, j int) bool {
	return configs[i].Name < configs[j].Name
}

func (configs StaticInterfaceConfigurations) Swap(i, j int) {
	configs[i], configs[j] = configs[j], configs[i]
}

func (configs StaticInterfaceConfigurations) HasVersion6() bool {
	for _, config := range configs {
		if config.IsVersion6() {
			return true
		}
	}
	return false
}

type DHCPInterfaceConfiguration struct {
	Name         string
	PostUpRoutes boshsettings.Routes
	Address      string
}

func (c DHCPInterfaceConfiguration) Version6() string {
	if c.IsVersion6() {
		return "6"
	}
	return ""
}

func (c DHCPInterfaceConfiguration) IsVersion6() bool {
	ip := net.ParseIP(c.Address)
	if ip == nil || ip.To4() != nil {
		return false
	}
	return true
}

type DHCPInterfaceConfigurations []DHCPInterfaceConfiguration

func (configs DHCPInterfaceConfigurations) Len() int {
	return len(configs)
}

func (configs DHCPInterfaceConfigurations) Less(i, j int) bool {
	return configs[i].Name < configs[j].Name
}

func (configs DHCPInterfaceConfigurations) Swap(i, j int) {
	configs[i], configs[j] = configs[j], configs[i]
}

func (configs DHCPInterfaceConfigurations) HasVersion6() bool {
	for _, config := range configs {
		if len(config.Version6()) > 0 {
			return true
		}
	}
	return false
}

type InterfaceConfigurationCreator interface {
	CreateInterfaceConfigurations(boshsettings.Networks, map[string]string, map[string]string) ([]StaticInterfaceConfiguration, []DHCPInterfaceConfiguration, error)
}

type interfaceConfigurationCreator struct {
	logger boshlog.Logger
	logTag string
}

func NewInterfaceConfigurationCreator(logger boshlog.Logger) InterfaceConfigurationCreator {
	return interfaceConfigurationCreator{
		logger: logger,
		logTag: "interfaceConfigurationCreator",
	}
}

func (creator interfaceConfigurationCreator) createInterfaceConfiguration(staticConfigs []StaticInterfaceConfiguration, dhcpConfigs []DHCPInterfaceConfiguration, ifaceName string, networkSettings boshsettings.Network) ([]StaticInterfaceConfiguration, []DHCPInterfaceConfiguration, error) {
	creator.logger.Debug(creator.logTag, "Creating network configuration with settings: %s", networkSettings)

	if (networkSettings.IsDHCP() || networkSettings.Mac == "") && networkSettings.Alias == "" {
		creator.logger.Debug(creator.logTag, "Using dhcp networking")
		dhcpConfigs = append(dhcpConfigs, DHCPInterfaceConfiguration{
			Name:         ifaceName,
			PostUpRoutes: networkSettings.Routes,
			Address:      networkSettings.IP,
		})
	} else {
		creator.logger.Debug(creator.logTag, "Using static networking")
		networkAddress, broadcastAddress, err := boshsys.CalculateNetworkAndBroadcast(networkSettings.IP, networkSettings.Netmask)
		if err != nil {
			return nil, nil, bosherr.WrapError(err, "Calculating Network and Broadcast")
		}

		conf := StaticInterfaceConfiguration{
			Name:                ifaceName,
			Address:             networkSettings.IP,
			Netmask:             networkSettings.Netmask,
			Network:             networkAddress,
			IsDefaultForGateway: networkSettings.IsDefaultFor("gateway"),
			Broadcast:           broadcastAddress,
			Mac:                 networkSettings.Mac,
			Gateway:             networkSettings.Gateway,
			PostUpRoutes:        networkSettings.Routes,
		}
		staticConfigs = append(staticConfigs, conf)
	}
	return staticConfigs, dhcpConfigs, nil
}

func (creator interfaceConfigurationCreator) CreateInterfaceConfigurations(networks boshsettings.Networks, physicalInterfacesByMAC map[string]string, virtualInterfacesByMAC map[string]string) ([]StaticInterfaceConfiguration, []DHCPInterfaceConfiguration, error) {
	// In cases where we only have one network and it has no MAC address (either because the IAAS doesn't give us one or
	// it's an old CPI), if we only have one interface, we should map them
	if len(networks) == 1 && len(physicalInterfacesByMAC) == 1 {
		networkSettings := creator.getFirstNetwork(networks)
		if networkSettings.Mac == "" {
			var ifaceName string
			networkSettings.Mac, ifaceName = creator.getFirstInterface(physicalInterfacesByMAC)
			return creator.createInterfaceConfiguration([]StaticInterfaceConfiguration{}, []DHCPInterfaceConfiguration{}, ifaceName, networkSettings)
		}
	}

	return creator.createMultipleInterfaceConfigurations(networks, physicalInterfacesByMAC, virtualInterfacesByMAC)
}

func (creator interfaceConfigurationCreator) createMultipleInterfaceConfigurations(networks boshsettings.Networks, physicalInterfacesByMAC map[string]string, virtualInterfacesByMAC map[string]string) ([]StaticInterfaceConfiguration, []DHCPInterfaceConfiguration, error) {
	if !networks.HasInterfaceAlias() && len(physicalInterfacesByMAC)+len(virtualInterfacesByMAC) < len(networks) {
		return nil, nil, bosherr.Errorf("Number of network settings '%d' is greater than the number of network devices '%d'", len(networks), len(physicalInterfacesByMAC)+len(virtualInterfacesByMAC))
	}

	interfacesToConfigure := map[string]string{}
	for mac, ifaceName := range physicalInterfacesByMAC {
		interfacesToConfigure[mac] = ifaceName
	}

	for name := range networks {
		if mac := networks[name].Mac; mac != "" {
			if _, ok := physicalInterfacesByMAC[mac]; ok {
				continue
			}
			// The target physical interface may be moved to another namespace and a virtual interface is created with the same mac address to replace it
			if ifaceName, ok := virtualInterfacesByMAC[mac]; ok {
				interfacesToConfigure[mac] = ifaceName
				continue
			}
			return nil, nil, bosherr.Errorf("No device found for network '%s' with MAC address '%s'", name, mac)
		}
	}

	// Configure interfaces with network settings matching MAC address.
	// If we cannot find a network setting with a matching MAC address, configure that interface as DHCP
	var networkSettings boshsettings.Network
	var err error
	staticConfigs := []StaticInterfaceConfiguration{}
	dhcpConfigs := []DHCPInterfaceConfiguration{}

	for mac, ifaceName := range interfacesToConfigure {
		networkSettings, _ = networks.NetworkForMac(mac)
		staticConfigs, dhcpConfigs, err = creator.createInterfaceConfiguration(staticConfigs, dhcpConfigs, ifaceName, networkSettings)
		if err != nil {
			return nil, nil, bosherr.WrapError(err, "Creating interface configuration")
		}
	}

	for _, networkSettings = range networks {
		if networkSettings.Mac != "" {
			continue
		}
		if networkSettings.Alias != "" {
			staticConfigs, dhcpConfigs, err = creator.createInterfaceConfiguration(staticConfigs, dhcpConfigs, networkSettings.Alias, networkSettings)
			if err != nil {
				return nil, nil, bosherr.WrapError(err, "Creating interface configuration using alias")
			}
		}
	}

	return staticConfigs, dhcpConfigs, nil
}

func (creator interfaceConfigurationCreator) getFirstNetwork(networks boshsettings.Networks) boshsettings.Network {
	for networkName := range networks {
		return networks[networkName]
	}
	return boshsettings.Network{}
}

func (creator interfaceConfigurationCreator) getFirstInterface(interfacesByMAC map[string]string) (string, string) {
	for mac := range interfacesByMAC {
		return mac, interfacesByMAC[mac]
	}
	return "", ""
}
