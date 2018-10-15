package net

type MACAddressDetector interface {
	DetectMacAddresses() (map[string]string, map[string]string, error)
}
